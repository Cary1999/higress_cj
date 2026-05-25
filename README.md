# Higress 插件集合

本项目是 Higress 网关的自定义 WASM 插件集合，用于扩展网关功能。

## 项目结构

```
higress_cj/
├── README.md                    # 项目说明
├── llm-tier-router/            # LLM 分层路由插件
│   ├── README.md               # 插件说明
│   ├── Dockerfile              # Docker 构建配置
│   ├── go.mod                  # Go 模块配置
│   ├── go.sum                  # Go 依赖校验
│   ├── main.go                 # 插件源码
│   └── llm-tier-router.wasm    # 编译后的 WASM 文件
└── [其他插件文件夹...]         # 其他插件
```

## 插件列表

| 插件名称 | 功能描述 |
|---------|---------|
| llm-tier-router | LLM 分层路由插件，根据用户累计 Token 使用量动态选择不同的 LLM 服务 |

## 环境部署

### 1. 部署本地 Docker 镜像仓库

为了方便开发和测试，需要部署本地 Docker 镜像仓库来存储自定义插件镜像。

#### 启动本地镜像仓库

```bash
docker run -d \
  -p 5000:5000 \
  --restart=always \
  --name registry \
  registry:2
```

#### 验证镜像仓库

```bash
curl http://localhost:5000/v2/_catalog
```

返回示例：
```json
{"repositories":[]}
```

#### 配置 Docker 允许 HTTP 访问

编辑 `~/.docker/daemon.json`（如果文件不存在则创建）：

```json
{
  "insecure-registries": ["host.docker.internal:5000"]
}
```

重启 Docker 服务：
```bash
# macOS
sudo killall Docker && open /Applications/Docker.app

# Linux
sudo systemctl restart docker
```

#### 推送镜像到本地仓库

详见插件内部的 README.md 文件。

### 2. 部署 Higress

#### 使用 Docker Compose 部署

创建 `docker-compose.yml` 文件：

```yaml
version: '3.8'

services:
  higress:
    image: higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/higress:latest
    container_name: higress-ai
    ports:
      - "8080:8080"
      - "8001:8001"
    volumes:
      - ./higress-config:/etc/higress
    environment:
      - HIGRESS_CONSOLE_DOMAIN=localhost:8001
      - HIGRESS_GATEWAY_DOMAIN=localhost:8081
    restart: always
```

启动 Higress：

```bash
docker-compose up -d
```

#### 使用 Kubernetes 部署

1. 添加 Higress Helm 仓库：

```bash
helm repo add higress https://higress.io/helm-charts
helm repo update
```

2. 安装 Higress：

```bash
helm install higress higress/higress \
  --namespace higress-system \
  --create-namespace \
  --set gateway.replicas=1 \
  --set console.replicas=1
```

3. 端口转发（本地访问）：

```bash
kubectl port-forward -n higress-system svc/higress-gateway 8081:80 &
kubectl port-forward -n higress-system svc/higress-console 8001:80 &
```

#### 验证 Higress 部署

访问 Higress 控制台：
- 本地 Docker 部署：http://localhost:8001
- Kubernetes 部署：http://localhost:8001

默认登录凭证（首次部署需要设置）。

### 3. 部署 Redis 服务

#### 使用 Docker 部署 Redis

```bash
docker run -d \
  -p 6379:6379 \
  --name redis \
  redis:7-alpine
```

#### 使用 Kubernetes 部署 Redis

```bash
kubectl create deployment redis --image=redis:7-alpine -n higress-system
kubectl expose deployment redis --port=6379 -n higress-system
```

## 插件使用

每个插件都有独立的 README.md 文件，包含详细的构建、部署和配置说明。

### 构建插件

详见插件内部的 README.md 文件。

### 在 Higress 中配置插件

1. 进入 Higress 控制台
2. 导航到 **插件市场**
3. 点击 **创建** 按钮
4. 填写插件信息：
   - 插件名称：详见插件内部的 README.md 文件。
   - 镜像地址：详见插件内部的 README.md 文件。
5. 根据插件 README.md 中的说明进行配置

## 工作流程

```
Client → sub2api → Higress(自定义插件) → LLM Service
```

1. **请求接收**：Higress 网关接收来自 sub2api 的请求
2. **插件处理**：自定义 WASM 插件处理请求逻辑
3. **流量分发**：根据业务规则将请求分发到不同的 LLM 服务
4. **响应返回**：将 LLM 服务的响应返回给客户端

## 故障排查

### 插件无法加载

1. 检查镜像仓库是否可访问：
   ```bash
   docker exec higress-ai curl http://host.docker.internal:5000/v2/_catalog
   ```

2. 查看 Higress 日志：
   ```bash
   docker exec higress-ai cat /var/log/higress/gateway.log | grep wasm
   ```

3. 检查插件配置是否正确

### Redis 连接失败

1. 验证 Redis 服务是否运行：
   ```bash
   docker ps | grep redis
   ```

2. 测试 Redis 连接：
   ```bash
   docker exec higress-ai redis-cli -h redis ping
   ```

### 请求无法通过网关

1. 检查网关服务状态：
   ```bash
   docker exec higress-ai curl http://localhost:8081/health
   ```

2. 查看网关日志：
   ```bash
   docker logs higress-ai
   ```