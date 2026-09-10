# Copilot API Dashboard

A read-only usage dashboard for multiple [Copilot API](https://github.com/caozhiyuan/copilot-api) instances and accounts.

<p align="center">
  <a href="docs/images/dashboard-overview.png">
    <img
      src="docs/images/dashboard-overview.png"
      alt="Copilot API Dashboard overview"
      height="600"
    />
  </a>
</p>

## Features

- Multi-account quotas, usage, costs, models, and request events
- Docker discovery and YAML endpoints
- Optional SQLite daily usage history

## Quick start

```sh
docker network create service
docker compose up -d --build
```

Open <http://127.0.0.1:9000>.

By default, the dashboard discovers `ghcr.io/caozhiyuan/copilot-api:latest` containers on the `service` network. Change `networks.copilot.name` in [`docker-compose.yml`](docker-compose.yml) to use another network.

> The dashboard has no authentication and mounts the Docker socket. Keep it on a trusted network or behind an authenticated proxy.

## Configuration

| Environment variable                        | Default                                                                |
| ------------------------------------------- | ---------------------------------------------------------------------- |
| `TZ`                                        | `UTC`                                                                  |
| `COPILOT_API_DASHBOARD_LISTEN_ADDR`         | `:9000`                                                                |
| `COPILOT_API_DASHBOARD_BASE_PATH`           | `/`                                                                    |
| `COPILOT_API_DASHBOARD_ENDPOINTS_FILE`      | `config/endpoints.yaml` if present, otherwise `/config/endpoints.yaml` |
| `COPILOT_API_DASHBOARD_DOCKER_IMAGE`        | `ghcr.io/caozhiyuan/copilot-api:latest`                                |
| `COPILOT_API_DASHBOARD_REQUEST_TIMEOUT`     | `5s`                                                                   |
| `COPILOT_API_DASHBOARD_MAX_CONCURRENCY`     | `32`                                                                   |
| `COPILOT_API_DASHBOARD_LOG_LEVEL`           | `info`                                                                 |
| `COPILOT_API_DASHBOARD_PERSISTENCE_ENABLED` | `false`                                                                |
| `COPILOT_API_DASHBOARD_DATABASE_PATH`       | `/data/dashboard.sqlite`                                               |
| `COPILOT_API_DASHBOARD_SYNC_INTERVAL`       | `10m`                                                                  |

Use the same `TZ` as the Copilot API containers.

### Endpoint configuration

```sh
cp config/endpoints.example.yaml config/endpoints.yaml
```

```yaml
endpoints:
  - name: copilot-api-account-a
    url: http://copilot-api-account-a:4141
    api_key_env: COPILOT_API_ACCOUNT_A_KEY
```

Use either `api_key_env` or `api_key`, not both. For Compose, uncomment the endpoint file mount and pass variables referenced by `api_key_env`. Local `config/endpoints.yaml` is ignored by Git.

### SQLite history

SQLite currently stores daily usage history only. Enable it by uncommenting the persistence variables and `/data` volume in `docker-compose.yml`. History syncs at startup, periodically, and on Refresh; quotas and request events remain live.

## Development

Requires Go 1.26 or later. Node.js is only needed for frontend tests.

```sh
make check
make run
```

```sh
node --test web/app.test.cjs
```

## License

[MIT](LICENSE)
