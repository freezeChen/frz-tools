GO ?= go
BIN_DIR ?= bin
PKGS := ./...

.PHONY: all fmt vet test test-race build cross verify-linux ci clean

all: fmt vet test

## fmt 在存在未格式化文件时失败。
fmt:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then \
		echo "gofmt required for:"; echo "$$files"; exit 1; \
	fi

vet:
	$(GO) vet $(PKGS)

test:
	$(GO) test $(PKGS)

test-race:
	$(GO) test -race $(PKGS)

build:
	$(GO) build -o $(BIN_DIR)/opsctl ./cmd/opsctl
	$(GO) build -o $(BIN_DIR)/opsd ./cmd/opsd

## cross 为受支持的 Linux 目标交叉编译两个二进制。
cross:
	@mkdir -p $(BIN_DIR)/linux-amd64 $(BIN_DIR)/linux-arm64
	GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux-amd64/opsd ./cmd/opsd
	GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN_DIR)/linux-amd64/opsctl ./cmd/opsctl
	GOOS=linux GOARCH=arm64 $(GO) build -o $(BIN_DIR)/linux-arm64/opsd ./cmd/opsd
	GOOS=linux GOARCH=arm64 $(GO) build -o $(BIN_DIR)/linux-arm64/opsctl ./cmd/opsctl

## verify-linux 在 Linux 容器内验证文件模式、属组、Unix Socket ACL 与 systemd。
## 需要 docker，因此不纳入 ci；CI 中作为独立 job 运行。
verify-linux:
	bash test/linux/verify.sh

ci: fmt vet test test-race cross

clean:
	rm -rf $(BIN_DIR)
