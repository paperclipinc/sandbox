IMG_CONTROLLER ?= ghcr.io/paperclipinc/mitos-controller:latest
IMG_FORKD ?= ghcr.io/paperclipinc/mitos-forkd:latest

.PHONY: all build test generate manifests proto docker-build docker-push install deploy

all: build

build:
	go build -o bin/controller ./cmd/controller/
	go build -o bin/forkd ./cmd/forkd/

test-unit:
	go test ./internal/fork/... ./internal/workspace/... ./internal/vsock/... -v -count=1

test-controller:
	eval $$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use 1.31 -p env) && \
		go test ./internal/controller/... -v -count=1 -timeout 120s

test-python:
	cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -v

test-e2e:
	bash hack/e2e-test.sh

test: test-unit test-python

generate:
	controller-gen object paths="./api/..."

manifests:
	controller-gen crd paths="./api/..." output:crd:artifacts:config=deploy/crds

proto:
	protoc \
		--go_out=. --go_opt=module=github.com/paperclipinc/mitos \
		--go-grpc_out=. --go-grpc_opt=module=github.com/paperclipinc/mitos \
		proto/forkd.proto

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
