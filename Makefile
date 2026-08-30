# Thin wrappers. Every target is one command a reader can run directly, because
# `make` is not present by default on Windows and this project is developed
# there. `go test ./...` remains the canonical documented command, and CI calls
# the underlying tools rather than these targets so a broken Makefile cannot
# mask a broken build. scripts/dev.ps1 mirrors these for PowerShell.

.PHONY: help test test-race vet fmt fmt-check build run validate freshness eval docker up down smoke console-dev tidy

help:
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "};{printf "  %-14s %s\n", $$1, $$2}'

test:       ## go test ./...
	go test ./...

test-race:  ## go test -race ./... (needs a C toolchain; CI runs this on Linux)
	go test -race ./...

vet:        ## go vet ./...
	go vet ./...

fmt:        ## gofmt -w .
	gofmt -w cmd internal

fmt-check:  ## fail if anything is unformatted
	@test -z "$$(gofmt -l cmd internal)" || { echo "unformatted:"; gofmt -l cmd internal; exit 1; }

build:      ## build both binaries
	go build -o relay ./cmd/relay
	go build -o relay-eval ./cmd/relay-eval

run:        ## run the gateway against config/
	go run ./cmd/relay

validate:   ## check config and report price-attestation ages
	go run ./cmd/relay -validate

freshness:  ## fail 14 days before the price attestations would stop the gateway
	go run ./cmd/relay -validate -max-price-age 1848h

eval:       ## run the offline quality eval suite
	go run ./cmd/relay-eval -suite config/eval/coding.yaml -dry-run

docker:     ## build the image
	docker build -t relay:dev .

up:         ## docker compose up --build
	docker compose up --build

down:       ## docker compose down -v
	docker compose down -v

smoke:      ## end-to-end check against a running compose stack
	./scripts/smoke.sh

console-dev: ## run the console against a local gateway
	cd web && npm run dev

tidy:
	go mod tidy
