PROJECT := mosdns-koyeb-doh-gateway
IMAGE ?= $(PROJECT):local

.PHONY: test check-config vet build docker-build run

test: check-config vet
	go test ./...

check-config:
	sh ./check-config.sh mosdns.yaml

vet:
	go vet ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o doh-gateway .

docker-build:
	docker build --build-arg GATEWAY_VERSION=$$(cat VERSION) -t $(IMAGE) .

run:
	PORT=8080 ./doh-gateway
