.PHONY: help build test vet lint fmt check clean

help:
	@echo "make build  — compile roozane to ./roozane"
	@echo "make test   — race-mode unit tests"
	@echo "make vet    — go vet"
	@echo "make lint   — golangci-lint run"
	@echo "make fmt    — gofumpt -w ."
	@echo "make check  — vet + test + lint (what CI runs)"
	@echo "make clean  — remove built binaries"

build:
	go build -o roozane ./cmd/roozane

test:
	go test -race -timeout 2m ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofumpt -w .

check: vet test lint

clean:
	rm -f roozane
