# Burrow build targets.
#
# Common:
#   make cli                     build the burrow CLI into build/burrow
#   make signal                  build burrow-signal for GOOS/GOARCH (default: linux/amd64)
#   make test                    run all tests
#   make release TAG=v0.1.0      cross-compile all release assets and publish a GitHub release

BUILD_DIR := build

# Version stamped into binaries via -X main.version.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Target platform for `make signal`.
GOOS   ?= linux
GOARCH ?= amd64

.DEFAULT_GOAL := help

# ---------------------------------------------------------------- build

.PHONY: build
build: ## Build all packages
	go build ./...

.PHONY: cli
cli: ## Build the burrow CLI into build/burrow
	@mkdir -p $(BUILD_DIR)
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BUILD_DIR)/burrow ./cmd/burrow
	@ls -lh $(BUILD_DIR)/burrow

.PHONY: signal
signal: ## Build burrow-signal into build/burrow-signal-$(GOOS)-$(GOARCH)
	@mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
		-o $(BUILD_DIR)/burrow-signal-$(GOOS)-$(GOARCH) ./cmd/burrow-signal
	@ls -lh $(BUILD_DIR)/burrow-signal-$(GOOS)-$(GOARCH)

# -checklinkname=0: github.com/wlynxg/anet reaches net.zoneCache through
# go:linkname, which the Go 1.23+ linker rejects by default.
.PHONY: cliandroid
cliandroid: ## Build the burrow CLI for Android arm64 into build/burrow-android-arm64
	@mkdir -p $(BUILD_DIR)
	GOOS=android GOARCH=arm64 go build -trimpath \
		-ldflags "-checklinkname=0 -X main.version=$(VERSION)" \
		-o $(BUILD_DIR)/burrow-android-arm64 ./cmd/burrow
	@ls -lh $(BUILD_DIR)/burrow-android-arm64

# ---------------------------------------------------------------- Android (gomobile)
#
# The bindings use the github.com/sagernet/gomobile fork, which must be listed
# in go.mod or gomobile's self-check fails.
#
# -androidapi must be explicit: gomobile defaults to 16, which recent NDKs no
# longer support.
#
# -checklinkname=0: see cliandroid. Without it, linking the 386 libgojni.so
# fails with "invalid reference to net.zoneCache".
#
# -X .../mobile.version stamps the build version (default dev).
ANDROID_API    ?= 23
JAVA_PKG       ?= com.getkiko.burrow
MOBILE_VERSION ?= dev

.PHONY: aar-install
aar-install: ## Install the gomobile toolchain (one-time)
	go install -v github.com/sagernet/gomobile/cmd/gomobile@v0.1.12
	go install -v github.com/sagernet/gomobile/cmd/gobind@v0.1.12

.PHONY: aar
aar: ## Build the Android .aar into build/tunnel.aar
	@mkdir -p $(BUILD_DIR)
	$(shell go env GOPATH)/bin/gomobile bind \
		-target android \
		-androidapi $(ANDROID_API) \
		-javapkg $(JAVA_PKG) \
		-ldflags "-checklinkname=0 -X github.com/getkiko/burrow/mobile.version=$(MOBILE_VERSION)" \
		-o $(BUILD_DIR)/tunnel.aar \
		./mobile
	@ls -lh $(BUILD_DIR)/tunnel.aar

.PHONY: aar-copy
aar-copy: aar ## Build the .aar and copy it into the Android project
	@mkdir -p android/app/libs
	cp $(BUILD_DIR)/tunnel.aar android/app/libs/tunnel.aar
	@ls -lh android/app/libs/tunnel.aar

.PHONY: apk
apk: aar-copy ## Build the Android debug APK
	cd android && ./gradlew assembleDebug --no-daemon
	@ls -lh android/app/build/outputs/apk/debug/app-debug.apk

# ---------------------------------------------------------------- development

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: fmt
fmt: ## Format code and organize imports
	@command -v goimports >/dev/null || { echo "goimports is required: go install golang.org/x/tools/cmd/goimports@latest"; exit 1; }
	goimports -w .

.PHONY: run-signal
run-signal: ## Run burrow-signal locally (ports 9960, 9962, 9963; metrics on 127.0.0.1:9961)
	go run ./cmd/burrow-signal -listen :9960,:9962,:9963 -stat 127.0.0.1:9961

# ---------------------------------------------------------------- release

.PHONY: release
release: ## Build release assets into dist/ and publish them (TAG=v1.2.3, DRY_RUN=1 to only build)
	@test -n "$(TAG)" || { echo "TAG is required, e.g. make release TAG=v0.1.0"; exit 1; }
	TAG='$(TAG)' ./scripts/release.sh

# ---------------------------------------------------------------- misc

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BUILD_DIR) dist

.PHONY: help
help: ## Show this help
	@echo "Burrow build targets"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
