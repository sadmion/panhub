# panhub 常用命令。
# Windows 下 make 可能没装，直接用 `go build` 也一样。

BIN := panhub
ifeq ($(OS),Windows_NT)
	BIN := panhub.exe
endif

.PHONY: all build web run dev test fmt vet clean docker

all: build

# 前端：产物直接写进 internal/webui/dist，Go 会 embed 进去
web:
	cd web && npm install --no-audit --no-fund && npm run build

# 编译（自动先构建前端）
build: web
	go build -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/panhub

# 只编译后端（前端已经构建过时用这个更快）
server:
	go build -trimpath -o $(BIN) ./cmd/panhub

run: server
	./$(BIN) -config config.yaml

# 前后端热重载：Go 在 5211，Vite 在 5173 并代理 /api 与 /dav
dev:
	@echo "先在一个终端跑: go run ./cmd/panhub -config config.yaml"
	cd web && npm run dev

test:
	pwsh -NoProfile -ExecutionPolicy Bypass -File scripts/smoke.ps1

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...

clean:
	rm -f $(BIN) panhub
	rm -rf internal/webui/dist/* web/node_modules

docker:
	docker build -t panhub:latest .
