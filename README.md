# portd — Dynamic Service Registry Plugin for Caddy

A Caddy plugin that adds a REST registration endpoint to Caddy's admin API. Services register themselves by name at startup, and portd automatically creates path-based reverse proxy routes so they're reachable at `https://hostname/<name>/`.

No more remembering or coordinating port numbers.

## How It Works

1. SolarDirector starts on port 8080, calls `POST localhost:2019/portd/register {"name":"solar","port":8080}`
2. SolarDirector is now reachable at `https://hostname/solar/`
3. Grafana starts on port 3000, registers as `grafana` — reachable at `https://hostname/grafana/`
4. Users never see a port number

```
┌──────────────────────────────────────────────────┐
│                 Caddy (single process)            │
│                                                   │
│  :80/:443  ── HTTP server ──┬── /solar/*  → :8080 │
│                             ├── /grafana/* → :3000│
│                             └── /dispatch/* → :5000│
│                                                   │
│  :2019     ── Admin API ────┬── /config/  (stock) │
│                             ├── /portd/register   │
│                             ├── /portd/deregister │
│                             └── /portd/services   │
└──────────────────────────────────────────────────┘
```

Single binary, single process. Built with xcaddy. One systemd service, one config, done.

## Build

```bash
# Install xcaddy if you don't have it
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

# Build Caddy with portd
xcaddy build --with github.com/sshoecraft/caddy-portd

# Result: ./caddy binary with portd baked in
```

## Configuration

Minimal Caddyfile:

```
{
    portd {
        persist_file /etc/caddy/portd-registry.json
        health_interval 30s
    }
}

:80 {
    # portd dynamically adds routes here
}
```

## API

All endpoints live under Caddy's admin API (default `:2019`).

### POST /portd/register

Register a service. Creates a reverse proxy route from `/<name>/*` to `localhost:<port>`.

```json
{
  "name": "solar",
  "port": 8080,
  "strip_prefix": true,
  "health_uri": "/health"
}
```

| Field         | Required | Default   | Description                                          |
|---------------|----------|-----------|------------------------------------------------------|
| `name`        | yes      |           | URL path prefix (alphanumeric, hyphens, underscores) |
| `port`        | yes      |           | Backend port on localhost                            |
| `strip_prefix`| no       | true      | Strip `/<name>` prefix before forwarding             |
| `health_uri`  | no       |           | Path for active health checks                        |
| `host`        | no       | 127.0.0.1 | Backend host (for non-local services)                |

### DELETE /portd/deregister

Remove a service registration.

```json
{
  "name": "solar"
}
```

### GET /portd/services

List all registered services with their health status.

## Usage

### curl

```bash
# Register
curl -X POST http://localhost:2019/portd/register \
  -H "Content-Type: application/json" \
  -d '{"name":"myapp","port":3000}'

# Deregister
curl -X DELETE http://localhost:2019/portd/deregister \
  -H "Content-Type: application/json" \
  -d '{"name":"myapp"}'

# List
curl http://localhost:2019/portd/services
```

### Shell helpers

Add to `.bashrc`:

```bash
portd-register() {
  curl -s -X POST http://localhost:2019/portd/register \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$1\",\"port\":$2}" | jq .
}

portd-deregister() {
  curl -s -X DELETE http://localhost:2019/portd/deregister \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$1\"}" | jq .
}

portd-list() {
  curl -s http://localhost:2019/portd/services | jq .
}
```

### Systemd integration

```ini
[Service]
ExecStart=/opt/myapp/myapp --port 8080
ExecStartPost=/usr/bin/curl -s -X POST http://localhost:2019/portd/register \
  -H "Content-Type: application/json" \
  -d '{"name":"myapp","port":8080}'
ExecStopPost=/usr/bin/curl -s -X DELETE http://localhost:2019/portd/deregister \
  -H "Content-Type: application/json" \
  -d '{"name":"myapp"}'
```

## Features

- **Automatic reverse proxy routes** — register a name and port, get a route
- **Prefix stripping** — `/solar/api/status` forwards to `localhost:8080/api/status`
- **Health checking** — optional periodic checks with automatic route removal/restoration
- **Persistence** — registry survives Caddy restarts
- **Idempotent registration** — re-registering the same name+port refreshes the entry
- **Landing page** — root path lists all registered services with links
- **Collision detection** — duplicate names return 409

## License

MIT
