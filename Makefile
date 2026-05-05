
# Load .env if present (provides K3S_SSH_HOST, K3S_REGISTRY, etc.)
-include .env
export




K3S_SSH_HOST ?= user@your-k3s-host
K3S_KUBECONFIG ?= /home/user/.kube/config
GO_SERVER_IMAGE ?= ghcr.io/jonathaneoliver/infinite-streaming:latest
K8S_MANIFESTS ?= k8s-infinite-streaming.yaml
K8S_DEPLOYMENT ?= infinite-streaming
IOS_SIM_DEVICE ?= iPad Pro 13-inch (M5)
IOS_APP_BUNDLE_ID ?= com.jeoliver.InfiniteStreamPlayer
IOS_API_BASE ?= http://$(K3S_HOST):40000
IOS_METRICS_DURATION ?= 900
IOS_SCORE_MIN ?= 60

run:
	docker compose up -d

run-ghcr:
	docker compose -f docker-compose.ghcr.yml up -d

stop-ghcr:
	docker compose -f docker-compose.ghcr.yml down

stop:
	docker compose down

shell:
	docker compose exec go-server /bin/sh

# Generate poster thumbnails for any content that doesn't already have one.
# Runs inside the running container so it has access to /media/dynamic_content
# and the ffmpeg already on the image. Targets the LOCAL Docker Compose
# stack — for the test-deploy-dev stack on the remote ubuntu host see
# the thumbnails-test-dev target below.
thumbnails:
	docker compose exec go-server /generate_abr/backfill_thumbnails.sh /media/dynamic_content

thumbnails-force:
	docker compose exec go-server /generate_abr/backfill_thumbnails.sh /media/dynamic_content --force

# Same, but against the test-deploy-dev stack on jonathanoliver-ubuntu.local.
# Use after `make test-deploy-dev` so the container has the latest script.
thumbnails-test-dev:
	ssh jonathanoliver@jonathanoliver-ubuntu.local 'cd ~/test-dev && docker compose exec -T go-server /generate_abr/backfill_thumbnails.sh /media/dynamic_content'

thumbnails-test-dev-force:
	ssh jonathanoliver@jonathanoliver-ubuntu.local 'cd ~/test-dev && docker compose exec -T go-server /generate_abr/backfill_thumbnails.sh /media/dynamic_content --force'

build:
	docker build --no-cache --progress=plain -t infinite-streaming .

buildkit:
	DOCKER_BUILDKIT=1 docker build -t infinite-streaming .

buildx:
	$(MAKE) buildx-amd64
	$(MAKE) buildx-arm64

buildx-amd64:
	docker buildx build --platform linux/amd64 -t infinite-streaming:amd64 --load .

buildx-arm64:
	docker buildx build --platform linux/arm64 -t infinite-streaming:arm64 --load .

buildx-push:
	docker buildx build --platform linux/amd64,linux/arm64 -t infinite-streaming:latest --push .

K3S_REGISTRY ?= localhost:5000
K3S_SERVER_REPO ?= infinite-streaming

build-k3s:
	docker build --no-cache --progress=plain -t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):latest .

buildx-k3s-amd64:
	docker buildx build --platform linux/amd64 -t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):amd64 --load .

buildx-k3s-arm64:
	docker buildx build --platform linux/arm64 -t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):arm64 --load .

buildx-k3s-all:
	$(MAKE) buildx-k3s-amd64
	$(MAKE) buildx-k3s-arm64

push-k3s:
	docker push $(K3S_REGISTRY)/$(K3S_SERVER_REPO):latest

push-k3s-all:
	docker push $(K3S_REGISTRY)/$(K3S_SERVER_REPO):amd64
	docker push $(K3S_REGISTRY)/$(K3S_SERVER_REPO):arm64

build-push-k3s: build-k3s push-k3s

build-push-k3s-all: buildx-k3s-all push-k3s-all

K3S_SERVER_IMAGE ?= $(K3S_REGISTRY)/$(K3S_SERVER_REPO):latest

deploy-k3s-local:
	@set -e; \
	echo "Cleaning up legacy split deployments/services"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl delete service go-server go-proxy --ignore-not-found=true"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl delete deployment go-server go-proxy --ignore-not-found=true"; \
	for manifest in $(K8S_MANIFESTS); do \
		echo "Applying $$manifest to $(K3S_SSH_HOST)"; \
		INFINITE_STREAM_RENDEZVOUS_URL='$(INFINITE_STREAM_RENDEZVOUS_URL)' \
		INFINITE_STREAM_ANNOUNCE_URL_K3S_DEV='$(INFINITE_STREAM_ANNOUNCE_URL_K3S_DEV)' \
		INFINITE_STREAM_ANNOUNCE_LABEL_K3S_DEV='$(INFINITE_STREAM_ANNOUNCE_LABEL_K3S_DEV)' \
		INFINITE_STREAM_ANNOUNCE_URL_K3S_RELEASE='$(INFINITE_STREAM_ANNOUNCE_URL_K3S_RELEASE)' \
		INFINITE_STREAM_ANNOUNCE_LABEL_K3S_RELEASE='$(INFINITE_STREAM_ANNOUNCE_LABEL_K3S_RELEASE)' \
		envsubst < $$manifest | ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl apply -f -"; \
	done; \
	echo "Updating deployment images"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl set image deployment/$(K8S_DEPLOYMENT) go-server=$(K3S_SERVER_IMAGE)"; \
	echo "Restarting deployments explicitly"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl rollout restart deployment/$(K8S_DEPLOYMENT)"; \
	echo "Waiting for rollout"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl rollout status deployment/$(K8S_DEPLOYMENT) --timeout=180s"; \
	echo "Deployment status"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl get pods -n default -o wide; echo; kubectl get svc -n default"

deploy-k3s: deploy-k3s-local

status-k3s:
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl get nodes; echo; kubectl get pods -A"

# `make deploy` and `make deploy-release` are end-to-end — they build +
# push the main image, apply the main app manifest, AND apply that
# stack's analytics tier (ClickHouse + forwarder + Grafana — separate
# pods, Services, and PVCs per stack so dev and release share nothing).
# Each stack picks its own `ANALYTICS_STACK` and `ANALYTICS_SSE_URL`
# via target-specific variables below.

deploy: ANALYTICS_STACK=dev
# Dev's go-proxy is reachable via the dev Service at port 40081
# (the NodePort's `port` value, mapped to container :30081).
deploy: ANALYTICS_SSE_URL=http://infinite-streaming-dev:40081/api/sessions/stream
deploy: analytics-deploy-k3s
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(shell cat VERSION) -t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):dev --push .
	$(MAKE) deploy-k3s K3S_KUBECONFIG=$(K3S_KUBECONFIG) K8S_MANIFESTS=k8s-infinite-streaming-dev.yaml K8S_DEPLOYMENT=infinite-streaming-dev K3S_SERVER_IMAGE=$(K3S_REGISTRY)/$(K3S_SERVER_REPO):dev

logs:
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl logs deploy/infinite-streaming-dev --all-containers -f"

deploy-release: ANALYTICS_STACK=release
deploy-release: ANALYTICS_SSE_URL=http://infinite-streaming:30081/api/sessions/stream
deploy-release: analytics-deploy-k3s
	docker buildx build --platform linux/amd64 \
		--build-arg VERSION=$(shell cat VERSION) \
		-t $(K3S_SERVER_IMAGE) \
		-t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):$(shell cat VERSION) \
		--push .
	$(MAKE) deploy-k3s K3S_KUBECONFIG=$(K3S_KUBECONFIG) K3S_SERVER_IMAGE=$(K3S_SERVER_IMAGE)

# ── Analytics tier deployment to k3s ───────────────────────────────────
# Per-stack analytics: dev and release each get their own ClickHouse,
# forwarder, and Grafana. Resources are named with `-${ANALYTICS_STACK}`
# suffix so both can coexist in the same namespace. Schemas + Grafana
# provisioning are inlined in k8s-analytics.yaml — no separate
# ConfigMap-from-file step.

# Build + push the forwarder image into the cluster's registry. Same
# image is shared across stacks (it's stack-agnostic); only the
# Deployment env vars differ.
analytics-build-forwarder-k3s:
	docker buildx build --platform linux/amd64 \
		-t $(K3S_REGISTRY)/infinite-streaming-forwarder:dev \
		--push ./analytics/go-forwarder

# Apply the analytics manifest for the `${ANALYTICS_STACK}` stack.
# Idempotent: `kubectl apply` re-converges on every run.
analytics-deploy-k3s: analytics-build-forwarder-k3s
	@if [ -z "$(ANALYTICS_STACK)" ]; then echo "ANALYTICS_STACK must be set (dev|release)"; exit 1; fi
	@if [ -z "$(ANALYTICS_SSE_URL)" ]; then echo "ANALYTICS_SSE_URL must be set"; exit 1; fi
	@echo "=== Applying analytics tier (stack=$(ANALYTICS_STACK)) ==="
	ANALYTICS_STACK='$(ANALYTICS_STACK)' \
	ANALYTICS_SSE_URL='$(ANALYTICS_SSE_URL)' \
	K3S_REGISTRY='$(K3S_REGISTRY)' \
		envsubst < k8s-analytics.yaml | \
		ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl apply -f -"
	@echo "=== Waiting for $(ANALYTICS_STACK) analytics rollout ==="
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); \
		kubectl rollout status statefulset/clickhouse-$(ANALYTICS_STACK) --timeout=120s; \
		kubectl rollout status deployment/forwarder-$(ANALYTICS_STACK) --timeout=120s; \
		kubectl rollout status deployment/grafana-$(ANALYTICS_STACK) --timeout=120s"

# Tear down a single stack (analytics + main app) so the other stack
# can be exercised in isolation. Removes Deployments, Services,
# StatefulSets, ConfigMaps, AND the PVC (so a subsequent
# `make deploy[-release]` starts from empty ClickHouse data — matches
# the user's "no data migration needed" model).
#
# Usage:
#   make teardown-k3s-dev        # wipes the dev stack
#   make teardown-k3s-release    # wipes the release stack
#
# Both targets also delete the main app deployment for that stack so
# you get a fully empty namespace slot.
teardown-k3s-dev:
	$(MAKE) teardown-k3s-stack ANALYTICS_STACK=dev MAIN_APP=infinite-streaming-dev
teardown-k3s-release:
	$(MAKE) teardown-k3s-stack ANALYTICS_STACK=release MAIN_APP=infinite-streaming

# Internal worker. Don't invoke directly; use the dev/release wrappers.
teardown-k3s-stack:
	@if [ -z "$(ANALYTICS_STACK)" ]; then echo "ANALYTICS_STACK required"; exit 1; fi
	@echo "=== Tearing down $(ANALYTICS_STACK) analytics + main app ==="
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); \
		kubectl delete --ignore-not-found=true \
			deployment/grafana-$(ANALYTICS_STACK) \
			deployment/forwarder-$(ANALYTICS_STACK) \
			statefulset/clickhouse-$(ANALYTICS_STACK) \
			service/grafana-$(ANALYTICS_STACK) \
			service/forwarder-$(ANALYTICS_STACK) \
			service/clickhouse-$(ANALYTICS_STACK) \
			configmap/analytics-grafana-dashboards-$(ANALYTICS_STACK) \
			configmap/analytics-grafana-datasources-$(ANALYTICS_STACK) \
			configmap/analytics-clickhouse-init-$(ANALYTICS_STACK); \
		kubectl delete pvc --ignore-not-found=true -l app=clickhouse-$(ANALYTICS_STACK); \
		kubectl delete --ignore-not-found=true deployment/$(MAIN_APP) service/$(MAIN_APP)"
	@echo "Stack $(ANALYTICS_STACK) torn down. Run \`make deploy$(if $(filter release,$(ANALYTICS_STACK)),-release,)\` to bring it back."

# ── Remote deployment testing ──────────────────────────────────────────
# Deploy all 4 installation methods to a remote Docker host for parallel testing.
# Configure TEST_SSH and TEST_MEDIA_DIR in .env.

TEST_SSH ?= user@test-host
TEST_MEDIA_DIR ?= /home/user/media
REPO_URL ?= https://github.com/jonathaneoliver/infinite-streaming.git
# Bare hostname for announce URLs (TEST_SSH = user@host).
TEST_HOST ?= $(lastword $(subst @, ,$(TEST_SSH)))

test-go:
	@echo "=== go-proxy ==="
	cd go-proxy && go vet ./... && go test -race ./...
	@echo "=== go-live ==="
	cd go-live && go vet ./... && go test -race ./...
	@echo "=== go-upload ==="
	cd go-upload && go vet ./... && go test -race ./...

test-deploy-all: test-deploy-compose test-deploy-run test-deploy-ghcr test-deploy-registry

test-deploy-dev:
	@echo "=== Dev: local working tree (port 21000) ==="
	ssh -n $(TEST_SSH) 'mkdir -p ~/test-dev'
	@echo "Syncing local working tree (excluding .git and .gitignore matches)..."
	rsync -az --delete \
		--filter=':- .gitignore' \
		--exclude='.git/' \
		--exclude='.env' \
		./ $(TEST_SSH):~/test-dev/
	ssh -n $(TEST_SSH) 'printf "CONTENT_DIR=%s\nINFINITE_STREAM_RENDEZVOUS_URL=%s\nINFINITE_STREAM_ANNOUNCE_URL=%s\nINFINITE_STREAM_ANNOUNCE_LABEL=%s\n" "$(TEST_MEDIA_DIR)" "$(INFINITE_STREAM_RENDEZVOUS_URL)" "$(INFINITE_STREAM_ANNOUNCE_URL)" "$(INFINITE_STREAM_ANNOUNCE_LABEL)" > ~/test-dev/.env'
	scp tests/deploy/override-dev.yml $(TEST_SSH):~/test-dev/docker-compose.override.yml
	ssh $(TEST_SSH) 'cd ~/test-dev && docker compose build && docker compose up -d'

# Iterate on Grafana provisioning (dashboards / datasources) without
# touching go-server. Sessions keep flowing live; Grafana auto-reloads
# the dashboard JSON within 30s, but we --force-recreate to pick it up
# immediately and to refresh the bind mount in case files were added.
analytics-update:
	@echo "=== Updating analytics provisioning on test-dev (no go-server restart) ==="
	rsync -az --delete \
		/Users/jonathanoliver/Projects/smashing/analytics/grafana/ \
		$(TEST_SSH):~/test-dev/analytics/grafana/
	ssh $(TEST_SSH) 'cd ~/test-dev && docker compose up -d --force-recreate grafana'

# Rebuild the forwarder binary and recreate just that container. Sessions
# keep flowing live (go-server is untouched); archival pauses for ~1s
# while the forwarder restarts. --no-deps prevents docker compose from
# pulling go-server into the recreate.
analytics-rebuild-forwarder:
	@echo "=== Rebuilding forwarder on test-dev (no go-server restart) ==="
	ssh $(TEST_SSH) 'mkdir -p ~/test-dev/analytics/go-forwarder'
	rsync -az --delete \
		/Users/jonathanoliver/Projects/smashing/analytics/go-forwarder/ \
		$(TEST_SSH):~/test-dev/analytics/go-forwarder/
	ssh $(TEST_SSH) 'cd ~/test-dev && docker compose build forwarder && docker compose up -d --no-deps forwarder'

# Apply a ClickHouse migration without restarting anything. Pipes the
# SQL through SSH's stdin → temp file on the host → curl, so the SQL
# can contain any characters including single quotes (which would
# otherwise collide with the outer single-quoted SSH command).
#
# A SQL-FILE may contain multiple `;`-separated statements; they are
# split and posted one at a time (ClickHouse 24.8's HTTP interface
# rejects multi-statement bodies). Single-quoted literals inside
# values are preserved — the splitter is dumb-on-`;` and trusts that
# migration SQL doesn't include `;` inside string literals.
#
# Usage:
#   make analytics-migrate SQL="ALTER TABLE session_snapshots ADD COLUMN foo String DEFAULT 'bar'"
#   make analytics-migrate SQL-FILE=/path/to/migration.sql
analytics-migrate:
	@if [ -n "$(SQL-FILE)" ]; then \
	  cat "$(SQL-FILE)"; \
	elif [ -n "$(SQL)" ]; then \
	  printf '%s\n' "$(SQL)"; \
	else \
	  echo "set SQL=... or SQL-FILE=..."; exit 1; \
	fi | \
	ssh $(TEST_SSH) ' \
	  cat > /tmp/.analytics-migrate.sql && \
	  printf ";\n" >> /tmp/.analytics-migrate.sql && \
	  rc=0; \
	  while IFS= read -r -d ";" stmt; do \
	    trimmed=$$(printf "%s" "$$stmt" | tr -d "\n" | sed "s/^[ \t]*//;s/[ \t]*$$//"); \
	    [ -z "$$trimmed" ] && continue; \
	    printf "→ %s\n" "$$(printf "%s" "$$trimmed" | cut -c1-100)"; \
	    if curl -fsS -X POST "http://localhost:21123/?database=infinite_streaming" --data-binary "$$trimmed"; then \
	      echo "  ok"; \
	    else \
	      echo "  FAILED"; rc=1; \
	    fi; \
	  done < /tmp/.analytics-migrate.sql; \
	  rm -f /tmp/.analytics-migrate.sql; \
	  exit $$rc'

test-clean-dev:
	ssh $(TEST_SSH) 'docker rm -f test-dev-server 2>/dev/null'

test-clean:
	ssh $(TEST_SSH) 'docker rm -f test-dev-server test-compose-server test-docker-run test-ghcr-server test-registry-server test-oobe-server 2>/dev/null; docker network prune -f 2>/dev/null'

test-status:
	@ssh $(TEST_SSH) 'for p in 21000 22000 23000 24000 25000 26000; do \
		proxy=$$((p / 1000 * 1000 + 81)); \
		ui=$$(curl -s -o /dev/null -w "%{http_code}" http://localhost:$$p/); \
		px=$$(curl -s -o /dev/null -w "%{http_code}" http://localhost:$$proxy/api/sessions 2>/dev/null); \
		echo "Port $$p: UI=$$ui Proxy=$$proxy=$$px"; \
	done'

# OOBE simulation: simulate a brand-new install by wiping the host media
# dir and the test-oobe project's docker volumes on every deploy. The
# point is to land on the first-run code paths (`/api/setup` warning,
# setup banner + modal in shared-nav.js) every single time.
#
# Three layers of defence keep this from ever touching test-dev's data:
#   1. Pinned project name (-p test-oobe everywhere; COMPOSE_PROJECT_NAME
#      in .env) so volume names cannot collide with other variants.
#   2. `docker compose -p test-oobe down -v` scopes the volume removal to
#      this project; bare `docker volume rm` is never called.
#   3. The `rm -rf` of TEST_OOBE_MEDIA_DIR refuses to run unless the path
#      literally ends in /test-oobe-media. Belt + suspenders.
#
# Derives the remote home from $(TEST_SSH) under the assumption that it
# follows user@host form (the convention in .env). If TEST_SSH is set to
# a bare hostname, `cut -d@ -f1` returns the hostname unchanged and this
# path becomes /home/<hostname>/test-oobe-media — wrong but harmless:
# safety guards still pass and the wipe targets a non-existent dir. Set
# TEST_OOBE_MEDIA_DIR explicitly in .env if your TEST_SSH lacks a user.
TEST_OOBE_MEDIA_DIR ?= /home/$(shell echo $(TEST_SSH) | cut -d@ -f1)/test-oobe-media

test-deploy-oobe:
	@echo "=== OOBE: fresh-install simulation (port 26000) ==="
	@case "$(TEST_OOBE_MEDIA_DIR)" in \
	  */test-oobe-media) ;; \
	  *) echo "REFUSING: TEST_OOBE_MEDIA_DIR must end in /test-oobe-media, got '$(TEST_OOBE_MEDIA_DIR)'"; exit 1 ;; \
	esac
	ssh $(TEST_SSH) 'mkdir -p ~/test-oobe ~/test-oobe-media && \
	  if [ -f ~/test-oobe/docker-compose.yml ]; then \
	    cd ~/test-oobe && docker compose -p test-oobe down -v --remove-orphans 2>/dev/null || true; \
	  fi'
	# Wipe the media dir via a privileged container so we can remove
	# files owned by the in-container clickhouse / root users that the
	# host login lacks permission to delete directly.
	ssh $(TEST_SSH) 'case "$(TEST_OOBE_MEDIA_DIR)" in \
	  */test-oobe-media) docker run --rm -v $(TEST_OOBE_MEDIA_DIR):/m alpine sh -c "rm -rf /m/* /m/.* 2>/dev/null; true" ;; \
	  *) echo "REFUSING wipe of $(TEST_OOBE_MEDIA_DIR)"; exit 1 ;; \
	esac'
	@echo "Syncing local working tree (excluding .git and .gitignore matches)..."
	rsync -az --delete \
		--filter=':- .gitignore' \
		--exclude='.git/' \
		--exclude='.env' \
		./ $(TEST_SSH):~/test-oobe/
	ssh -n $(TEST_SSH) 'printf "COMPOSE_PROJECT_NAME=test-oobe\nCONTENT_DIR=%s\nINFINITE_STREAM_RENDEZVOUS_URL=%s\nINFINITE_STREAM_ANNOUNCE_URL=http://%s:26000\nINFINITE_STREAM_BASE_URL=http://%s:26000\n" \
		"$(TEST_OOBE_MEDIA_DIR)" "$(INFINITE_STREAM_RENDEZVOUS_URL)" "$(TEST_HOST)" "$(TEST_HOST)" > ~/test-oobe/.env'
	scp tests/deploy/override-oobe.yml $(TEST_SSH):~/test-oobe/docker-compose.override.yml
	ssh $(TEST_SSH) 'cd ~/test-oobe && docker compose -p test-oobe build && docker compose -p test-oobe up -d'

test-clean-oobe:
	@case "$(TEST_OOBE_MEDIA_DIR)" in \
	  */test-oobe-media) ;; \
	  *) echo "REFUSING: TEST_OOBE_MEDIA_DIR must end in /test-oobe-media, got '$(TEST_OOBE_MEDIA_DIR)'"; exit 1 ;; \
	esac
	ssh $(TEST_SSH) 'if [ -f ~/test-oobe/docker-compose.yml ]; then \
	    cd ~/test-oobe && docker compose -p test-oobe down -v --remove-orphans 2>/dev/null || true; \
	  fi; \
	  case "$(TEST_OOBE_MEDIA_DIR)" in \
	    */test-oobe-media) docker run --rm -v $(TEST_OOBE_MEDIA_DIR):/m alpine sh -c "rm -rf /m/* /m/.* 2>/dev/null; true"; rmdir $(TEST_OOBE_MEDIA_DIR) 2>/dev/null; true ;; \
	    *) echo "REFUSING wipe of $(TEST_OOBE_MEDIA_DIR)"; exit 1 ;; \
	  esac'

test-deploy-compose:
	@echo "=== Option 1: Docker Compose from source (port 22000) ==="
	ssh $(TEST_SSH) 'if [ -d ~/test-compose/.git ]; then cd ~/test-compose && git checkout -- . && git pull; else git clone $(REPO_URL) ~/test-compose; fi'
	ssh -n $(TEST_SSH) 'printf "CONTENT_DIR=%s\nINFINITE_STREAM_RENDEZVOUS_URL=%s\nINFINITE_STREAM_ANNOUNCE_URL=http://%s:22000\n" "$(TEST_MEDIA_DIR)" "$(INFINITE_STREAM_RENDEZVOUS_URL)" "$(TEST_HOST)" > ~/test-compose/.env'
	scp tests/deploy/override-compose.yml $(TEST_SSH):~/test-compose/docker-compose.override.yml
	ssh $(TEST_SSH) 'cd ~/test-compose && docker compose build && docker compose up -d'

test-deploy-run:
	@echo "=== Option 2: Docker run (port 23000) ==="
	ssh $(TEST_SSH) 'docker rm -f test-docker-run 2>/dev/null; \
		docker run -d --name test-docker-run --cap-add NET_ADMIN --privileged \
		-p 23000:30000 -p 23081:30081 -p 23181:30181 -p 23281:30281 -p 23381:30381 -p 23481:30481 \
		-p 23581:30581 -p 23681:30681 -p 23781:30781 -p 23881:30881 \
		-e INFINITE_STREAM_RENDEZVOUS_URL=$(INFINITE_STREAM_RENDEZVOUS_URL) \
		-e INFINITE_STREAM_ANNOUNCE_URL=http://$(TEST_HOST):23000 \
		-v $(TEST_MEDIA_DIR):/media \
		infinite-streaming:latest /sbin/launch.sh 1'

test-deploy-ghcr:
	@echo "=== Option 3: GHCR pre-built (port 24000) ==="
	ssh $(TEST_SSH) 'mkdir -p ~/test-ghcr'
	ssh $(TEST_SSH) 'if [ -d ~/test-compose ]; then cp ~/test-compose/docker-compose.ghcr.yml ~/test-ghcr/docker-compose.yml; fi'
	ssh -n $(TEST_SSH) 'printf "CONTENT_DIR=%s\nINFINITE_STREAM_RENDEZVOUS_URL=%s\nINFINITE_STREAM_ANNOUNCE_URL=http://%s:24000\n" "$(TEST_MEDIA_DIR)" "$(INFINITE_STREAM_RENDEZVOUS_URL)" "$(TEST_HOST)" > ~/test-ghcr/.env'
	scp tests/deploy/override-ghcr.yml $(TEST_SSH):~/test-ghcr/docker-compose.override.yml
	ssh $(TEST_SSH) 'cd ~/test-ghcr && docker compose up -d'

test-deploy-registry:
	@echo "=== Option 4: Private registry (port 25000) ==="
	ssh $(TEST_SSH) 'mkdir -p ~/test-registry'
	scp tests/deploy/docker-compose.registry.yml $(TEST_SSH):~/test-registry/docker-compose.yml
	ssh -n $(TEST_SSH) 'printf "CONTENT_DIR=%s\nK3S_REGISTRY=%s\nINFINITE_STREAM_RENDEZVOUS_URL=%s\nINFINITE_STREAM_ANNOUNCE_URL=http://%s:25000\n" "$(TEST_MEDIA_DIR)" "$(K3S_REGISTRY)" "$(INFINITE_STREAM_RENDEZVOUS_URL)" "$(TEST_HOST)" > ~/test-registry/.env'
	ssh $(TEST_SSH) 'cd ~/test-registry && docker compose up -d'

# ── Screenshots ────────────────────────────────────────────────────────

# ── Android TV ─────────────────────────────────────────────────────────

ANDROIDTV_DIR ?= android/InfiniteStreamPlayer
JAVA_HOME_ANDROID ?= /Applications/Android Studio.app/Contents/jbr/Contents/Home
ANDROID_SDK_HOME ?= $(HOME)/Library/Android/sdk

APPLETV_PROJECT ?= apple/InfiniteStreamPlayer/InfiniteStreamPlayer.xcodeproj
APPLETV_SCHEME ?= InfiniteStreamPlayer (tvOS)
APPLETV_BUNDLE_ID ?= com.jeoliver.InfiniteStreamPlayerTV
APPLETV_DEVICE_ID ?=
APPLETV_XCODE_ID ?=
APPLETV_DERIVED_DATA ?= /tmp/appletv-build

IPHONE_SCHEME ?= InfiniteStreamPlayer (iOS)
IPHONE_BUNDLE_ID ?= com.jeoliver.InfiniteStreamPlayer
IPHONE_DEVICE_ID ?=
IPHONE_XCODE_ID ?=
IPHONE_DERIVED_DATA ?= /tmp/iphone-build

deploy-appletv:
	@if [ -z "$(APPLETV_DEVICE_ID)" ] || [ -z "$(APPLETV_XCODE_ID)" ]; then \
		echo "APPLETV_DEVICE_ID / APPLETV_XCODE_ID not set in .env" >&2; \
		exit 1; \
	fi
	xcodebuild \
		-project "$(APPLETV_PROJECT)" \
		-scheme "$(APPLETV_SCHEME)" \
		-destination "id=$(APPLETV_XCODE_ID)" \
		-configuration Debug \
		-derivedDataPath "$(APPLETV_DERIVED_DATA)" \
		build
	xcrun devicectl device install app \
		--device "$(APPLETV_DEVICE_ID)" \
		"$(APPLETV_DERIVED_DATA)/Build/Products/Debug-appletvos/InfiniteStreamPlayerTV.app"
	xcrun devicectl device process launch \
		--device "$(APPLETV_DEVICE_ID)" \
		"$(APPLETV_BUNDLE_ID)"

deploy-iphone:
	@if [ -z "$(IPHONE_DEVICE_ID)" ] || [ -z "$(IPHONE_XCODE_ID)" ]; then \
		echo "IPHONE_DEVICE_ID / IPHONE_XCODE_ID not set in .env" >&2; \
		exit 1; \
	fi
	xcodebuild \
		-project "$(APPLETV_PROJECT)" \
		-scheme "$(IPHONE_SCHEME)" \
		-destination "id=$(IPHONE_XCODE_ID)" \
		-configuration Debug \
		-derivedDataPath "$(IPHONE_DERIVED_DATA)" \
		build
	xcrun devicectl device install app \
		--device "$(IPHONE_DEVICE_ID)" \
		"$(IPHONE_DERIVED_DATA)/Build/Products/Debug-iphoneos/InfiniteStreamPlayer.app"
	xcrun devicectl device process launch \
		--device "$(IPHONE_DEVICE_ID)" \
		"$(IPHONE_BUNDLE_ID)"

deploy-androidtv:
	# Don't `adb uninstall` first — gradlew installDebug performs an
	# in-place upgrade that preserves SharedPreferences (server list,
	# selected protocol/segment/codec, etc.) since the local debug
	# keystore signature is stable across rebuilds. Use
	# `make uninstall-androidtv` if a real reset is needed (e.g. after
	# a signature change).
	cd $(ANDROIDTV_DIR) && \
		JAVA_HOME="$(JAVA_HOME_ANDROID)" \
		ANDROID_HOME="$(ANDROID_SDK_HOME)" \
		PATH="$(ANDROID_SDK_HOME)/platform-tools:$$PATH" \
		./gradlew installDebug
	$(ANDROID_SDK_HOME)/platform-tools/adb shell am start -n com.infinitestream.player/.MainActivity

uninstall-androidtv:
	$(ANDROID_SDK_HOME)/platform-tools/adb uninstall com.infinitestream.player 2>/dev/null || true

# ── Synthetic test pattern ─────────────────────────────────────────────
# Generate a 4K mezzanine file from FFmpeg's `testsrc` source (colour
# chart + scrolling gradient + built-in timestamp) with a solid-colour
# flash at the tail to mark the loop boundary, and a per-second 1 kHz
# audio pulse for A/V sync checking. Output lands under CONTENT_DIR's
# originals/ subdir so go-upload picks it up via INFINITE_STREAM_SOURCES_DIR.
#
# Usage:
#   make test-pattern
#   make test-pattern TEST_PATTERN_DURATION=120 TEST_PATTERN_FLASH_DURATION=2
#   make test-pattern TEST_PATTERN_FLASH_COLOR=yellow
#   make test-pattern TEST_PATTERN_SIZE=1920x1080 TEST_PATTERN_RATE=30

CONTENT_DIR ?= ./sample-content
TEST_PATTERN_OUTPUT_NAME ?= testpattern_2160p60.mp4
TEST_PATTERN_OUTPUT_DIR  ?= $(CONTENT_DIR)/originals
TEST_PATTERN_OUTPUT      ?= $(TEST_PATTERN_OUTPUT_DIR)/$(TEST_PATTERN_OUTPUT_NAME)
TEST_PATTERN_SIZE        ?= 3840x2160
TEST_PATTERN_RATE        ?= 60
# 120s total = 118s testsrc2 + 2s solid-colour flash at the tail.
# Divides cleanly into both 4s (create_abr_ladder.sh segments) and
# 6s (go-live LL-HLS segments). Override if you want a longer source.
TEST_PATTERN_DURATION       ?= 120
TEST_PATTERN_FLASH_DURATION ?= 2
TEST_PATTERN_FLASH_COLOR    ?= pink
TEST_PATTERN_CRF            ?= 18

test-pattern:
	@mkdir -p "$(TEST_PATTERN_OUTPUT_DIR)"
	ffmpeg -y \
		-f lavfi -i "testsrc=size=$(TEST_PATTERN_SIZE):rate=$(TEST_PATTERN_RATE):duration=$$(( $(TEST_PATTERN_DURATION) - $(TEST_PATTERN_FLASH_DURATION) ))" \
		-f lavfi -i "color=c=$(TEST_PATTERN_FLASH_COLOR):size=$(TEST_PATTERN_SIZE):rate=$(TEST_PATTERN_RATE):duration=$(TEST_PATTERN_FLASH_DURATION)" \
		-f lavfi -i "sine=frequency=1000:duration=0.05:sample_rate=48000,volume=0.4,apad=pad_dur=0.95,aloop=loop=-1:size=48000,atrim=duration=$(TEST_PATTERN_DURATION)" \
		-filter_complex "[0:v]format=yuv420p[main];[1:v]format=yuv420p[flash];[main][flash]concat=n=2:v=1:a=0[vout]" \
		-map "[vout]" -map "2:a" \
		-c:v libx264 -preset medium -crf $(TEST_PATTERN_CRF) \
		-g $(TEST_PATTERN_RATE) -keyint_min $(TEST_PATTERN_RATE) -sc_threshold 0 \
		-c:a aac -ar 48000 -b:a 192k \
		-movflags +faststart \
		"$(TEST_PATTERN_OUTPUT)"
	@echo "Wrote $(TEST_PATTERN_OUTPUT)"

# ── iOS testing ────────────────────────────────────────────────────────

test-ios-sim-metrics:
	PYTEST_DISABLE_PLUGIN_AUTOLOAD=1 \
	IOS_SIM_TEST_RUN=1 \
	IOS_VERBOSE=1 \
	IOS_SIM_DEVICE="$(IOS_SIM_DEVICE)" \
	IOS_APP_BUNDLE_ID="$(IOS_APP_BUNDLE_ID)" \
	IOS_METRICS_DURATION=$(IOS_METRICS_DURATION) \
	IOS_SCORE_MIN=$(IOS_SCORE_MIN) \
	pytest tests/integration -k ios_simulator_pyramid_metrics -m integration -vv --api-base $(IOS_API_BASE)
