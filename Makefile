.PHONY: build test lint fmt
build:
	go build -o bin/nest-bridge ./cmd/nest-bridge
test:
	go test -race -cover ./...
fmt:
	gofmt -w .
lint:
	go vet ./...
