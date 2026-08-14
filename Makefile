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

.PHONY: build test race vet fmt install uninstall update clean dist

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

# The whole upgrade in one command: stop the background service, sweep stale
# copies out of the usual install locations, put the fresh build where
# `make install` puts it, and bring the service back -- but only if it was
# installed to begin with.
#
# Run it as yourself, never under sudo: the build must not be root's (see
# install), and a login service belongs to your own session. The one step
# that can need root -- copying into $(PREFIX)/bin -- asks for it by itself.
update: $(BINARY)
	@if [ "$$(id -u)" = 0 ]; then \
		echo "Run 'make update' as yourself, not with sudo."; \
		echo "It asks for root by itself, for the one step that needs it."; \
		exit 1; \
	fi
	@set -e; \
	target="$(DESTDIR)$(PREFIX)/bin/$(BINARY)"; \
	unit_mac="$$HOME/Library/LaunchAgents/com.justin06lee.henri.plist"; \
	unit_linux="$${XDG_CONFIG_HOME:-$$HOME/.config}/systemd/user/henri.service"; \
	had_service=no; \
	if [ -f "$$unit_mac" ] || [ -f "$$unit_linux" ]; then had_service=yes; fi; \
	if [ "$$had_service" = yes ]; then \
		echo "stopping the background service"; \
		./$(BINARY) service uninstall || true; \
	fi; \
	gobin="$$(go env GOPATH)/bin/$(BINARY)"; \
	for old in "$$HOME/.local/bin/$(BINARY)" "$$gobin"; do \
		if [ -e "$$old" ] && [ "$$old" != "$$target" ]; then \
			echo "removing old copy  $$old"; \
			rm -f "$$old"; \
		fi; \
	done; \
	SUDO=""; \
	if ! install -d "$(DESTDIR)$(PREFIX)/bin" 2>/dev/null || [ ! -w "$(DESTDIR)$(PREFIX)/bin" ]; then \
		SUDO="sudo"; \
		echo "installing to $$target (this is the step that needs root)"; \
	else \
		echo "installing to $$target"; \
	fi; \
	$$SUDO install -d "$(DESTDIR)$(PREFIX)/bin"; \
	$$SUDO install -m 0755 "$(BINARY)" "$$target"; \
	if [ "$$had_service" = yes ]; then \
		echo "reinstalling the background service"; \
		"$$target" service install; \
	else \
		echo "no background service was installed, so none was set up."; \
		echo "updated $$target -- 'henri service install' runs it at login."; \
	fi; \
	found="$$(command -v $(BINARY) 2>/dev/null || true)"; \
	if [ -n "$$found" ] && [ "$$found" != "$$target" ]; then \
		echo ""; \
		echo "note: another henri is still first in your PATH: $$found"; \
		echo "      remove it, or your shell will keep running the old one."; \
	fi

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
