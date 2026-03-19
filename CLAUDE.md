# portd — Dynamic Service Registry Plugin for Caddy

## Overview

`portd` is a Caddy plugin (module) that adds a simple REST registration endpoint to Caddy's admin API. Services register themselves by name at startup, and portd automatically creates path-based reverse proxy routes so they're reachable at `https://hostname/<name>/`. No more remembering or coordinating port numbers.

**Example flow:**
1. SolarDirector starts on port 8080, calls `POST localhost:2019/portd/register {"name":"solar","port":8080}`
2. SolarDirector is now reachable at `https://hostname/solar/`
3. Grafana starts on port 3000, registers as `grafana` → reachable at `https://hostname/grafana/`
4. Users never see a port number

**Single binary, single process.** Built with `xcaddy build --with github.com/sshoecraft/caddy-portd`. The result is stock Caddy with one extra admin endpoint. One systemd service, one config, done.

## Problem Statement

Every web-based service on a machine needs its own port. Production and dev machines alike end up with services scattered across 3000, 5000, 8080, 8443, 9000, etc. Users have to remember which port goes with which service. Bookmarks break when ports change. There is no modern equivalent of the old Sun RPC portmapper for HTTP services.

## Architecture

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
│                                                   │
│  portd module:                                    │
│    - Maintains service registry (name → port)     │
│    - Manipulates Caddy route config internally    │
│    - Health checks registered services            │
│    - Persists registry to disk for restart        │
└──────────────────────────────────────────────────┘
```

## Registration API

All endpoints live under Caddy's admin API (default `:2019`).

### POST /portd/register

Register a service. Creates a reverse proxy route from `/<n>/*` to `localhost:<port>`.

**Request:**
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
| `strip_prefix`| no       | true      | Strip `/<n>` prefix before forwarding             |
| `health_uri`  | no       |           | Path for active health checks                        |
| `host`        | no       | 127.0.0.1 | Backend host (for non-local services)                |

**Response (200):**
```json
{
  "status": "registered",
  "name": "solar",
  "url": "https://hostname/solar/"
}
```

**Response (409 — name already registered):**
```json
{
  "error": "service 'solar' already registered on port 8080"
}
```

### DELETE /portd/deregister

Remove a service registration. Removes the reverse proxy route.

**Request:**
```json
{
  "name": "solar"
}
```

**Response (200):**
```json
{
  "status": "deregistered",
  "name": "solar"
}
```

### GET /portd/services

List all registered services.

**Response (200):**
```json
{
  "services": [
    {
      "name": "solar",
      "port": 8080,
      "host": "127.0.0.1",
      "strip_prefix": true,
      "health_uri": "/health",
      "healthy": true,
      "registered_at": "2026-03-19T10:30:00Z"
    },
    {
      "name": "grafana",
      "port": 3000,
      "host": "127.0.0.1",
      "strip_prefix": true,
      "healthy": true,
      "registered_at": "2026-03-19T10:31:00Z"
    }
  ]
}
```

## Implementation Details

### Caddy Module Structure

The plugin implements the `caddy.Module` and `caddy.AdminRouter` interfaces to register admin API routes.

```
caddy-portd/
├── CLAUDE.md           # This file
├── go.mod
├── go.sum
├── portd.go            # Module registration, admin route handlers
├── registry.go         # Service registry data structure + persistence
├── routes.go           # Caddy config manipulation (add/remove reverse proxy routes)
├── health.go           # Periodic health checker for registered services
├── portd_test.go       # Tests
└── README.md
```

### Key Behaviors

**Route management:** When a service registers, the plugin uses Caddy's internal config API (`caddy.ReplaceModule` or equivalent) to add a route that matches `/<n>/*`, strips the prefix (if configured), and reverse proxies to `host:port`. No external HTTP calls — it manipulates the running config directly.

**Persistence:** The registry is saved to a JSON file (default: `~/.config/portd/registry.json`) so that after a Caddy restart, all previously registered services are automatically re-registered. Services that fail health checks after restart are marked unhealthy but kept in the registry.

**Health checking:** If `health_uri` is provided, the plugin periodically (every 30s) hits `http://host:port/<health_uri>`. After 3 consecutive failures, the route is removed from Caddy's live config but kept in the registry as unhealthy. When the service comes back, the route is re-added automatically.

**Prefix stripping:** With `strip_prefix: true` (default), a request to `/solar/api/status` is forwarded to `localhost:8080/api/status`. The service doesn't need to know what path prefix it's registered under.

**Collision handling:** Service names must be unique. Attempting to register a name that's already taken returns 409. A service can re-register itself on the same name+port (idempotent) to refresh its registration.

### Landing Page

When no path matches a registered service, Caddy serves a simple index page at `/` listing all registered services with links. This makes it easy to discover what's running on a machine.

## Build & Install

### Build
```bash
# Install xcaddy
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

# Build Caddy with portd plugin
xcaddy build --with github.com/sshoecraft/caddy-portd

# Result: ./caddy binary with portd baked in
```

### Install
```bash
# Copy binary
sudo cp caddy /usr/local/bin/caddy

# Create systemd service
sudo tee /etc/systemd/system/caddy.service << 'EOF'
[Unit]
Description=Caddy with portd
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/caddy run --config /etc/caddy/Caddyfile --resume
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile
Restart=on-failure
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl enable --now caddy
```

### Minimal Caddyfile
```
{
    # Enable portd plugin
    portd {
        persist_file /etc/caddy/portd-registry.json
        health_interval 30s
    }
}

# Listen on all interfaces
:80 {
    # portd dynamically adds routes here
}
```

## Client Integration

### Shell (curl)
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

### Shell helper (add to .bashrc)
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

### From application startup scripts
```bash
#!/bin/bash
# start-solar.sh
cd /opt/solardirector
./solardirector --port 8080 &
curl -s -X POST http://localhost:2019/portd/register \
  -d '{"name":"solar","port":8080,"health_uri":"/api/health"}'
```

### Systemd integration (ExecStartPost)
```ini
[Service]
ExecStart=/opt/solardirector/solardirector --port 8080
ExecStartPost=/usr/bin/curl -s -X POST http://localhost:2019/portd/register \
  -H "Content-Type: application/json" \
  -d '{"name":"solar","port":8080}'
ExecStopPost=/usr/bin/curl -s -X DELETE http://localhost:2019/portd/deregister \
  -H "Content-Type: application/json" \
  -d '{"name":"solar"}'
```

## GitHub

Repository: `github.com/sshoecraft/caddy-portd`

## Tech Stack

- **Language:** Go (required for Caddy plugins)
- **Dependencies:** Caddy v2 module API, standard library only
- **Build tool:** xcaddy

## Future Considerations

- WebSocket support (should work transparently via Caddy's reverse proxy)
- Optional authentication on the registration endpoint
- Subdomain-based routing as alternative to path-based (`solar.hostname` vs `hostname/solar`)
- mDNS/DNS-SD advertisement of registered services
- Client libraries for common languages (Python, Node, C)
