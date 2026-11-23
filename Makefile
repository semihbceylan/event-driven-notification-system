.PHONY: up down logs build test lint fmt

up:            ## Start the whole stack
	docker compose up --build

down:          ## Stop the stack and delete its data
	docker compose down -v

logs:
	docker compose logs -f api worker

build:
	go build ./...

test:          ## Run all tests (integration tests need Docker)
	go test -race -count=1 ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .
