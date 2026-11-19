# Task runner. `make help` lists the targets.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# Use the versions pinned in mise.toml even when mise isn't activated in the shell
# (CI, agents, non-interactive shells).
MISE_BIN_PATHS := $(shell mise bin-paths 2>/dev/null | paste -sd: -)
ifneq ($(MISE_BIN_PATHS),)
export PATH := $(MISE_BIN_PATHS):$(PATH)
endif

DEV_COMPOSE := docker compose -f compose.dev.yml

# compose.dev.yml's database. CI passes its own.
TEST_DATABASE_URL ?= postgres://app:dev@127.0.0.1:5433/app_test

.PHONY: help setup test test-integration lint fmt check db-up db-down

help: ## List available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(firstword $(MAKEFILE_LIST))

setup: ## Install pinned tools (mise) and Go modules
	mise install
	mise exec -- go mod download

test: ## Unit tests (no database)
	env -u TEST_DATABASE_URL go test ./...

# Unit tests run too. -count=1 because the database's state isn't part of Go's test cache key.
test-integration: ## Unit and integration tests against TEST_DATABASE_URL (make db-up first)
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -count=1 ./...

# golangci-lint also fails on code its formatters (gofmt, goimports) would change.
lint: ## go.mod tidiness, go vet, golangci-lint
	go mod tidy -diff
	go vet ./...
	golangci-lint run ./...

fmt: ## Format Go code (gofmt + goimports)
	golangci-lint fmt ./...

check: lint test ## Everything that must pass before a change is done

db-up: ## Start the integration tests' Postgres 18 on 127.0.0.1:5433 and wait until it's ready
	$(DEV_COMPOSE) up -d --wait db

db-down: ## Stop it (its data is thrown away)
	$(DEV_COMPOSE) down
