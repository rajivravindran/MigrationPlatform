SHELL := /usr/bin/env bash
COMPOSE := docker compose -f infra/docker-compose.yml
COMPOSE_HUB := docker compose -f infra/docker-compose.yml -f infra/docker-compose.hub.yml
PROJECT := migration-platform
HUB_IMAGE_PREFIX ?= rajivravindran
IMAGE_TAG ?= demo

.PHONY: help up hub-up hub-pull demo-pack down logs ps build rebuild seed smoke test test-rust test-go test-py test-web lint fmt clean keys

help:
	@echo "Migration Platform — laptop developer targets"
	@echo
	@echo "  make keys        Generate dev JWT RSA keypair into infra/secrets/"
	@echo "  make up          Start the full stack (compose, builds locally)"
	@echo "  make hub-pull    Pull app images from Docker Hub"
	@echo "  make hub-up      Start stack using Hub images (no local build)"
	@echo "  make demo-pack   Zip infra/demo-pack for sharing (no git needed)"
	@echo "  make down        Stop and remove the stack (keeps volumes)"
	@echo "  make nuke        Stop and remove the stack INCLUDING volumes"
	@echo "  make ps          Show service status"
	@echo "  make logs s=api  Tail logs for a service"
	@echo "  make build       Build all images"
	@echo "  make rebuild     Force rebuild of all images"
	@echo "  make seed        Create demo org, admin user, sample template + CSV"
	@echo "  make smoke       Run k6 compose smoke (100k rows by default)"
	@echo "  make test        Run all unit/integration tests"
	@echo
	@echo "Hub defaults: HUB_IMAGE_PREFIX=$(HUB_IMAGE_PREFIX) IMAGE_TAG=$(IMAGE_TAG)"
	@echo

keys:
	@mkdir -p infra/secrets
	@test -f infra/secrets/jwt_private.pem || ( \
	    openssl genpkey -algorithm RSA -out infra/secrets/jwt_private.pem -pkeyopt rsa_keygen_bits:2048 && \
	    openssl rsa -in infra/secrets/jwt_private.pem -pubout -out infra/secrets/jwt_public.pem && \
	    echo "Generated dev JWT keypair under infra/secrets/" )

up: keys
	$(COMPOSE) up -d --build
	@echo "\nMigration Platform is starting..."
	@echo "  Web:      http://localhost:3000"
	@echo "  API:      http://localhost:8080"
	@echo "  Temporal: http://localhost:8233"
	@echo "  Grafana:  http://localhost:3001"
	@echo "  MinIO:    http://localhost:9001 (minio / minio123)"

hub-pull:
	@for s in api orchestrator transform-worker web; do \
	  echo "Pulling $(HUB_IMAGE_PREFIX)/migration-$$s:$(IMAGE_TAG)"; \
	  docker pull "$(HUB_IMAGE_PREFIX)/migration-$$s:$(IMAGE_TAG)"; \
	done

hub-up: keys hub-pull
	HUB_IMAGE_PREFIX=$(HUB_IMAGE_PREFIX) IMAGE_TAG=$(IMAGE_TAG) $(COMPOSE_HUB) up -d --no-build
	@echo "\nMigration Platform is starting from Docker Hub ($(HUB_IMAGE_PREFIX)/*:$(IMAGE_TAG))..."
	@echo "  Web:      http://localhost:3000"
	@echo "  API:      http://localhost:8080"
	@echo "  Temporal: http://localhost:8088"
	@echo "  Grafana:  http://localhost:3001"
	@echo "  MinIO:    http://localhost:9001 (minio / minio123)"
	@echo "Next: make seed   # admin@example.com / admin123"

# Shareable zip: compose + scripts only. Recipients need Docker, not git.
demo-pack:
	@mkdir -p dist
	@rm -f dist/migration-demo-pack.zip
	@cd infra && zip -r ../dist/migration-demo-pack.zip demo-pack \
	  -x 'demo-pack/secrets/*' 'demo-pack/**/*.pem' 'demo-pack/**/.DS_Store'
	@echo "Wrote dist/migration-demo-pack.zip — share this file (no git access required)."
	@echo "Recipient: unzip && cd demo-pack && ./up.sh"

down:
	$(COMPOSE) down

nuke:
	$(COMPOSE) down -v --remove-orphans

ps:
	$(COMPOSE) ps

logs:
	@[ -n "$(s)" ] || { echo "usage: make logs s=<service>"; exit 1; }
	$(COMPOSE) logs -f $(s)

build:
	$(COMPOSE) build

rebuild:
	$(COMPOSE) build --no-cache

seed:
	./scripts/seed.sh

smoke:
	ROWS=$${ROWS:-100000} BASE_URL=$${BASE_URL:-http://localhost:8080} \
	  k6 run tests/load/smoke-compose.js

test: test-rust test-go test-py test-web

test-rust:
	cargo test --manifest-path apps/api/Cargo.toml

test-go:
	cd apps/orchestrator-go && go test ./...

test-py:
	cd apps/transform-worker && python -m pytest

test-web:
	cd apps/web && npm test || true

lint:
	cargo clippy --manifest-path apps/api/Cargo.toml -- -D warnings
	cd apps/orchestrator-go && golangci-lint run ./...
	cd apps/transform-worker && ruff check .
	cd apps/web && npm run lint

fmt:
	cargo fmt --manifest-path apps/api/Cargo.toml
	cd apps/orchestrator-go && gofmt -w .
	cd apps/transform-worker && ruff format .
