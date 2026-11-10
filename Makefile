VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: run test build docker clean runner runner-all runner-docker

run:
	ATELIER_DATA=./data go run ./cmd/atelier

test:
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/atelier ./cmd/atelier

docker:
	docker build --build-arg VERSION=$(VERSION) -t atelier:latest .

# atelier-runner for this machine, and cross-compiled for every device
RUNNER_GO := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

runner:
	$(RUNNER_GO) -o bin/atelier-runner ./cmd/atelier-runner

runner-all:
	GOOS=darwin  GOARCH=arm64 $(RUNNER_GO) -o bin/atelier-runner-darwin-arm64      ./cmd/atelier-runner
	GOOS=darwin  GOARCH=amd64 $(RUNNER_GO) -o bin/atelier-runner-darwin-amd64      ./cmd/atelier-runner
	GOOS=linux   GOARCH=amd64 $(RUNNER_GO) -o bin/atelier-runner-linux-amd64       ./cmd/atelier-runner
	GOOS=linux   GOARCH=arm64 $(RUNNER_GO) -o bin/atelier-runner-linux-arm64       ./cmd/atelier-runner
	GOOS=windows GOARCH=amd64 $(RUNNER_GO) -o bin/atelier-runner-windows-amd64.exe ./cmd/atelier-runner
	GOOS=windows GOARCH=arm64 $(RUNNER_GO) -o bin/atelier-runner-windows-arm64.exe ./cmd/atelier-runner

runner-docker:
	docker build --build-arg VERSION=$(VERSION) --target runner -t atelier-runner:latest .

clean:
	rm -rf bin
