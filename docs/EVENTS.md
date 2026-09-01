# Optional motion events (Pub/Sub → ONVIF)

By default the bridge **does not** pull from Google Pub/Sub. Streaming works with only
OAuth (`tokens.json`). Enable this path when your ONVIF client honours third-party
motion events, or when another tool on the LAN subscribes to ONVIF Events.

Google Cloud setup (Pub/Sub subscription, service account, Google Home notification
settings) is in [`GOOGLE-CLOUD.md`](GOOGLE-CLOUD.md). Client-specific behaviour is in
the [compatibility table](../README.md#onvif-client-compatibility) in the README.

## Two-level configuration

Motion forwarding uses a **global master switch** and **per-camera** opt-in. Both must
be set for Nest detections to reach a camera's ONVIF timeline.

| Level | Config | Meaning |
| --- | --- | --- |
| Global | `events.onvif: true` | Bridge pulls Pub/Sub and may POST to `/trigger/motion` |
| Per camera | `event: { linger: 60s }` on that camera | This camera receives forwarded motion |

If `events.onvif` is `false`, the bridge never pulls Pub/Sub — even when individual
cameras have an `event:` block. If `events.onvif` is `true` but a camera has no
`event:` block, that camera is skipped (streaming only).

**Setup wizard:** on the Cameras step, check **ONVIF motion events** per camera (shown
after Pub/Sub is configured on the Google step). Deploy then writes `event:` only on
checked cameras and sets `events.onvif: true` automatically when **any** camera is
checked.

**Manual `config.yaml` example:**

```yaml
events:
  onvif: true

google:
  pubsub_subscription: "projects/<your-gcp-project>/subscriptions/sdm-events"
  service_account_key: "pubsub-sa.json"

cameras:
  - name: "Driveway"
    event: { linger: 60s }          # motion events enabled for this camera
    onvif: { mac: "...", ip: "..." }

  - name: "Front doorbell"
    # no event: block — streaming only, no Pub/Sub → ONVIF forwarding
    onvif: { mac: "...", ip: "..." }
```

| Setting | When `events.onvif: true` | Purpose |
| --- | --- | --- |
| `events.onvif` | — | Master switch |
| `event:` on a camera | Per camera | Opt in that camera for forwarding |
| `event.linger` | Optional | Hold motion on after last detection (default **60s**) |
| `google.pubsub_subscription` | Required | Pull subscription in **your** Cloud project |
| `google.service_account_key` | Required | Service account with **Pub/Sub Subscriber** on that subscription |

OAuth and the service account are separate: the SDM token cannot read Pub/Sub.

## Pipeline

```
Nest camera → SDM → Pub/Sub (sdm-prod)
                         ↓ pull (cameras with event: only)
                  nest-bridge subscriber
                         ↓ motion / person / chime
                  per-camera motion tracker (linger window)
                         ↓ on/off edge
                  POST http://<camera-ip>/trigger/motion?state=true|false
                         ↓
                  ONVIF sidecar fans out three motion topics
                         ↓
                  WS-PullPoint subscribers
```

### SDM → motion level

Google sends **pulses** (a detection happened), never an explicit “motion ended.” The
tracker reference-counts detections and holds motion **on** for `linger` after the last
pulse, then emits a single **off** edge.

| SDM Pub/Sub event | Tracker kind | Notes |
| --- | --- | --- |
| `sdm.devices.events.CameraMotion.Motion` | motion | General motion |
| `sdm.devices.events.CameraPerson.Person` | person | Collapsed into same motion window |
| `sdm.devices.events.DoorbellChime.Chime` | chime | Useful for doorbell timeline markers |

Person and chime are logged separately but delivered to ONVIF as motion — most ONVIF
clients treat all three as a single motion signal.

### ONVIF motion topics

Each `/trigger/motion` edge fans out to **three** ONVIF notification topics:

| Topic name | ONVIF path | Payload field |
| --- | --- | --- |
| `MotionAlarm` | `tns1:VideoSource/MotionAlarm` | `State` boolean |
| `CellMotionDetector` | `tns1:RuleEngine/CellMotionDetector/Motion` | `IsMotion` boolean |
| `MotionRegionDetector` | `tns1:RuleEngine/MotionRegionDetector/Motion` | `IsMotion` boolean |

All three are advertised in `GetEventProperties` and queued on every state change so
clients that subscribe without a topic filter receive compatible notifications.

Some recorders also run their own motion analysis on the RTSP stream. ONVIF events from
this bridge are optional timeline markers from Nest/Google detections — not required for
recording. Client-specific behaviour is in the [compatibility table](../README.md#onvif-client-compatibility).

## Verifying

### 1. Config and bridge logs

After deploy with events enabled:

```bash
NEST_BRIDGE_LOG_LEVEL=debug docker compose up -d bridge   # in deploy/
docker compose logs -f bridge | grep -E 'motion|pubsub'
```

Expect `nest detections → ONVIF motion enabled` at startup. If disabled, the log
names the reason (`events.onvif is false`, no cameras with `event:`, missing Pub/Sub
subscription, or missing service-account key).

### 2. ONVIF trigger (no Pub/Sub)

The ONVIF container always exposes `/trigger/motion` for testing:

```bash
curl -s -X POST "http://<camera-ip>/trigger/motion?state=true"
```

A successful response includes `"topics"` listing all three motion topic paths and
`"subscribers"` showing how many WS-PullPoint clients are connected (`1` when your NVR
is subscribed).

### 3. End-to-end from Nest

1. Enable notifications for the camera in the **Google Home** app (per detection type).
2. Trigger a detection (doorbell press is on demand).
3. Look for `pubsub message` and `motion level up` in bridge logs at debug level.

An empty Pub/Sub pull is normal when nothing has been detected recently. At debug
level, trait-only messages distinguish “no detections” from “camera not publishing.”

### 4. After ONVIF events code changes

Rebuild only the sidecar:

```bash
cd deploy && docker compose up -d --build onvif
```

With `events.onvif: false`, the bridge does not drive `/trigger/motion` from Nest
detections — manual curl testing still works.
