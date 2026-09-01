# Agent and contributor guide

Guidance for humans and coding agents working in this repository.

## What this project is

**nest-to-ONVIF** is a Go control plane that pulls live Nest video through Google's Smart Device
Management (SDM) API and republishes each camera as a virtual ONVIF device on the LAN. Runtime
deployment is **Linux-only** (macvlan + host-networked Docker). Development on macOS is fine for
building and unit tests; integration testing needs a Linux VM or the production host.

Read these before making non-trivial changes:

| Doc | When to read |
| --- | --- |
| [README.md](README.md) | Architecture, constraints, client compatibility |
| [docs/SETUP.md](docs/SETUP.md) | VM, wizard, deploy, adoption |
| [docs/GOOGLE-CLOUD.md](docs/GOOGLE-CLOUD.md) | OAuth, Device Access, Pub/Sub |
| [docs/STREAMING.md](docs/STREAMING.md) | Continuous streaming, session renewal, wire power |
| [docs/EVENTS.md](docs/EVENTS.md) | Optional motion events |

## Repository layout

| Path | Purpose |
| --- | --- |
| `cmd/nest-bridge/` | CLI entrypoint |
| `internal/cli/` | Subcommands: `auth`, `serve`, `setup`, config generators |
| `internal/sdm/`, `internal/session/`, `internal/media/` | OAuth, WebRTC, RTP → RTSP |
| `internal/setup/` | Setup wizard (embedded UI + HTTP API) |
| `internal/onvif/`, `internal/mediamtx/` | Generated sidecar config |
| `internal/events/`, `internal/viewer/` | Optional Pub/Sub motion; LAN viewer |
| `deploy/` | Docker Compose, ONVIF sidecar patches, macvlan scripts |
| `bin/` | Shell helpers for host setup (tracked) |
| `docs/` | User-facing documentation and setup screenshots |

## Build and verify

```bash
make build          # → bin/nest-bridge
make test           # go test -race -cover ./...
make fmt            # gofmt
make lint           # go vet
```

Run tests after Go changes. Config generators have golden tests under `internal/onvif/` and
`internal/mediamtx/`.

## Secrets and generated files — do not commit

Never commit credentials or machine-local state:

- `config.yaml`, `tokens.json`, `setup-draft.yaml`, `*-sa.json`, `*.bak`
- `bin/nest-bridge` (compiled binary)
- `deploy/onvif.yml`, `deploy/mediamtx.yml` (generated from config)

Use `config.example.yaml` for documented defaults. Scrub OAuth client IDs, Device Access project
UUIDs, Pub/Sub subscription paths, and real camera names from docs and screenshots.

## Coding conventions

- **Minimize scope.** Fix the task; do not refactor unrelated code.
- **Match existing style.** Read surrounding packages before adding abstractions.
- **Prefer extending** existing helpers over parallel implementations.
- **Comments** only for non-obvious behaviour (SDM quotas, macvlan, WebRTC renewal timing).
- **Tests** when behaviour is easy to get wrong; golden tests for generated YAML. Skip trivial
  assertions.
- **User docs** live in `docs/`; keep [README.md](README.md) short and link out.

### Domain constraints agents should respect

- `serve` is **continuous 24/7 streaming** per camera, renewed via `ExtendWebRtcStream`.
- **Battery-powered Nest cameras are unsupported** for this model.
- Each virtual camera needs a stable **MAC + IP**; changing either after NVR adoption presents a
  new device.
- SDM has a **global 10 QPM** quota; the scheduler prioritises session renewals.
- Nest delivers Opus over WebRTC; AAC is transcoded for ONVIF when `audio: true`.

## Common change areas

| Change | Touch |
| --- | --- |
| New CLI flag or subcommand | `internal/cli/`, `cmd/nest-bridge/` |
| SDM / WebRTC / renewal | `internal/sdm/`, `internal/session/` |
| Setup wizard UI | `internal/setup/static/index.html`, handlers in `internal/setup/` |
| ONVIF profiles / events | `internal/onvif/`, `deploy/onvif/` patches |
| MediaMTX paths / HLS | `internal/mediamtx/`, `deploy/mediamtx.yml` template |
| Docker deploy | `deploy/docker-compose.yml`, `internal/setup/deploy.go` |
| LAN viewer | `internal/viewer/` |

The ONVIF sidecar is a patched upstream image (`deploy/onvif/patch-onvif-server.js`). Changes there
need the Docker build path exercised, not only Go unit tests.

## Deployment notes for agents

Production runs on a Linux VM (e.g. `/opt/nest-bridge`). Typical flow:

1. `bin/build-bridge` on the host
2. `./bin/nest-bridge setup` (wizard on `127.0.0.1:8190`; SSH tunnel from laptop)
3. Wizard deploy → writes `config.yaml`, generates sidecar configs, `docker compose up`

Do not assume Docker Desktop on macOS/Windows can run the full stack. Do not force-push `main`.

## Pull requests

- One logical change per PR when possible.
- Include a short **why** in the commit message.
- Note test commands run (`make test`, manual VM check, etc.).
- Update `docs/` when user-visible behaviour or setup steps change.
- Do not commit secrets, local `scripts/` tooling artifacts, or `node_modules`.

## Screenshots and docs

Setup wizard screenshots live in `docs/images/setup/`. Use obfuscated fixture data (placeholder
Google IDs, fake device IDs, example IPs). Do not publish real home or GCP identifiers.
