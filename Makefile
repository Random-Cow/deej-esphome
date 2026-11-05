BINARY     := deej
PKG        := ./pkg/deej/cmd
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null)
BUILD_TYPE ?= release
LDFLAGS    := -s -w -X main.versionTag=$(VERSION) -X main.gitCommit=$(COMMIT) -X main.buildType=$(BUILD_TYPE)

PREFIX           ?= $(HOME)/.local
SYSTEMD_USER_DIR ?= $(HOME)/.config/systemd/user

.PHONY: all build dev test lint fmt install uninstall clean

all: lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

# dev build: logs to stderr, no release optimisations
dev:
	go build -o $(BINARY) $(PKG)

test:
	go test -race ./...

fmt:
	gofmt -w pkg

lint:
	@test -z "$$(gofmt -l pkg)" || (echo "gofmt needed on:"; gofmt -l pkg; exit 1)
	go vet ./...
	@if command -v golangci-lint >/dev/null; then golangci-lint run; else echo "golangci-lint not installed, skipping"; fi

install: build
	install -Dm755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	install -Dm644 contrib/deej.service $(SYSTEMD_USER_DIR)/deej.service
	@test -f $(HOME)/.config/deej/config.yaml || install -Dm644 config.example.yaml $(HOME)/.config/deej/config.yaml
	@echo
	@echo "Installed. Edit ~/.config/deej/config.yaml, then run:"
	@echo "  systemctl --user daemon-reload && systemctl --user enable --now deej"

uninstall:
	-systemctl --user disable --now deej
	rm -f $(PREFIX)/bin/$(BINARY) $(SYSTEMD_USER_DIR)/deej.service

clean:
	rm -f $(BINARY)
	rm -rf dist
