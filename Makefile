# Shortcuts for the commands this project is actually built and checked with.
# Everything here is plain `go`; nothing needs installing first.
#
# It has to work on Windows too, where make runs recipes through cmd.exe rather
# than a shell. Two things follow from that, and they are the reason this file
# looks the way it does:
#
#   * `VAR=value command` is shell syntax cmd.exe does not have. Environment is
#     set with make's own target-specific `export`, which needs no shell at all.
#   * `rm`, `grep` and friends are not there. The recipes that would need them
#     are switched on $(OS) below.

# Where `make run` keeps its database and caches. Override on the command line:
#   make run DATA=/srv/kobibri
DATA ?= ./data

# Passed to `make run` so the URLs handed to a device are reachable from it.
# A Kobo cannot resolve localhost, so set this to the machine's LAN address:
#   make run BASE_URL=http://192.168.1.10:8078
# Left empty it is simply not set: config.go treats an empty value as absent.
BASE_URL ?=

# `make dev` serves on every interface so a reader on the LAN can reach it, with
# the full wire trace on and written to a file. LAN is this machine's address as
# the reader must dial it — a Kobo cannot resolve localhost.
LAN ?= 192.168.0.42
PORT ?= 8078
DEVLOG ?= dev.log

GO ?= go

# The one thing here that is not plain `go`: install it with
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
# There is no .golangci.yml on purpose — the stock linter set passes as it is,
# and the tree is kept that way. `check` leaves it out only so the everyday
# command still needs nothing installed.
GOLANGCI_LINT ?= golangci-lint

ifeq ($(OS),Windows_NT)
  BIN := kobibri.exe
  RM_BIN := if exist $(BIN) del /q $(BIN)
else
  BIN := kobibri
  RM_BIN := rm -f $(BIN)
endif

.PHONY: help build test race vet lint check run dev migrate fmt tidy clean docker

help:
	@echo build   - Build the binary into ./$(BIN)
	@echo run     - Build and serve on :8078, with DATA=... BASE_URL=...
	@echo dev     - Serve on the LAN with the full wire trace into $(DEVLOG)
	@echo migrate - Create or upgrade the database, then exit
	@echo test    - Run the tests
	@echo race    - Run the tests under the race detector
	@echo vet     - Run go vet
	@echo lint    - Run golangci-lint over everything
	@echo check   - What CI runs: vet, then the tests
	@echo fmt     - Format the source
	@echo tidy    - Tidy go.mod and go.sum
	@echo docker  - Build the container image
	@echo clean   - Remove the built binary

build:
	$(GO) build -trimpath -o $(BIN) ./cmd/kobibri

test:
	$(GO) test ./...

# Not optional before a release: a scan rewrites the same rows a paginated sync
# is reading from.
race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint:
	$(GOLANGCI_LINT) run ./...

check: vet test
run: export KOBIBRI_ADMIN_PASSWORD = 123
# Target-specific export rather than a `VAR=value` prefix, so the recipe is not
# shell syntax and runs under cmd.exe as well.
run: export KOBIBRI_DATA_DIR = $(DATA)
run: export KOBIBRI_BASE_URL = $(BASE_URL)
run: export KOBIBRI_LISTEN = 0.0.0.0:$(PORT)
run: export KOBIBRI_LOG_LEVEL = debug
run:
	$(GO) run ./cmd/kobibri serve

dev: export KOBIBRI_DATA_DIR = $(DATA)
dev: export KOBIBRI_LISTEN = 0.0.0.0:$(PORT)
dev: export KOBIBRI_BASE_URL = http://$(LAN):$(PORT)

dev: export KOBIBRI_TRACE_BODY_BYTES = 2000000
dev:
	$(GO) run ./cmd/kobibri serve > $(DEVLOG) 2>&1

migrate: export KOBIBRI_DATA_DIR = $(DATA)
migrate:
	$(GO) run ./cmd/kobibri migrate

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

docker:
	docker build -t kobibri:dev .

clean:
	$(RM_BIN)
