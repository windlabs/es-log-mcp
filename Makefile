BIN     := bin/es-log-mcp
GO      ?= go
INSTALL := $(HOME)/.local/bin
VERSION := 0.1.0

.PHONY: all build install test vet fmt clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BIN) .

install: build
	install -d $(INSTALL)
	install -m 0755 $(BIN) $(INSTALL)/es-log-mcp

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin
