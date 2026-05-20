.PHONY: up down build build-rotate-key test lint tidy

up:
	docker compose up -d

down:
	docker compose down

tidy:
	go mod tidy

build:
	go build ./...

build-rotate-key:
	go build -o bin/rotate-key ./cmd/rotate-key

test:
	go test ./...

lint:
	go vet ./...
