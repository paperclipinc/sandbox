IMG_CONTROLLER ?= ghcr.io/paperclipinc/sandbox-controller:latest
IMG_FORKD ?= ghcr.io/paperclipinc/sandbox-forkd:latest

.PHONY: all build test generate manifests docker-build docker-push install deploy

all: build

build:
	go build -o bin/controller ./cmd/controller/
	go build -o bin/forkd ./cmd/forkd/

test:
	go test ./... -v

generate:
	controller-gen object paths="./api/..."

manifests:
	controller-gen crd paths="./api/..." output:crd:artifacts:config=deploy/crds

proto:
	protoc --go_out=. --go-grpc_out=. proto/forkd.proto

docker-build:
	docker build -f Dockerfile.controller -t $(IMG_CONTROLLER) .
	docker build -f Dockerfile.forkd -t $(IMG_FORKD) .

docker-push:
	docker push $(IMG_CONTROLLER)
	docker push $(IMG_FORKD)

install:
	kubectl apply -f deploy/controller/namespace.yaml
	kubectl apply -f deploy/crds/
	kubectl apply -f deploy/controller/
	kubectl apply -f deploy/daemon/

deploy: docker-build docker-push install

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/
