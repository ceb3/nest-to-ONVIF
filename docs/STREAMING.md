# Continuous streaming

nest-bridge is built for **always-on** live video: one SDM session per configured camera,
renewed in place and republished to MediaMTX/ONVIF for as long as `nest-bridge serve`
runs. It is not an on-demand or event-triggered viewer.

## How sessions stay alive

Google Nest cameras expose live video over **WebRTC** through the SDM API
(`GenerateWebRtcStream`). Each session is valid for about **five minutes**.

The bridge does **not** tear down and reopen the stream every five minutes. While a
camera is healthy, it calls [`ExtendWebRtcStream`](https://developers.google.com/nest/device-access/traits/device/camera-live-stream#extendwebrtcstream)
with the existing `mediaSessionId` before expiry (at roughly **60% of the remaining
lifetime**, ~3 minutes into a 5-minute window). That pushes `expiresAt` forward while
keeping the same WebRTC peer connection and RTP → RTSP path.

A **full reconnect** (`GenerateWebRtcStream` again, new peer connection) happens only
when:

- Google rejects extend (unsupported device or error)
- the WebRTC connection drops
- SDM rate limits or other errors end the renewal loop

Protect and other recorders therefore see a steady RTSP feed with occasional brief gaps
if a camera reconnects.

## Wire power required

**Battery-powered Nest cameras are not supported** for continuous use with this bridge.

Google only honours `ExtendWebRtcStream` on **wire-powered** cameras. For battery models:

- On **battery**, extend may be **ignored** — the session still dies after ~5 minutes
  even if the API appears to succeed.
- **Battery doorbells** do not support extend at all; Google expects you to stop and
  generate a new stream.

Outdoor battery cameras count as wire-powered only while **plugged in for charging**;
on battery alone they behave like unsupported devices for extend.

Because nest-bridge holds a stream open **24/7** for every configured camera, it
targets the same deployment model as a wired IP camera: permanent mains power (wired
doorbell, wired indoor, floodlight cam, or outdoor cam on a permanent power adapter).

| Power | Supported for continuous streaming? |
| --- | --- |
| Wired doorbell / wired indoor / floodlight | Yes — extend in place |
| Outdoor battery on **permanent** mains adapter | Yes — treated as wire-powered |
| Outdoor battery on **battery only** | **No** — ~5 min gaps or unstable stream |
| Battery doorbell | **No** — must regenerate every ~5 min; not suitable for 24/7 |

If you need only occasional live view, use the Google Home app or nest-bridge's
`stream` subcommand for a single camera — not `serve` with multiple always-on paths.

## Choosing cameras in the wizard

When selecting devices in [`SETUP.md`](SETUP.md), prefer cameras that will stay on
mains power. `./bin/nest-bridge devices` lists each device's SDM type; cross-check
Google's device list for wired vs battery models before adding a camera to `config.yaml`.

## SDM quota

Continuous streaming uses Google's per-project quota (10 commands/minute globally, 5 per
command per device). The bridge prioritises **session renewals** over new connections so
live streams are less likely to drop during contention. Each configured camera consumes
quota continuously — budget headroom when bridging many devices.
