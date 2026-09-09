# Copilot API Dashboard

一个面向 [Copilot API](https://github.com/caozhiyuan/copilot-api) 的多容器、多账号用量面板。

## 功能

- 汇总或按账号查看配额、Token、请求数与费用
- 展示模型分布、每日趋势和请求记录
- 自动发现 Docker 中运行的 Copilot API 容器
- 支持通过 YAML 配置额外端点
- 无数据库，数据直接从上游读取

## 快速开始

默认配置会发现同一 Docker 网络中使用 `ghcr.io/caozhiyuan/copilot-api:latest` 镜像的运行中容器。

如果 `service` 网络尚不存在，先运行 `docker network create service`，然后启动 Dashboard：

```sh
docker compose up -d --build
```

访问 <http://127.0.0.1:9000>。

Copilot API 容器需要加入 `service` 网络。若使用其他网络，请修改 [`docker-compose.yml`](docker-compose.yml) 中的 `networks.copilot.name`。

> Dashboard 默认仅监听本机且不提供身份认证。对外开放时，请使用带认证的反向代理。挂载 Docker socket 会授予容器较高权限，请仅在可信环境中部署。

## 配置端点

除 Docker 自动发现外，也可以使用 YAML 配置端点：

```sh
cp config/endpoints.example.yaml config/endpoints.yaml
```

编辑 `config/endpoints.yaml`，再启用 `docker-compose.yml` 中对应的文件挂载。凭据通过 `api_key_env` 引用环境变量，不要直接写入配置文件。

常用环境变量：

| 变量                                    | 默认值                                  |
| --------------------------------------- | --------------------------------------- |
| `COPILOT_API_DASHBOARD_LISTEN_ADDR`     | `:9000`                                 |
| `COPILOT_API_DASHBOARD_ENDPOINTS_FILE`  | `/config/endpoints.yaml`                |
| `COPILOT_API_DASHBOARD_DOCKER_IMAGE`    | `ghcr.io/caozhiyuan/copilot-api:latest` |
| `COPILOT_API_DASHBOARD_REQUEST_TIMEOUT` | `5s`                                    |
| `COPILOT_API_DASHBOARD_MAX_CONCURRENCY` | `32`                                    |
| `COPILOT_API_DASHBOARD_LOG_LEVEL`       | `info`                                  |

## 本地开发

需要 Go 1.26 或更高版本。前端资源已嵌入 Go 程序，无需额外安装前端依赖。

```sh
go test ./...
go vet ./...
go run ./cmd/dashboard
```

前端测试：

```sh
node --test web/app.test.cjs
```

## License

[MIT](LICENSE)
