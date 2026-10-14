PROJECT := mosdns-koyeb-doh-gateway
IMAGE ?= $(PROJECT):local
VERSION := $(shell cat VERSION)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: test fmt-check check-config entrypoint-test vet build docker-build run clean

# Unit tests always run with the race detector: the gateway is heavily
# concurrent (slots, limiter, shared readiness probe).
test: fmt-check check-config entrypoint-test vet
	go test -race ./...

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check-config:
	sh ./check-config.sh mosdns.yaml

entrypoint-test:
	sh ./test-entrypoint.sh

vet:
	go vet ./...

# Builds for the host platform; the Dockerfile handles the container target.
build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w -buildid= -X main.version=$(VERSION)' -o doh-gateway .

docker-build:
	docker build --build-arg GATEWAY_VERSION=$(VERSION) --build-arg BUILD_DATE=$(BUILD_DATE) -t $(IMAGE) .

run: build
	PORT=8080 ./doh-gateway

clean:
	rm -f doh-gateway
