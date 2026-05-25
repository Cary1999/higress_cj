# LLM Tier Router 插件

## 功能概述

该插件实现了基于用户累计 Token 使用量的动态路由功能，可根据每日 Token 消耗自动选择不同的 LLM 服务，实现成本优化和服务分层。

## 目录结构

```
llm-tier-router/
├── README.md               # 本文件
├── Dockerfile              # Docker 构建配置
├── go.mod                  # Go 模块配置
├── go.sum                  # Go 依赖校验
├── main.go                 # 插件源码
└── llm-tier-router.wasm    # 编译后的 WASM 文件
```

## 构建指南

### 环境要求

- Go 1.24+
- Docker

### 编译步骤

1. 进入插件目录：

```bash
cd llm-tier-router
```

2. 下载依赖：

```bash
go mod tidy
```

3. 编译 WASM 文件：

```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o llm-tier-router.wasm main.go
```

### 构建 Docker 镜像

```bash
docker build -t llm-tier-router:v1.0.0 .
```

### 推送镜像到本地仓库

```bash
docker tag llm-tier-router:v1.0.0 localhost:5000/llm-tier-router:1.0.0
docker push localhost:5000/llm-tier-router:1.0.0
```

### 一键构建并推送

```bash
bash build-and-push.sh
```

### 镜像地址

| 环境 | 镜像地址 |
|------|---------|
| 本地开发 | `oci://localhost:5000/llm-tier-router:1.0.0` |
| 集群内部 | `oci://registry.higress-system.svc.cluster.local:5000/llm-tier-router:1.0.0` |
| Docker Compose | `oci://host.docker.internal:5000/llm-tier-router:1.0.0` |

**Higress 插件配置中的镜像地址：**
```yaml
url: oci://host.docker.internal:5000/llm-tier-router:1.0.0
```

## 部署验证

### 1. 检查插件加载状态

查看 Higress 网关日志：

```bash
docker exec higress-ai cat /var/log/higress/gateway.log | grep llm-tier-router
```

成功加载的日志示例：

```
info    wasm    fetching image llm-tier-router from registry host.docker.internal:5000 with tag 1.0.0
info    wasm    fetching image with plain text from host.docker.internal:5000/llm-tier-router:1.0.0
```

### 2. 测试请求

#### 测试1：缺少 Authorization Header（预期返回 403）

```bash
curl -v http://localhost:8081/v1/chat/completions \
  -H "X-User-API-Key: user123" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "hello"}]}'
```

#### 测试2：缺少 X-User-API-Key（预期返回 401）

```bash
curl -v http://localhost:8081/v1/chat/completions \
  -H "Authorization: Bearer your-internal-secret" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "hello"}]}'
```

#### 测试3：正常请求（预期成功转发）

```bash
curl -v http://localhost:8081/v1/chat/completions \
  -H "Authorization: Bearer your-internal-secret" \
  -H "X-User-API-Key: user123" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "hello"}]}'
```

### 3. 验证 Redis 统计

检查 Redis 中的 Token 统计：

```bash
redis-cli GET "higress:llm:token:$(date +%Y%m%d):user123"
```

## 配置说明

### 配置示例

```yaml
internal_key: "higress-2026-newapi-secret"
redis_port: 6379
redis_service: "redis"
tiers:
  - max_token: 1000
    target_model: "glm-4.5-air"
    target_host: "https://open.bigmodel.cn"
    target_key: "sk-your-api-key"
  - max_token: 5000
    target_model: "glm-4"
    target_host: "https://api.xiaomimimo.com/v1"
    target_key: "sk-your-api-key"
  - max_token: 9999999
    target_model: "mimo-v2.5-pro"
    target_host: "https://api.xiaomimimo.com/v1"
    target_key: "sk-your-api-key"
```

### 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| internal_key | string | 是 | 内部鉴权密钥 |
| redis_service | string | 是 | Redis 服务名称 |
| redis_port | int | 否 | Redis 端口，默认 6379 |
| redis_pass | string | 否 | Redis 密码 |
| tiers | array | 是 | 分层配置 |
| tiers[].max_token | int | 是 | Token 上限 |
| tiers[].target_model | string | 否 | 目标模型 |
| tiers[].target_host | string | 否 | 目标主机 |
| tiers[].target_key | string | 否 | 目标 API Key |

## 插件工作流程

```
┌─────────────────────────────────────────────────────────────────┐
│                      请求进入插件                              │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  1. 内部鉴权：验证 Authorization: Bearer <internal_key>       │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  2. 用户识别：获取 X-User-API-Key                              │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  3. 查询 Redis：获取当日累计 Token 数                          │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  4. 选择 Tier：根据累计 Token 选择对应的服务层                  │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  5. 重写请求：替换请求体中的 model 字段                         │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  6. 转发请求：添加 X-Higress-* Header 后转发                   │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  7. 响应统计：从响应提取 total_tokens 并累加到 Redis            │
└─────────────────────────────────────────────────────────────────┘
```

## 生成的 Header

插件会在请求中添加以下 Header：

| Header | 说明 |
|--------|------|
| X-Higress-Tier-Limit | 当前 Tier 的 Token 上限 |
| X-Higress-Used-Token | 用户当日已使用的 Token 数 |
| X-Higress-Target-Model | 目标模型名称 |
| X-Higress-Target-Host | 目标主机地址 |
| X-Higress-Target-Key | 目标 API Key |

## 故障排查

### 常见问题

1. **插件加载失败**

   检查日志：
   ```bash
   docker exec higress-ai cat /var/log/higress/gateway.log | grep -E "(llm-tier-router|wasm.*error)"
   ```

   可能原因：
   - 镜像地址不正确
   - 镜像仓库不可访问
   - 配置格式错误

2. **请求被拒绝**

   检查是否携带了正确的 Header：
   - `Authorization: Bearer <internal_key>`
   - `X-User-API-Key: <user_key>`

3. **Redis 连接失败**

   确保 Redis 服务正常运行且可被 Higress 访问：
   ```bash
   docker exec higress-ai ping -c 1 redis
   ```
