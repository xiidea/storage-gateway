.PHONY: up down build test lint tidy

up:
	docker compose up -d

down:
	docker compose down

tidy:
	go mod tidy

build:
	go build ./...

test:
	go test ./...

lint:
	go vet ./...
