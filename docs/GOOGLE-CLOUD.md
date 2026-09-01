# Google Cloud configuration reference

Console setup for streaming and optional motion events: Cloud project, OAuth, Device
Access, Pub/Sub, IAM, and verification.

| Related | Doc |
| --- | --- |
| VM, wizard, deploy | [`SETUP.md`](SETUP.md) |
| `events.onvif` behaviour | [`EVENTS.md`](EVENTS.md) |

Read this when you need *what* exists and *why*, when something returns 403, or when
reproducing the setup in a new project. Follow [`SETUP.md`](SETUP.md) for the ordered
host-side walkthrough.

## Three identifiers that are easy to confuse

Most setup failures come from mixing these up. They are unrelated values.

| Identifier | Example shape | Where it is used |
|---|---|---|
| Google Cloud project ID | `my-project-123456` | Cloud console, enabling APIs, Pub/Sub resources, IAM |
| Device Access project ID | a UUID | `google.project_id` in `config.yaml`, and every SDM API path |
| OAuth client ID | `…apps.googleusercontent.com` | `google.client_id`, and registered with Device Access |

`google.project_id` is the **Device Access UUID**, never the Cloud project ID.
Getting this wrong produces authorization failures and empty device lists rather
than a clear error.

## What gets created

| # | Resource | Console | Cost |
|---|---|---|---|
| 1 | Google Cloud project | Cloud console | free |
| 2 | Smart Device Management API, enabled | Cloud console | free |
| 3 | Cloud Pub/Sub API, enabled | Cloud console | free tier is ample |
| 4 | OAuth consent configuration (Branding, Audience) | Google Auth Platform | free |
| 5 | OAuth client ID, type Web application | Google Auth Platform | free |
| 6 | Device Access project, with events enabled | Device Access console | **$5 one-time** |
| 7 | Pub/Sub topic for device events | created via Device Access | free tier |
| 8 | Pub/Sub **subscription** to that topic | Cloud console | free tier |
| 9 | Service account for reading the subscription | Cloud console | free |
| 10 | IAM grant: Pub/Sub Subscriber **on the subscription** | Cloud console | free |

The only charge is Google's one-time $5 Device Access registration fee. This
integration does **not** require a Nest Aware subscription.

## Account restrictions

Use a consumer Google account that owns or has access to the Nest home. The SDM
API rejects Google Workspace accounts and accounts enrolled in the Advanced
Protection Program. Whichever account grants consent is the account whose homes
and devices the bridge can see.

---

## 1–2. Cloud project and the SDM API

Create a project, then enable the
[Smart Device Management API](https://console.cloud.google.com/apis/library/smartdevicemanagement.googleapis.com)
in it.

## 3. The Pub/Sub API

Enable [Cloud Pub/Sub](https://console.cloud.google.com/apis/library/pubsub.googleapis.com)
in the same project. This is separate from SDM and is only needed for motion
events; live streaming works without it.

## 4. OAuth consent (Google Auth Platform)

The old single "OAuth consent screen" page is now split across pages. On a project
with no auth configured, opening any of them starts a **Get started** flow that
collects everything at once.

- [Branding](https://console.cloud.google.com/auth/branding): application name and
  your email. **Leave the entire App domain section empty.**
- [Audience](https://console.cloud.google.com/auth/audience): user type
  **External**, and add your consumer Google account under **Test users**.

Leaving App domain empty is deliberate, not laziness. That section is
all-or-nothing: filling in a home page, terms link, or authorized domain makes the
privacy policy link mandatory and Branding refuses to save without one. With every
field blank nothing is required. No authorized domain is needed because Google
exempts loopback addresses (`localhost` and `127.0.0.1`), which is why the redirect
below uses a loopback host.

Skip the app logo — uploading one forces the app into verification. Leave
publishing status in **Testing** for the same reason.

## 5. OAuth client

On [Clients](https://console.cloud.google.com/auth/clients), create an **OAuth
client ID** of type **Web application**, with this authorised redirect URI:

```text
http://127.0.0.1:8190/oauth2callback
```

Google treats `localhost` and `127.0.0.1` as different URIs — register whichever you
will use and match it byte-for-byte in `google.redirect_uri`. This project uses
`127.0.0.1` because the setup wizard listens on `127.0.0.1:8190` and SSH port
forwarding is typically `ssh -L 8190:127.0.0.1:8190`.

It must match `google.redirect_uri` byte for byte. Record the client ID and
secret; the secret is a credential and must never be committed.

## 6. Device Access project

In the [Device Access Console](https://console.nest.google.com/device-access),
accept the terms and pay the one-time **$5** fee. Then create a project:

1. Supply the OAuth **client ID** from step 5.
2. **Enable events** — without this Google publishes nothing and motion events
   cannot work. Live streaming still would, which makes this easy to miss.
3. For the Pub/Sub topic, choose **create a new topic**.
4. Record the Device Access **Project ID**, a UUID. This is `google.project_id`.

## 7. The events topic

The topic is created by the Device Access console and is **owned by Google**, not
by your Cloud project. You cannot administer it, and it will not appear in your
project's topic list. The console shows its full resource name; it names a
Google-managed project and your enterprise UUID.

## 8. The subscription

This you create, in **your** Cloud project, pointing at Google's topic. It is a
**pull** subscription — the bridge pulls; nothing needs to be reachable from the
internet, so no push endpoint, no TLS certificate, and no inbound firewall rule.

Name it `sdm-events`, giving the full name that goes into
`google.pubsub_subscription`:

```text
projects/<your-cloud-project-id>/subscriptions/sdm-events
```

Note the asymmetry: the subscription is in your project while the topic is in
Google's. A subscription name under a Google-managed project is a sign the topic
path was pasted where the subscription path belongs.

## 9–10. Service account and IAM

The bridge needs two **separate** credentials, and they are not interchangeable:

| Credential | Grants | Used for |
|---|---|---|
| OAuth user token (`tokens.json`) | scope `https://www.googleapis.com/auth/sdm.service` | Listing devices, starting and extending WebRTC streams |
| Service-account key (JSON) | scope `https://www.googleapis.com/auth/pubsub` | Pulling motion events |

The SDM token cannot read Pub/Sub — `sdm.service` is the only scope the SDM API
accepts, and it confers nothing else. The alternative to a service account is
adding `cloud-platform` to the OAuth client, which was rejected: it forces
re-consent, invalidates the working token, and grants the bridge broad access to
the whole project to read one subscription.

Create a service account — `nest-bridge-events` is the name assumed here — then grant it the
**Pub/Sub Subscriber** role **on the subscription itself**, not at project level.
Download a JSON key.

With `gcloud`, that is:

```bash
PROJECT=<your-cloud-project-id>
SA=nest-bridge-events@$PROJECT.iam.gserviceaccount.com

gcloud iam service-accounts create nest-bridge-events \
  --display-name="nest-bridge Pub/Sub reader" --project="$PROJECT"

# On the subscription, not the project.
gcloud pubsub subscriptions add-iam-policy-binding sdm-events \
  --member="serviceAccount:$SA" --role=roles/pubsub.subscriber --project="$PROJECT"

gcloud iam service-accounts keys create pubsub-sa.json --iam-account="$SA"
```

> **If you find the grant at project level.** This deployment originally had it there,
> and it works identically — the role is narrow and there is one subscription — but it
> would cover any future subscription automatically. Moving it needs no new key, since
> the identity does not change:
>
> ```bash
> gcloud pubsub subscriptions add-iam-policy-binding sdm-events \
>   --member="serviceAccount:$SA" --role=roles/pubsub.subscriber
> gcloud projects remove-iam-policy-binding "$PROJECT" \
>   --member="serviceAccount:$SA" --role=roles/pubsub.subscriber
> ```
>
> **Order matters.** Add the narrow grant and confirm a pull still succeeds *before*
> removing the broad one, or the bridge loses access in between. Done that way here on
> 2026-08-31, the running bridge saw no interruption.

Google's download filename embeds the project and key ID, so it matches no
predictable pattern. The repository ignores several shapes to compensate, but
**check `git check-ignore` on the actual filename before committing anything**. A
key committed once must be revoked, not deleted. The safest move is to rename it
to `pubsub-sa.json`, which is ignored by name, and point
`google.service_account_key` in `config.yaml` at it.

That setting is what enables motion events. With it unset — or naming a file the
bridge cannot read or parse — the bridge logs `motion events disabled` once at
startup and carries on streaming. Events are an enhancement; nothing about them
can stop video.

### The 403 that is supposed to happen

`roles/pubsub.subscriber` grants consume and acknowledge, but **not**
`pubsub.subscriptions.get`. So:

- Pulling messages returns 200.
- Reading subscription metadata returns 403 `IAM_PERMISSION_DENIED`.

This is correct least privilege, not a misconfiguration. Do not widen the role to
make the 403 disappear, and never use a metadata call as a startup health check —
it will always fail.

---

## Verifying the Google side with `gcloud`

Every command below was run against this deployment on 2026-08-31 and the outputs are
the real ones, lightly trimmed. Run them as **your** user account, not the service
account — most of these read metadata, which the service account deliberately cannot do.

```bash
gcloud auth login
gcloud config set project <your-cloud-project-id>
```

**1. The two APIs are enabled.**

```bash
gcloud services list --enabled \
  --filter="config.name:(smartdevicemanagement.googleapis.com OR pubsub.googleapis.com)" \
  --format="table(config.name)"
```

```text
NAME
pubsub.googleapis.com
smartdevicemanagement.googleapis.com
```

Both must appear. `smartdevicemanagement` missing breaks streaming; `pubsub` missing
breaks events only.

**2. The subscription points at Google's topic.** This is the single most valuable
check, and the one that answers "did I wire the topic up correctly?".

```bash
gcloud pubsub subscriptions describe sdm-events \
  --format="yaml(name,topic,state,ackDeadlineSeconds,messageRetentionDuration,pushConfig)"
```

```yaml
ackDeadlineSeconds: 10
messageRetentionDuration: 3600s
name: projects/<your-cloud-project-id>/subscriptions/sdm-events
pushConfig: {}
state: ACTIVE
topic: projects/sdm-prod/topics/enterprise-<your-device-access-uuid>
```

What each line is telling you:

- `topic:` must name **`sdm-prod`**, Google's project — not yours. A topic in your own
  project means the subscription was pointed at a self-made topic that nothing ever
  publishes to, and the symptom is a subscription that stays empty forever.
- The UUID after `enterprise-` must equal `google.project_id` in `config.yaml`.
- `pushConfig: {}` confirms it is a **pull** subscription, so nothing needs to be
  reachable from the internet.
- `state: ACTIVE` — a subscription can expire after prolonged inactivity.

**3. The service account exists and is enabled.**

```bash
gcloud iam service-accounts list --filter="email:nest-bridge-events*" \
  --format="table(email,disabled)"
```

```text
EMAIL                                                       DISABLED
nest-bridge-events@<project>.iam.gserviceaccount.com        False
```

**4. Where the Subscriber role is actually granted.** Check both scopes, because a grant
in either place works and only one is intended:

```bash
gcloud pubsub subscriptions get-iam-policy sdm-events            # preferred location
gcloud projects get-iam-policy <your-cloud-project-id> \
  --flatten="bindings[].members" \
  --filter="bindings.members:nest-bridge-events@<project>.iam.gserviceaccount.com" \
  --format="table(bindings.role)"                                # broader fallback
```

In this deployment the first now returns the binding and the second returns nothing:

```yaml
bindings:
- members:
  - serviceAccount:nest-bridge-events@<project>.iam.gserviceaccount.com
  role: roles/pubsub.subscriber
```

That is the intended shape. A binding in the project policy instead is the case covered
by the note above. If **neither** returns anything, the bridge cannot pull at all.

**5. The key on disk is the one Google knows about.**

```bash
gcloud iam service-accounts keys list \
  --iam-account=nest-bridge-events@<project>.iam.gserviceaccount.com \
  --managed-by=user --format="table(name.basename(),validAfterTime)"

python3 -c "import json;d=json.load(open('pubsub-sa.json'));print(d['client_email'],d['private_key_id'])"
```

The `private_key_id` from the file must appear in the listed key ids. More than one
user-managed key is worth pruning: each is a separate credential that can leak.

**6. The key is not about to be committed.**

```bash
git check-ignore -v pubsub-sa.json
```

```text
.gitignore:20:*-sa.json	pubsub-sa.json
```

Silence here means the file is **not** ignored. Fix that before your next commit — a key
committed once must be revoked, not deleted.

**7. The service account can pull, and cannot do more.** Run these two *as the service
account*, which is the only pair here that should use it:

```bash
SA=nest-bridge-events@<project>.iam.gserviceaccount.com
gcloud pubsub subscriptions pull sdm-events --limit=1 --account="$SA"
gcloud pubsub subscriptions describe sdm-events --account="$SA"
```

The pull returns `Listed 0 items.` when nothing is queued — which is the normal, healthy
state. The describe returns `IAM_PERMISSION_DENIED` on `pubsub.subscriptions.get`, and
that failure is **correct**; see the section above.

> Do not read an empty pull as a fault. An idle event pipeline is indistinguishable
> from a broken one until a detection actually happens. Google publishes only when a
> camera detects something, so verify events by triggering one — a doorbell press is
> on-demand — and then watching the bridge log for `pubsub message` at debug level.
> This path is only active when `events.onvif` is `true`.

**What `gcloud` cannot check.** Two things, and between them they account for most
"no events ever arrive" reports.

First, whether **events are enabled** on the Device Access project. That setting lives
in the Device Access console, has no API, and is easy to miss because streaming works
perfectly without it.

Second, and less obvious: the **Google Home app's notification settings, per camera**.
These govern what SDM publishes to Pub/Sub, not just what reaches your phone. A camera
whose notifications are off is silent on the feed no matter how correct everything in
this document is.

| Google Home setting | Effect on the Pub/Sub feed |
|---|---|
| Notifications: **Push** | Required for **any** detection event to publish |
| Notifications: **Away-Only** | Publishes only while you are detected as away |
| Seen: **Motion** | Required for `CameraMotion.Motion` events |
| Seen: **Person** | Required for `CameraPerson.Person` events |

This is per **camera**, so a doorbell that publishes reliably tells you nothing about
the rest. Away-Only is the cruellest of the four: events arrive normally while you are
out and stop the moment you come home, which reads as an intermittent bug.

If everything above passes and a deliberate trigger still produces nothing, check these
before suspecting anything in Cloud.

## Verifying the Google side without `gcloud`

Confirm the SDM credential and its scope:

```bash
curl -s "https://oauth2.googleapis.com/tokeninfo?access_token=$(
  python3 -c "import json;print(json.load(open('tokens.json'))['access_token'])")"
```

The reported scope should be exactly `https://www.googleapis.com/auth/sdm.service`.
Tokens are short-lived, so refresh first if this reports an invalid token.

Confirm devices are visible:

```bash
./bin/nest-bridge devices
```

Confirm the events credential can pull. A `200` with an empty body means
authentication, authorization, and the subscription are all correct and nothing is
queued — which is the normal state when no camera has seen motion recently:

```text
POST https://pubsub.googleapis.com/v1/projects/<project>/subscriptions/<sub>:pull
{"maxMessages":5,"returnImmediately":true}
```

A `404` means the subscription name is wrong. A `403` on the pull itself means the
IAM grant is missing or on the wrong resource — but remember that a 403 on
*metadata* is expected.

## Quotas

Google enforces these per Device Access project, and the bridge's scheduler is
built around them:

- 10 queries per minute globally
- 5 queries per minute per command per device
- 100 queries per hour per camera or doorbell

Session renewals are prioritised over new connections, because a missed renewal
drops a live stream while a delayed connection only postpones one. Running
`nest-bridge stream` against a camera the deployed bridge is already serving opens
a second session for the same device and consumes double the quota against these
limits.

## Rotation and what must never change

Safe to rotate: the service-account key, and the OAuth client secret (which
invalidates `tokens.json` and requires re-running `nest-bridge auth`).

Effectively permanent: the **Device Access project UUID**, since it appears in
every device path in `config.yaml`, and the **Pub/Sub topic**, which is bound to
that project. Recreating the Device Access project means paying the $5 again and
re-authorizing every device.

## Gotchas, in the order people hit them

1. Using the Cloud project ID as `google.project_id` instead of the Device Access
   UUID.
2. Forgetting to enable events in the Device Access console. Streaming works, so
   this surfaces much later as "no motion events ever arrive".
3. Filling in any App domain field on Branding, which then demands a privacy
   policy URL.
4. A redirect URI that does not match `google.redirect_uri` exactly.
5. Consenting with a Workspace account, or one under Advanced Protection.
6. Pasting the topic path where the subscription path belongs.
7. Widening the service account's role to silence the expected metadata 403.
8. Granting Subscriber at project level rather than on the subscription. It works, so
   nothing ever complains — check 4 above is the only thing that will tell you.
9. Reading an empty subscription as a broken one. It is the normal idle state.
10. Leaving Google Home notifications off, or on Away-Only, for some cameras. Nothing in
    Cloud is wrong; those cameras simply never publish.
