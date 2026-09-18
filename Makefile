.PHONY: build run test tidy tui check release deploy fleet-builder merger-image fleet-image verify-fleet-tags fleet-deploy diff-fleet merger-deploy versions codex-auth codex-auth-check sbx-codex-auth

VERSION ?= $(shell git describe --tags --always --dirty)

# --- fleet images (internal registry, multi-arch for the amd64 + arm64 clusters) ---
FLEET_REGISTRY  ?= docker.summercamp.eastharbor.casa:32050
FLEET_TAG       ?= latest
FLEET_PLATFORMS ?= linux/amd64,linux/arm64
FLEET_BUILDER   ?= odonian-fleet

# Cluster contexts + namespace for fleet rollouts. Worker/reviewer run on the amd64 cp
# cluster; the merger runs on the arm64 lab (Pi) cluster. Override per-environment.
CP_CONTEXT      ?= admin@summercamp-cp
LAB_CONTEXT     ?= admin@summercamp-lab
FLEET_NAMESPACE ?= odonian-fleet

# Server deployment (the odonian API). SERVER_CONTEXT empty = use the CURRENT kube
# context, matching `make deploy` (which sets no --context).
SERVER_CONTEXT   ?=
SERVER_NAMESPACE ?= odonian

# `sbx` sandbox container the host-side codex auth seeding targets (see sbx-codex-auth below).
SBX_NAME ?= claude-odonian-sbx

build:
	mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o bin/odonian ./cmd/odonian

tui:
	mkdir -p bin
	go build -o bin/odonian-tui ./cmd/odonian-tui

run: build
	./bin/odonian

test:
	go test ./...

tidy:
	go mod tidy

check:
	@echo "Running gofmt check..."
	@out=$$(gofmt -e -l . 2>&1); rc=$$?; if [ "$$rc" -ne 0 ] || [ -n "$$out" ]; then echo "gofmt issues:"; echo "$$out"; exit 1; fi
	@echo "Running go vet..."
	@go vet ./...
	@echo "Checking go mod tidy..."
	@go mod tidy -diff || (echo "go.mod/go.sum not tidy; run 'make tidy'"; exit 1)

release:
	@if ! echo "$(VERSION)" | grep -qE "^v[0-9]+\.[0-9]+\.[0-9]+$$"; then echo "ERROR: VERSION must be a semantic version (e.g., v0.8.0). Usage: make release VERSION=vX.Y.Z"; exit 1; fi
	@if ! git diff --quiet; then echo "ERROR: Working tree has uncommitted changes"; exit 1; fi
	@if ! git diff --cached --quiet; then echo "ERROR: Index has staged changes"; exit 1; fi
	@if [ "$$(git rev-parse --abbrev-ref HEAD)" != "main" ]; then echo "ERROR: Not on main branch"; exit 1; fi
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t ghcr.io/boldfield/odonian:$(VERSION) .
	docker push ghcr.io/boldfield/odonian:$(VERSION)
	git tag -a $(VERSION) -m "$(VERSION)"
	git push origin $(VERSION)
	@echo "Released ghcr.io/boldfield/odonian:$(VERSION) (linux/amd64); deploy with: make deploy VERSION=$(VERSION)"

# One-time: create a buildx builder that can push to the INSECURE (HTTP) internal registry.
# Multi-arch `--push` needs the docker-container driver, so a daemon.json insecure-registries entry
# isn't enough — buildkit itself must mark the registry as http. Re-run any time to recreate it.
fleet-builder:
	@printf '[registry."%s"]\n  http = true\n  insecure = true\n' '$(FLEET_REGISTRY)' > /tmp/odonian-buildkitd.toml
	-docker buildx rm $(FLEET_BUILDER) 2>/dev/null
	docker buildx create --name $(FLEET_BUILDER) --driver docker-container \
	  --config /tmp/odonian-buildkitd.toml --bootstrap
	@echo "buildx builder '$(FLEET_BUILDER)' ready (insecure HTTP push to $(FLEET_REGISTRY))"

# Build + push the multi-arch MERGER fleet image to the internal registry.
# Requires `docker buildx` and the builder from `make fleet-builder` (run that once first).
merger-image:
	docker buildx build --builder $(FLEET_BUILDER) --platform $(FLEET_PLATFORMS) \
	  --build-arg VERSION=$(VERSION) \
	  -t $(FLEET_REGISTRY)/odonian/merger:$(FLEET_TAG) \
	  -t $(FLEET_REGISTRY)/odonian/merger:$(VERSION) \
	  -f deploy/fleet/Dockerfile.merger --push .
	@echo "Pushed $(FLEET_REGISTRY)/odonian/merger:$(FLEET_TAG) and :$(VERSION) ($(FLEET_PLATFORMS))"

# Build + push the heavy WORKER/REVIEWER fleet image (claude + toolchains). amd64-only for now (the
# cp cluster); arm64 comes with the cross-arch build/test dimension. Needs `make fleet-builder` once.
fleet-image:
	docker buildx build --builder $(FLEET_BUILDER) --platform linux/amd64 \
	  --build-arg VERSION=$(VERSION) \
	  -t $(FLEET_REGISTRY)/odonian/fleet:$(FLEET_TAG) \
	  -t $(FLEET_REGISTRY)/odonian/fleet:$(VERSION) \
	  -f deploy/fleet/Dockerfile.fleet --push .
	@echo "Pushed $(FLEET_REGISTRY)/odonian/fleet:$(FLEET_TAG) and :$(VERSION) (linux/amd64)"

# Fleet desired state is owned by boldfield/manifests and reconciled by ArgoCD.
# Keep old command names as explicit errors so stale runbooks cannot overwrite GitOps.
verify-fleet-tags fleet-deploy diff-fleet merger-deploy:
	@echo "Fleet deployment moved to https://github.com/boldfield/manifests (cp/odonian-fleet, lab/odonian-fleet)."
	@echo "Build/push images here, then update image pins there and merge the reviewed PR."
	@exit 1

# --- codex (gpt-5.5) reviewer auth ---
#
# codex runs under `auth_mode: chatgpt`, which uses refresh-token ROTATION: every refresh
# mints a new refresh token and REVOKES the previous one. The cluster secret is therefore a
# SNAPSHOT that decays — each reviewer pod seeds a writable ~/.codex emptyDir from it at pod
# start and rotates independently, never writing back. Multiple pods (and your laptop) sharing
# one lineage revoke each other, so expect to re-run `make codex-auth` periodically. Symptom:
# reviewers log "refresh token was revoked" + 401 and stall the review queue indefinitely.

# Read-only: compare the cluster secret's token freshness against the local one. A cluster
# timestamp much older than the local one means the reviewers are running a revoked token.
codex-auth-check:
	@LOCAL=$$(jq -r '.last_refresh // empty' "$$HOME/.codex/auth.json" 2>/dev/null); \
	[ -n "$$LOCAL" ] || LOCAL="(no local ~/.codex/auth.json)"; \
	RAW=$$(kubectl --context $(CP_CONTEXT) -n $(FLEET_NAMESPACE) get secret codex-auth \
	  -o jsonpath='{.data.auth\.json}' 2>/dev/null | base64 -d 2>/dev/null); \
	if [ -z "$$RAW" ]; then CLUSTER="(unreachable / secret not found)"; \
	else CLUSTER=$$(printf '%s' "$$RAW" | jq -r '.last_refresh // empty' 2>/dev/null); \
	  [ -n "$$CLUSTER" ] || CLUSTER="(unparseable secret)"; fi; \
	printf '%-10s %s\n' local "$$LOCAL"; \
	printf '%-10s %s\n' cluster "$$CLUSTER"; \
	case "$$LOCAL$$CLUSTER" in \
	  *"("*) echo "INDETERMINATE — could not compare (see above); NOT necessarily stale" ;; \
	  *) [ "$$LOCAL" = "$$CLUSTER" ] && echo "in sync" \
	       || echo "OUT OF SYNC — run 'codex login' then 'make codex-auth'" ;; \
	esac

# Re-seed the reviewers' codex auth from the local ~/.codex/auth.json, then re-roll them.
# Run `codex login` FIRST (interactive browser flow — cannot be automated here).
# The rollout restart is MANDATORY: the codex-auth-setup initContainer copies the secret into
# the writable codex-home emptyDir only at pod start, so updating the secret alone does nothing.
codex-auth:
	@test -f "$$HOME/.codex/auth.json" || { echo "ERROR: no ~/.codex/auth.json — run 'codex login' first"; exit 1; }
	@jq -e '.tokens.refresh_token // empty' "$$HOME/.codex/auth.json" >/dev/null 2>&1 \
	  || { echo "ERROR: ~/.codex/auth.json has no refresh token — re-run 'codex login'"; exit 1; }
	@echo "Seeding codex-auth secret from ~/.codex/auth.json ($(CP_CONTEXT))"
	@kubectl --context $(CP_CONTEXT) -n $(FLEET_NAMESPACE) create secret generic codex-auth \
	  --from-file=auth.json="$$HOME/.codex/auth.json" \
	  --dry-run=client -o yaml \
	  | kubectl --context $(CP_CONTEXT) -n $(FLEET_NAMESPACE) apply -f -
	kubectl --context $(CP_CONTEXT) -n $(FLEET_NAMESPACE) rollout restart deploy/reviewer
	kubectl --context $(CP_CONTEXT) -n $(FLEET_NAMESPACE) rollout status deploy/reviewer --timeout=300s
	@$(MAKE) --no-print-directory codex-auth-check

# Host-side half of sbx-agent-bootstrap Deliverable 2 (docs/features/sbx-agent-bootstrap.md).
# `codex login` is an interactive browser flow that cannot complete headless inside a sandbox
# container, so — exactly like the cluster's codex-auth above — auth is seeded by copying the
# host's already-authenticated ~/.codex/auth.json in, not by logging in there. This is a Makefile
# target (not a mode of harness/sbx.sh) because sbx.sh's whole job is to run INSIDE the sandbox and
# boot the Odonian stack; this step runs the OPPOSITE direction, from the HOST, reaching INTO a
# sandbox via `sbx cp`/`sbx exec` — it belongs with the other host-side codex-auth target instead.
#
# The in-sandbox counterpart, harness/sbx-agent-setup.sh, installs codex but cannot authenticate
# it; this target is what makes that install usable.
sbx-codex-auth:
	@test -f "$$HOME/.codex/auth.json" || { echo "ERROR: no ~/.codex/auth.json — run 'codex login' first"; exit 1; }
	@jq -e '.tokens.refresh_token // empty' "$$HOME/.codex/auth.json" >/dev/null 2>&1 \
	  || { echo "ERROR: ~/.codex/auth.json has no refresh token — re-run 'codex login'"; exit 1; }
	@echo "Seeding codex auth into sandbox '$(SBX_NAME)' from ~/.codex/auth.json"
	@sbx exec $(SBX_NAME) -- sudo install -d -o agent -g agent -m 700 /home/agent/.codex
	@sbx cp "$$HOME/.codex/auth.json" "$(SBX_NAME):/tmp/odonian-codex-auth.json.incoming"
	@sbx exec $(SBX_NAME) -- sudo install -o agent -g agent -m 600 \
	  /tmp/odonian-codex-auth.json.incoming /home/agent/.codex/auth.json
	@sbx exec $(SBX_NAME) -- sudo rm -f /tmp/odonian-codex-auth.json.incoming
	@echo "Verifying: running 'codex exec -m gpt-5.5' inside sandbox '$(SBX_NAME)'..."
	@# `sbx exec` runs argv directly through the container runtime (like `docker exec`) — there is
	@# no shell to interpret builtins, so `command -v codex` would try to exec a literal `command`
	@# binary and always fail. Invoke the codex binary itself with a harmless flag instead.
	@if ! sbx exec $(SBX_NAME) -- codex --version >/dev/null 2>&1; then \
	  echo "codex auth seeded, but codex itself is not installed in sandbox '$(SBX_NAME)' yet — cannot verify."; \
	  echo "Run harness/sbx-agent-setup.sh inside the sandbox first, then re-run 'make sbx-codex-auth'."; \
	  exit 1; \
	fi
	@# Two non-obvious requirements, each verified independently against a live sandbox:
	@#  --skip-git-repo-check: `sbx exec` starts in /home/agent/workspace, which is NOT a git repo,
	@#    and codex refuses to run outside a trusted directory ("Not inside a trusted directory and
	@#    --skip-git-repo-check was not specified"). The fleet never hits this because agent.sh cd's
	@#    into the repo/worktree before dispatching; this target has no such cwd.
	@#  </dev/null: with stdin an open pipe rather than a TTY, `codex exec` blocks on "Reading
	@#    additional input from stdin..." and never runs the prompt. Closing stdin makes the prompt
	@#    argument the whole input.
	@# Without both, this verification failed even though the auth copy above had fully succeeded.
	@if sbx exec $(SBX_NAME) -- codex exec --skip-git-repo-check -m gpt-5.5 "reply with the single word: ok" </dev/null >/tmp/sbx-codex-auth-verify.out 2>&1; then \
	  echo "OK: codex authenticated in sandbox '$(SBX_NAME)' (gpt-5.5 invocation succeeded)"; \
	  rm -f /tmp/sbx-codex-auth-verify.out; \
	else \
	  echo "ERROR: codex auth was seeded but the verification invocation failed in sandbox '$(SBX_NAME)':"; \
	  cat /tmp/sbx-codex-auth-verify.out; \
	  rm -f /tmp/sbx-codex-auth-verify.out; \
	  exit 1; \
	fi

deploy:
	@echo "Resolving image digest for ghcr.io/boldfield/odonian:$(VERSION)..."
	@STDERR_FILE=$$(mktemp); \
	DIGEST=""; \
	for attempt in 1 2 3 4 5; do \
	  DIGEST=$$(docker buildx imagetools inspect "ghcr.io/boldfield/odonian:$(VERSION)" 2>"$$STDERR_FILE" | awk '/^Digest:/{print $$2; exit}'); \
	  if echo "$$DIGEST" | grep -qE '^sha256:[a-f0-9]{64}$$'; then break; fi; \
	  if [ $$attempt -lt 5 ]; then sleep 2; fi; \
	done; \
	if ! echo "$$DIGEST" | grep -qE '^sha256:[a-f0-9]{64}$$'; then \
	  STDERR_TEXT=$$(cat "$$STDERR_FILE" 2>/dev/null || echo "(no stderr captured)"); \
	  rm -f "$$STDERR_FILE"; \
	  echo "ERROR: Image tag $(VERSION) not found in registry. Last error: $$STDERR_TEXT"; \
	  exit 1; \
	fi; \
	rm -f "$$STDERR_FILE"; \
	echo "Deploying ghcr.io/boldfield/odonian@$$DIGEST"; \
	kubectl -n odonian set image deploy/odonian odonian="ghcr.io/boldfield/odonian@$$DIGEST"; \
	kubectl -n odonian rollout status deploy/odonian --timeout=180s

# Read-only: show the image (digest or tag) and ready replicas each deployment is currently
# running, across all deployments / both clusters. The server uses the CURRENT kube context
# (like `make deploy`); worker/reviewer use $(CP_CONTEXT); merger uses $(LAB_CONTEXT). An
# unreachable cluster or missing deployment degrades to a "(unreachable / not found)" row
# rather than failing the whole command.
versions:
	@printf '%-10s %-22s %-15s %-58s %s\n' DEPLOYMENT CONTEXT NAMESPACE IMAGE READY
	@for spec in \
	  "$(SERVER_CONTEXT)|$(SERVER_NAMESPACE)|odonian" \
	  "$(CP_CONTEXT)|$(FLEET_NAMESPACE)|worker" \
	  "$(CP_CONTEXT)|$(FLEET_NAMESPACE)|reviewer" \
	  "$(LAB_CONTEXT)|$(FLEET_NAMESPACE)|merger"; do \
	  ctx="$${spec%%|*}"; r="$${spec#*|}"; ns="$${r%%|*}"; dep="$${r#*|}"; \
	  cf=""; [ -n "$$ctx" ] && cf="--context $$ctx"; \
	  img="$$(kubectl $$cf -n "$$ns" get deploy "$$dep" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"; \
	  rdy="$$(kubectl $$cf -n "$$ns" get deploy "$$dep" -o jsonpath='{.status.readyReplicas}/{.status.replicas}' 2>/dev/null)"; \
	  if [ -z "$$img" ]; then img="(unreachable / not found)"; rdy="-"; fi; \
	  ctxshow="$$ctx"; [ -z "$$ctx" ] && ctxshow="(current)"; \
	  printf '%-10s %-22s %-15s %-58s %s\n' "$$dep" "$$ctxshow" "$$ns" "$$img" "$$rdy"; \
	done
