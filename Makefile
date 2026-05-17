
# Load .env if present (provides K3S_SSH_HOST, K3S_REGISTRY, etc.)
-include .env
export




K3S_SSH_HOST ?= user@your-k3s-host
GO_SERVER_IMAGE ?= ghcr.io/jonathaneoliver/infinite-streaming:latest
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

# OpenAPI / Swagger spec generation. Annotations live in
# go-proxy/cmd/server/openapi.go and analytics/go-forwarder/openapi.go
# (separate from the handlers so main.go stays clean). Output drops in
# api/openapi/{proxy,forwarder}/swagger.{json,yaml}; v2 hand-written
# specs live in api/openapi/v2/.
SWAG         := $(or $(SWAG),$(shell go env GOPATH)/bin/swag)
OAPICODEGEN  := $(or $(OAPICODEGEN),$(shell go env GOPATH)/bin/oapi-codegen)

openapi-tools:
	go install github.com/swaggo/swag/v2/cmd/swag@v2.0.0
	go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.0
	@echo "installed: $(SWAG)"
	@echo "installed: $(OAPICODEGEN)"

openapi:
	@test -x "$(SWAG)" || { echo "swag not installed — run 'make openapi-tools'"; exit 1; }
	@mkdir -p api/openapi/proxy api/openapi/forwarder
	cd go-proxy && $(SWAG) init --v3.1 -g cmd/server/openapi.go --output ../api/openapi/proxy --outputTypes json,yaml --parseInternal
	@if [ -f analytics/go-forwarder/openapi.go ]; then \
	  cd analytics/go-forwarder && $(SWAG) init --v3.1 -g openapi.go --output ../../api/openapi/forwarder --outputTypes json,yaml --parseInternal; \
	else \
	  echo "skipping forwarder spec — analytics/go-forwarder/openapi.go not present yet"; \
	fi
	@mkdir -p content/dashboard/api-docs
	@cp api/openapi/proxy/swagger.json content/dashboard/api-docs/proxy.json
	@if [ -f api/openapi/forwarder/swagger.json ]; then \
	  cp api/openapi/forwarder/swagger.json content/dashboard/api-docs/forwarder.json; \
	fi
	@if [ -f api/openapi/v2/proxy.yaml ]; then \
	  cp api/openapi/v2/proxy.yaml content/dashboard/api-docs/proxy-v2.yaml; \
	fi
	@if [ -f api/openapi/v2/forwarder.yaml ]; then \
	  cp api/openapi/v2/forwarder.yaml content/dashboard/api-docs/forwarder-v2.yaml; \
	fi
	@if [ -f api/openapi/v2/proxy.yaml ] && [ -x "$(OAPICODEGEN)" ]; then \
	  cd go-proxy/internal/v2/oapigen && $(OAPICODEGEN) -config config.yaml ../../../../api/openapi/v2/proxy.yaml; \
	  echo "v2 server interface regenerated: go-proxy/internal/v2/oapigen/oapigen.gen.go"; \
	else \
	  echo "skipping v2 codegen — oapi-codegen not installed (run 'make openapi-tools')"; \
	fi
	@echo "specs regenerated under api/openapi/"
	@echo "scalar UI mirror: content/dashboard/api-docs/{proxy,forwarder,proxy-v2,forwarder-v2}.{json,yaml}"

openapi-clean:
	rm -rf api/openapi/proxy api/openapi/forwarder
	rm -f content/dashboard/api-docs/proxy.json content/dashboard/api-docs/forwarder.json
	rm -f content/dashboard/api-docs/proxy-v2.yaml content/dashboard/api-docs/forwarder-v2.yaml
	rm -f go-proxy/internal/v2/oapigen/oapigen.gen.go

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

# ── k3d cluster lifecycle ──────────────────────────────────────────────
# Two independent k3d clusters on $K3S_SSH_HOST: `dev` (host ports
# 40000-40881, API :6543) and `release` (host ports 30000-30881, API
# :6544). Each is a Docker-in-Docker k3s, so cluster identity is the
# kubeconfig context, not a resource-name suffix. Manifests share one
# template — a single `infinite-streaming` Deployment per cluster, no
# `-dev` / `-release` disambiguation.
#
# API ports avoid 6443 (whatever else might be on the host's k3s).

K3D_DEV_KUBECONFIG     ?= ~/.config/k3d/smashing-dev-kubeconfig.yaml
K3D_RELEASE_KUBECONFIG ?= ~/.config/k3d/smashing-release-kubeconfig.yaml

# Bootstrap both k3d clusters on $K3S_SSH_HOST. Idempotent: re-running
# is a no-op once both clusters exist. Installs k3d if missing into
# ~/.local/bin (no sudo). Subsequent ssh commands prepend
# ~/.local/bin to PATH so the install location doesn't have to be
# in the noninteractive shell's default PATH.
# Writes per-cluster kubeconfigs to ~/.kube/smashing-{dev,release}.yaml
# so subsequent `make deploy` / `make deploy-release` targets can pick
# the right context with KUBECONFIG=… without depending on whichever
# kubeconfig happens to be active in the user's shell.

# Wrapper to push ~/.local/bin onto PATH for non-interactive ssh
# sessions where ~/.bashrc / ~/.profile may not be sourced.
K3D_REMOTE_SHELL = export PATH=$$HOME/.local/bin:$$PATH

k3d-bootstrap:
	@echo "=== Ensuring k3d is installed on $(K3S_SSH_HOST) ==="
	ssh $(K3S_SSH_HOST) 'mkdir -p ~/.local/bin && \
		(test -x $$HOME/.local/bin/k3d && echo "k3d already installed: $$HOME/.local/bin/k3d") || \
		(USE_SUDO=false K3D_INSTALL_DIR=$$HOME/.local/bin curl -sfL https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | USE_SUDO=false K3D_INSTALL_DIR=$$HOME/.local/bin bash || true) && \
		test -x $$HOME/.local/bin/k3d'
	@echo "=== Writing registries.yaml so both clusters can pull from $(K3S_REGISTRY) (HTTP) ==="
	@printf 'mirrors:\n  "%s":\n    endpoint:\n      - "http://%s"\nconfigs:\n  "%s":\n    tls:\n      insecure_skip_verify: true\n' \
		'$(K3S_REGISTRY)' '$(K3S_REGISTRY)' '$(K3S_REGISTRY)' | \
		ssh $(K3S_SSH_HOST) 'mkdir -p ~/.config/k3d && cat > ~/.config/k3d/smashing-registries.yaml'
	@echo "=== Creating k3d cluster `dev` (api :6543, host ports 40000-40881) ==="
	ssh $(K3S_SSH_HOST) '$(K3D_REMOTE_SHELL); k3d cluster list dev 2>/dev/null | grep -q "^dev " || k3d cluster create dev \
		--api-port 6543 \
		--port "40000:30000@loadbalancer" \
		--port "40081:30081@loadbalancer" \
		--port "40181:30181@loadbalancer" \
		--port "40281:30281@loadbalancer" \
		--port "40381:30381@loadbalancer" \
		--port "40481:30481@loadbalancer" \
		--port "40581:30581@loadbalancer" \
		--port "40681:30681@loadbalancer" \
		--port "40781:30781@loadbalancer" \
		--port "40881:30881@loadbalancer" \
		--registry-config ~/.config/k3d/smashing-registries.yaml \
		--volume "$(K3S_MEDIA_DIR):$(K3S_MEDIA_DIR)@server:0" \
		--volume "$(K3S_CERTS_DIR):$(K3S_CERTS_DIR)@server:0" \
		--kubeconfig-update-default=false \
		--kubeconfig-switch-context=false'
	ssh $(K3S_SSH_HOST) '$(K3D_REMOTE_SHELL); mkdir -p ~/.config/k3d && k3d kubeconfig get dev > $(K3D_DEV_KUBECONFIG)'
	@echo "=== Creating k3d cluster `release` (api :6544, host ports 30000-30881) ==="
	ssh $(K3S_SSH_HOST) '$(K3D_REMOTE_SHELL); k3d cluster list release 2>/dev/null | grep -q "^release " || k3d cluster create release \
		--api-port 6544 \
		--port "30000:30000@loadbalancer" \
		--port "30081:30081@loadbalancer" \
		--port "30181:30181@loadbalancer" \
		--port "30281:30281@loadbalancer" \
		--port "30381:30381@loadbalancer" \
		--port "30481:30481@loadbalancer" \
		--port "30581:30581@loadbalancer" \
		--port "30681:30681@loadbalancer" \
		--port "30781:30781@loadbalancer" \
		--port "30881:30881@loadbalancer" \
		--registry-config ~/.config/k3d/smashing-registries.yaml \
		--volume "$(K3S_MEDIA_DIR):$(K3S_MEDIA_DIR)@server:0" \
		--volume "$(K3S_CERTS_DIR):$(K3S_CERTS_DIR)@server:0" \
		--kubeconfig-update-default=false \
		--kubeconfig-switch-context=false'
	ssh $(K3S_SSH_HOST) '$(K3D_REMOTE_SHELL); mkdir -p ~/.config/k3d && k3d kubeconfig get release > $(K3D_RELEASE_KUBECONFIG)'
	@echo "Both clusters ready. Run \`make deploy\` for dev / \`make deploy-release\` for release."

status-k3s:
	ssh $(K3S_SSH_HOST) '$(K3D_REMOTE_SHELL); k3d cluster list; \
		echo; echo "--- dev ---"; export KUBECONFIG=$(K3D_DEV_KUBECONFIG); kubectl get pods -A; \
		echo; echo "--- release ---"; export KUBECONFIG=$(K3D_RELEASE_KUBECONFIG); kubectl get pods -A'

# `make deploy` and `make deploy-release` are end-to-end — they build +
# push the main image, apply the consolidated `k8s-infinite-streaming.yaml.tmpl`
# AND the analytics tier (ClickHouse + forwarder + Grafana) into the
# stack's k3d cluster. Each cluster has exactly one set of resources —
# stack identity is the cluster context, not a name suffix.

# Each target binds its KUBECONFIG_FILE, SERVER_ID, ANNOUNCE_URL/LABEL,
# and EXTERNAL_PORT_BASE so the same template renders cleanly per stack.
deploy: KUBECONFIG_FILE=$(K3D_DEV_KUBECONFIG)
deploy: SERVER_ID=infinite-streaming-dev
deploy: ANNOUNCE_URL=$(INFINITE_STREAM_ANNOUNCE_URL_K3S_DEV)
deploy: ANNOUNCE_LABEL=$(INFINITE_STREAM_ANNOUNCE_LABEL_K3S_DEV)
deploy: EXTERNAL_PORT_BASE=40081
deploy: analytics-deploy-k3s
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(shell cat VERSION) -t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):dev --push .
	$(MAKE) deploy-k3d K3S_SERVER_IMAGE=$(K3S_REGISTRY)/$(K3S_SERVER_REPO):dev \
		KUBECONFIG_FILE=$(KUBECONFIG_FILE) \
		SERVER_ID=$(SERVER_ID) \
		ANNOUNCE_URL=$(ANNOUNCE_URL) \
		ANNOUNCE_LABEL=$(ANNOUNCE_LABEL) \
		EXTERNAL_PORT_BASE=$(EXTERNAL_PORT_BASE)

deploy-release: KUBECONFIG_FILE=$(K3D_RELEASE_KUBECONFIG)
deploy-release: SERVER_ID=infinite-streaming-release
deploy-release: ANNOUNCE_URL=$(INFINITE_STREAM_ANNOUNCE_URL_K3S_RELEASE)
deploy-release: ANNOUNCE_LABEL=$(INFINITE_STREAM_ANNOUNCE_LABEL_K3S_RELEASE)
deploy-release: EXTERNAL_PORT_BASE=30081
deploy-release: analytics-deploy-k3s
	docker buildx build --platform linux/amd64 \
		--build-arg VERSION=$(shell cat VERSION) \
		-t $(K3S_SERVER_IMAGE) \
		-t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):$(shell cat VERSION) \
		--push .
	$(MAKE) deploy-k3d K3S_SERVER_IMAGE=$(K3S_SERVER_IMAGE) \
		KUBECONFIG_FILE=$(KUBECONFIG_FILE) \
		SERVER_ID=$(SERVER_ID) \
		ANNOUNCE_URL=$(ANNOUNCE_URL) \
		ANNOUNCE_LABEL=$(ANNOUNCE_LABEL) \
		EXTERNAL_PORT_BASE=$(EXTERNAL_PORT_BASE)

# Inner worker — applies the consolidated main-app template against
# whichever k3d cluster's kubeconfig was passed in. Used by both
# `deploy` and `deploy-release`.
deploy-k3d:
	@if [ -z "$(KUBECONFIG_FILE)" ]; then echo "KUBECONFIG_FILE required"; exit 1; fi
	@echo "=== Applying main app to k3d cluster ($(KUBECONFIG_FILE)) ==="
	K3S_REGISTRY='$(K3S_REGISTRY)' \
	K3S_MEDIA_DIR='$(K3S_MEDIA_DIR)' \
	K3S_CERTS_DIR='$(K3S_CERTS_DIR)' \
	SERVER_ID='$(SERVER_ID)' \
	ANNOUNCE_URL='$(ANNOUNCE_URL)' \
	ANNOUNCE_LABEL='$(ANNOUNCE_LABEL)' \
	RENDEZVOUS_URL='$(INFINITE_STREAM_RENDEZVOUS_URL)' \
	EXTERNAL_PORT_BASE='$(EXTERNAL_PORT_BASE)' \
		envsubst < k8s-infinite-streaming.yaml.tmpl | \
		ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(KUBECONFIG_FILE); kubectl apply -f -"
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(KUBECONFIG_FILE); kubectl set image deployment/infinite-streaming go-server=$(K3S_SERVER_IMAGE)"
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(KUBECONFIG_FILE); kubectl rollout restart deployment/infinite-streaming"
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(KUBECONFIG_FILE); kubectl rollout status deployment/infinite-streaming --timeout=180s"
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(KUBECONFIG_FILE); kubectl get pods -o wide; echo; kubectl get svc"

logs:
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3D_DEV_KUBECONFIG); kubectl logs deploy/infinite-streaming --all-containers -f"

# ── Analytics tier deployment ──────────────────────────────────────────
# One analytics tier per cluster (no shared state — each k3d cluster
# has its own clickhouse, forwarder, grafana). Inside both clusters the
# forwarder's SSE source is the same in-cluster URL, since each cluster
# has exactly one `infinite-streaming` Service at NodePort 30081.

ANALYTICS_SSE_URL ?= http://infinite-streaming:30081/api/sessions/stream

# Build + push the forwarder image into the cluster's registry. Same
# image is shared across stacks (cluster-agnostic).
analytics-build-forwarder-k3s:
	docker buildx build --platform linux/amd64 \
		-t $(K3S_REGISTRY)/infinite-streaming-forwarder:dev \
		--push ./analytics/go-forwarder

# Apply the analytics manifest into the cluster pointed at by
# $(KUBECONFIG_FILE). Idempotent.
analytics-deploy-k3s: analytics-build-forwarder-k3s
	@if [ -z "$(KUBECONFIG_FILE)" ]; then echo "KUBECONFIG_FILE required (set by deploy / deploy-release targets)"; exit 1; fi
	@echo "=== Applying analytics tier ($(KUBECONFIG_FILE)) ==="
	ANALYTICS_SSE_URL='$(ANALYTICS_SSE_URL)' \
	K3S_REGISTRY='$(K3S_REGISTRY)' \
		envsubst < k8s-analytics.yaml | \
		ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(KUBECONFIG_FILE); kubectl apply -f -"
	@echo "=== Waiting for analytics rollout ==="
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(KUBECONFIG_FILE); \
		kubectl rollout status statefulset/clickhouse --timeout=120s; \
		kubectl rollout status deployment/forwarder --timeout=120s; \
		kubectl rollout status deployment/grafana --timeout=120s"

# Tear down a single k3d cluster — wipes everything in it (analytics +
# main app + PVC + node containers). The other cluster stays untouched
# so each can be exercised in isolation.
teardown-dev:
	ssh $(K3S_SSH_HOST) '$(K3D_REMOTE_SHELL); k3d cluster delete dev'
	@echo "Cluster `dev` deleted. Re-run \`make k3d-bootstrap\` then \`make deploy\` to bring it back."

teardown-release:
	ssh $(K3S_SSH_HOST) '$(K3D_REMOTE_SHELL); k3d cluster delete release'
	@echo "Cluster `release` deleted. Re-run \`make k3d-bootstrap\` then \`make deploy-release\` to bring it back."

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

test-deploy-all: test-deploy-compose test-deploy-ghcr test-deploy-registry

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
	ssh $(TEST_SSH) 'docker rm -f test-dev-server test-compose-server test-ghcr-server test-registry-server test-oobe-server 2>/dev/null; docker network prune -f 2>/dev/null'

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

# `test-deploy-run` (the bare `docker run` of a single container)
# was removed in #394 — the install method it tested can't support
# analytics by definition (one image, no sidecars), and analytics is
# now a baseline expectation in every deploy path. The
# `test-deploy-{compose,ghcr,registry}` variants cover the actual
# install methods documented in the README.

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
