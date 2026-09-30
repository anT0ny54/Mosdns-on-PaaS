PROJECT := mosdns-koyeb-doh-gateway
IMAGE ?= $(PROJECT):local
VERSION := $(shell cat VERSION)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: test check-config vet build docker-build run clean

test: check-config vet
	go test ./...

check-config:
	sh ./check-config.sh mosdns.yaml

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
