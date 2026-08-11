BINARY  := henri
PREFIX  ?= /usr/local
DESTDIR ?=
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# -buildvcs=false: the version is stamped above from git describe, so the extra
# VCS metadata buys nothing, and asking the toolchain for it makes the build
# fail outright wherever git declines to talk -- a repo on an external drive
# mounted `noowners`, a bind mount in Docker owned by another uid, a source
# tarball with no .git at all.
GOFLAGS_BUILD := -buildvcs=false

SOURCES := $(shell find . -type f -name '*.go') go.mod

.PHONY: build test race vet fmt install uninstall clean dist

# A real file target, not a phony one, so `sudo make install` after `make build`
# has nothing left to do and never invokes the compiler as root.
$(BINARY): $(SOURCES)
	go build $(GOFLAGS_BUILD) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/henri

build: $(BINARY)

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Deliberately does not depend on the build. Compiling as root would drop
# root-owned files into the working tree and break the next ordinary `make`.
install:
	@test -x "$(BINARY)" || { \
		echo "henri is not built yet."; \
		echo ""; \
		echo "Build it as yourself, then install it as root:"; \
		echo ""; \
		echo "    make build"; \
		echo "    sudo make install"; \
		echo ""; \
		echo "Or install somewhere that needs no root at all:"; \
		echo ""; \
		echo "    make install PREFIX=\$$HOME/.local"; \
		echo ""; \
		exit 1; \
	}
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	@echo "installed $(DESTDIR)$(PREFIX)/bin/$(BINARY)"

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)

# Cross-compile the platforms henri is tested on.
dist:
	@mkdir -p dist/bin
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; \
		[ "$$os" = windows ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -ldflags "$(LDFLAGS)" \
			-o dist/bin/$(BINARY)-$$os-$$arch$$ext ./cmd/henri || exit 1; \
	done

clean:
	rm -rf $(BINARY) dist/bin
