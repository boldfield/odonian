#!/usr/bin/env python3
"""Rotate or inspect fleet subscription credentials without logging secret material."""

import argparse
import base64
import binascii
import getpass
import hashlib
import hmac
import json
import os
from pathlib import Path
import subprocess
import sys
import warnings


SECRET_NAME = "claude-oauth"
TOKEN_ENV = "CLAUDE_CODE_OAUTH_TOKEN"
DEPLOYMENTS = ("worker", "reviewer")
POD_DIGEST_COMMAND = (
    'test -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" || exit 3; '
    'printf %s "$CLAUDE_CODE_OAUTH_TOKEN" | sha256sum'
)


class AuthError(Exception):
    """Only sanitized, operator-facing messages belong in this exception."""


def validate_token(token):
    token = token.rstrip("\r\n")
    if not token or any(character.isspace() for character in token):
        raise AuthError("Token must be nonempty and contain no whitespace.")
    if any(not 33 <= ord(character) <= 126 for character in token):
        raise AuthError("Token must contain only printable ASCII characters.")
    return token


def read_token(required=False):
    if "CLAUDE_AUTH_TOKEN_FILE" in os.environ:
        try:
            token = Path(os.environ["CLAUDE_AUTH_TOKEN_FILE"]).read_text()
        except (OSError, UnicodeError, ValueError):
            raise AuthError("Cannot read CLAUDE_AUTH_TOKEN_FILE; no fallback attempted.") from None
    elif TOKEN_ENV in os.environ:
        token = os.environ[TOKEN_ENV]
    elif required:
        # getpass otherwise falls back to an echoed stdin read when no TTY exists.
        try:
            with open("/dev/tty", "r+") as terminal:
                if not terminal.isatty():
                    raise OSError("not a terminal")
                with warnings.catch_warnings():
                    warnings.simplefilter("error", getpass.GetPassWarning)
                    token = getpass.getpass("Paste token from claude setup-token: ", stream=terminal)
        except (OSError, EOFError, getpass.GetPassWarning):
            raise AuthError(
                "Run claude setup-token, then use an interactive terminal, "
                "CLAUDE_AUTH_TOKEN_FILE or inherited CLAUDE_CODE_OAUTH_TOKEN."
            ) from None
    else:
        return None
    return validate_token(token)


class FleetAuth:
    def __init__(self, context, namespace):
        self.context = context
        self.namespace = namespace

    def kubectl(self, arguments, payload=None, timeout=45):
        command = ["kubectl", "--context", self.context, "--namespace", self.namespace]
        command += ["--request-timeout=30s"] + arguments
        environment = os.environ.copy()
        environment.pop(TOKEN_ENV, None)
        environment.pop("CLAUDE_AUTH_TOKEN_FILE", None)
        try:
            result = subprocess.run(
                command, input=None if payload is None else json.dumps(payload),
                text=True, capture_output=True, timeout=timeout, env=environment,
            )
        except FileNotFoundError:
            raise AuthError("kubectl is not installed.") from None
        except subprocess.TimeoutExpired:
            raise AuthError("Kubernetes operation timed out; check connectivity and rollout state.") from None
        except OSError:
            raise AuthError("Could not execute kubectl.") from None
        if result.returncode:
            # kubectl errors can quote request bodies. Never forward its raw output.
            error_text = result.stderr.lower()
            if "forbidden" in error_text or "unauthorized" in error_text:
                reason = "access denied; check Kubernetes credentials and RBAC"
            elif "notfound" in error_text or "not found" in error_text:
                reason = "resource not found"
            elif "conflict" in error_text:
                reason = "resource changed concurrently; retry"
            else:
                reason = "failed; check connectivity, resource state and command permissions"
            raise AuthError("Kubernetes " + arguments[0] + " " + reason + ".")
        return result.stdout

    def get(self, resource, name=None, optional=False, selector=None):
        arguments = ["get", resource]
        if name:
            arguments.append(name)
        if optional:
            arguments.append("--ignore-not-found")
        if selector:
            arguments.extend(["--selector", selector])
        arguments.extend(["-o", "json"])
        output = self.kubectl(arguments)
        if optional and not output.strip():
            return None
        try:
            value = json.loads(output)
            if not isinstance(value, dict):
                raise ValueError()
            return value
        except ValueError:
            raise AuthError("Kubernetes returned an invalid resource response.") from None

    def update(self, token):
        token = validate_token(token)
        secret = self.get("secret", SECRET_NAME, optional=True)
        encoded_token = base64.b64encode(token.encode()).decode()
        if secret is None:
            payload = {
                "apiVersion": "v1", "kind": "Secret", "type": "Opaque",
                "metadata": {"name": SECRET_NAME, "namespace": self.namespace},
                "data": {"token": encoded_token},
            }
            self.kubectl(["create", "-f", "-", "-o", "name"], payload)
        else:
            # Merge only this key; resourceVersion prevents overwriting a concurrent rotation.
            payload = {
                "metadata": {"resourceVersion": secret["metadata"]["resourceVersion"]},
                "data": {"token": encoded_token},
            }
            self.kubectl(
                ["patch", "secret", SECRET_NAME, "--type=merge", "--patch-file=/dev/stdin", "-o", "name"],
                payload,
            )
        print("Claude Secret updated. Restarting worker/reviewer; in-flight work may be interrupted.")
        try:
            for deployment in DEPLOYMENTS:
                self.kubectl(["rollout", "restart", "deployment/" + deployment])
                print(deployment + ": restart requested")
            for deployment in DEPLOYMENTS:
                self.kubectl(
                    ["rollout", "status", "deployment/" + deployment, "--timeout=300s"], timeout=330,
                )
                print(deployment + ": rollout complete")
            return self.check(token)
        except AuthError as error:
            raise AuthError("Secret was updated, but rollout/verification is incomplete. " + str(error)) from None

    def check(self, local_token=None):
        secret = self.get("secret", SECRET_NAME, optional=True)
        if secret is None:
            raise AuthError("claude-oauth Secret is missing; run make claude-auth.")
        try:
            token = base64.b64decode(secret.get("data", {}).get("token", ""), validate=True)
            validate_token(token.decode("ascii"))
        except (ValueError, UnicodeError, binascii.Error, AuthError):
            raise AuthError("claude-oauth token key is missing, empty or malformed.") from None
        digest = hashlib.sha256(token).hexdigest()
        healthy = True
        print("cluster: token present")
        if local_token is None:
            print("local comparison: not requested (no file/environment token)")
        else:
            matches = hmac.compare_digest(local_token.encode(), token)
            healthy = healthy and matches
            print("local comparison: " + ("matches" if matches else "MISMATCH"))
        for deployment_name in DEPLOYMENTS:
            try:
                deployment_ok = self.check_deployment(deployment_name, digest)
                healthy = healthy and deployment_ok
            except AuthError as error:
                healthy = False
                print(deployment_name + ": UNKNOWN — " + str(error))
        print("Live provider authentication and subscription allowance were not tested.")
        if not healthy:
            raise AuthError(
                "Credential synchronization is incomplete. Resolve access/readiness errors or "
                "run make claude-auth with a fresh setup-token."
            )
        print("Credentials synchronized across worker/reviewer pods.")
        return 0

    def check_deployment(self, name, digest):
        deployment = self.get("deployment", name)
        spec = deployment["spec"]
        replicas = spec.get("replicas", 1)
        status = deployment.get("status", {})
        containers = []
        for container in spec["template"]["spec"]["containers"]:
            entries = [entry for entry in container.get("env", []) if entry["name"] == TOKEN_ENV]
            if not entries:
                continue
            reference = entries[0].get("valueFrom", {}).get("secretKeyRef", {})
            if (len(entries) != 1 or reference.get("name") != SECRET_NAME
                    or reference.get("key") != "token" or "value" in entries[0]):
                raise AuthError("Deployment has an unexpected Claude credential reference.")
            containers.append(container["name"])
        if not containers:
            raise AuthError("Deployment does not reference claude-oauth/token.")
        if replicas == 0:
            raise AuthError("Scaled to zero; no running credential can be verified.")
        selector = label_selector(spec["selector"])
        pods = self.get("pods", selector=selector).get("items", [])
        pods = [pod for pod in pods if not pod["metadata"].get("deletionTimestamp")]
        healthy = (
            len(pods) == replicas
            and status.get("observedGeneration", 0) >= deployment["metadata"].get("generation", 1)
            and status.get("updatedReplicas", 0) == replicas
            and status.get("readyReplicas", 0) == replicas
        )
        if not healthy:
            print(name + ": rollout/replica readiness incomplete")
        for pod in pods:
            pod_name = pod["metadata"]["name"]
            statuses = {item["name"]: item for item in pod.get("status", {}).get("containerStatuses", [])}
            for container in containers:
                container_status = statuses.get(container, {})
                if not container_status.get("ready") or "running" not in container_status.get("state", {}):
                    healthy = False
                    print(pod_name + "/" + container + ": NOT READY (credential unverified)")
                    continue
                try:
                    observed = self.kubectl([
                        "exec", pod_name, "-c", container, "--", "sh", "-c", POD_DIGEST_COMMAND,
                    ]).split()
                    matches = bool(observed) and hmac.compare_digest(observed[0], digest)
                    healthy = healthy and matches
                    print(pod_name + "/" + container + ": " + ("matches" if matches else "MISMATCH"))
                except AuthError as error:
                    healthy = False
                    print(pod_name + "/" + container + ": UNKNOWN — " + str(error))
        return healthy


def label_selector(selector):
    terms = [key + "=" + value for key, value in sorted(selector.get("matchLabels", {}).items())]
    for expression in selector.get("matchExpressions", []):
        key, operation = expression["key"], expression["operator"]
        if operation in ("In", "NotIn"):
            terms.append(key + (" in (" if operation == "In" else " notin (") + ",".join(expression["values"]) + ")")
        elif operation in ("Exists", "DoesNotExist"):
            terms.append(("!" if operation == "DoesNotExist" else "") + key)
        else:
            raise AuthError("Unsupported deployment label selector.")
    if not terms:
        raise AuthError("Deployment selector is empty; refusing to inspect unrelated pods.")
    return ",".join(terms)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("update", "check"))
    parser.add_argument("--context", required=True)
    parser.add_argument("--namespace", required=True)
    args = parser.parse_args()
    print("Claude fleet auth: context=" + args.context + " namespace=" + args.namespace)
    try:
        token = read_token(required=args.operation == "update")
        fleet = FleetAuth(args.context, args.namespace)
        return fleet.update(token) if args.operation == "update" else fleet.check(token)
    except AuthError as error:
        print("ERROR: " + str(error), file=sys.stderr)
        return 1
    except (KeyError, TypeError, ValueError):
        # Malformed resource data must not leak a Secret through a traceback.
        print("ERROR: Kubernetes resource data is malformed; synchronization is unverified.", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("Interrupted; inspect rollout state before retrying.", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
