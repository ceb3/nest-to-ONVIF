# nest-to-ONVIF

A Go control plane that pulls live video from Google Nest cameras through the official
Smart Device Management (SDM) API and republishes each one as a virtual ONVIF camera on
your LAN. Any recorder that adopts third-party ONVIF devices can use them like ordinary
IP cameras.

Live SDM access does not require Nest Aware. Google charges a one-time **$5 USD**
Device Access registration fee. 

## What it provides

Each configured camera becomes a distinct ONVIF device (unique MAC and IP) with RTSP
streams, JPEG snapshots, optional AAC audio, and an optional ONVIF Events service.

| Layer | Role |
| --- | --- |
| **Control plane** (`nest-bridge serve`) | OAuth, SDM/WebRTC sessions, RTP → RTSP, optional LAN viewer (`:8090`) |
| **Media plane** (MediaMTX + ffmpeg) | HQ/LQ H.264, AAC transcode, snapshots, HLS for the viewer |
| **ONVIF sidecar** | WS-Discovery, profiles, RTSP proxy, Events service |
| **Events (optional)** | Pub/Sub → motion tracker → ONVIF `/trigger/motion` |

## Documentation

| Doc | Contents |
| --- | --- |
| [**SETUP.md**](docs/SETUP.md) | VM, host scripts, setup wizard, deploy, live view, adoption |
| [**GOOGLE-CLOUD.md**](docs/GOOGLE-CLOUD.md) | Cloud project, OAuth, Device Access, Pub/Sub, IAM, `gcloud` checks |
| [**EVENTS.md**](docs/EVENTS.md) | Optional Nest detections → ONVIF motion (`events.onvif`) |
| [**STREAMING.md**](docs/STREAMING.md) | Continuous streaming, session renewal, wire-power requirement |

**Quick start:** provision a Linux VM → [`docs/SETUP.md`](docs/SETUP.md) → run
`./bin/nest-bridge setup` (SSH tunnel `8190` to the VM) → adopt cameras by IP in your
NVR.

## ONVIF client compatibility

Profile S: H.264, optional AAC, HQ + LQ profiles, JPEG snapshots, ONVIF Events. Clients
vary in what they use.

| Client | Video | Audio | Snapshots | HQ + LQ | ONVIF motion from `events.onvif` | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| **UniFi Protect 7.x** | H.264 | AAC when `audio: true` | JPEG | Both held open | Ignored — motion from RTSP analysis | Pub/Sub not needed for recording |
| **Blue Iris** | H.264 | AAC when `audio: true` | JPEG | Usually one | Often honoured | |
| **Home Assistant** | H.264 | AAC when `audio: true` | JPEG | Usually one | Often honoured | |
| **Synology SS** | H.264 | AAC when `audio: true` | JPEG | Varies | Varies | |
| **Scrypted** | H.264 | AAC when `audio: true` | JPEG | Varies | Varies | May use RTSP URL directly |
| **Agent DVR** | H.264 | AAC when `audio: true` | JPEG | Varies | Often honoured | |
| **ZoneMinder** | H.264 | AAC when `audio: true` | JPEG | Varies | Varies | |
| **Frigate** | — | — | — | — | — | Use RTSP (`rtsp://<host>:8554/<camera>-hq`), not ONVIF adoption |

Adopt by **IP address** (all cameras share the `nest-bridge Nest Virtual Camera` label).
Each camera needs a unique `cameras[].onvif` MAC and IP; changing either after adoption
presents a new device. Requires a **Linux host** with macvlan — not Docker Desktop on
macOS/Windows.

Nest delivers Opus over WebRTC; the bridge transcodes to **AAC** for ONVIF when
`audio: true`. Two-way talk is not supported.

## Requirements

- Go 1.27+ (to build from source)
- Consumer Google account (not Workspace / Advanced Protection)
- Google Cloud project + Device Access project ($5 one-time)
- **Wire-powered** Nest cameras (see [continuous streaming](docs/STREAMING.md))
- Internet between camera, Google, and the bridge host
- **Linux** deployment host (macvlan + host networking)

Pub/Sub credentials are required only when [`events.onvif`](docs/EVENTS.md) is enabled.

nest-bridge `serve` opens a **24/7** SDM/WebRTC session per camera and renews it with
Google's `ExtendWebRtcStream` API. **Battery-powered cameras are not supported** for
that model — Google does not extend streams on battery, and battery doorbells cannot
extend at all. Use permanently mains-powered devices only.

## Bandwidth

Nest video is cloud-routed. A measured doorbell sample was ~1.33 Mbps sustained (~430 GB/month
per camera). Six cameras can exceed **2.5 TB/month** on WAN. See [known constraints](#known-constraints).

## Known constraints

| Constraint | Consequence |
| --- | --- |
| Continuous streaming only | `serve` holds one live session per camera; not for on-demand viewing |
| Wire power required | Battery cameras unsupported; see [STREAMING.md](docs/STREAMING.md) |
| Cloud-routed video | WAN use; stops on internet outage |
| WebRTC sessions ~5 min | Renewed via `ExtendWebRtcStream`, not full reconnect each time |
| SDM quota (10 QPM global) | Scheduler prioritises renewals |
| One MAC + IP per virtual camera | Linux macvlan required |
| HQ + LQ often both held open | Budget LAN and WAN bandwidth |
| Recorder camera limits | Each virtual camera counts |

## Architecture

| Path | Role |
| --- | --- |
| `cmd/nest-bridge/` | CLI entrypoint |
| `internal/cli/` | `auth`, `devices`, `stream`, `serve`, `setup`, config generators |
| `internal/sdm/`, `internal/session/`, `internal/media/` | OAuth, WebRTC, RTSP publish |
| `internal/events/`, `internal/viewer/` | Optional Pub/Sub motion; LAN viewer |
| `internal/onvif/`, `internal/mediamtx/` | Generated sidecar configs |

Video RTP is forwarded without re-encoding; MediaMTX/ffmpeg derives HQ/LQ AAC renditions
and JPEG snapshots per camera.

## Acknowledgements

Inspired by Den Delimarsky's
[research on recording Nest video](https://den.dev/blog/free-nest-video-recording/).
Current Nest hardware is WebRTC-only; this project uses Google's official SDM API.

## Disclaimer

Unaffiliated with Google. SDM behaviour may change without notice. Not suitable for
life-safety or critical monitoring.
