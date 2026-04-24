
# Load .env if present (provides K3S_SSH_HOST, K3S_REGISTRY, etc.)
-include .env
export




K3S_SSH_HOST ?= user@your-k3s-host
K3S_KUBECONFIG ?= /home/user/.kube/config
GO_SERVER_IMAGE ?= ghcr.io/jonathaneoliver/infinite-streaming:latest
GO_PROXY_IMAGE ?= ghcr.io/jonathaneoliver/go-proxy:latest
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
K3S_PROXY_REPO ?= go-proxy

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

build-go-proxy-k3s:
	docker build --no-cache --progress=plain --build-arg VERSION=$(shell cat VERSION) -t $(K3S_REGISTRY)/$(K3S_PROXY_REPO):latest ./go-proxy

push-go-proxy-k3s:
	docker push $(K3S_REGISTRY)/$(K3S_PROXY_REPO):latest

build-push-go-proxy-k3s: build-go-proxy-k3s push-go-proxy-k3s

K3S_SERVER_IMAGE ?= $(K3S_REGISTRY)/$(K3S_SERVER_REPO):latest
K3S_PROXY_IMAGE ?= $(K3S_REGISTRY)/$(K3S_PROXY_REPO):latest

deploy-k3s-local:
	@set -e; \
	echo "Cleaning up legacy split deployments/services"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl delete service go-server go-proxy boss-server --ignore-not-found=true"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl delete deployment go-server go-proxy boss-server --ignore-not-found=true"; \
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

deploy:
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(shell cat VERSION) -t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):dev --push .
	$(MAKE) deploy-k3s K3S_KUBECONFIG=$(K3S_KUBECONFIG) K8S_MANIFESTS=k8s-infinite-streaming-dev.yaml K8S_DEPLOYMENT=infinite-streaming-dev K3S_SERVER_IMAGE=$(K3S_REGISTRY)/$(K3S_SERVER_REPO):dev

logs:
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl logs deploy/infinite-streaming-dev --all-containers -f"

deploy-release:
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(shell cat VERSION) -t $(K3S_SERVER_IMAGE) --push .
	$(MAKE) deploy-k3s K3S_KUBECONFIG=$(K3S_KUBECONFIG) K3S_SERVER_IMAGE=$(K3S_SERVER_IMAGE)

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

test-clean-dev:
	ssh $(TEST_SSH) 'docker rm -f test-dev-server 2>/dev/null'

test-clean:
	ssh $(TEST_SSH) 'docker rm -f test-dev-server test-compose-server test-docker-run test-ghcr-server test-registry-server 2>/dev/null; docker network prune -f 2>/dev/null'

test-status:
	@ssh $(TEST_SSH) 'for p in 21000 22000 23000 24000 25000; do \
		proxy=$$((p / 1000 * 1000 + 81)); \
		ui=$$(curl -s -o /dev/null -w "%{http_code}" http://localhost:$$p/); \
		px=$$(curl -s -o /dev/null -w "%{http_code}" http://localhost:$$proxy/api/sessions 2>/dev/null); \
		echo "Port $$p: UI=$$ui Proxy=$$proxy=$$px"; \
	done'

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
	$(ANDROID_SDK_HOME)/platform-tools/adb uninstall com.infinitestream.player 2>/dev/null || true
	cd $(ANDROIDTV_DIR) && \
		JAVA_HOME="$(JAVA_HOME_ANDROID)" \
		ANDROID_HOME="$(ANDROID_SDK_HOME)" \
		PATH="$(ANDROID_SDK_HOME)/platform-tools:$$PATH" \
		./gradlew installDebug
	$(ANDROID_SDK_HOME)/platform-tools/adb shell am start -n com.infinitestream.player/.MainActivity

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
