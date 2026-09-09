# Copilot API Dashboard

一个面向 [Copilot API](https://github.com/caozhiyuan/copilot-api) 的多容器、多账号用量面板。

<p align="center">
  <a href="docs/images/dashboard-overview.png">
    <img
      src="docs/images/dashboard-overview.png"
      alt="Copilot API Dashboard 界面预览"
      height="600"
    />
  </a>
</p>

## 功能

- 汇总或按账号查看配额、Token、请求数与费用
- 展示模型分布、每日趋势和请求记录
- 自动发现 Docker 中运行的 Copilot API 容器
- 支持通过 YAML 配置额外端点
- 默认直接从上游读取，也可启用 SQLite 保存日级 Token、请求数、模型和费用聚合

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

本地运行时，如果未设置 `COPILOT_API_DASHBOARD_ENDPOINTS_FILE` 且当前目录存在 `config/endpoints.yaml`，Dashboard 会自动加载该文件。Docker Compose 部署仍需启用 `docker-compose.yml` 中对应的文件挂载。

端点 URL 可以是根地址，也可以包含反向代理子路径。端点凭据可以使用以下任一方式，两者不可同时配置：

- `api_key_env`：引用传入 Dashboard 容器的环境变量，推荐用于避免在配置文件中保存凭据。
- `api_key`：直接在 YAML 中明文保存凭据。使用此方式时，应限制配置文件的读取权限，并避免提交到版本库。

不需要认证的端点可以省略这两个字段。具体格式参见 `config/endpoints.example.yaml`。

YAML 端点与 Docker 自动发现结果的名称或 URL 相同时，以 YAML 配置为准。

常用环境变量：

| 变量                                        | 默认值                                                           |
| ------------------------------------------- | ---------------------------------------------------------------- |
| `COPILOT_API_DASHBOARD_LISTEN_ADDR`         | `:9000`                                                          |
| `COPILOT_API_DASHBOARD_BASE_PATH`           | `/`                                                              |
| `COPILOT_API_DASHBOARD_ENDPOINTS_FILE`      | `config/endpoints.yaml`（存在时），否则 `/config/endpoints.yaml` |
| `COPILOT_API_DASHBOARD_DOCKER_IMAGE`        | `ghcr.io/caozhiyuan/copilot-api:latest`                          |
| `COPILOT_API_DASHBOARD_REQUEST_TIMEOUT`     | `5s`                                                             |
| `COPILOT_API_DASHBOARD_MAX_CONCURRENCY`     | `32`                                                             |
| `COPILOT_API_DASHBOARD_PERSISTENCE_ENABLED` | `false`                                                          |
| `COPILOT_API_DASHBOARD_DATABASE_PATH`       | `/data/dashboard.sqlite`                                         |
| `COPILOT_API_DASHBOARD_SYNC_INTERVAL`       | `10m`                                                            |
| `COPILOT_API_DASHBOARD_LOG_LEVEL`           | `info`                                                           |

## 数据持久化

持久化默认关闭。启用后，Dashboard 会在后台定期从每个账号的 `/token-usage/daily?period=lifetime` 同步日级聚合，并使用 SQLite 计算所选周期的 Token、请求数、模型分布和分币种费用。页面加载、账号切换和周期切换只读取 SQLite；点击 Refresh 会先触发一次同步：

```yaml
environment:
  COPILOT_API_DASHBOARD_PERSISTENCE_ENABLED: "true"
  COPILOT_API_DASHBOARD_DATABASE_PATH: /data/dashboard.sqlite
  COPILOT_API_DASHBOARD_SYNC_INTERVAL: 10m
volumes:
  - ./data:/data
```

配额、套餐、重置日期和请求事件始终实时读取上游；Dashboard 不保存请求事件、API Key，也不会根据本地价格表重新计算历史费用。SQLite 是已采集日级历史的持久化来源：未返回的日期和已经结束的日期会继续保留；当天累计值正常增长时会更新为最新快照；当天累计值回退时会作为新的内部数据段继续累计，避免容器丢失记录后覆盖已有历史。不完整快照不会清空已经保存的模型或费用明细。

所有 Copilot API 容器应与 Dashboard 使用相同的时区，否则各上游生成的每日边界可能不同。数据库发生错误时会明确报告；如果已显式开启持久化但数据库无法初始化，Dashboard 将拒绝启动。

## 本地开发

需要 Go 1.26 或更高版本。前端资源已嵌入 Go 程序，无需额外安装前端依赖。

```sh
make check
make run
```

也可以分别执行 `make fmt`、`make fmt-check`、`make lint`、`make test`、`make vet` 和 `make build`。

前端测试：

```sh
node --test web/app.test.cjs
```

## License

[MIT](LICENSE)
