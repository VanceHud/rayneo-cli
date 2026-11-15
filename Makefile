BINARY  := rayneo
PKG     := ./cmd/rayneo
BINDIR  := ./bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build install test vet clean

all: build

build:
	@mkdir -p $(BINDIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(BINARY) $(PKG)

install:
	go install $(PKG)

test:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -rf $(BINDIR) ./dist $(BINARY) coverage.*
