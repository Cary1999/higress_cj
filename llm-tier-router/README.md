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
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o llm-tier-router.wasm main.go
docker build -t llm-tier-router:v1.0.0 .
docker tag llm-tier-router:v1.0.0 localhost:5000/llm-tier-router:1.0.0
docker push localhost:5000/llm-tier-router:1.0.0
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

## 部署&验证

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

### 2. 配置说明

#### 配置示例与流程

配置流程:
![alt text](image/image-5.png)

![alt text](image/image-6.png)

![alt text](image/image-7.png)

配置示例:
```yaml
internal_key: "higress-2026-newapi-secret"
redis_port: 80
redis_service: "redis.static"
tiers:
  - max_token: 1000
    target_provider: "zhipu"
    target_model: "glm-4.5-air"
  - max_token: 9999999
    target_provider: "xiaomi"
    target_model: "mimo-v2.5-pro"
```

#### 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| internal_key | string | 是 | 内部鉴权密钥 |
| redis_service | string | 是 | Redis 服务名称。Docker `all-in-one` 场景下应填写 Higress 服务来源生成的内部服务名，例如 `redis.static` |
| redis_port | int | 否 | Redis 服务端口。Docker `all-in-one` 场景下应填写 `80`，而不是 Redis 容器实际监听的 `6379` |
| redis_pass | string | 否 | Redis 密码 |
| tiers | array | 是 | 分层配置 |
| tiers[].max_token | int | 是 | Token 上限 |
| tiers[].target_provider | string | 是 | 目标 provider 标识。建议使用稳定值，如 `zhipu`、`xiaomi` |
| tiers[].target_model | string | 是 | 目标模型 |

#### Docker `all-in-one` 部署的 Redis 注意事项

本插件在 Docker 版 `higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/all-in-one` 中使用 Redis 时，需要先在 Higress Console 中创建 Redis 服务来源，然后把插件配置指向 Higress 生成的内部服务名。

![alt text](image/image-3.png)

推荐按官方 AI Token 管理文档中的方式配置 Redis 服务来源：

- 类型：`固定地址`
- 名称：`redis`
- 服务端口：`80`
- 服务地址：`<redis-ip>:6379`
- 服务协议：`HTTP`

例如 Redis 容器地址为 `172.20.0.2` 时，服务来源应填写为：

```text
类型: 固定地址
名称: redis
服务端口: 80
服务地址: 172.20.0.2:6379
服务协议: HTTP
查询redis ip指令（仅供参考）: docker inspect redis --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'
```
![alt text](image/image-4.png)

创建成功后，插件中应配置：

```yaml
redis_service: "redis.static"
redis_port: 80
```

不要直接写成下面这种 K8s/容器名思路的配置：

```yaml
redis_service: "redis"
redis_port: 6379
```

因为在 Docker `all-in-one` 场景下，Wasm 插件访问的是 Higress 服务来源生成的内部服务，而不是直接访问 Docker Compose 容器名。

#### 添加ai提供者

![alt text](image/image-8.png)

以自定义 OpenAI 服务为例:
![alt text](image/image-9.png)

#### AI 路由配置注意事项

客户端请求固定走 `/v1/chat/completions`。当前方案由插件在网关内补齐目标模型，并写入 provider 路由 Header：

![alt text](image/image-1.png)

以下拿小米与智普举例：
- `X-Tier-Provider: zhipu`
- `X-Tier-Provider: xiaomi`

因此，Higress Console 中不要再使用“一条 AI 路由下挂多个 provider 按权重分流”的配置方式，而应拆成两条 AI 路由：

1. 路由 A
   - Path：`/v1`
   - Header 匹配：`X-Tier-Provider = zhipu`
   - 目标 AI 服务：仅 `智普`
   - 请求比例：`100`
2. 路由 B
   - Path：`/v1`
   - Header 匹配：`X-Tier-Provider = xiaomi`
   - 目标 AI 服务：仅 `小米`
   - 请求比例：`100`

![alt text](image/image-2.png)

注意：

- 每条 AI 路由只绑定一个 provider，避免继续发生 90/10 这类随机分流。
- provider 的地址、密钥、鉴权方式都继续在 Higress 的“AI 服务提供者”中维护，不再写入插件配置。
- 插件会自动把请求体补成目标模型，因此客户端请求无需携带 `model` 字段。


### 3. 测试请求

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

### 4. 验证 Redis 统计

检查 Redis 中的 Token 统计：

```bash
redis-cli GET "higress:llm:token:$(date +%Y%m%d):user123"
```

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
│  5. 重写请求：补齐/覆盖请求体中的 model 字段                    │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  6. 转发请求：添加 X-Tier-Provider 等 Header 后转发            │
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
| X-Tier-Provider | 目标 provider 标识，用于让 Higress 命中对应 AI 路由 |
| X-Higress-Tier-Limit | 当前 Tier 的 Token 上限 |
| X-Higress-Used-Token | 用户当日已使用的 Token 数 |
| X-Higress-Target-Model | 目标模型名称 |

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

3. **请求命中了错误的 AI 服务提供者**

   重点检查：

   - 插件配置中的 `tiers[].target_provider` 是否与 Higress AI 路由中的 Header 匹配值一致
   - Higress 是否拆成了两条 AI 路由，并使用 `X-Tier-Provider` 做 Header 匹配
   - 每条 AI 路由下是否只保留一个 provider，且比例为 `100`

4. **Redis 连接失败**

   确保 Redis 服务正常运行且可被 Higress 访问：
   ```bash
   docker exec higress-ai ping -c 1 redis
   ```

   如果 Redis 容器正常，但请求仍返回 `{"error":"redis unavailable"}`，重点检查：

   - Redis 服务来源是否按“固定地址 + HTTP + `<redis-ip>:6379`”创建
   - 插件中的 `redis_service` 是否为 Higress 内部服务名，例如 `redis.static`
   - 插件中的 `redis_port` 是否为 `80`
