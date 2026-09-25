# Odonian fleet on Kubernetes

Run the worker/reviewer/merger fleet in-cluster instead of on a laptop. **Merger-first**: the merger
is non-LLM and repo-less (`odonian merge` is pure REST), so it proves the in-cluster plumbing with
zero subscription-auth risk. Workers and reviewers (which need `claude` + a build toolchain) come
next, on a separate, heavier image.

## Topology

- Two **separate** clusters: `summercamp-cp` (3× amd64) and `summercamp-lab` (8× arm64 Pis). They
  are not one mixed cluster, so fleet pods reach the server via its **public ingress**
  (`https://odonian.summercamp.eastharbor.casa`), not a cluster-DNS service name.
- **Placement:** mergers are cheap → run them on the **Pi (lab)** cluster. Workers/reviewers are
  memory-hungry (they build/test the target repo) → the **amd64 (cp)** cluster, with the 8 GB Pi as
  overflow. The real cap is the **subscription rate**, not node count — keep claude-agent replicas
  modest (the harness backs off on rate-limit).
- Images live in the internal registry `docker.summercamp.eastharbor.casa:32050/odonian/*`.

## Build + push the merger image (multi-arch)

```bash
make fleet-builder                     # ONCE: buildx builder that can push to the insecure (HTTP) registry
make merger-image                      # buildx → docker.summercamp.eastharbor.casa:32050/odonian/merger:latest
make merger-image FLEET_TAG=v1         # pin a tag
```

The internal registry serves **HTTP**, and multi-arch `--push` requires the `docker-container`
buildx driver — so a `daemon.json` `insecure-registries` entry is **not** enough; buildkit itself
must be told the registry is http. `make fleet-builder` creates a builder with that config (a
`buildkitd.toml` + `docker buildx create --driver docker-container`). `merger-image` then builds
`linux/amd64,linux/arm64` so the same tag runs on both clusters.

## Deployments are owned by manifests

Build and push images here. Update the matching pins in
[boldfield/manifests](https://github.com/boldfield/manifests), open a PR, and merge after review:

- `cp/odonian-fleet`: workers and reviewers, including the Codex auth init container.
- `lab/odonian-fleet`: mergers.
- `cp/argocd/apps/odonian-fleet-{cp,lab}.yaml`: ArgoCD applications.

ArgoCD applies the manifests. There are no deployment copies in this repository.
The retired `fleet-deploy`, `merger-deploy`, `diff-fleet`, and `verify-fleet-tags` targets
fail with a pointer to the manifests project. This prevents an old command from reverting
GitOps-managed image pins. Runtime credentials remain out of band; see `secret.example.yaml`
and the manifests fleet READMEs. Server deployment is a separate migration.

## Workers + reviewers (amd64 / cp cluster)

These run `claude -p` against a real build, so they use the heavier `Dockerfile.fleet` (claude CLI +
Go/Rust/Python/C toolchains + git/gh + the harness + odonian CLI). **amd64-only for now** — the
arm64 (Pi) build comes later with the cross-arch build/test dimension. The merger stays multi-arch
and keeps running on the Pis.

**Note on Go symlinks:** The Dockerfile creates symlinks from `/usr/local/go/bin/{go,gofmt}` to
`/usr/local/bin/` because the Codex-based gpt-5.5 reviewer rebuilds its shell's PATH and does not
inherit Docker `ENV` additions. Go binaries must be in `/usr/local/bin` or Go reviews fail spuriously.
The existing `PATH` env var in the Dockerfile still helps all other environments.

**Note on PyYAML:** The Dockerfile installs `python3-yaml` because the manifests repo and other
projects validate Kubernetes manifests through a capability ladder: `kubectl kustomize` if kubectl
is present, else a python3 `yaml.safe_load_all` parse. Without PyYAML, validation silently skips
and broken manifests would pass review. Installing the system package ensures the fallback validation works.

### 1. Build + push the fleet image

```sh
make fleet-builder   # once, if you haven't already (insecure-registry buildx builder)
make fleet-image     # builds linux/amd64, pushes to the internal registry
```

### 2. Subscription auth secret (token never goes through git/logs)

Each worker/reviewer authenticates claude with a long-lived **`claude setup-token`** value (a
subscription OAuth token, NOT an API key). Generate it on your laptop and create the secret directly
so the value never transits anything else:

```sh
claude setup-token   # prints a token; copy it
kubectl --context admin@summercamp-cp -n odonian-fleet \
  create secret generic claude-oauth --from-literal=token='<paste-token>'
```

The `odonian-fleet` (server API token) and `odonian-forge-tokens` secrets from the merger setup are
reused — create them in this namespace on the cp cluster too if they aren't there yet.

### 2b. codex auth for gpt-5.5 reviewers — READ THIS, IT EXPIRES

Reviewers that run `gpt-5.5` (via `AGENT_CODEX_MODELS`) authenticate codex with the `codex-auth`
secret, seeded from `~/.codex/auth.json` (see `secret.example.yaml`). Unlike the claude
`setup-token`, **this one decays and will take your review queue down.**

Under `auth_mode: chatgpt`, codex uses **refresh-token rotation**: every refresh mints a new refresh
token and *revokes the previous one*. The secret is therefore a **snapshot**. The
`codex-auth-setup` initContainer copies it into a writable `codex-home` emptyDir **only at pod
start**, and each pod rotates its own copy independently, never writing back. So every reviewer
replica plus any machine where you run `codex` locally are all rotating the same lineage and
revoking each other. Expect it to break periodically — a 4-replica fleet survived ~13 days.

Symptom: reviewers log `Your access token could not be refreshed because your refresh token was
revoked` plus `401 Unauthorized`, and every `gpt-5.5` review dispatch exits `rc=1`. Because the
harness peeks the queue head without claiming it, the failing task stays at the head and stalls the
whole review queue rather than just its own task.

Check and repair:

```sh
make codex-auth-check    # compares local vs cluster token freshness
codex login              # interactive; rotates to a fresh token locally
make codex-auth          # re-seeds the secret AND rolls the reviewers
```

The rollout restart is **mandatory** — updating the secret alone does nothing, because the
initContainer only reads it at pod start.

Running exactly one codex-capable reviewer reduces the churn (one rotation lineage instead of N) at
the cost of `gpt-5.5` review throughput. `auth_mode: apikey` avoids rotation entirely but moves you
from subscription to API billing.

### 3. Request the rollout in manifests

Verify the image tag exists in the internal registry, update every corresponding pin in
`boldfield/manifests`, and open the rollout PR there. ArgoCD deploys after the human merge gate.
Preserve existing replica counts and authentication Secret references. Do not apply workload
YAML or change workload images directly from this repository.

### Repo clone cache (multi-project mode)

In multi-project mode (`ODONIAN_PROJECT=all`, the fleet default) each pod clones every repo it
ever touches into `$ODONIAN_HOME/repos`. Reviewers accumulate the widest set — one clone per repo
they've ever reviewed — because they poll across all boards. On 2026-08-07 that unbounded cache
filled the 20Gi `emptyDir` HOME and got 37 pods evicted (23 reviewer / 14 worker) with `Usage of
EmptyDir volume "home" exceeds the limit "20Gi"`, with no node under disk/memory/PID pressure — it
was purely the per-pod cap.

`harness/agent.sh` now prunes that cache itself, once per dispatch, before setting up the task's
clone/worktree: it measures `$ODONIAN_HOME` usage and, once it crosses a high-water mark, deletes
the least-recently-used clones (oldest `.odonian-last-used` marker first, never the clone the
in-flight task needs) until usage is back at or under a low-water mark. Two env vars tune it:

- `ODONIAN_REPOS_HIGH_GIB` (default `14`) — usage above this triggers a prune pass.
- `ODONIAN_REPOS_LOW_GIB` (default `8`) — prune deletes clones until usage is at or below this.

Defaults are sized for the current 20Gi emptyDir (being raised to 30Gi separately as breathing
room, not as a substitute for this bound), leaving headroom for worktrees and tool caches, which
also live under `$ODONIAN_HOME` but are not touched by this prune — only `$ODONIAN_HOME/repos`
is in scope.

### PDF rendering and source inspection

The fleet image includes Poppler utilities (`pdfinfo`, `pdftoppm`, `pdftotext`) for PDF rendering
and text extraction, used by research workflows that inspect source PDFs.

**Source-inspection procedure:**

To render or inspect a PDF file within a worker/reviewer pod:

1. **Get PDF metadata:** `pdfinfo /path/to/file.pdf` — lists page count, dimensions, encryption
   status, and other document properties. Redacted PDFs report `text "cannot" be extracted`.
2. **Render a page to image:** `pdftoppm -png -singlefile -f 2 -l 2 /path/to/file.pdf /tmp/page` —
   renders page 2 of the PDF to a PNG image at `/tmp/page.png`. Use `-f` and `-l` to specify the
   page range; `-png` sets the output format.
3. **Extract text from a page:** `pdftotext -f 2 -l 2 /path/to/file.pdf -` — extracts text from
   pages 2 through 2, writing to stdout. If the PDF is redacted or malformed, extraction may fail
   silently (producing empty output), which is NOT treated as proof of redaction without visual
   verification.

**Performance note:** Large PDFs or full-document operations (extracting all pages) can be
memory-intensive. Extract specific page ranges when possible. Rendering outputs (images and
extracted text) are ephemeral — store them in temporary directories (`/tmp` or pod-local
`emptyDir` volumes) and clean them up to avoid filling the pod's home volume.

### Releasing a new fleet image

1. Build and push an explicit version: `make fleet-image VERSION=<version> FLEET_TAG=<version>`
   and `make merger-image VERSION=<version> FLEET_TAG=<version>`.
2. Verify the published registry manifests and architectures before proposing image pins.
3. Open the rollout PR in `boldfield/manifests`, including the image digest and validation.
4. After review and merge, observe the ArgoCD fleet applications and deployment rollouts.

Do not reuse an existing release tag or use `:latest` for deployed workloads. If image construction
uses an existing runtime base, record its digest and the replacement CLI/harness source revision
in the rollout PR. A rollback is another reviewed change to the manifest pins.
