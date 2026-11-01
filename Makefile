.PHONY: build build-mac run dev test vet fmt sync docker

build:
	go build -o gamesdb ./cmd/gamesdb

# Static Apple Silicon binary for the bare-metal deployment (no CGO needed).
# A real file target: nothing happens when no Go source, module file,
# template or asset changed since the last build.
GO_SOURCES := $(shell find cmd internal -type f \( -name '*.go' -o -name '*.sql' -o -name '*.html' -o -name '*.css' -o -name '*.js' \)) go.mod go.sum
build-mac: gamesdb-darwin-arm64
gamesdb-darwin-arm64: $(GO_SOURCES)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o $@ ./cmd/gamesdb

run:
	MAIL_MODE=log DATA_DIR=./data go run ./cmd/gamesdb serve

# Like run, but templates/CSS/JS edits show up on the next browser reload.
dev:
	DEV_RELOAD=1 MAIL_MODE=log DATA_DIR=./data go run ./cmd/gamesdb serve

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

# One-off catalog import / incremental sync (uses .env)
sync:
	go run ./cmd/gamesdb sync $(ARGS)

docker:
	docker build -t utm-games-db .
