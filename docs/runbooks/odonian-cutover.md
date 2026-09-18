# Cutover runbook: agentask → odonian

The repo-side rename (module path, binaries, env vars, manifests, skills, docs) lands in one
PR; this runbook is the operational half. Everything here is human-run. Names on the left of
each `→` are the pre-rename values still live in the clusters.

Decided 2026-08-17. Naming notes: CLI is `odonian` — do **not** alias it to `odo` (that is
Red Hat's OpenShift developer CLI). ntfy topics are dashed `odonian-<action>` (ntfy 404s dots).

## 0. Pre-flight (before merging the PR)

- [ ] Register `odonian.dev` and `odonian.io`.
- [ ] Claim `odonian` on PyPI and npm (placeholder publishes).
- [ ] Rename the GitHub repo `boldfield/agentask` → `boldfield/odonian`. GitHub redirects
      git, API, and `go get` for the old name, so nothing breaks ahead of the code landing.
- [ ] `git remote set-url origin git@github.com:boldfield/odonian.git`

## 1. Merge the rename PR, cut a release

- [ ] Merge, then on `main`: `make release VERSION=vX.Y.Z` — first push creates the
      `ghcr.io/boldfield/odonian` package (check ghcr auth; make the package visibility match
      the old one).
- [ ] Fleet images: `make merger-image` and `make fleet-image` — these now push
      `$(FLEET_REGISTRY)/odonian/{merger,fleet}`. The buildx builder (`odonian-fleet` after
      the rename) may need recreating: `make fleet-builder`.

## 2. Server cutover (SERVER_CONTEXT, ns `agentask` → `odonian`)

The server deployment is imperative (only `make deploy` mutates it), so recreate it in the
new namespace. The DB PVC is the only real state; the image is distroless (no exec/cp into
the server pod) — use throwaway busybox pods to move data, per the established pattern.

- [ ] Quiesce: `kubectl -n agentask scale deploy/agentask --replicas=0`
- [ ] Find the PVC: `kubectl -n agentask get pvc`
- [ ] Back up: run a busybox pod in `agentask` mounting the PVC; `kubectl cp` the DB
      (all of `*.db*` — include WAL/SHM if present) to the workstation. **Verify the copy
      opens with `sqlite3` before proceeding.**
- [ ] `kubectl create ns odonian`
- [ ] Recreate config: export `deploy/agentask`, its Service (and NodePort/Ingress if any),
      and secrets (`agentask-secret`, `agentask-bearer-token`) with
      `kubectl -n agentask get <kind> <name> -o yaml`, strip status/uid fields,
      `sed 's/agentask/odonian/g'`, apply into `odonian`. Scale the new deploy to 0 first.
- [ ] New PVC (same spec, name sedded) in `odonian`; busybox pod mounting it; `kubectl cp`
      the DB in; ownership/permissions to match the old volume.
- [ ] `make deploy VERSION=vX.Y.Z` (Makefile now targets `-n odonian deploy/odonian`).
- [ ] Smoke: hit the API with the bearer token; confirm boards/tasks are all present.

## 3. Fleet cutover (CP + LAB contexts, ns `agentask-fleet` → `odonian-fleet`)

Fleet state is disposable: the repos PVCs are cache, pods are stateless. Fresh namespace,
recreated secrets, clean apply.

- [ ] On both clusters: `kubectl create ns odonian-fleet`
- [ ] Recreate secrets from the old namespace (values unchanged, names/namespace sedded):
      bearer token, `agentask-forge-tokens` (per-owner PATs), `agentask-codex-auth`.
      Remember the codex mount gotcha: `~/.codex` must be writable (initContainer copy) —
      the manifests already encode this; don't "fix" it while sedding.
- [ ] Fresh repos-cache PVCs per the manifests (no data to migrate).
- [ ] Update `cp/odonian-fleet` and `lab/odonian-fleet` in `boldfield/manifests`, merge the reviewed PR, and verify ArgoCD convergence, with the
      new images from step 1. Manifests now point at `odonian-fleet` and
      `.../odonian/{fleet,merger}` images.
- [ ] `make versions` — all four rows healthy.
- [ ] End-to-end smoke: create a trivial task, watch a worker claim → PR → review → merge.

## 4. Notifications

- [ ] notifier-proxy is generic (`<system>-<action>`) — no proxy change. Topics auto-create
      on first publish.
- [ ] Re-subscribe the phone to the `odonian-*` topics (blocked, review, merged, failed, …);
      unsubscribe `agentask-*` after soak.

## 5. Workstation + sandbox

- [ ] `mv ~/.agentask ~/.odonian`; rename any `AGENTASK_*` vars in shell rc / `.env` files.
- [ ] `mv ~/projects/agentask ~/projects/odonian`, then **copy the Claude Code project data**
      so session memory survives the path change:
      `cp -R ~/.claude/projects/-Users-boldfield-projects-agentask ~/.claude/projects/-Users-boldfield-projects-odonian`
- [ ] Local dev DB: `mv agentask.db odonian.db` (plus `-wal`/`-shm` if present) in the repo,
      or set `ODONIAN_DB` explicitly.
- [ ] Recreate the sandbox: delete `claude-agentask-sbx`; `harness/sbx.sh` provisions
      `claude-odonian-sbx` (project auto-create handles the board side), then
      `make sbx-codex-auth`.

## 6. Decommission (after ≥1 week soak)

- [ ] Confirm the DB backup from step 2 still opens.
- [ ] `kubectl delete ns agentask` (server context); `kubectl delete ns agentask-fleet`
      (both clusters).
- [ ] Optional cleanup: board project rows still storing `github.com/boldfield/agentask`
      URLs work via GitHub's redirect indefinitely; update them via the API only if the
      redirect ever becomes a problem.
- [ ] Let `agenta.sk` lapse if it was purchased.
