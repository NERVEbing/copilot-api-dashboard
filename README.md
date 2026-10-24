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
- Cache hit rate, request duration, and HTTP/body completion outcomes
- Docker discovery and YAML endpoints
- Optional account display names in the Accounts table, toggled with `d`
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

| Environment variable                         | Default                                                                |
| -------------------------------------------- | ---------------------------------------------------------------------- |
| `TZ`                                         | `UTC`                                                                  |
| `COPILOT_API_DASHBOARD_LISTEN_ADDR`          | `:9000`                                                                |
| `COPILOT_API_DASHBOARD_BASE_PATH`            | `/`                                                                    |
| `COPILOT_API_DASHBOARD_ENDPOINTS_FILE`       | `config/endpoints.yaml` if present, otherwise `/config/endpoints.yaml` |
| `COPILOT_API_DASHBOARD_ACCOUNT_ALIASES_FILE` | Empty (disabled)                                                       |
| `COPILOT_API_DASHBOARD_DOCKER_IMAGE`         | `ghcr.io/caozhiyuan/copilot-api:latest`                                |
| `COPILOT_API_DASHBOARD_REQUEST_TIMEOUT`      | `5s`                                                                   |
| `COPILOT_API_DASHBOARD_MAX_CONCURRENCY`      | `32`                                                                   |
| `COPILOT_API_DASHBOARD_LOG_LEVEL`            | `info`                                                                 |
| `COPILOT_API_DASHBOARD_PERSISTENCE_ENABLED`  | `false`                                                                |
| `COPILOT_API_DASHBOARD_DATABASE_PATH`        | `/data/dashboard.sqlite`                                               |
| `COPILOT_API_DASHBOARD_SYNC_INTERVAL`        | `10m`                                                                  |

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

For authenticated containers, configure a YAML endpoint with the same URL as Docker discovery and a regular API key from the upstream `config.json` `auth.apiKeys` list. YAML takes precedence for the same normalized URL. Docker discovery does not read the upstream configuration file.

### Account display names

```sh
cp config/account-aliases.example.yaml config/account-aliases.yaml
```

Edit the file to map complete account logins to display names:

```yaml
aliases:
  account-a: Alice
  account-b: Bob
```

Set `COPILOT_API_DASHBOARD_ACCOUNT_ALIASES_FILE` to the file path. For Compose, uncomment both the variable and the read-only file mount. The file is loaded at startup; restart the dashboard after editing it. An unreadable or invalid file prevents startup. Logins match case-insensitively; duplicate logins and empty or non-string display names are invalid. Local `config/account-aliases.yaml` is ignored by Git.

Press `d` to toggle display names in the Accounts table. Unmapped accounts keep their logins; mapped names that duplicate another account's label include the original login. Sorting uses the displayed names. Account selection, detail headings, API queries, and history continue to use original logins.

The shortcut works when the Accounts table is visible and at least one account has a display name. It ignores input fields, selects, editable content, Ctrl/Command/Alt combinations, repeated keydown events, and input method composition. The display mode survives Refresh and filter changes within the page, and resets to original logins on a full page reload. Display names and the selected display mode are not stored in SQLite or browser storage.

### Usage metrics

Cache hit rate is cache-read tokens divided by the sum of input, cache-read, and cache-creation tokens. It appears in the summary and Model breakdown. All-account values use the combined token counts. Missing counters or a zero denominator display `—`; a positive denominator with no cache reads displays `0%`.

Request events show duration and outcome from the live upstream response. Duration is displayed in milliseconds below one second, or seconds otherwise. Missing measurements display `—`. Outcome describes HTTP/body completion: `Completed`, `Cancelled`, or `Error`. `Completed` does not certify model success; an SSE protocol error can still end with a completed response body.

### SQLite history

SQLite currently stores daily usage history only. Enable it by uncommenting the persistence variables and `/data` volume in `docker-compose.yml`. History syncs at startup, periodically, and on Refresh; quotas and request events remain live. Newer snapshots can correct previously stored past dates; dates omitted by upstream remain stored.

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
