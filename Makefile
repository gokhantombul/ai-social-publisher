.PHONY: help build run test tidy fmt vet lint docker-up docker-down migrate-up migrate-down

BINARY := bin/ai-social-publisher
PKG := ./...

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## Build the server binary
	go build -o $(BINARY) ./cmd/server

run: ## Run the server (uses config.yaml + .env)
	go run ./cmd/server

test: ## Run unit tests
	go test $(PKG) -count=1

tidy: ## go mod tidy
	go mod tidy

fmt: ## Format code
	go fmt $(PKG)

vet: ## Run go vet
	go vet $(PKG)

docker-up: ## Start docker compose stack (postgres + app)
	docker compose up --build -d

docker-down: ## Stop docker compose stack
	docker compose down

# Migrations are also applied automatically on startup when database.auto_migrate=true.
# These targets are for running goose manually via docker.
migrate-up: ## Apply all migrations using goose docker image
	docker compose run --rm app /app/ai-social-publisher migrate up

migrate-down: ## Roll back the last migration
	docker compose run --rm app /app/ai-social-publisher migrate down
