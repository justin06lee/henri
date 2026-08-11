BINARY  := henri
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX  ?= /usr/local

.PHONY: build test race vet fmt install uninstall clean dist

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/henri

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

# Cross-compile the platforms henri is tested on.
dist:
	@mkdir -p dist/bin
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; \
		[ "$$os" = windows ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" \
			-o dist/bin/$(BINARY)-$$os-$$arch$$ext ./cmd/henri || exit 1; \
	done

clean:
	rm -rf $(BINARY) dist/bin
