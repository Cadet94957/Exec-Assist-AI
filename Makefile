# Everything you need day to day. Run `make` to see the list.
.DEFAULT_GOAL := help

help: ## Show these commands
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-14s %s\n", $$1, $$2}'

build: ## Compile the server
	CGO_ENABLED=1 go build -o bin/server ./cmd/server

test: ## Run every test
	CGO_ENABLED=1 go test ./...

check: ## Everything CI would run — do this before deploying
	@test -z "$$(gofmt -l .)" || { echo "needs gofmt:"; gofmt -l .; exit 1; }
	go vet ./...
	CGO_ENABLED=1 go test ./...

run: build ## Run the bare server on :8080 with insecure cookies
	INSECURE_COOKIES=1 ./bin/server serve

start: build ## Set up an account, seed a demo day, and open your browser
	./bin/server

tenant: build ## Create a tenant: make tenant ID=dave-glass NAME="Dave's Glass"
	./bin/server create-tenant -id "$(ID)" -name "$(NAME)" -tz "$(or $(TZ),UTC)"

eval: ## Measure the brief prompt against the real API (costs money)
	CGO_ENABLED=1 go test -tags eval -v -count=1 ./internal/brief/eval/

deploy: check ## Deploy to Fly (only after check passes)
	fly deploy

.PHONY: help build test check run start tenant eval deploy
