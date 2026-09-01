#!/usr/bin/env node
/**
 * Bolts the ONVIF Events service onto emberstonel/onvif-virtual-camera at image
 * build time.
 *
 * Patch discipline vendored from jwallen2139/onvif-events-bridge (MIT), file
 * src/onvif-events/patch-onvif-server.js, upstream commit
 * 484e4e6c5784ada22cdc44f979451fd37008eca8. The anchors themselves are ours:
 * upstream patches daniela-hase/onvif-server, a single src/onvif-server.js,
 * whereas this fork splits handlers into src/services/*.js and none of the
 * upstream anchors apply. See deploy/onvif/NOTES.md.
 *
 * Every edit asserts its anchor occurs exactly once and exits non-zero
 * otherwise. A patch that silently no-ops yields an image with no event support
 * and an adoption failure that surfaces hours later with no clue why.
 *
 * Usage: node patch-onvif-server.js /app/src
 */

const fs = require("fs");
const path = require("path");

const srcDir = process.argv[2];
if (!srcDir) {
    console.error("usage: patch-onvif-server.js <path to /app/src>");
    process.exit(1);
}

function fail(label, message) {
    console.error(`PATCH FAILED: anchor "${label}": ${message}`);
    console.error("The emberstonel onvif-virtual-camera source has changed shape; " +
        "re-derive the anchors and update deploy/onvif/NOTES.md.");
    process.exit(1);
}

function patch(file, edits) {
    const target = path.join(srcDir, file);
    let s;
    try {
        s = fs.readFileSync(target, "utf8");
    } catch (err) {
        console.error(`PATCH FAILED: cannot read ${target}: ${err.message}`);
        process.exit(1);
    }

    const before = s.length;
    for (const [label, needle, replacement] of edits) {
        const n = s.split(needle).length - 1;
        if (n !== 1) {
            fail(label, `matched ${n} times (expected exactly 1)`);
        }
        s = s.replace(needle, replacement);
    }

    fs.writeFileSync(target, s, "utf8");
    console.log(`  ${file}: ${before} -> ${s.length} bytes, ${edits.length} edit(s)`);
}

const EVENTS_URL = "`http://${network.ip}:${onvifPort}/onvif/events_service`";

// Anchor 1. Built from the camera's own address alongside the other endpoints,
// so the advertised XAddr is per-camera by construction and GetCapabilities and
// GetServices cannot disagree about it.
patch("camera-manager.js", [[
    "endpoint URLs",
    "                mediaServiceUrl: `http://${network.ip}:${onvifPort}/onvif/media_service`,",
    "                mediaServiceUrl: `http://${network.ip}:${onvifPort}/onvif/media_service`,\n" +
    `                eventsServiceUrl: ${EVENTS_URL},`
]]);

// Anchors 2 and 3, plus the capability builder they share.
patch("services/device-service.js", [
    [
        "buildEventsCapabilities insertion point",
        "    // ONVIF: GetDeviceInformation",
        `    buildEventsCapabilities() {
        return {
            XAddr: this.camera.endpoints.eventsServiceUrl,
            WSSubscriptionPolicySupport: false,
            WSPullPointSupport: true,
            WSPausableSubscriptionManagerInterfaceSupport: false
        };
    }

    // ONVIF: GetDeviceInformation`
    ],
    [
        "GetCapabilities category gate",
        '        const includeMedia = allRequested || requested.includes("Media");',
        '        const includeMedia = allRequested || requested.includes("Media");\n' +
        '        const includeEvents = allRequested || requested.includes("Events");'
    ],
    [
        "GetCapabilities Events section",
        `        if (includeMedia) {
            capabilities.Media = this.buildMediaCapabilities();
        }`,
        `        if (includeMedia) {
            capabilities.Media = this.buildMediaCapabilities();
        }

        if (includeEvents) {
            capabilities.Events = this.buildEventsCapabilities();
        }`
    ],
    [
        // Appended, never inserted: IncludeCapability indexes this array
        // positionally as services[0] and services[1].
        "GetServices service array",
        `            {
                Namespace: "http://www.onvif.org/ver10/media/wsdl",
                XAddr: this.camera.endpoints.mediaServiceUrl,
                Version: {
                    Major: 2,
                    Minor: 5
                }
            }
        ];`,
        `            {
                Namespace: "http://www.onvif.org/ver10/media/wsdl",
                XAddr: this.camera.endpoints.mediaServiceUrl,
                Version: {
                    Major: 2,
                    Minor: 5
                }
            },
            {
                Namespace: "http://www.onvif.org/ver10/events/wsdl",
                XAddr: this.camera.endpoints.eventsServiceUrl,
                Version: {
                    Major: 2,
                    Minor: 5
                }
            }
        ];`
    ]
]);

// Anchor 4, plus the require and construction the routing depends on. The route
// goes after the device_service/media_service guard, which returns without
// responding so that the soap.listen handlers attached later can answer; ahead
// of it the SOAP services would break. The snapshot check stays first.
patch("onvif-server.js", [
    [
        "require block",
        'const SnapshotService = require("./services/snapshot-service");',
        'const SnapshotService = require("./services/snapshot-service");\n' +
        'const EventsService = require("./events-service");'
    ],
    [
        "service construction",
        "        this.snapshotService = new SnapshotService(camera);",
        "        this.snapshotService = new SnapshotService(camera);\n" +
        "        this.eventsService = new EventsService(camera);"
    ],
    [
        "404 fallback",
        `                res.statusCode = 404;
                res.end("Not Found");`,
        `                if (this.eventsService.canHandleRequest(req)) {
                    this.eventsService.handleRequest(req, res);
                    return;
                }

                res.statusCode = 404;
                res.end("Not Found");`
    ]
]);

// ---------------------------------------------------------------------------
// Audio. The stock image has no audio support at all: "audio" appears nowhere
// in /app/src outside the WSDL directory, so a media profile advertises only a
// VideoEncoderConfiguration and Protect never asks for the audio track that the
// RTSP renditions already carry. Everything below teaches the media service to
// advertise an AAC audio source and encoder, per camera, when the generated
// config says that camera has audio.
// ---------------------------------------------------------------------------

// Anchor 5. The config loader silently drops unknown YAML keys, so audio_hq and
// audio_lq have to be parsed explicitly or they never reach the media service.
// Validation is strict and eager: a typo that left audio off would otherwise
// present as Protect playing silence, with nothing anywhere saying why.
patch("config-loader.js", [
    [
        "audio normalisers",
        "function normalizeIdentity(cam) {",
        `// ONVIF Media1 constrains AudioEncoderConfiguration/Encoding to this enum.
const AUDIO_ENCODINGS = ["G711", "G726", "AAC"];

function normalizeAudioStream(audio, label) {
    if (typeof audio !== "object" || audio === null) {
        throw new Error(\`\${label} must be a mapping.\`);
    }

    const encoding = normalizeOptionalString(audio.encoding, \`\${label}.encoding\`);
    if (!AUDIO_ENCODINGS.includes(encoding)) {
        throw new Error(\`\${label}.encoding must be one of \${AUDIO_ENCODINGS.join(", ")}.\`);
    }

    const normalized = {
        encoding,
        sampleRate: normalizePositiveInteger(audio.sample_rate, \`\${label}.sample_rate\`),
        channels: normalizePositiveInteger(audio.channels, \`\${label}.channels\`),
        bitrate: normalizePositiveInteger(audio.bitrate, \`\${label}.bitrate\`)
    };

    const missingKeys = ["sampleRate", "channels", "bitrate"]
        .filter((key) => normalized[key] === undefined);
    if (missingKeys.length > 0) {
        throw new Error(\`\${label} config is missing required field(s): \${missingKeys.join(", ")}.\`);
    }

    return normalized;
}

// Returns null for a camera with no audio. That null is what keeps a silent
// camera from advertising an audio track Protect would then wait on forever.
function normalizeAudioConfig(cam, label) {
    const hasHq = cam.audio_hq !== undefined && cam.audio_hq !== null;
    const hasLq = cam.audio_lq !== undefined && cam.audio_lq !== null;

    if (!hasHq && !hasLq) {
        return null;
    }

    if (hasHq !== hasLq) {
        throw new Error(\`\${label} must configure both audio_hq and audio_lq, or neither.\`);
    }

    return {
        hq: normalizeAudioStream(cam.audio_hq, \`\${label}.audio_hq\`),
        lq: normalizeAudioStream(cam.audio_lq, \`\${label}.audio_lq\`)
    };
}

function normalizeIdentity(cam) {`
    ],
    [
        "camera audio field",
        "            identity: normalizeIdentity(cam),",
        "            identity: normalizeIdentity(cam),\n" +
        "            audio: normalizeAudioConfig(cam, `virtual_camera '${cam.name}'`),"
    ],
    [
        "camera audio validation",
        "    normalizeIdentity(cam);",
        "    normalizeIdentity(cam);\n" +
        "    normalizeAudioConfig(cam, `virtual_camera '${cam.name}'`);"
    ]
]);

// Anchors 6-11. Audio tokens mirror the video ones exactly: one shared source
// token per camera, and a per-rendition source-config and encoder-config token.
patch("services/media-service.js", [
    [
        "audio token construction",
        "        this.videoEncoderTokenLq = `video_encoder_lq_${tokenSuffix}`;",
        "        this.videoEncoderTokenLq = `video_encoder_lq_${tokenSuffix}`;\n" +
        "        this.audioSourceToken = `audio_source_${tokenSuffix}`;\n" +
        "        this.audioSourceConfigTokenHq = `audio_source_config_hq_${tokenSuffix}`;\n" +
        "        this.audioSourceConfigTokenLq = `audio_source_config_lq_${tokenSuffix}`;\n" +
        "        this.audioEncoderTokenHq = `audio_encoder_hq_${tokenSuffix}`;\n" +
        "        this.audioEncoderTokenLq = `audio_encoder_lq_${tokenSuffix}`;"
    ],
    [
        "audio config name construction",
        "        this.videoEncoderConfigNameLq = `VideoEncoderConfig_LQ_${tokenSuffix}`;",
        "        this.videoEncoderConfigNameLq = `VideoEncoderConfig_LQ_${tokenSuffix}`;\n" +
        "        this.audioSourceConfigNameHq = `AudioSourceConfig_HQ_${tokenSuffix}`;\n" +
        "        this.audioSourceConfigNameLq = `AudioSourceConfig_LQ_${tokenSuffix}`;\n" +
        "        this.audioEncoderConfigNameHq = `AudioEncoderConfig_HQ_${tokenSuffix}`;\n" +
        "        this.audioEncoderConfigNameLq = `AudioEncoderConfig_LQ_${tokenSuffix}`;"
    ],
    [
        // camera.audio is null for a camera without audio, and every audio
        // builder keys off that, so gating happens in exactly one place.
        "LQ profile definition audio",
        `                stream: this.camera.streams.lq,
                streamUri: this.camera.endpoints.rtspUriLq`,
        `                stream: this.camera.streams.lq,
                streamUri: this.camera.endpoints.rtspUriLq,
                audio: this.camera.audio ? this.camera.audio.lq : null,
                audioSourceConfigToken: this.audioSourceConfigTokenLq,
                audioSourceConfigName: this.audioSourceConfigNameLq,
                audioEncoderToken: this.audioEncoderTokenLq,
                audioEncoderConfigName: this.audioEncoderConfigNameLq`
    ],
    [
        "HQ profile definition audio",
        `            stream: this.camera.streams.hq,
            streamUri: this.camera.endpoints.rtspUriHq`,
        `            stream: this.camera.streams.hq,
            streamUri: this.camera.endpoints.rtspUriHq,
            audio: this.camera.audio ? this.camera.audio.hq : null,
            audioSourceConfigToken: this.audioSourceConfigTokenHq,
            audioSourceConfigName: this.audioSourceConfigNameHq,
            audioEncoderToken: this.audioEncoderTokenHq,
            audioEncoderConfigName: this.audioEncoderConfigNameHq`
    ],
    [
        // tt:Profile is an xs:sequence, so AudioSourceConfiguration belongs
        // between the two video elements, not appended after them. node-soap
        // serialises in object key order, so insertion order is the wire order.
        //
        // GetVideoEncoderConfiguration opens an identically indented
        // VideoEncoderConfiguration block, so the anchor reaches back into the
        // preceding VideoSourceConfiguration to stay unique to buildProfile.
        "profile AudioSourceConfiguration",
        `                SourceToken: this.videoSourceToken
            },
            VideoEncoderConfiguration: {`,
        `                SourceToken: this.videoSourceToken
            },
            ...this.buildAudioSourceConfiguration(profile),
            VideoEncoderConfiguration: {`
    ],
    [
        "profile AudioEncoderConfiguration",
        `            }
        };
    }

    // ONVIF: GetProfiles`,
        `            },
            ...this.buildAudioEncoderConfiguration(profile)
        };
    }

    // ONVIF: GetProfiles`
    ],
    [
        // Each builder returns a spreadable fragment: the whole element vanishes
        // for a camera with no audio rather than appearing empty.
        "audio builders and operations",
        "    // ONVIF: GetVideoSources",
        `    hasAudio() {
        return !!this.camera.audio;
    }

    buildAudioSourceConfiguration(profile) {
        if (!profile.audio) {
            return {};
        }

        return {
            AudioSourceConfiguration: {
                $attributes: {
                    token: profile.audioSourceConfigToken
                },
                Name: profile.audioSourceConfigName,
                UseCount: 1,
                SourceToken: this.audioSourceToken
            }
        };
    }

    buildAudioEncoderConfiguration(profile) {
        if (!profile.audio) {
            return {};
        }

        return {
            AudioEncoderConfiguration: {
                $attributes: {
                    token: profile.audioEncoderToken
                },
                Name: profile.audioEncoderConfigName,
                UseCount: 1,
                Encoding: profile.audio.encoding,
                Bitrate: profile.audio.bitrate,
                // ONVIF states SampleRate in kHz; the config carries Hz.
                SampleRate: Math.round(profile.audio.sampleRate / 1000)
            }
        };
    }

    audioProfileDefinitions() {
        if (!this.hasAudio()) {
            return [];
        }

        return [
            this.getProfileDefinitionByToken(this.profileTokenHq),
            this.getProfileDefinitionByToken(this.profileTokenLq)
        ];
    }

    getAudioProfileDefinitionByConfigurationToken(token) {
        if (token === this.audioSourceConfigTokenLq || token === this.audioEncoderTokenLq) {
            return this.getProfileDefinitionByToken(this.profileTokenLq);
        }

        return this.getProfileDefinitionByToken(this.profileTokenHq);
    }

    // ONVIF: GetAudioSources
    async GetAudioSources() {
        if (!this.hasAudio()) {
            return { AudioSources: [] };
        }

        return {
            AudioSources: [
                {
                    $attributes: {
                        token: this.audioSourceToken
                    },
                    Channels: this.camera.audio.hq.channels
                }
            ]
        };
    }

    // ONVIF: GetAudioSourceConfigurations
    async GetAudioSourceConfigurations() {
        return {
            Configurations: this.audioProfileDefinitions()
                .map((profile) => this.buildAudioSourceConfiguration(profile).AudioSourceConfiguration)
        };
    }

    // ONVIF: GetAudioEncoderConfigurations
    async GetAudioEncoderConfigurations() {
        return {
            Configurations: this.audioProfileDefinitions()
                .map((profile) => this.buildAudioEncoderConfiguration(profile).AudioEncoderConfiguration)
        };
    }

    // ONVIF: GetAudioSourceConfiguration
    async GetAudioSourceConfiguration(args) {
        const profile = this.getAudioProfileDefinitionByConfigurationToken(args && args.ConfigurationToken);

        logger.debug("media",
            \`GetAudioSourceConfiguration called for \${this.camera.name} \` +
            \`(ConfigurationToken=\${args && args.ConfigurationToken}, kind=\${profile.kind})\`
        );

        const configuration = this.buildAudioSourceConfiguration(profile).AudioSourceConfiguration;
        return configuration ? { Configuration: configuration } : {};
    }

    // ONVIF: GetAudioEncoderConfiguration
    async GetAudioEncoderConfiguration(args) {
        const profile = this.getAudioProfileDefinitionByConfigurationToken(args && args.ConfigurationToken);

        logger.debug("media",
            \`GetAudioEncoderConfiguration called for \${this.camera.name} \` +
            \`(ConfigurationToken=\${args && args.ConfigurationToken}, kind=\${profile.kind})\`
        );

        const configuration = this.buildAudioEncoderConfiguration(profile).AudioEncoderConfiguration;
        return configuration ? { Configuration: configuration } : {};
    }

    // ONVIF: GetAudioEncoderConfigurationOptions
    async GetAudioEncoderConfigurationOptions(args) {
        const profile = this.getAudioProfileDefinitionByConfigurationToken(args && args.ConfigurationToken);

        if (!profile.audio) {
            return { Options: {} };
        }

        // The transcode is fixed, so the only supported option is the one
        // already in use. Offering a range would invite Protect to request a
        // configuration nothing downstream can actually produce.
        return {
            Options: {
                Options: [
                    {
                        Encoding: profile.audio.encoding,
                        BitrateList: {
                            Items: [profile.audio.bitrate]
                        },
                        SampleRateList: {
                            Items: [Math.round(profile.audio.sampleRate / 1000)]
                        }
                    }
                ]
            }
        };
    }

    // ONVIF: GetVideoSources`
    ],
    [
        "audio service definition entries",
        "            GetVideoEncoderConfiguration: this.GetVideoEncoderConfiguration.bind(this)",
        "            GetVideoEncoderConfiguration: this.GetVideoEncoderConfiguration.bind(this),\n" +
        "            GetAudioSources: this.GetAudioSources.bind(this),\n" +
        "            GetAudioSourceConfigurations: this.GetAudioSourceConfigurations.bind(this),\n" +
        "            GetAudioEncoderConfigurations: this.GetAudioEncoderConfigurations.bind(this),\n" +
        "            GetAudioSourceConfiguration: this.GetAudioSourceConfiguration.bind(this),\n" +
        "            GetAudioEncoderConfiguration: this.GetAudioEncoderConfiguration.bind(this),\n" +
        "            GetAudioEncoderConfigurationOptions: this.GetAudioEncoderConfigurationOptions.bind(this)"
    ]
]);

// Anchors 12-13. The image ships a hand-trimmed types.xsd, not the real ONVIF
// schema, so none of the audio types exist. node-soap serialises from the WSDL:
// an element with no type declared here is dropped from the response, which is
// the failure mode that looks exactly like the patch not having applied.
patch("wsdl/types.xsd", [
    [
        "audio schema types",
        '    <xs:complexType name="Profile">',
        `    <xs:complexType name="AudioSource">
        <xs:sequence>
            <xs:element name="Channels" type="xs:int"/>
        </xs:sequence>
        <xs:attribute name="token" type="tt:ReferenceToken" use="optional"/>
    </xs:complexType>

    <xs:complexType name="AudioSourceConfiguration">
        <xs:sequence>
            <xs:element name="Name" type="xs:string" minOccurs="0"/>
            <xs:element name="UseCount" type="xs:int" minOccurs="0"/>
            <xs:element name="SourceToken" type="tt:ReferenceToken" minOccurs="0"/>
        </xs:sequence>
        <xs:attribute name="token" type="tt:ReferenceToken" use="optional"/>
    </xs:complexType>

    <xs:complexType name="AudioEncoderConfiguration">
        <xs:sequence>
            <xs:element name="Name" type="xs:string" minOccurs="0"/>
            <xs:element name="UseCount" type="xs:int" minOccurs="0"/>
            <xs:element name="Encoding" type="xs:string" minOccurs="0"/>
            <xs:element name="Bitrate" type="xs:int" minOccurs="0"/>
            <xs:element name="SampleRate" type="xs:int" minOccurs="0"/>
        </xs:sequence>
        <xs:attribute name="token" type="tt:ReferenceToken" use="optional"/>
    </xs:complexType>

    <xs:complexType name="IntList">
        <xs:sequence>
            <xs:element name="Items" type="xs:int" minOccurs="0" maxOccurs="unbounded"/>
        </xs:sequence>
    </xs:complexType>

    <xs:complexType name="AudioEncoderConfigurationOption">
        <xs:sequence>
            <xs:element name="Encoding" type="xs:string"/>
            <xs:element name="BitrateList" type="tt:IntList"/>
            <xs:element name="SampleRateList" type="tt:IntList"/>
        </xs:sequence>
    </xs:complexType>

    <xs:complexType name="AudioEncoderConfigurationOptions">
        <xs:sequence>
            <xs:element name="Options" type="tt:AudioEncoderConfigurationOption" minOccurs="0" maxOccurs="unbounded"/>
        </xs:sequence>
    </xs:complexType>

    <xs:complexType name="Profile">`
    ],
    [
        // Sequence order is the ONVIF one; the audio elements interleave with
        // the video elements rather than following them.
        "Profile audio elements",
        `            <xs:element name="VideoSourceConfiguration" type="tt:VideoSourceConfiguration" minOccurs="0"/>
            <xs:element name="VideoEncoderConfiguration" type="tt:VideoEncoderConfiguration" minOccurs="0"/>`,
        `            <xs:element name="VideoSourceConfiguration" type="tt:VideoSourceConfiguration" minOccurs="0"/>
            <xs:element name="AudioSourceConfiguration" type="tt:AudioSourceConfiguration" minOccurs="0"/>
            <xs:element name="VideoEncoderConfiguration" type="tt:VideoEncoderConfiguration" minOccurs="0"/>
            <xs:element name="AudioEncoderConfiguration" type="tt:AudioEncoderConfiguration" minOccurs="0"/>`
    ]
]);

// Anchors 14-17. soap.listen only answers operations present in the WSDL, so
// each audio operation needs an element pair, a message pair, a portType entry
// and a binding entry. The four sections are generated from one list to keep
// them from drifting out of step with each other.
const AUDIO_OPERATIONS = [
    "GetAudioSources",
    "GetAudioSourceConfigurations",
    "GetAudioEncoderConfigurations",
    "GetAudioSourceConfiguration",
    "GetAudioEncoderConfiguration",
    "GetAudioEncoderConfigurationOptions"
];

const MEDIA_NS = "http://www.onvif.org/ver10/media/wsdl";

const audioElements = `            <xs:element name="GetAudioSources">
                <xs:complexType/>
            </xs:element>
            <xs:element name="GetAudioSourcesResponse">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="AudioSources" type="tt:AudioSource" minOccurs="0" maxOccurs="unbounded"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
            <xs:element name="GetAudioSourceConfigurations">
                <xs:complexType/>
            </xs:element>
            <xs:element name="GetAudioSourceConfigurationsResponse">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="Configurations" type="tt:AudioSourceConfiguration" minOccurs="0" maxOccurs="unbounded"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
            <xs:element name="GetAudioEncoderConfigurations">
                <xs:complexType/>
            </xs:element>
            <xs:element name="GetAudioEncoderConfigurationsResponse">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="Configurations" type="tt:AudioEncoderConfiguration" minOccurs="0" maxOccurs="unbounded"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
            <xs:element name="GetAudioSourceConfiguration">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="ConfigurationToken" type="tt:ReferenceToken" minOccurs="0"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
            <xs:element name="GetAudioSourceConfigurationResponse">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="Configuration" type="tt:AudioSourceConfiguration" minOccurs="0"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
            <xs:element name="GetAudioEncoderConfiguration">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="ConfigurationToken" type="tt:ReferenceToken" minOccurs="0"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
            <xs:element name="GetAudioEncoderConfigurationResponse">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="Configuration" type="tt:AudioEncoderConfiguration" minOccurs="0"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
            <xs:element name="GetAudioEncoderConfigurationOptions">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="ConfigurationToken" type="tt:ReferenceToken" minOccurs="0"/>
                        <xs:element name="ProfileToken" type="tt:ReferenceToken" minOccurs="0"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
            <xs:element name="GetAudioEncoderConfigurationOptionsResponse">
                <xs:complexType>
                    <xs:sequence>
                        <xs:element name="Options" type="tt:AudioEncoderConfigurationOptions" minOccurs="0"/>
                    </xs:sequence>
                </xs:complexType>
            </xs:element>
`;

const audioMessages = AUDIO_OPERATIONS.map((op) => `    <wsdl:message name="${op}Request">
        <wsdl:part name="parameters" element="trt:${op}"/>
    </wsdl:message>
    <wsdl:message name="${op}Response">
        <wsdl:part name="parameters" element="trt:${op}Response"/>
    </wsdl:message>
`).join("");

const audioPortTypeOperations = AUDIO_OPERATIONS.map((op) => `        <wsdl:operation name="${op}">
            <wsdl:input message="trt:${op}Request"/>
            <wsdl:output message="trt:${op}Response"/>
        </wsdl:operation>
`).join("");

const audioBindingOperations = AUDIO_OPERATIONS.map((op) => `        <wsdl:operation name="${op}">
            <soap:operation soapAction="${MEDIA_NS}/${op}"/>
            <wsdl:input>
                <soap:body use="literal"/>
            </wsdl:input>
            <wsdl:output>
                <soap:body use="literal"/>
            </wsdl:output>
        </wsdl:operation>
`).join("");

patch("wsdl/media_service.wsdl", [
    ["audio schema elements", "        </xs:schema>", audioElements + "        </xs:schema>"],
    ["audio messages", '    <wsdl:portType name="MediaPort">', audioMessages + '\n    <wsdl:portType name="MediaPort">'],
    ["audio portType operations", "    </wsdl:portType>", audioPortTypeOperations + "    </wsdl:portType>"],
    ["audio binding operations", "    </wsdl:binding>", audioBindingOperations + "    </wsdl:binding>"]
]);

// node --check cannot see the WSDL, and node-soap answers only operations the
// WSDL declares while silently dropping elements whose type it could not
// resolve. Either mistake yields a container that starts cleanly and serves
// audio-free profiles, which looks exactly like the patch never having run.
// Loading the WSDL through the same library the server uses, and asserting the
// operations it exposes, turns both into build failures.
function verifyMediaWsdl() {
    const soap = require(path.join(srcDir, "..", "node_modules", "soap"));
    const wsdlPath = path.join(srcDir, "wsdl", "media_service.wsdl");

    soap.createClient(wsdlPath, {}, (err, client) => {
        if (err) {
            console.error(`WSDL FAILED: ${wsdlPath} does not parse: ${err.message}`);
            process.exit(1);
        }

        const exposed = Object.keys(client.describe().MediaService.MediaPort);
        const missing = AUDIO_OPERATIONS.filter((op) => !exposed.includes(op));
        if (missing.length > 0) {
            console.error(`WSDL FAILED: media service does not expose ${missing.join(", ")}`);
            process.exit(1);
        }

        console.log(`  wsdl/media_service.wsdl: exposes ${exposed.length} operations, ` +
            `including ${AUDIO_OPERATIONS.length} audio operation(s)`);
        console.log("PATCH OK: ONVIF Events service enabled; media profiles advertise AAC audio");
    });
}

verifyMediaWsdl();
