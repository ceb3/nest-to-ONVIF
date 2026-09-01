# Optional motion events (Pub/Sub → ONVIF)

By default the bridge **does not** pull from Google Pub/Sub. Streaming works with only
OAuth (`tokens.json`). Enable this path when your ONVIF client honours third-party
motion events, or when another tool on the LAN subscribes to ONVIF Events.

Google Cloud setup (Pub/Sub subscription, service account, Google Home notification
settings) is in [`GOOGLE-CLOUD.md`](GOOGLE-CLOUD.md). Client-specific behaviour is in
the [compatibility table](../README.md#onvif-client-compatibility) in the README.

## Configuration

```yaml
events:
  onvif: false   # default

google:
  pubsub_subscription: "projects/<your-gcp-project>/subscriptions/sdm-events"
  service_account_key: "pubsub-sa.json"
```

| Setting | When `events.onvif: true` | Purpose |
| --- | --- | --- |
| `events.onvif` | — | Master switch |
| `google.pubsub_subscription` | Required | Pull subscription in **your** Cloud project |
| `google.service_account_key` | Required | Service account with **Pub/Sub Subscriber** on that subscription |

OAuth and the service account are separate: the SDM token cannot read Pub/Sub.

Per camera, enable forwarding in the setup wizard or with an `event:` block. Override
linger (default **60s**):

```yaml
event: { linger: 60s }
```

## Pipeline

```
Nest camera → SDM → Pub/Sub (sdm-prod)
                         ↓ pull
                  nest-bridge subscriber
                         ↓ motion / person / chime
                  per-camera motion tracker
                         ↓ rising edge only
                  POST http://<camera-ip>/trigger/motion?state=true|false
                         ↓
                  ONVIF Events → subscribing clients
```

The tracker collapses detection bursts into one motion window. SDM kinds mapped:

| SDM event | Forwarded as |
| --- | --- |
| `CameraMotion.Motion` | Motion |
| `CameraPerson.Person` | Person |
| `DoorbellChime.Chime` | Chime |

## Verifying

1. Deploy with `events.onvif: true` and Pub/Sub credentials ([SETUP](SETUP.md)).
2. Run with debug logging: `NEST_BRIDGE_LOG_LEVEL=debug docker compose up -d bridge`
3. Trigger a detection (doorbell press is on demand).
4. Look for `pubsub message` and `motion level up` in bridge logs.

An empty Pub/Sub pull is normal when nothing has been detected recently. At debug
level, trait-only messages distinguish “no detections” from “camera not publishing.”

## ONVIF trigger without Pub/Sub

The ONVIF container always exposes `/trigger/motion` for testing:

```bash
curl -X POST "http://<camera-ip>/trigger/motion?state=true"
```

With `events.onvif: false`, the bridge does not drive these from Nest detections.
