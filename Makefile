.PHONY: help setup build build-pi build-pi32 dist image test vet fmt check install install-completions clean release next-version

SHELL := /bin/bash

APP_NAME ?= ms365backupd
APP_VSN  ?= $(patsubst v%,%,$(or $(shell git describe --tags --always --dirty 2>/dev/null),dev))
LDFLAGS  := -s -w -X main.version=$(APP_VSN)
GOBUILD  := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"
PREFIX   ?= /usr/local

# Release binaries: GOOS/GOARCH[/GOARM] -> bin/$(APP_NAME)-<suffix>
DIST := linux/arm64:linux-arm64 linux/arm/7:linux-armv7 linux/amd64:linux-amd64 \
        darwin/arm64:darwin-arm64 darwin/amd64:darwin-amd64

default: help

help:
	@echo "$(APP_NAME) $(APP_VSN)"
	@perl -nle'print $& if m{^[a-zA-Z0-9_-]+:.*?## .*$$}' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

setup: ## Install the build and Pi image tools: Go, xz, Docker, Raspberry Pi Imager (Homebrew or apt)
	script/setup.sh

build: ## Build bin/ms365backupd for this machine
	$(GOBUILD) -o bin/$(APP_NAME) ./cmd/ms365backupd

build-pi: ## Build for Raspberry Pi 3/4/5 with 64-bit Raspberry Pi OS (bin/*-linux-arm64)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME)-linux-arm64 ./cmd/ms365backupd

build-pi32: ## Build for 32-bit Raspberry Pi OS, Pi 2/3/4/Zero 2 (bin/*-linux-armv7)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME)-linux-armv7 ./cmd/ms365backupd

image: build-pi ## Build a headless Raspberry Pi OS Lite image with ms365backupd and tools (bin/*.img.xz; needs Docker, see README)
	APP_VSN=$(APP_VSN) script/pi-image.sh

dist: ## Build all release binaries plus bin/SHA256SUMS
	@set -e; for t in $(DIST); do \
		target=$${t%%:*}; suffix=$${t#*:}; IFS=/ read -r os arch arm <<< "$$target"; \
		echo "build $$suffix"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$$arm go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME)-$$suffix ./cmd/ms365backupd; \
	done
	cd bin && shasum -a 256 $(APP_NAME)-* > SHA256SUMS

test: ## Run the tests (or with e.g. PKG=./internal/tui make test)
	go test $(or $(PKG),./...)

vet: ## Run go vet
	go vet ./...

fmt: ## Format the code with gofmt
	gofmt -w .

check: ## gofmt check, vet and tests, as CI runs them
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }
	go vet ./...
	go test ./...

install: build ## Install bin/ms365backupd to /usr/local/bin (or PREFIX=...)
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m755 bin/$(APP_NAME) $(DESTDIR)$(PREFIX)/bin/$(APP_NAME)

install-completions: build ## Install bash, zsh and fish completions for the current user
	@mkdir -p $(HOME)/.local/share/bash-completion/completions $(HOME)/.zfunc $(HOME)/.config/fish/completions
	bin/$(APP_NAME) completion bash > $(HOME)/.local/share/bash-completion/completions/$(APP_NAME)
	bin/$(APP_NAME) completion zsh  > $(HOME)/.zfunc/_$(APP_NAME)
	bin/$(APP_NAME) completion fish > $(HOME)/.config/fish/completions/$(APP_NAME).fish
	@echo "zsh: make sure ~/.zshrc contains: fpath=(~/.zfunc \$$fpath); autoload -U compinit; compinit"

clean: ## Remove build output
	rm -rf bin

release: ## Tag the next vx.y.z on main and wait for the GitHub release with binaries (main only, all committed; VERSION=1.2.3 overrides)
	VERSION=$(VERSION) script/release.sh

next-version: ## Show the next release tag, the last one, and the latest GitHub release
	@set -- `script/release.sh --next`; \
	printf "%-12s %-16s (last %s, published %s)\n" "$$1" "make release" "$$2" "$$3"
	@echo -e "\nOverride the version with e.g. \"VERSION=1.2.3 make release\""
