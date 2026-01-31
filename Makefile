

run:
	./boss.sh 1 run

stop:
	./boss.sh 1 stop

shell:
	docker exec -it boss1 /bin/sh

build:
	docker build --no-cache --progress=plain -t boss-server .

buildkit:
	DOCKER_BUILDKIT=1 docker build -t boss-server .

buildx:
	$(MAKE) buildx-amd64
	$(MAKE) buildx-arm64

buildx-amd64:
	docker buildx build --platform linux/amd64 -t boss-server:amd64 --load .

buildx-arm64:
	docker buildx build --platform linux/arm64 -t boss-server:arm64 --load .

buildx-push:
	docker buildx build --platform linux/amd64,linux/arm64 -t boss-server:latest --push .
