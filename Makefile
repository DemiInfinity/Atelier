VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: run test build docker clean

run:
	ATELIER_DATA=./data go run ./cmd/atelier

test:
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/atelier ./cmd/atelier

docker:
	docker build --build-arg VERSION=$(VERSION) -t atelier:latest .

clean:
	rm -rf bin
