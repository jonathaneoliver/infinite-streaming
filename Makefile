
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
	./start.sh 1 run

run-ghcr:
	docker compose -f docker-compose.ghcr.yml up -d

stop-ghcr:
	docker compose -f docker-compose.ghcr.yml down

stop:
	./start.sh 1 stop

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

K3S_REGISTRY ?= 100.111.190.54:5000
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
		envsubst < $$manifest | ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl apply -f -"; \
	done; \
	echo "Updating deployment images"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl set image deployment/$(K8S_DEPLOYMENT) go-server=$(K3S_SERVER_IMAGE)"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl set image deployment/$(K8S_DEPLOYMENT) go-proxy=$(K3S_PROXY_IMAGE)"; \
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
	docker buildx build --platform linux/amd64 -t $(K3S_REGISTRY)/$(K3S_SERVER_REPO):dev --push .
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(shell cat VERSION) -t $(K3S_REGISTRY)/$(K3S_PROXY_REPO):dev --push ./go-proxy
	$(MAKE) deploy-k3s K3S_KUBECONFIG=$(K3S_KUBECONFIG) K8S_MANIFESTS=k8s-infinite-streaming-dev.yaml K8S_DEPLOYMENT=infinite-streaming-dev K3S_SERVER_IMAGE=$(K3S_REGISTRY)/$(K3S_SERVER_REPO):dev K3S_PROXY_IMAGE=$(K3S_REGISTRY)/$(K3S_PROXY_REPO):dev

logs:
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl logs deploy/infinite-streaming-dev --all-containers -f"

deploy-release:
	docker buildx build --platform linux/amd64 -t $(K3S_SERVER_IMAGE) --push .
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(shell cat VERSION) -t $(K3S_PROXY_IMAGE) --push ./go-proxy
	$(MAKE) deploy-k3s K3S_KUBECONFIG=$(K3S_KUBECONFIG) K3S_SERVER_IMAGE=$(K3S_SERVER_IMAGE) K3S_PROXY_IMAGE=$(K3S_PROXY_IMAGE)

# ── Remote deployment testing ──────────────────────────────────────────
# Deploy all 4 installation methods to a remote Docker host for parallel testing.
# Configure TEST_SSH and TEST_MEDIA_DIR in .env.

TEST_SSH ?= user@test-host
TEST_MEDIA_DIR ?= /home/user/media
REPO_URL ?= https://github.com/jonathaneoliver/infinite-streaming.git

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
	ssh -n $(TEST_SSH) 'echo "CONTENT_DIR=$(TEST_MEDIA_DIR)" > ~/test-dev/.env'
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
	ssh $(TEST_SSH) 'echo "CONTENT_DIR=$(TEST_MEDIA_DIR)" > ~/test-compose/.env'
	scp tests/deploy/override-compose.yml $(TEST_SSH):~/test-compose/docker-compose.override.yml
	ssh $(TEST_SSH) 'cd ~/test-compose && docker compose build && docker compose up -d'

test-deploy-run:
	@echo "=== Option 2: Docker run (port 23000) ==="
	ssh $(TEST_SSH) 'docker rm -f test-docker-run 2>/dev/null; \
		docker run -d --name test-docker-run --cap-add NET_ADMIN --privileged \
		-p 23000:30000 -p 23081:30081 -p 23181:30181 -p 23281:30281 -p 23381:30381 -p 23481:30481 \
		-p 23581:30581 -p 23681:30681 -p 23781:30781 -p 23881:30881 \
		-v $(TEST_MEDIA_DIR):/media \
		infinite-streaming:latest /sbin/launch.sh 1'

test-deploy-ghcr:
	@echo "=== Option 3: GHCR pre-built (port 24000) ==="
	ssh $(TEST_SSH) 'mkdir -p ~/test-ghcr'
	ssh $(TEST_SSH) 'if [ -d ~/test-compose ]; then cp ~/test-compose/docker-compose.ghcr.yml ~/test-ghcr/docker-compose.yml; fi'
	ssh $(TEST_SSH) 'echo "CONTENT_DIR=$(TEST_MEDIA_DIR)" > ~/test-ghcr/.env'
	scp tests/deploy/override-ghcr.yml $(TEST_SSH):~/test-ghcr/docker-compose.override.yml
	ssh $(TEST_SSH) 'cd ~/test-ghcr && docker compose up -d'

test-deploy-registry:
	@echo "=== Option 4: Private registry (port 25000) ==="
	ssh $(TEST_SSH) 'mkdir -p ~/test-registry'
	scp tests/deploy/docker-compose.registry.yml $(TEST_SSH):~/test-registry/docker-compose.yml
	ssh $(TEST_SSH) 'echo "CONTENT_DIR=$(TEST_MEDIA_DIR)" > ~/test-registry/.env && echo "K3S_REGISTRY=$(K3S_REGISTRY)" >> ~/test-registry/.env'
	ssh $(TEST_SSH) 'cd ~/test-registry && docker compose up -d'

# ── Screenshots ────────────────────────────────────────────────────────

SCREENSHOT_HOST ?= http://localhost:30000
SCREENSHOT_VENV ?= /tmp/ism-shot-venv

screenshots:
	@[ -d "$(SCREENSHOT_VENV)" ] || (python3 -m venv "$(SCREENSHOT_VENV)" \
		&& "$(SCREENSHOT_VENV)/bin/pip" install -q -r scripts/requirements.txt \
		&& "$(SCREENSHOT_VENV)/bin/playwright" install chrome)
	"$(SCREENSHOT_VENV)/bin/python" scripts/capture-screenshots.py --base-url=$(SCREENSHOT_HOST)

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
