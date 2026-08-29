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
	go build -v -o ./bin/health-connect-converter ./cmd/health-connect-converter

.PHONY: run
run:
	go run ./cmd/health-connect-converter
