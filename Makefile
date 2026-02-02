

LENOVO_HOST ?= jonathanoliver@lenovo.local
K3S_KUBECONFIG ?= /home/jonathanoliver/.kube/config
GO_SERVER_IMAGE ?= ghcr.io/jonathaneoliver/infinite-streaming:latest
GO_PROXY_IMAGE ?= ghcr.io/jonathaneoliver/go-proxy:latest
K8S_MANIFESTS ?= k8s-infinite-streaming.yaml

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

LENOVO_REGISTRY ?= 192.168.0.189:5000
LENOVO_SERVER_REPO ?= infinite-streaming
LENOVO_PROXY_REPO ?= go-proxy

build-lenovo:
	docker build --no-cache --progress=plain -t $(LENOVO_REGISTRY)/$(LENOVO_SERVER_REPO):latest .

buildx-lenovo-amd64:
	docker buildx build --platform linux/amd64 -t $(LENOVO_REGISTRY)/$(LENOVO_SERVER_REPO):amd64 --load .

buildx-lenovo-arm64:
	docker buildx build --platform linux/arm64 -t $(LENOVO_REGISTRY)/$(LENOVO_SERVER_REPO):arm64 --load .

buildx-lenovo-all:
	$(MAKE) buildx-lenovo-amd64
	$(MAKE) buildx-lenovo-arm64

push-lenovo:
	docker push $(LENOVO_REGISTRY)/$(LENOVO_SERVER_REPO):latest

push-lenovo-all:
	docker push $(LENOVO_REGISTRY)/$(LENOVO_SERVER_REPO):amd64
	docker push $(LENOVO_REGISTRY)/$(LENOVO_SERVER_REPO):arm64

build-push-lenovo: build-lenovo push-lenovo

build-push-lenovo-all: buildx-lenovo-all push-lenovo-all

build-go-proxy-lenovo:
	docker build --no-cache --progress=plain -t $(LENOVO_REGISTRY)/$(LENOVO_PROXY_REPO):latest ./go-proxy

push-go-proxy-lenovo:
	docker push $(LENOVO_REGISTRY)/$(LENOVO_PROXY_REPO):latest

build-push-go-proxy-lenovo: build-go-proxy-lenovo push-go-proxy-lenovo

LENOVO_SERVER_IMAGE ?= 192.168.0.189:5000/infinite-streaming:latest
LENOVO_PROXY_IMAGE ?= 192.168.0.189:5000/go-proxy:latest

deploy-lenovo-k3s-local:
	@set -e; \
	echo "Cleaning up legacy split deployments/services"; \
	ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl delete service go-server go-proxy memcached boss-server --ignore-not-found=true"; \
	ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl delete deployment go-server go-proxy memcached boss-server --ignore-not-found=true"; \
	for manifest in $(K8S_MANIFESTS); do \
		echo "Applying $$manifest to $(LENOVO_HOST)"; \
		ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl apply -f -" < $$manifest; \
	done; \
	echo "Updating deployment images"; \
	ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl set image deployment/infinite-streaming go-server=$(LENOVO_SERVER_IMAGE)"; \
	ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl set image deployment/infinite-streaming go-proxy=$(LENOVO_PROXY_IMAGE)"; \
	echo "Restarting deployments explicitly"; \
	ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl rollout restart deployment/infinite-streaming"; \
	echo "Waiting for rollout"; \
	ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl rollout status deployment/infinite-streaming --timeout=180s"; \
	echo "Deployment status"; \
	ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl get pods -n default -o wide; echo; kubectl get svc -n default"

deploy-lenovo-k3s: deploy-lenovo-k3s-local

status-lenovo-k3s:
	ssh $(LENOVO_HOST) "export KUBECONFIG=$(K3S_KUBECONFIG); kubectl get nodes; echo; kubectl get pods -A"

deploy:
	docker buildx build --platform linux/amd64 -t $(LENOVO_SERVER_IMAGE) --push .
	docker buildx build --platform linux/amd64 -t $(LENOVO_PROXY_IMAGE) --push ./go-proxy
	$(MAKE) deploy-lenovo-k3s K3S_KUBECONFIG=$(K3S_KUBECONFIG) LENOVO_SERVER_IMAGE=$(LENOVO_SERVER_IMAGE) LENOVO_PROXY_IMAGE=$(LENOVO_PROXY_IMAGE)
