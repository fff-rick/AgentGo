.PHONY: build run tui test test-milvus benchmark benchmark-e2e benchmark-sse benchmark-intent benchmark-retrieval lint clean docker-build docker-run docker-stop docker-logs fmt vet

APP_NAME := ai-agent-go
APP_IMAGE ?= $(APP_NAME):local
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS  := -ldflags "-s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)"

# 构建二进制
build:
	@echo ">>> 构建 $(APP_NAME)..."
	CGO_ENABLED=0 go build $(LDFLAGS) -o bin/$(APP_NAME) ./cmd/server

# 本地运行
run:
	@echo ">>> 启动开发服务器..."
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; go run ./cmd/server

# 启动可观察 TUI（连接已运行的 AgentGo 服务）
tui:
	@echo ">>> 启动 AgentGo 可观察 TUI..."
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; go run ./cmd/tui

# 运行测试
test:
	@echo ">>> 运行测试..."
	go test -v -race -coverprofile=coverage.out ./...

# 对 .env 指向的真实 Milvus 执行 insert/search/delete 集成测试
test-milvus:
	@echo ">>> 运行 Milvus 集成测试..."
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; \
		MILVUS_INTEGRATION=1 go test -v ./internal/vectordb -run TestMilvusIntegration

# 运行不依赖外部服务的 Go 微基准
benchmark:
	@echo ">>> 运行离线微基准..."
	go test -run '^$$' -bench . -benchmem ./...

# 对已启动的服务运行黑盒 smoke benchmark；可覆盖 BENCH_* 参数
BENCH_URL ?= http://localhost:8080
BENCH_DATASET ?= benchmarks/datasets/smoke.example.jsonl
BENCH_CONCURRENCY ?= 1
BENCH_REPEAT ?= 1
benchmark-e2e:
	@echo ">>> 运行端到端 benchmark..."
	go run ./cmd/benchmark -base-url $(BENCH_URL) -dataset $(BENCH_DATASET) \
		-concurrency $(BENCH_CONCURRENCY) -repeat $(BENCH_REPEAT)

benchmark-sse:
	go run ./cmd/benchmark-sse -base-url $(BENCH_URL)

benchmark-intent:
	go run ./cmd/benchmark-intent

benchmark-retrieval:
	go run ./cmd/benchmark-retrieval

# 代码格式化
fmt:
	@echo ">>> 格式化代码..."
	gofmt -s -w .
	goimports -w .

# 静态检查
vet:
	@echo ">>> 静态分析..."
	go vet ./...

# lint 检查
lint:
	@echo ">>> Lint 检查..."
	golangci-lint run ./...

# 清理产物
clean:
	@echo ">>> 清理构建产物..."
	rm -rf bin/ coverage.out

# 构建 Docker 镜像
docker-build:
	@echo ">>> 构建 Docker 镜像..."
	docker build -t $(APP_IMAGE) .

# 使用 Compose 启动 AgentGo、Redis 和 Milvus，并在需要时自动构建镜像
docker-run:
	@echo ">>> 启动 AgentGo、PostgreSQL、Redis 和 Milvus..."
	AGENTGO_IMAGE=$(APP_IMAGE) docker compose up -d --build

docker-stop:
	@echo ">>> 停止 AgentGo、PostgreSQL、Redis 和 Milvus..."
	docker compose down

docker-logs:
	docker compose logs -f agentgo

# 生成模拟依赖（用于测试）
mock:
	@echo ">>> 生成 Mock..."
	mockgen -source=internal/llm/client.go -destination=internal/llm/mock_client.go -package=llm
	mockgen -source=internal/tool/base.go -destination=internal/tool/mock_tool.go -package=tool
