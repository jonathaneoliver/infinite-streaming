
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
	./boss.sh 1 run

stop:
	./boss.sh 1 stop

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

K3S_REGISTRY ?= your-registry:5000
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
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl delete service go-server go-proxy memcached boss-server --ignore-not-found=true"; \
	ssh $(K3S_SSH_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl delete deployment go-server go-proxy memcached boss-server --ignore-not-found=true"; \
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

test-ios-sim-metrics:
	PYTEST_DISABLE_PLUGIN_AUTOLOAD=1 \
	IOS_SIM_TEST_RUN=1 \
	IOS_VERBOSE=1 \
	IOS_SIM_DEVICE="$(IOS_SIM_DEVICE)" \
	IOS_APP_BUNDLE_ID="$(IOS_APP_BUNDLE_ID)" \
	IOS_METRICS_DURATION=$(IOS_METRICS_DURATION) \
	IOS_SCORE_MIN=$(IOS_SCORE_MIN) \
	pytest tests/integration -k ios_simulator_pyramid_metrics -m integration -vv --api-base $(IOS_API_BASE)
