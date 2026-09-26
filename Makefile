# CasaNova - Unified Build System
# By Leeway Open Software
#
# Usage:
#   make build          - Build all Go services (Linux amd64)
#   make build-all      - Build for amd64 + arm64
#   make frontend       - Build the Vue 3 frontend
#   make full           - Build everything into build/full/
#   make tidy           - Run go mod tidy on all repos
#   make vet            - Run go vet on all repos
#   make clean          - Remove build artifacts
#   make status         - Show Go version and module status for all repos

SHELL := /bin/bash
GOOS ?= linux
GOARCH ?= amd64

# ─── Version Scheme ──────────────────────────────────────────────────────────
# Base semantic version — bumped manually on major releases
# Kept in step with what production actually runs and with the manifest inside
# the staged package: both are 18.6.2. While this said 18.6.1, `make package`
# produced a tarball the updater correctly refused as a DOWNGRADE, so the build
# pipeline could not ship anything at all.
BASE_VERSION ?= 18.6.2
# Display version — user-facing label on the About page (YY.MM)
DISPLAY_VERSION ?= 26.10
# Release channel: dev (development), st (stable), beta
# This is release/26.10: the line stabilising toward CasaNova 26.10.
# BASE_VERSION deliberately stays 18.6.2 — the internal build version is a
# separate axis from the public label (see docs/development/release-strategy.md),
# and renumbering a line that is already installed on the test box buys nothing
# while risking the updater's downgrade guard.
CHANNEL ?= beta
# Build number within this channel (auto-incremented or set by CI)
BUILD_NUM ?= 1
# Compile timestamp: DDMMYYHHmm
BUILD_TS := $(shell date +%d%m%y%H%M)
# Full build version: 5.0.0.dev1.0604261430
BUILD_VERSION := $(BASE_VERSION).$(CHANNEL)$(BUILD_NUM).$(BUILD_TS)
# Legacy VERSION for backward compat
VERSION := $(BUILD_VERSION)

# All Go service repos in build order (dependencies first)
UPDATE_TOOLS := casanova-update.sh casanova-package.sh casaos-to-casanova.sh

GO_REPOS = \
	CasaNova-Common \
	CasaNova-Gateway \
	CasaNova-MessageBus \
	CasaNova-UserService \
	CasaNova-LocalStorage \
	CasaNova-JobManagement \
	CasaNova-AppManagement \
	CasaNova-Driver \
	CasaNova-Virt \
	CasaNova \
	CasaNova-CLI

BUILD_DIR = build/full/bin

# Inject version into every binary:
#   - main.Version (legacy compat for -v flag)
#   - common.BUILD_VERSION (the full timestamped version)
#   - common.CHANNEL (release channel)
COMMON_PKG = github.com/LeewayOpenSoftware/CasaNova/common
LDFLAGS = -s -w \
	-X main.Version=$(BUILD_VERSION) \
	-X $(COMMON_PKG).BUILD_VERSION=$(BUILD_VERSION) \
	-X $(COMMON_PKG).CHANNEL=$(CHANNEL)
# Each service has its own common package — inject there too
GW_LDFLAGS = -s -w -X main.Version=$(BUILD_VERSION) -X github.com/LeewayOpenSoftware/CasaNova-Gateway/common.BUILD_VERSION=$(BUILD_VERSION)
MB_LDFLAGS = -s -w -X main.Version=$(BUILD_VERSION) -X github.com/LeewayOpenSoftware/CasaNova-MessageBus/common.BUILD_VERSION=$(BUILD_VERSION)
US_LDFLAGS = -s -w -X main.Version=$(BUILD_VERSION) -X github.com/LeewayOpenSoftware/CasaNova-UserService/common.BUILD_VERSION=$(BUILD_VERSION)
LS_LDFLAGS = -s -w -X main.Version=$(BUILD_VERSION) -X github.com/LeewayOpenSoftware/CasaNova-LocalStorage/common.BUILD_VERSION=$(BUILD_VERSION)
JM_LDFLAGS = -s -w -X main.Version=$(BUILD_VERSION) -X github.com/LeewayOpenSoftware/CasaNova-JobManagement/common.BUILD_VERSION=$(BUILD_VERSION)
AM_LDFLAGS = -s -w -X main.Version=$(BUILD_VERSION) -X github.com/LeewayOpenSoftware/CasaNova-AppManagement/common.BUILD_VERSION=$(BUILD_VERSION)
DR_LDFLAGS = -s -w -X github.com/LeewayOpenSoftware/CasaNova-Driver/common.BUILD_VERSION=$(BUILD_VERSION)
VIRT_LDFLAGS = -s -w -X github.com/LeewayOpenSoftware/CasaNova-Virt/common.BUILD_VERSION=$(BUILD_VERSION)
CLI_LDFLAGS = -s -w -X main.version=$(BUILD_VERSION) -X main.commit=$(shell git -C CasaNova-CLI rev-parse --short HEAD 2>/dev/null || echo no-git) -X main.date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: all build build-all frontend full tidy vet test-package clean status help

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

all: full ## Build everything (Go + frontend) into build/full/

status: ## Show Go version and module status
	@echo "Go version: $$(go version)"
	@echo "Node version: $$(node -v 2>/dev/null || echo 'not installed')"
	@echo ""
	@for repo in $(GO_REPOS); do \
		gover=$$(grep '^go ' $$repo/go.mod | awk '{print $$2}'); \
		echo "  $$repo: go $$gover"; \
	done

tidy: ## Run go mod tidy on all repos
	@for repo in $(GO_REPOS); do \
		echo "==> go mod tidy: $$repo"; \
		(cd $$repo && go mod tidy) || exit 1; \
	done

vet: ## Run go vet on all repos
	@for repo in $(GO_REPOS); do \
		echo "==> go vet: $$repo"; \
		(cd $$repo && GOOS=$(GOOS) GOARCH=$(GOARCH) go vet ./...) 2>&1 | head -5 || true; \
	done

build: ## Build all Go services for GOOS/GOARCH
	@mkdir -p $(BUILD_DIR)
	@echo "Building CasaNova $(BUILD_VERSION) ($(CHANNEL) channel)"
	@echo ""
	@echo "==> verify CasaNova-Common (library only)"
	@(cd CasaNova-Common && GOOS=$(GOOS) GOARCH=$(GOARCH) go build ./...) || exit 1
	@echo "==> building CasaNova-Gateway → casanova-gateway"
	@(cd CasaNova-Gateway && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(GW_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-gateway .) || exit 1
	@echo "==> building CasaNova-MessageBus → casanova-message-bus"
	@(cd CasaNova-MessageBus && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(MB_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-message-bus .) || exit 1
	@echo "==> building CasaNova-UserService → casanova-user-service"
	@(cd CasaNova-UserService && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(US_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-user-service .) || exit 1
	@echo "==> building CasaNova-LocalStorage → casanova-local-storage"
	@(cd CasaNova-LocalStorage && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(LS_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-local-storage .) || exit 1
	@echo "==> building CasaNova-JobManagement → casanova-job-management"
	@(cd CasaNova-JobManagement && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(JM_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-job-management .) || exit 1
	@echo "==> building CasaNova-AppManagement → casanova-app-management"
	@(cd CasaNova-AppManagement && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(AM_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-app-management .) || exit 1
	@echo "==> building CasaNova-Driver → casanova-driver"
	@(cd CasaNova-Driver && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(DR_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-driver .) || exit 1
	@echo "==> building CasaNova-Virt → casanova-virt"
	@(cd CasaNova-Virt && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(VIRT_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-virt .) || exit 1
	@echo "==> building CasaNova → casanova"
	@(cd CasaNova && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o ../$(BUILD_DIR)/casanova .) || exit 1
	@echo "==> building CasaNova-CLI → casanova-cli"
	@(cd CasaNova-CLI && GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="$(CLI_LDFLAGS)" -o ../$(BUILD_DIR)/casanova-cli .) || exit 1
	@echo ""
	@echo "Go build complete: $(BUILD_DIR)/ — $(BUILD_VERSION)"
	@ls -lh $(BUILD_DIR)/

frontend: ## Build the Vue 3 frontend
	@echo "==> building CasaNova-UI-v2"
	@(cd CasaNova-UI-v2 && npm install --prefer-offline && VITE_STRICT_API=1 VITE_DISPLAY_VERSION=$(DISPLAY_VERSION) npm run build) || exit 1
	# vite.config.ts sets build.outDir to build/sysroot/var/lib/casanova/www
	# so the packaged release can slot straight in. Copy from that path,
	# not from ./dist (which only existed on an earlier Vite config).
	# Clear build/full/www first — Vite's content-hashed filenames change
	# every build, so a plain cp -r would ACCUMULATE stale chunks across
	# builds (1180 files instead of ~206), bloating the update package.
	@rm -rf build/full/www && mkdir -p build/full/www
	@cp -r CasaNova-UI-v2/build/sysroot/var/lib/casanova/www/* build/full/www/
	@echo "Frontend build complete: build/full/www/"

full: build frontend ## Build Go + frontend + collect systemd units + configs
	@echo "==> collecting systemd units"
	@mkdir -p build/full/systemd build/full/config build/full/tools
	@for repo in $(GO_REPOS); do \
		cp $$repo/build/sysroot/usr/lib/systemd/system/casanova*.service build/full/systemd/ 2>/dev/null || true; \
	done
	@echo "==> collecting config samples"
	@for repo in $(GO_REPOS); do \
		cp $$repo/build/sysroot/etc/casaos/*.conf.sample build/full/config/ 2>/dev/null || true; \
		cp $$repo/build/sysroot/etc/casaos/*.ini.sample build/full/config/ 2>/dev/null || true; \
		cp $$repo/build/sysroot/etc/casanova/*.conf.sample build/full/config/ 2>/dev/null || true; \
	done
	@echo "==> copying update tools"
	# NOT "|| true". These scripts ARE the update mechanism: the updater that
	# applies release N is the one installed by release N-1. A silently failed
	# copy ships the previous release's script inside a package that claims to
	# contain the fix, which is how casaos-to-casanova.sh stayed frozen on every
	# box for months. A copy that cannot happen must break the build, loudly.
	# casaos-to-casanova.sh was missing from this list while being present in
	# the verification below — the copy handled two scripts and the check
	# compared three. It only ever matched because build/full happened to hold
	# an identical copy; the first edit to the source turned it into a hard
	# build failure. The comment above names this exact script as the one that
	# stayed frozen for months, and the fix for that omitted it.
	#
	# The list is now derived, so a fourth script cannot be added to one place
	# and forgotten in the other.
	@for s in $(UPDATE_TOOLS); do cp build/tools/$$s build/full/tools/; done

	@echo "==> verifying packaged tools match source"
	# The copies above are now mandatory, so in THIS target the check is
	# belt-and-braces. It earns its place for `package`, which consumes a
	# build/full/ that an earlier run may have populated: there, a tool left
	# over from a previous version is invisible without comparing content.
	# Content, not mtime — a stale file and a partial write look identical by
	# timestamp.
	@for f in $(UPDATE_TOOLS); do \
		if [ -f build/tools/$$f ] && ! cmp -s build/tools/$$f build/full/tools/$$f; then \
			echo "FATAL: build/full/tools/$$f does not match build/tools/$$f;" \
			     "the package would ship a stale updater"; exit 1; \
		fi; \
	done
	# casaos-to-casanova.sh is what PostUpdateRollback executes on the target
	# host (/usr/share/casanova/tools/casaos-to-casanova.sh). It was never
	# copied here, so it never entered the package and the copy on every box
	# stayed frozen at whatever the original install put there — including,
	# until now, a rollback_upgrade() with no ERR trap, which could stop all
	# nine services and abort before restoring anything.
	# Same rule: this is the script PostUpdateRollback executes on the target
	# host. A silent copy failure is how it stayed frozen on every box.
	@cp build/tools/casaos-to-casanova.sh build/full/tools/
	@echo "==> generating manifest"
	@python3 -c "\
	import json, os, time; \
	m = { \
	  'version': '$(BUILD_VERSION)', \
	  'display_version': '$(DISPLAY_VERSION)', \
	  'base_version': '$(BASE_VERSION)', \
	  'channel': '$(CHANNEL)', \
	  'built_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), \
	  'arch': '$(GOARCH)', \
	}; \
	json.dump(m, open('build/manifest.json','w'), indent=2); \
	json.dump(m, open('build/full/manifest.json','w'), indent=2); \
	print('  manifest.json: ' + m['version'])" 2>/dev/null || echo "  (manifest generation skipped — python3 not available)"
	@echo ""
	@echo "=========================================="
	@echo "  CasaNova $(BUILD_VERSION) build complete"
	@echo "  Display: $(DISPLAY_VERSION)"
	@echo "  Channel: $(CHANNEL)"
	@echo "=========================================="
	@echo "  Binaries:  build/full/bin/"
	@echo "  Frontend:  build/full/www/"
	@echo "  SystemD:   build/full/systemd/"
	@echo "  Configs:   build/full/config/"
	@echo "  Tools:     build/full/tools/"
	@echo ""
	@du -sh build/full/

package: full ## Build everything then create a .tar.gz update package
	@bash build/tools/casanova-package.sh --channel=$(CHANNEL)

# Does NOT depend on full: it builds its own fixture in a temp dir, so the
# packager's preflight can be tested without a 500MB build tree. A target
# exists because a test nobody can find is as decorative as the check it
# guards -- and a decorative check is exactly what this test was written after.
test-package: ## Test the packager preflight and what the package contains
	@bash build/tools/casanova-package.test.sh

build-all: ## Build for amd64 + arm64
	@$(MAKE) build GOARCH=amd64
	@$(MAKE) build GOARCH=arm64

clean: ## Remove regenerable build artifacts (NEVER the static assets)
	# build/ also holds NON-regenerable files with no git history behind
	# them: install.sh/migrate.sh/upgrade.sh, tools/*.sh, tmpfiles.d/,
	# library/. A blanket `rm -rf build/` would destroy them irreversibly —
	# only remove what `make build`/`frontend`/`full` can recreate.
	rm -rf build/full/bin build/full/www build/manifest.json build/full/manifest.json build/full/systemd build/full/config
