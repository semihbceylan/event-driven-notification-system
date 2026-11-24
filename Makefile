.PHONY: up down logs build test test-web smoke webhook-url lint fmt

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

test-web:      ## Dashboard unit tests
	cd web && npm ci --no-audit --no-fund && npm test

smoke:         ## End-to-end checks against a running stack (make up first)
	./scripts/smoke.sh

webhook-url:   ## Fresh webhook.site URL into .env (free URLs: 50 requests); SELECT=1 also switches to it
	SELECT=$(SELECT) ./scripts/webhook-url.sh

lint:          ## Needs golangci-lint v2 installed; CI runs it on every push
	golangci-lint run

fmt:
	gofmt -w .
