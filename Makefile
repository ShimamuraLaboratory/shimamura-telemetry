BINARY  := shimamura-telemetry
PKG     := github.com/ShimamuraLaboratory/shimamura-telemetry
CMD     := ./cmd/$(BINARY)
BIN_DIR := bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)"

# ビルド対象のプラットフォーム（build-linux で使う）
PLATFORMS := linux/amd64 linux/arm64

.PHONY: all build build-linux test lint fmt clean

all: lint test build

## build: 手元の OS/アーキテクチャ向けにビルドする
build:
	CGO_ENABLED=0 go build $(GOFLAGS_BUILD) -o $(BIN_DIR)/$(BINARY) $(CMD)

## build-linux: linux/amd64 と linux/arm64 向けにビルドする
build-linux:
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) \
			-o $(BIN_DIR)/$(BINARY)-$$os-$$arch $(CMD) || exit 1; \
	done

## test: テストを実行する
test:
	go test -race ./...

## lint: golangci-lint を実行する
lint:
	golangci-lint run ./...

## fmt: コードを整形する
fmt:
	golangci-lint fmt ./...

## clean: ビルド成果物を削除する
clean:
	rm -rf $(BIN_DIR)
