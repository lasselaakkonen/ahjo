VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG     := ./cmd/ahjo
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath -ldflags '$(LDFLAGS)'

PLATFORMS := darwin-arm64 darwin-amd64 linux-arm64 linux-amd64
DIST_BINS := $(addprefix dist/ahjo-,$(PLATFORMS))
# Source set excludes generated daemon binaries (avoid cycle: build → generate
# → write feature/ahjo-mirror.linux-* → triggers GO_SRC → re-build).
GO_SRC    := $(shell find cmd internal -name '*.go' -o -name '*.sh' -o -name '*.json' -o -name '*.toml' -o -name '*.yaml' -o -name '*.service') go.mod go.sum
MIRROR_BINS := internal/ahjoruntime/feature/ahjo-mirror.linux-arm64 internal/ahjoruntime/feature/ahjo-mirror.linux-amd64

HOST_GOOS   := $(shell go env GOOS)
HOST_GOARCH := $(shell go env GOARCH)

# On darwin, `make build` also produces dist/ahjo-linux-<host-arch> so that
# `./ahjo init` finds the in-VM binary via its sibling-`dist/` lookup rule.
HOST_BUILD_DEPS :=
ifeq ($(HOST_GOOS),darwin)
HOST_BUILD_DEPS := dist/ahjo-linux-$(HOST_GOARCH)
endif

VM_NAME ?= ahjo
VM_AHJO := /usr/local/bin/ahjo

.PHONY: build dist clean print-version install-vm generate-mirror hooks

# Build embedded ahjo-mirror daemon binaries via the same `go generate`
# directives CI gates on. Order: generate first (produces the linux daemon
# binaries embedded by the host CLI), then $(HOST_BUILD_DEPS) (which itself
# embeds those daemon binaries via FeatureFS), then the host build.
build: generate-mirror $(HOST_BUILD_DEPS)
	go build $(GOFLAGS) -o ahjo $(PKG)

generate-mirror: $(MIRROR_BINS)

$(MIRROR_BINS) &: $(shell find cmd/ahjo-mirror internal/mirror -name '*.go') go.mod go.sum
	@echo "  gen    cmd/ahjo-mirror -> $(MIRROR_BINS)"
	@go generate ./cmd/ahjo-mirror/

dist: $(DIST_BINS) dist/SHA256SUMS

# Push the freshly-built dist/ahjo-linux-<host-arch> into the Lima VM at
# /usr/local/bin/ahjo. Fast path for iterating on the in-VM binary without
# the full `ahjo update` rebuild (ahjo-runtime Feature re-application +
# ahjo-base republish). Mac-only — Linux hosts run ahjo directly with no VM.
install-vm: dist/ahjo-linux-$(HOST_GOARCH)
ifneq ($(HOST_GOOS),darwin)
	@echo "install-vm is only meaningful on macOS hosts (current: $(HOST_GOOS))" >&2
	@exit 1
endif
	@command -v limactl >/dev/null 2>&1 || { echo "limactl not on PATH; run \`ahjo init\` first" >&2; exit 1; }
	@echo "  push   dist/ahjo-linux-$(HOST_GOARCH) -> $(VM_NAME):$(VM_AHJO)  ($(VERSION))"
	@limactl shell $(VM_NAME) -- sudo install -m 0755 /dev/stdin $(VM_AHJO) < dist/ahjo-linux-$(HOST_GOARCH)
	@limactl shell $(VM_NAME) -- $(VM_AHJO) --version

dist/ahjo-darwin-arm64: $(GO_SRC) $(MIRROR_BINS) ; @$(MAKE) --no-print-directory _xbuild GOOS=darwin GOARCH=arm64 OUT=$@
dist/ahjo-darwin-amd64: $(GO_SRC) $(MIRROR_BINS) ; @$(MAKE) --no-print-directory _xbuild GOOS=darwin GOARCH=amd64 OUT=$@
dist/ahjo-linux-arm64:  $(GO_SRC) $(MIRROR_BINS) ; @$(MAKE) --no-print-directory _xbuild GOOS=linux  GOARCH=arm64 OUT=$@
dist/ahjo-linux-amd64:  $(GO_SRC) $(MIRROR_BINS) ; @$(MAKE) --no-print-directory _xbuild GOOS=linux  GOARCH=amd64 OUT=$@

.PHONY: _xbuild
_xbuild:
	@mkdir -p dist
	@echo "  build  $(OUT)  ($(GOOS)/$(GOARCH), $(VERSION))"
	@CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -o $(OUT) $(PKG)

dist/SHA256SUMS: $(DIST_BINS)
	@cd dist && shasum -a 256 $(notdir $(DIST_BINS)) > SHA256SUMS

clean:
	rm -rf dist ahjo

# Activate the repo-tracked git hooks under .githooks/. Idempotent — git
# rewrites core.hooksPath each call. Bypass any hook with SKIP_HOOKS=1 or
# --no-verify; see .githooks/pre-commit and .githooks/pre-push for what runs.
hooks:
	@git config core.hooksPath .githooks
	@echo "  hooks  .githooks/ active (pre-commit + pre-push)"

print-version:
	@echo $(VERSION)
