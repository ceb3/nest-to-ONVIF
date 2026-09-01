# Patching `emberstonel/onvif-virtual-camera` for ONVIF Events

Findings from Task 1 of `docs/superpowers/plans/2026-08-30-motion-events.md`, taken
from the image running on the deployment host on 2026-08-30.

The prior art ([jwallen2139/onvif-events-bridge](https://github.com/jwallen2139/onvif-events-bridge),
MIT) patches `daniela-hase/onvif-server`, which is a single `src/onvif-server.js`.
Our image is the `emberstonel` fork, which splits handlers into
`src/services/*.js`. None of the upstream patch anchors apply verbatim. The
replacements below were located instead.

## The fork is structurally easier to patch, not harder

The risk the plan flagged — that one process serves several cameras and the
router might not know which camera a request arrived for — does not exist here.

`OnvifServer` is instantiated **once per camera** (`camera-manager.js`), holds
that camera as `this.camera`, and binds its own HTTP server to that camera's own
address:

```js
server.listen(this.camera.onvifPort, this.camera.ip, async () => {
```

So per-camera identity is inherent in the instance. The events service can be
constructed per `OnvifServer` and needs no keying, routing table, or lookup. This
is simpler than the upstream arrangement the prior art had to work around.

## Anchors

### 1. `src/camera-manager.js:57` — endpoint URLs

Endpoints are built from the camera's own address, so the events URL is derived
the same way and is automatically correct per camera:

```js
deviceServiceUrl: `http://${network.ip}:${onvifPort}/onvif/device_service`,
```

Add `eventsServiceUrl` alongside it. Building the `XAddr` here rather than at the
point of advertisement keeps it consistent between `GetCapabilities` and
`GetServices`, which must agree.

### 2. `src/services/device-service.js:111` — `GetCapabilities`

Already normalises `Category` into a `requested` array and gates each section on
it. An events section follows the existing `includeMedia` shape exactly:

```js
if (includeMedia) {
    capabilities.Media = this.buildMediaCapabilities();
}
```

The advertised capability must set `WSPullPointSupport: true`; that is the part
Protect acts on.

### 3. `src/services/device-service.js:153` — `GetServices`

A literal array of two services. A third entry is appended with namespace
`http://www.onvif.org/ver10/events/wsdl`. Note that `IncludeCapability` indexes
the array positionally (`services[0]`, `services[1]`), so appending is safe but
inserting is not.

### 4. `src/onvif-server.js:210` — the HTTP router

The request handler ends in a 404 fallback:

```js
res.statusCode = 404;
res.end("Not Found");
```

`/onvif/events_service` and `/trigger/motion` are routed immediately before it.
Two ordering constraints:

- The snapshot service is checked first and must stay first.
- Requests to `/onvif/device_service` and `/onvif/media_service` deliberately
  `return` without responding, leaving them to the `soap.listen` handlers
  attached later. Our routes must come after that guard, not before it, or SOAP
  breaks.

## Why the events service is not a third `soap.listen`

The device and media services are served by `soap.listen` from generated WSDL.
The events service is not, and should not be: WS-BaseNotification is awkward to
express through `soap`, and the prior art's `events-service.js` already answers
the five operations Protect actually calls by handling the raw request and
writing the SOAP envelope itself. Routing it from the plain HTTP handler is both
less code and closer to a proven implementation.

## Audio anchors

Added 2026-08-30. The media already carried AAC: both Protect-facing renditions
probe as `h264` plus `aac 48000 2`, and VLC plays the sound. Protect did not,
because a `GetProfiles` response contained four `VideoEncoderConfiguration` and
zero audio configurations, so Protect only ever asked for video. The stock image
has no audio support whatever — `grep -ci audio /app/src/services/media-service.js`
returns 0, and outside `src/wsdl` the word does not appear in `/app/src`.

Audio is advertised per camera, gated on the generated config. Advertising it on
a camera that sends no audio track would leave Protect waiting for a stream that
never arrives, which is worse than silence, so the gate is the point of the
change rather than a detail of it.

### 5. `src/config-loader.js` — `audio_hq` / `audio_lq`

Unknown YAML keys are silently dropped, so the generator's audio section reaches
nothing unless the loader is taught to read it. Three edits: the
`normalizeAudioConfig` / `normalizeAudioStream` pair inserted ahead of
`normalizeIdentity`, an `audio:` field on the camera object, and a call from
`validateVirtualCamera` so a malformed section fails at load rather than
presenting as silence in Protect. `normalizeAudioConfig` returns `null` for a
camera with no audio, and that null is the single gate everything else keys off.

`camera-manager.js` needs no edit: `createCameraRuntime` spreads
`...this.cameraConfig`, so `audio` reaches the runtime camera by construction.

### 6-11. `src/services/media-service.js`

Audio tokens mirror the video ones exactly — one `audio_source_*` per camera,
plus per-rendition source-config and encoder-config tokens — so the two halves of
a profile are shaped alike.

Two anchors sit inside `buildProfile`. `tt:Profile` is an `xs:sequence` and
node-soap serialises in object key order, so `AudioSourceConfiguration` must be
inserted *between* the two video elements, not appended:

```
Name, VideoSourceConfiguration, AudioSourceConfiguration,
VideoEncoderConfiguration, AudioEncoderConfiguration
```

**One anchor needed adjusting.** `GetVideoEncoderConfiguration` opens a
`VideoEncoderConfiguration` block at exactly the same indentation as
`buildProfile` does, so the obvious anchor matched twice and the build failed as
designed. It now reaches back into the preceding `SourceToken:
this.videoSourceToken` line to stay unique to `buildProfile`. The identical trick
is why the `AudioEncoderConfiguration` anchor carries the trailing
`// ONVIF: GetProfiles` comment: the two `RateControl` blocks are byte-identical
and only their successors differ.

Each builder returns a spreadable fragment (`{}` when the camera has no audio)
rather than a nullable element, so the element disappears entirely instead of
being emitted empty.

Six operations are added: `GetAudioSources`, `GetAudioSourceConfigurations`,
`GetAudioEncoderConfigurations`, `GetAudioSourceConfiguration`,
`GetAudioEncoderConfiguration`, `GetAudioEncoderConfigurationOptions`. The
options response offers only the configuration already in use, since the
transcode is fixed and offering a range would invite Protect to request
something nothing downstream can produce.

### 12-13. `src/wsdl/types.xsd`

The image ships a hand-trimmed schema, not the real ONVIF one, so none of the
audio types exist. node-soap drops an element whose type it cannot resolve
*silently*, which looks exactly like the patch not having applied. Added
`AudioSource`, `AudioSourceConfiguration`, `AudioEncoderConfiguration`, `IntList`,
`AudioEncoderConfigurationOption` and `AudioEncoderConfigurationOptions`, and
interleaved the two audio elements into the `Profile` sequence.

`AudioEncoderConfiguration/SampleRate` is in kHz per ONVIF, while the generated
config carries Hz — the unit the transcode and every ffprobe reading use. The
media service divides. `Encoding` is `AAC`; the Media1 enum admits only G711,
G726 and AAC.

`Multicast` and `SessionTimeout` are omitted, matching what the stock
`VideoEncoderConfiguration` already does and Protect already accepts.

### 14-17. `src/wsdl/media_service.wsdl`

`soap.listen` answers only operations the WSDL declares, so each audio operation
needs an element pair, a message pair, a portType entry and a binding entry. The
last three are generated from a single `AUDIO_OPERATIONS` list in the patch
script so they cannot drift out of step.

### Build-time WSDL verification

`node --check` cannot see XML. The patch script therefore ends by loading the
patched WSDL through the same node-soap the server uses and asserting the audio
operations are exposed, which turns both a malformed schema and a forgotten
binding into build failures:

```
  wsdl/media_service.wsdl: exposes 12 operations, including 6 audio operation(s)
PATCH OK: ONVIF Events service enabled; media profiles advertise AAC audio
```

### Verifying the gate

The live deployment has audio on all four cameras, so it no longer contains a
control camera. The gate is covered by unit test in `internal/onvif` instead —
`TestGenerateEmitsAudioOnlyForCamerasThatHaveIt`, whose golden file deliberately
keeps one camera with audio and one without regardless of what `config.yaml`
does. The container half can be exercised directly:

```bash
docker exec -w /app onvif node -e 'global.runtime={enable_debug_logs:false};
  const M=require("/app/src/services/media-service");
  const s={encoding:"H264",width:1,height:1,framerate:1,bitrate:1,quality:1};
  const mk=(a)=>new M({name:"t",identity:{serialNumber:"AA"},audio:a,
    streams:{hq:s,lq:s},endpoints:{rtspUriHq:"a",rtspUriLq:"b"}});
  mk(null).GetProfiles().then((p)=>console.log(Object.keys(p.Profiles[0]).join(",")))'
```

A camera with no audio must print only
`$attributes,Name,VideoSourceConfiguration,VideoEncoderConfiguration`.

### Audio advertisement

```bash
curl -s -X POST http://192.168.1.8/onvif/media_service \
  -H 'Content-Type: application/soap+xml' \
  -d '<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/></s:Body></s:Envelope>'
```

Verified 2026-08-30 on `192.168.1.8` and `192.168.1.9`, HQ at 64 kbps and LQ at
32 kbps:

```
<trt:AudioSourceConfiguration token="audio_source_config_hq_024e53540001">
  <trt:SourceToken>audio_source_024e53540001</trt:SourceToken>
<trt:AudioEncoderConfiguration token="audio_encoder_hq_024e53540001">
  <trt:Encoding>AAC</trt:Encoding><trt:Bitrate>64</trt:Bitrate>
  <trt:SampleRate>48</trt:SampleRate>
```

The `trt:` prefix on children of `tt:Profile` is pre-existing node-soap
behaviour — the stock `VideoEncoderConfiguration` is serialised the same way and
Protect accepts it — so it is left alone rather than corrected.

## Reproducible checks

All four anchors applied unmodified on 2026-08-30 against the digest pinned in
`Dockerfile`. Three further edits were needed that the Task 1 survey did not
enumerate, because they are mechanical rather than a matter of placement: the
`require` and the `new EventsService(camera)` in `onvif-server.js`, and a
`buildEventsCapabilities()` helper in `device-service.js` that `GetCapabilities`
calls. `GetServices` deliberately does not attach `Capabilities` to the third
entry, matching upstream's positional `services[0]`/`services[1]` indexing.

Run these from the deployment host.

### Advertisement

Confirm the advertisement reaches Protect's view of the camera:

```bash
curl -s -X POST http://<camera-ip>/onvif/device_service \
  -H 'Content-Type: application/soap+xml' \
  -d '<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"><Category>All</Category></GetCapabilities></s:Body></s:Envelope>' \
  | grep -i events
```

The Events `XAddr` in the response must name that same camera's IP. If it names
the deployment host or another camera, the endpoint derivation in anchor 1 is
wrong. Verified on 2026-08-30 against `192.168.1.8` and `192.168.1.9`, each
naming its own address:

```
<tds:Events><tds:XAddr>http://192.168.1.8:80/onvif/events_service</tds:XAddr>
  <tds:WSPullPointSupport>true</tds:WSPullPointSupport></tds:Events>
<tds:Events><tds:XAddr>http://192.168.1.9:80/onvif/events_service</tds:XAddr>
  <tds:WSPullPointSupport>true</tds:WSPullPointSupport></tds:Events>
```

`GetServices` must list three namespaces, the third being
`http://www.onvif.org/ver10/events/wsdl`, with the same `XAddr`.

### Pull-point round trip

The check to reach for when Protect later disagrees. `CreatePullPointSubscription`
seeds the queue with the current state, so the first `PullMessages` returns an
`IsMotion=false` message before any trigger; drain it first or the `true` edge
looks like it arrived early.

```bash
E=http://192.168.1.8/onvif/events_service
SOAP='Content-Type: application/soap+xml'

curl -s -X POST "$E" -H "$SOAP" \
  -d '<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><CreatePullPointSubscription xmlns="http://www.onvif.org/ver10/events/wsdl"><InitialTerminationTime>PT60S</InitialTerminationTime></CreatePullPointSubscription></s:Body></s:Envelope>'

PULL='<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><PullMessages xmlns="http://www.onvif.org/ver10/events/wsdl"><Timeout>PT1S</Timeout><MessageLimit>10</MessageLimit></PullMessages></s:Body></s:Envelope>'

curl -s -X POST "$E?sub=sub1" -H "$SOAP" -d "$PULL"   # drains the seeded false
curl -s -X POST 'http://192.168.1.8/trigger/motion?state=on'
curl -s -X POST "$E?sub=sub1" -H "$SOAP" -d "$PULL"   # the true edge
curl -s -X POST 'http://192.168.1.8/trigger/motion?state=off'
```

The subscription reference names the camera, and the trigger reports how many
subscribers it reached:

```
<wsa:Address>http://192.168.1.8:80/onvif/events_service?sub=sub1</wsa:Address>
{"ok":true,"motion":true,"changed":true,"subscribers":1}
```

The message pulled after the trigger:

```
<wsnt:Topic Dialect="http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet">
  tns1:RuleEngine/CellMotionDetector/Motion</wsnt:Topic>
<tt:Message UtcTime="2026-08-30T20:34:01Z" PropertyOperation="Changed">
  <tt:Data><tt:SimpleItem Name="IsMotion" Value="true"/></tt:Data></tt:Message>
```

`changed` is `false` on a repeated trigger of the same state and nothing is
queued, so the caller may be as chatty as it likes.

### Per-camera isolation

The check that would catch a regression to shared module state. Subscribe on
`192.168.1.9` while `192.168.1.8` is held on; the seeded message must read
`false`, not `true`. Verified 2026-08-30.

### Streaming is unaffected

The events service is additive, but the router edit sits in the same handler as
the snapshot service, so confirm both after any rebuild:

```bash
curl -s -o /tmp/s.jpg -w '%{http_code} %{content_type} %{size_download}\n' \
  http://192.168.1.8/cam-front-doorbell.jpg
ffprobe -v error -rtsp_transport tcp -select_streams v:0 \
  -show_entries stream=codec_name,width,height -of csv=p=0 \
  rtsp://192.168.1.8:8554/cam-front-doorbell-hq
```

On 2026-08-30 all four cameras returned a `200 image/jpeg` of roughly 50 kB and
probed as `h264` — `960x1280` for `Front doorbell`, `1920x1080` for the other
three. `GetCapabilities` still carries the Media `XAddr`.
