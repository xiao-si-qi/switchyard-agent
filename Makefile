# Switchyard Agent 构建
#
#   make build     本机架构（开发用）
#   make dist      交叉编译全部架构，产物在 dist/
#   make install   装到 /usr/local/bin
#   make vet fmt   检查/格式化

BINARY  := switchyard-agent
PKG     := ./cmd/switchyard-agent
LDFLAGS := -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

ARCHES  := amd64 arm64 arm armv6 loong64 riscv64
DIST    := dist
PREFIX  ?= /usr/local

.PHONY: all build dist install uninstall vet fmt clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

# 交叉编译：GOOS 固定 linux，GOARM 只对 arm 变体有效
dist: clean-dist
	@mkdir -p $(DIST)
	@for arch in $(ARCHES); do \
		case $$arch in \
			arm)    goarm=7 ;; \
			armv6)  goarm=6 ;; \
			*)      goarm= ;; \
		esac; \
		goarch=$$arch; [ "$$arch" = "armv6" ] && goarch=arm; \
		out=$(DIST)/$(BINARY)-linux-$$arch; \
		printf '  %-28s' "$$out"; \
		if GOOS=linux GOARCH=$$goarch GOARM=$$goarm CGO_ENABLED=0 \
			go build -trimpath -ldflags "$(LDFLAGS)" -o $$out $(PKG); then \
			printf 'ok  %s\n' "$$(du -h $$out | cut -f1)"; \
		else \
			printf 'FAILED\n'; exit 1; \
		fi; \
	done

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)

vet:
	go vet ./...

fmt:
	gofmt -s -w .

clean: clean-dist
	rm -f $(BINARY)

clean-dist:
	rm -rf $(DIST)
