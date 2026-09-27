PROJECT := mosdns-koyeb-doh-gateway
IMAGE ?= $(PROJECT):local

.PHONY: test build docker-build run

test:
	go test ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w -buildid=' -o doh-gateway .

docker-build:
	docker build --build-arg GATEWAY_VERSION=$$(cat VERSION) -t $(IMAGE) .

run:
	PORT=8080 ./doh-gateway
