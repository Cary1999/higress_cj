# Higress 插件集合

本项目是 Higress 网关的自定义 WASM 插件集合，用于扩展网关功能。

## 项目结构

```
higress_cj/
├── README.md                    # 项目说明
├── docker-compose.yml           # Docker Compose 配置
├── llm-tier-router/            # LLM 分层路由插件
│   ├── README.md               # 插件说明
│   ├── Dockerfile              # Docker 构建配置
│   ├── go.mod                  # Go 模块配置
│   ├── go.sum                  # Go 依赖校验
│   ├── main.go                 # 插件源码
│   ├── llm-tier-router.wasm    # 编译后的 WASM 文件
│   └── build-and-push.sh       # 构建并推送脚本
```

## 插件列表

| 插件名称 | 功能描述 |
|---------|---------|
| llm-tier-router | LLM 分层路由插件，根据用户累计 Token 使用量动态选择不同的 LLM 服务 |

## 快速开始（Docker Compose）

### 一键启动

```bash
# 启动所有服务（Higress + Redis + 镜像仓库）
docker-compose up -d
```

### 服务说明

| 服务 | 端口 | 说明 |
|------|------|------|
| Higress 网关 | 8002 | API 入口（宿主机端口） |
| Higress Console | 8001 | Web UI 管理界面 |
| Redis | 6379 | Token 统计缓存 |
| 镜像仓库 | 5000 | 本地镜像存储 |

### 访问地址

- **Higress Console（Web UI）**: http://localhost:8001
- **API 入口**: http://localhost:8002

### 验证服务

```bash
# 检查容器状态
docker-compose ps

# 测试 Redis 连接
docker-compose exec redis redis-cli ping

# 测试镜像仓库
curl -s http://localhost:5000/v2/_catalog

# 测试 Higress
curl -s http://localhost:8002/
```

### 构建和推送插件

```bash
cd llm-tier-router
bash build-and-push.sh
```

### Docker 版 Redis 配置注意事项

本项目使用的是 Docker 版 `higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/all-in-one`，Redis 接入方式和 K8s 部署不同，不能直接照搬 `redis.default.svc.cluster.local` 这一类配置。

在 Higress Console 中为 Redis 创建服务来源时，请使用官方 AI 场景文档中的固定地址方案：

- 类型：`固定地址`
- 名称：`redis`
- 服务端口：保持 `80`
- 服务地址：填写 Redis 容器的 `IP:6379`，例如 `172.20.0.2:6379`
- 服务协议：`HTTP`

注意：

- `DNS域名` 类型不适用于当前 Docker Compose 场景，Console 会对域名格式做额外校验，`redis`、`redis.higress-network` 这类 Docker 内部名称通常无法通过。
- 这里的 `服务端口` 是 Higress 固定地址服务来源生成内部服务时使用的端口，不是 Redis 容器真实监听端口。
- Redis 容器真实端口仍然通过“服务地址”中的 `:6379` 指定。
- 创建成功后，插件配置中应使用 Higress 生成的内部服务名，例如 `redis.static`，而不是 Docker 容器名 `redis`。

### 停止服务

```bash
docker-compose down
```

## 工作流程

```
Client → sub2api → Higress(自定义插件) → LLM Service
```

1. **请求接收**：Higress 网关接收来自 sub2api 的请求
2. **插件处理**：自定义 WASM 插件根据 Redis 中的累计 Token 选择目标 provider 和 model
3. **流量分发**：Higress 根据插件写入的路由 Header 将请求转发到对应 AI 服务提供者
4. **响应返回**：将 LLM 服务的响应返回给客户端

### AI 路由配置注意事项

客户端请求固定走 `/v1/chat/completions`。当前方案由插件在网关内补齐目标模型，并写入 provider 路由 Header：

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

注意：

- 每条 AI 路由只绑定一个 provider，避免继续发生 90/10 这类随机分流。
- provider 的地址、密钥、鉴权方式都继续在 Higress 的“AI 服务提供者”中维护，不再写入插件配置。
- 插件会自动把请求体补成目标模型，因此客户端请求无需携带 `model` 字段。

## 故障排查

### 插件无法加载

```bash
docker-compose logs higress-ai | grep wasm
```

### Redis 连接失败

```bash
docker-compose exec redis redis-cli ping
```

如果 Redis 容器正常，但插件仍返回 `{"error":"redis unavailable"}`，优先检查：

1. Higress Console 中 Redis 服务来源是否按“固定地址 + HTTP + IP:6379”的方式创建。
2. 插件配置中的 `redis_service` 是否已经改成 Higress 内部服务名（例如 `redis.static`）。
3. 插件配置中的 `redis_port` 是否与内部服务端口一致，Docker `all-in-one` 场景下应使用 `80`。

### 请求无法通过网关

```bash
docker-compose logs higress-ai
```

如果请求命中了错误的 AI 服务提供者，优先检查：

1. 是否仍然保留了“一条 AI 路由 + 多个 provider + 权重”的配置。
2. 是否已经拆成两条 AI 路由，并使用 `X-Tier-Provider` 作为 Header 匹配条件。
3. 两条 AI 路由的目标 AI 服务是否分别只保留一个 provider，且比例为 `100`。

### 镜像推送失败

```bash
curl -s http://localhost:5000/v2/_catalog
```
