SHELL := /bin/bash

GO ?= go
BIN_DIR ?= bin
SERVER_BIN := $(BIN_DIR)/saturnus-server
AGENT_BIN := $(BIN_DIR)/saturnus-agent

ADDR ?= :8787
DATA ?= saturnus.db
STATIC ?= web/dist

CGO_ENABLED ?= 1
UNAME_S := $(shell uname -s)

ifeq ($(UNAME_S),Darwin)
SDKROOT ?= $(shell xcrun --show-sdk-path 2>/dev/null)
BUILD_CC ?= /usr/bin/clang
ifneq ($(SDKROOT),)
CGO_CFLAGS ?= -isysroot $(SDKROOT)
CGO_LDFLAGS ?= -isysroot $(SDKROOT)
endif
else
BUILD_CC ?= cc
endif

GO_BUILD_ENV := CGO_ENABLED=$(CGO_ENABLED) CC="$(BUILD_CC)" CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)"

.PHONY: help all build build-server build-agent run-server lark-bridge test test-agent check-cgo clean

help:
	@echo "Targets:"
	@echo "  make build        Build server and agent binaries into $(BIN_DIR)/"
	@echo "  make build-server Build $(SERVER_BIN)"
	@echo "  make build-agent  Build $(AGENT_BIN)"
	@echo "  make run-server   Build and run $(SERVER_BIN)"
	@echo "  make lark-bridge  Build and run $(AGENT_BIN) lark-bridge"
	@echo "  make test         Run go test ./..."
	@echo "  make clean        Remove built binaries"
	@echo
	@echo "Variables:"
	@echo "  ADDR=$(ADDR)"
	@echo "  DATA=$(DATA)"
	@echo "  STATIC=$(STATIC)"
	@echo "  BUILD_CC=$(BUILD_CC)"
	@echo "  SDKROOT=$(SDKROOT)"

all: build

build: build-server build-agent

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

check-cgo:
	@echo "Checking cgo compiler: $(BUILD_CC)"
	@$(BUILD_CC) $(CGO_CFLAGS) -E -x c -include stdlib.h /dev/null >/dev/null
	@$(BUILD_CC) $(CGO_CFLAGS) -E -x c -include pthread.h /dev/null >/dev/null

build-server: $(BIN_DIR) check-cgo
	$(GO_BUILD_ENV) $(GO) build -o $(SERVER_BIN) ./cmd/saturnus-server

build-agent: $(BIN_DIR)
	$(GO_BUILD_ENV) $(GO) build -o $(AGENT_BIN) ./cmd/saturnus-agent

run-server: build-server
	$(SERVER_BIN) -addr $(ADDR) -data $(DATA) -static $(STATIC)

lark-bridge: build-agent
	$(AGENT_BIN) lark-bridge

test-agent:
	$(GO_BUILD_ENV) $(GO) test ./internal/larkbridge ./cmd/saturnus-agent

test: check-cgo
	$(GO_BUILD_ENV) $(GO) test ./...

clean:
	rm -f $(SERVER_BIN) $(AGENT_BIN)
