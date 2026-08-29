.PHONY: setup
setup:
	go mod download

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: test
test:
	go test -v -race -coverprofile=coverage.out ./...

.PHONY: build
build:
	mkdir -p ./bin
	go build -v -o ./bin/hc-export ./cmd/hc-export

.PHONY: run
run:
	go run ./cmd/hc-export
