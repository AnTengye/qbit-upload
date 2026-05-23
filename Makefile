APP ?= qbit-upload
DIST_DIR ?= dist
GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w

ifeq ($(OS),Windows_NT)
MKDIR_DIST = if not exist "$(DIST_DIR)" mkdir "$(DIST_DIR)"
RM_DIST = if exist "$(DIST_DIR)" rmdir /s /q "$(DIST_DIR)"
RM_LOCAL = if exist "$(APP)" del /q "$(APP)" & if exist "$(APP).exe" del /q "$(APP).exe"
BUILD_ENV = set GOOS=$(1)&& set GOARCH=$(2)&& set CGO_ENABLED=0&&
else
MKDIR_DIST = mkdir -p $(DIST_DIR)
RM_DIST = rm -rf $(DIST_DIR)
RM_LOCAL = rm -f $(APP) $(APP).exe
BUILD_ENV = GOOS=$(1) GOARCH=$(2) CGO_ENABLED=0
endif

PLATFORMS := \
	windows/amd64 \
	windows/arm64 \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64

.PHONY: all test build dist clean checksums $(PLATFORMS)

all: test dist

test:
	$(GO) test ./...

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(APP) .

dist: $(PLATFORMS)

windows/amd64:
	@$(MKDIR_DIST)
	$(call BUILD_ENV,windows,amd64) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(APP)-windows-amd64.exe .

windows/arm64:
	@$(MKDIR_DIST)
	$(call BUILD_ENV,windows,arm64) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(APP)-windows-arm64.exe .

linux/amd64:
	@$(MKDIR_DIST)
	$(call BUILD_ENV,linux,amd64) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(APP)-linux-amd64 .

linux/arm64:
	@$(MKDIR_DIST)
	$(call BUILD_ENV,linux,arm64) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(APP)-linux-arm64 .

darwin/amd64:
	@$(MKDIR_DIST)
	$(call BUILD_ENV,darwin,amd64) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(APP)-darwin-amd64 .

darwin/arm64:
	@$(MKDIR_DIST)
	$(call BUILD_ENV,darwin,arm64) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(APP)-darwin-arm64 .

checksums: dist
	cd $(DIST_DIR) && sha256sum $(APP)-* > SHA256SUMS

clean:
	$(RM_DIST)
	$(RM_LOCAL)
