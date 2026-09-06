.PHONY: build test vet lint tidy run up down logs smoke clean

GO ?= go

build:
	$(GO) build -trimpath -o bin/rates$(shell $(GO) env GOEXE) ./cmd/server

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

tidy:
	$(GO) mod tidy

run:
	$(GO) run ./cmd/server

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f app

smoke:
	pwsh -NoProfile -ExecutionPolicy Bypass -File scripts/smoke.ps1

clean:
	rm -rf bin
