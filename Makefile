VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG     := ./cmd/ahjo
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath -ldflags '$(LDFLAGS)'

PLATFORMS := darwin-arm64 darwin-amd64 linux-arm64 linux-amd64
DIST_BINS := $(addprefix dist/ahjo-,$(PLATFORMS))
GO_SRC    := $(shell find cmd internal -name '*.go') go.mod go.sum

HOST_GOOS   := $(shell go env GOOS)
HOST_GOARCH := $(shell go env GOARCH)

# On darwin, `make build` also produces dist/ahjo-linux-<host-arch> so that
# `./ahjo init` finds the in-VM binary via its sibling-`dist/` lookup rule.
HOST_BUILD_DEPS :=
ifeq ($(HOST_GOOS),darwin)
HOST_BUILD_DEPS := dist/ahjo-linux-$(HOST_GOARCH)
endif

.PHONY: build dist clean print-version

build: $(HOST_BUILD_DEPS)
	go build $(GOFLAGS) -o ahjo $(PKG)

dist: $(DIST_BINS) dist/SHA256SUMS

dist/ahjo-darwin-arm64: $(GO_SRC) ; @$(MAKE) --no-print-directory _xbuild GOOS=darwin GOARCH=arm64 OUT=$@
dist/ahjo-darwin-amd64: $(GO_SRC) ; @$(MAKE) --no-print-directory _xbuild GOOS=darwin GOARCH=amd64 OUT=$@
dist/ahjo-linux-arm64:  $(GO_SRC) ; @$(MAKE) --no-print-directory _xbuild GOOS=linux  GOARCH=arm64 OUT=$@
dist/ahjo-linux-amd64:  $(GO_SRC) ; @$(MAKE) --no-print-directory _xbuild GOOS=linux  GOARCH=amd64 OUT=$@

.PHONY: _xbuild
_xbuild:
	@mkdir -p dist
	@echo "  build  $(OUT)  ($(GOOS)/$(GOARCH), $(VERSION))"
	@CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -o $(OUT) $(PKG)

dist/SHA256SUMS: $(DIST_BINS)
	@cd dist && shasum -a 256 $(notdir $(DIST_BINS)) > SHA256SUMS

clean:
	rm -rf dist ahjo

print-version:
	@echo $(VERSION)
