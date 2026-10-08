import base64
import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import claude_auth


TOKEN = "synthetic-claude-credential-do-not-print"
ENCODED_TOKEN = base64.b64encode(TOKEN.encode()).decode()


class FakeKubectl:
    def __init__(self):
        self.calls = []
        self.secret = {
            "metadata": {"name": "claude-oauth", "resourceVersion": "7", "labels": {"keep": "yes"}},
            "data": {"token": ENCODED_TOKEN, "unrelated": "cHJlc2VydmU="},
        }
        self.deployments = {}
        self.pods = {}
        self.mismatch = set()
        self.failure = None
        for name in ("worker", "reviewer"):
            self.deployments[name] = {
                "metadata": {"generation": 1},
                "spec": {
                    "replicas": 2, "selector": {"matchLabels": {"app": name}},
                    "template": {"spec": {"containers": [{
                        "name": name, "env": [{"name": claude_auth.TOKEN_ENV, "valueFrom": {
                            "secretKeyRef": {"name": "claude-oauth", "key": "token"},
                        }}],
                    }]}},
                },
                "status": {"observedGeneration": 1, "updatedReplicas": 2, "readyReplicas": 2},
            }
            self.pods[name] = [{
                "metadata": {"name": name + "-" + str(index)},
                "status": {"containerStatuses": [{"name": name, "ready": True, "state": {"running": {}}}]},
            } for index in range(2)]

    def run(self, command, **options):
        arguments = command[6:]
        self.calls.append((command, options))
        if self.failure and self.failure(arguments):
            # Even provider/kubectl diagnostics containing the token must be suppressed.
            return subprocess.CompletedProcess(command, 1, TOKEN, "Forbidden " + TOKEN)
        result = ""
        if arguments[:2] == ["get", "secret"]:
            result = "" if self.secret is None else json.dumps(self.secret)
        elif arguments[:2] == ["get", "deployment"]:
            result = json.dumps(self.deployments[arguments[2]])
        elif arguments[:2] == ["get", "pods"]:
            name = arguments[arguments.index("--selector") + 1].split("=")[1]
            result = json.dumps({"items": self.pods[name]})
        elif arguments[0] == "exec":
            result = hashlib.sha256(("different" if arguments[1] in self.mismatch else TOKEN).encode()).hexdigest() + "  -\n"
        elif arguments[0] == "patch":
            payload = json.loads(options["input"])
            self.secret["data"].update(payload["data"])
        elif arguments[0] == "create":
            self.secret = json.loads(options["input"])
        elif arguments[:2] not in (["rollout", "restart"], ["rollout", "status"]):
            raise AssertionError("Unexpected kubectl operation: " + repr(arguments))
        return subprocess.CompletedProcess(command, 0, result, "")


class FleetAuthTests(unittest.TestCase):
    def setUp(self):
        self.fake = FakeKubectl()
        self.fleet = claude_auth.FleetAuth("test-context", "test-namespace")
        self.output = io.StringIO()
        self.addCleanup(patch.stopall)
        patch("claude_auth.subprocess.run", side_effect=self.fake.run).start()
        patch.dict(os.environ, {claude_auth.TOKEN_ENV: TOKEN}, clear=True).start()
        self.redirect = contextlib.redirect_stdout(self.output)
        self.redirect.__enter__()
        self.addCleanup(self.redirect.__exit__, None, None, None)

    def tearDown(self):
        for secret in (TOKEN, ENCODED_TOKEN, hashlib.sha256(TOKEN.encode()).hexdigest()):
            self.assertNotIn(secret, self.output.getvalue())
        for command, options in self.fake.calls:
            self.assertEqual(command[:6], ["kubectl", "--context", "test-context", "--namespace", "test-namespace", "--request-timeout=30s"])
            self.assertNotIn(TOKEN, repr(command))
            self.assertNotIn(ENCODED_TOKEN, repr(command))
            self.assertNotIn(claude_auth.TOKEN_ENV, options["env"])

    def test_update_preserves_other_secret_fields_and_checks_every_replica(self):
        original = copy.deepcopy(self.fake.secret)
        self.fake.secret["data"]["token"] = base64.b64encode(b"previous-token").decode()
        self.assertEqual(self.fleet.update(TOKEN), 0)
        self.assertEqual(self.fake.secret, original)
        calls = [command[6:] for command, _ in self.fake.calls]
        self.assertEqual([call[2] for call in calls if call[:2] == ["rollout", "restart"]], ["deployment/worker", "deployment/reviewer"])
        self.assertEqual(sum(call[0] == "exec" for call in calls), 4)
        patch_call = next(options for command, options in self.fake.calls if command[6] == "patch")
        self.assertEqual(json.loads(patch_call["input"])["metadata"], {"resourceVersion": "7"})
        self.assertIn("were not tested", self.output.getvalue())

    def test_update_creates_missing_secret(self):
        self.fake.secret = None
        self.fleet.update(TOKEN)
        self.assertEqual(self.fake.secret["metadata"]["namespace"], "test-namespace")
        self.assertEqual(self.fake.secret["data"]["token"], ENCODED_TOKEN)

    def test_write_failure_never_restarts_and_redacts_error(self):
        self.fake.failure = lambda arguments: arguments[0] == "patch"
        with self.assertRaisesRegex(claude_auth.AuthError, "access denied") as error:
            self.fleet.update(TOKEN)
        self.assertNotIn(TOKEN, str(error.exception))
        self.assertFalse(any(command[6] == "rollout" for command, _ in self.fake.calls))

    def test_rollout_failure_reports_partial_success(self):
        self.fake.failure = lambda arguments: arguments[:2] == ["rollout", "status"]
        with self.assertRaisesRegex(claude_auth.AuthError, "Secret was updated.*incomplete"):
            self.fleet.update(TOKEN)

    def test_check_is_read_only_and_optional_local_comparison_is_not_requested(self):
        self.fleet.check()
        self.assertIn("not requested", self.output.getvalue())
        self.assertTrue(all(command[6] in ("get", "exec") for command, _ in self.fake.calls))
        self.assertTrue(all("sha256sum" in command[-1] for command, _ in self.fake.calls if command[6] == "exec"))

    def test_one_stale_replica_fails_check(self):
        self.fake.mismatch.add("reviewer-1")
        with self.assertRaisesRegex(claude_auth.AuthError, "synchronization is incomplete"):
            self.fleet.check(TOKEN)
        self.assertIn("reviewer-1/reviewer: MISMATCH", self.output.getvalue())

    def test_exec_denied_is_unknown_not_healthy(self):
        self.fake.failure = lambda arguments: arguments[0] == "exec"
        with self.assertRaises(claude_auth.AuthError):
            self.fleet.check()
        self.assertIn("UNKNOWN", self.output.getvalue())

    def test_local_mismatch_fails(self):
        with self.assertRaises(claude_auth.AuthError):
            self.fleet.check("different")
        self.assertIn("local comparison: MISMATCH", self.output.getvalue())

    def test_missing_and_empty_secret(self):
        for secret in (None, {"data": {}}, {"data": {"token": "%%%"}}):
            with self.subTest(secret=secret):
                self.fake.secret = secret
                with self.assertRaises(claude_auth.AuthError):
                    self.fleet.check()

    def test_wrong_reference_scaled_zero_missing_unready_and_rolling_pods(self):
        mutations = [
            lambda: self.fake.deployments["worker"]["spec"]["template"]["spec"]["containers"][0]["env"][0].update({"value": "bad"}),
            lambda: self.fake.deployments["worker"]["spec"].update({"replicas": 0}),
            lambda: self.fake.pods.update({"worker": []}),
            lambda: self.fake.pods["worker"][0]["status"]["containerStatuses"][0].update({"ready": False}),
            lambda: self.fake.deployments["worker"]["status"].update({"updatedReplicas": 1}),
        ]
        for mutation in mutations:
            self.fake = FakeKubectl()
            patch("claude_auth.subprocess.run", side_effect=self.fake.run).start()
            mutation()
            with self.assertRaises(claude_auth.AuthError):
                self.fleet.check()

    def test_transport_failure_and_timeout_are_sanitized(self):
        self.fake.failure = lambda arguments: True
        with self.assertRaisesRegex(claude_auth.AuthError, "access denied"):
            self.fleet.check()
        with patch("claude_auth.subprocess.run", side_effect=subprocess.TimeoutExpired([TOKEN], 1, output=TOKEN)):
            with self.assertRaisesRegex(claude_auth.AuthError, "timed out") as error:
                self.fleet.check()
            self.assertNotIn(TOKEN, str(error.exception))

    def test_command_returns_nonzero_without_raw_error_output(self):
        self.fake.failure = lambda arguments: True
        errors = io.StringIO()
        with patch("sys.argv", ["claude_auth.py", "check", "--context", "test-context", "--namespace", "test-namespace"]):
            with contextlib.redirect_stderr(errors):
                self.assertEqual(claude_auth.main(), 1)
        self.assertIn("access denied", errors.getvalue())
        self.assertNotIn(TOKEN, errors.getvalue())


class InputTests(unittest.TestCase):
    def test_file_precedes_environment_and_invalid_file_does_not_fall_back(self):
        with tempfile.TemporaryDirectory() as directory:
            token_file = Path(directory) / "token"
            token_file.write_text(TOKEN + "\n")
            with patch.dict(os.environ, {"CLAUDE_AUTH_TOKEN_FILE": str(token_file), claude_auth.TOKEN_ENV: "different"}, clear=True):
                self.assertEqual(claude_auth.read_token(), TOKEN)
                token_file.unlink()
                with self.assertRaises(claude_auth.AuthError):
                    claude_auth.read_token()

    def test_environment_and_empty_input(self):
        with patch.dict(os.environ, {claude_auth.TOKEN_ENV: TOKEN}, clear=True):
            self.assertEqual(claude_auth.read_token(), TOKEN)
        with patch.dict(os.environ, {}, clear=True):
            self.assertIsNone(claude_auth.read_token())
            with patch("builtins.open", side_effect=OSError()):
                with self.assertRaisesRegex(claude_auth.AuthError, "claude setup-token"):
                    claude_auth.read_token(required=True)
        for invalid in ("", "  ", "two tokens", "a\nb", "\x00bad", "é"):
            with self.subTest(invalid=repr(invalid)):
                with self.assertRaises(claude_auth.AuthError):
                    claude_auth.validate_token(invalid)

    def test_make_dry_run_never_expands_environment_token(self):
        environment = os.environ.copy()
        environment[claude_auth.TOKEN_ENV] = TOKEN
        repository = Path(__file__).resolve().parents[2]
        result = subprocess.run(
            ["make", "-n", "claude-auth", "claude-auth-check", "CP_CONTEXT=test-context", "FLEET_NAMESPACE=test-namespace"],
            cwd=repository, env=environment, text=True, capture_output=True, check=True,
        )
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        self.assertIn('--context "test-context" --namespace "test-namespace"', result.stdout)

    def test_selector_expressions(self):
        self.assertEqual(claude_auth.label_selector({
            "matchLabels": {"app": "worker"},
            "matchExpressions": [{"key": "role", "operator": "In", "values": ["worker", "reviewer"]}],
        }), "app=worker,role in (worker,reviewer)")
        with self.assertRaises(claude_auth.AuthError):
            claude_auth.label_selector({})


if __name__ == "__main__":
    unittest.main()
