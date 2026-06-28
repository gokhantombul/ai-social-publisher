.PHONY: help build run dev test test-race fmt fmt-check tidy vet vuln lint ci db-up db-down docker-up docker-down migrate-up migrate-down

BINARY := bin/ai-social-publisher
PKG := ./...
CONFIG ?= config.yaml
APP_ENV ?= development

ifneq ($(filter dev,$(MAKECMDGOALS)),)
APP_ENV := development
endif

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## Build the server binary
	go build -o $(BINARY) ./cmd/server

run: ## Run the server (uses CONFIG=config.yaml + .env)
	APP_ENV=$(APP_ENV) go run ./cmd/server serve --config $(CONFIG)

dev: ## Select development environment (use: make run dev)
	@:

test: ## Run unit tests
	go test $(PKG) -count=1

test-race: ## Run tests with race detector
	go test -race $(PKG) -count=1

tidy: ## go mod tidy
	go mod tidy

fmt: ## Format code
	go fmt $(PKG)

fmt-check: ## Check formatting without modifying files
	@test -z "$$(gofmt -l .)"

vet: ## Run go vet
	go vet $(PKG)

vuln: ## Scan reachable code for known vulnerabilities
	govulncheck $(PKG)

lint: fmt-check vet ## Run static checks

ci: lint test-race build ## Run the local CI subset

db-up: ## Start only the development postgres container
	docker compose up -d postgres

db-down: ## Stop the development postgres container
	docker compose stop postgres

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
