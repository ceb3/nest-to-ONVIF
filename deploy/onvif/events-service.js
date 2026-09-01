/**
 * ONVIF Events service for an emberstonel/onvif-virtual-camera virtual camera.
 *
 * Vendored from jwallen2139/onvif-events-bridge (MIT), file
 * src/onvif-events/events-service.js, upstream commit
 * 484e4e6c5784ada22cdc44f979451fd37008eca8.
 *
 * MIT License. Copyright (c) jwallen2139. Permission is hereby granted, free of
 * charge, to any person obtaining a copy of this software and associated
 * documentation files (the "Software"), to deal in the Software without
 * restriction, including without limitation the rights to use, copy, modify,
 * merge, publish, distribute, sublicense, and/or sell copies of the Software,
 * and to permit persons to whom the Software is furnished to do so, subject to
 * the following conditions: the above copyright notice and this permission
 * notice shall be included in all copies or substantial portions of the
 * Software. THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
 * EXPRESS OR IMPLIED.
 *
 * Adapted from the upstream module-level singleton to a class. Upstream patches
 * daniela-hase/onvif-server, where one process serves every camera from shared
 * module state; this fork instantiates one OnvifServer per camera bound to that
 * camera's own address, so each needs its own subscriptions and motion level or
 * a trigger on one camera would surface on all four.
 *
 * The upstream MQTT bridge is dropped: the Go bridge POSTs to /trigger/motion.
 *
 * HTTP surface, on the camera's own address and ONVIF port:
 *
 *   POST /onvif/events_service
 *        GetEventProperties | CreatePullPointSubscription | PullMessages |
 *        Renew | Unsubscribe | GetServiceCapabilities
 *
 *   GET|POST /trigger/motion?state=on|off
 *
 * Responses are hand-written SOAP rather than a third soap.listen: the soap
 * library needs a WSDL, and WS-BaseNotification is awkward to express through
 * it for the five operations Protect actually calls.
 *
 * Environment:
 *   ONVIF_MOTION_TOPIC     optional — if set, emit only this topic (legacy)
 *   ONVIF_SUB_TIMEOUT_SECS default 60 - subscription lifetime advertised
 */

const url = require("url");
const logger = require("./log-manager");

const VIDEO_SOURCE_TOKEN = "video_source_config";

// Standard ONVIF motion topic names emitted on each state change.
const MOTION_TOPICS = [
    {
        path: "tns1:VideoSource/MotionAlarm",
        kind: "motionAlarm",
    },
    {
        path: "tns1:RuleEngine/CellMotionDetector/Motion",
        kind: "ruleEngine",
        ruleName: "CellMotionDetector",
    },
    {
        path: "tns1:RuleEngine/MotionRegionDetector/Motion",
        kind: "ruleEngine",
        ruleName: "MotionRegionDetector",
    },
];

const SUB_TIMEOUT_SECS = parseInt(process.env.ONVIF_SUB_TIMEOUT_SECS || "60", 10);
const SUB_TIMEOUT_MS = SUB_TIMEOUT_SECS * 1000;

const NS =
    'xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope" ' +
    'xmlns:wsa="http://www.w3.org/2005/08/addressing" ' +
    'xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2" ' +
    'xmlns:wstop="http://docs.oasis-open.org/wsn/t-1" ' +
    'xmlns:tev="http://www.onvif.org/ver10/events/wsdl" ' +
    'xmlns:tt="http://www.onvif.org/ver10/schema" ' +
    'xmlns:tns1="http://www.onvif.org/ver10/topics" ' +
    'xmlns:xs="http://www.w3.org/2001/XMLSchema"';

function activeTopics() {
    const custom = process.env.ONVIF_MOTION_TOPIC;
    if (custom) {
        return [{ path: custom, kind: "ruleEngine", ruleName: "MotionDetector" }];
    }
    return MOTION_TOPICS;
}

function iso(d) {
    return new Date(d).toISOString().replace(/\.\d{3}Z$/, "Z");
}

function envelope(body) {
    return '<?xml version="1.0" encoding="UTF-8"?>' +
        "<SOAP-ENV:Envelope " + NS + "><SOAP-ENV:Body>" + body +
        "</SOAP-ENV:Body></SOAP-ENV:Envelope>";
}

function send(response, xml) {
    response.writeHead(200, { "Content-Type": "application/soap+xml; charset=utf-8" });
    response.end(xml);
}

// GetEventProperties' TopicSet wants each topic as nested elements, not the
// colon-and-slash form: <tns1:RuleEngine><CellMotionDetector>...
function topicPathToSetXml(topicPath, messageDescription) {
    const parts = topicPath.split("/");
    const open = parts
        .map((p, i) => "<" + p + (i === parts.length - 1 ? ' wstop:topic="true"' : "") + ">")
        .join("");
    const close = parts.slice().reverse().map((p) => "</" + p + ">").join("");
    return open + messageDescription + close;
}

function motionAlarmDescription() {
    return '<tt:MessageDescription IsProperty="true">' +
        "<tt:Source>" +
        '<tt:SimpleItemDescription Name="Source" Type="tt:ReferenceToken"/>' +
        "</tt:Source>" +
        '<tt:Data><tt:SimpleItemDescription Name="State" Type="xs:boolean"/></tt:Data>' +
        "</tt:MessageDescription>";
}

function ruleEngineDescription() {
    return '<tt:MessageDescription IsProperty="true">' +
        "<tt:Source>" +
        '<tt:SimpleItemDescription Name="VideoSourceConfigurationToken" Type="tt:ReferenceToken"/>' +
        '<tt:SimpleItemDescription Name="RuleName" Type="xs:string"/>' +
        "</tt:Source>" +
        '<tt:Data><tt:SimpleItemDescription Name="IsMotion" Type="xs:boolean"/></tt:Data>' +
        "</tt:MessageDescription>";
}

function topicSetXml() {
    return activeTopics().map((topic) => {
        const desc = topic.kind === "motionAlarm"
            ? motionAlarmDescription()
            : ruleEngineDescription();
        return topicPathToSetXml(topic.path, desc);
    }).join("");
}

function notificationMessage(topicPath, body) {
    return "<wsnt:NotificationMessage>" +
        '<wsnt:Topic Dialect="http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet">' +
        topicPath + "</wsnt:Topic>" +
        "<wsnt:Message>" + body + "</wsnt:Message></wsnt:NotificationMessage>";
}

function motionAlarmMessage(state, when) {
    const body =
        '<tt:Message UtcTime="' + iso(when) + '" PropertyOperation="Changed">' +
        "<tt:Source>" +
        '<tt:SimpleItem Name="Source" Value="' + VIDEO_SOURCE_TOKEN + '"/>' +
        "</tt:Source>" +
        '<tt:Data><tt:SimpleItem Name="State" Value="' + (state ? "true" : "false") + '"/></tt:Data>' +
        "</tt:Message>";
    return notificationMessage("tns1:VideoSource/MotionAlarm", body);
}

function ruleEngineMessage(topicPath, state, when, ruleName) {
    const body =
        '<tt:Message UtcTime="' + iso(when) + '" PropertyOperation="Changed">' +
        "<tt:Source>" +
        '<tt:SimpleItem Name="VideoSourceConfigurationToken" Value="' + VIDEO_SOURCE_TOKEN + '"/>' +
        '<tt:SimpleItem Name="RuleName" Value="' + ruleName + '"/>' +
        "</tt:Source>" +
        '<tt:Data><tt:SimpleItem Name="IsMotion" Value="' + (state ? "true" : "false") + '"/></tt:Data>' +
        "</tt:Message>";
    return notificationMessage(topicPath, body);
}

function motionMessages(state, when) {
    return activeTopics().map((topic) => {
        if (topic.kind === "motionAlarm") {
            return motionAlarmMessage(state, when);
        }
        return ruleEngineMessage(topic.path, state, when, topic.ruleName);
    });
}

function getEventProperties() {
    return envelope(
        "<tev:GetEventPropertiesResponse>" +
        "<tev:TopicNamespaceLocation>http://www.onvif.org/onvif/ver10/topics/topicns.xml</tev:TopicNamespaceLocation>" +
        "<wsnt:FixedTopicSet>true</wsnt:FixedTopicSet>" +
        "<wstop:TopicSet>" + topicSetXml() + "</wstop:TopicSet>" +
        "<wsnt:TopicExpressionDialect>http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet</wsnt:TopicExpressionDialect>" +
        "<wsnt:TopicExpressionDialect>http://docs.oasis-open.org/wsn/t-1/TopicExpression/Simple</wsnt:TopicExpressionDialect>" +
        "<tev:MessageContentFilterDialect>http://www.onvif.org/ver10/tev/messageContentFilter/ItemFilter</tev:MessageContentFilterDialect>" +
        "</tev:GetEventPropertiesResponse>");
}

class EventsService {
    constructor(camera) {
        this.camera = camera;
        this.subscriptions = {};
        this.motionState = false;
        this.subCounter = 0;
    }

    // Drop subscriptions Protect has clearly abandoned, so the map cannot grow
    // without bound over a long uptime. The grace period is deliberately
    // generous (5x the advertised lifetime) because a live client refreshes
    // termination on every PullMessages and Renew.
    reapSubscriptions() {
        const cutoff = Date.now() - (SUB_TIMEOUT_MS * 5);
        for (const id of Object.keys(this.subscriptions)) {
            if (this.subscriptions[id].termination < cutoff) {
                delete this.subscriptions[id];
                logger.debug("events", `Reaped abandoned subscription ${id} for ${this.camera.name}`);
            }
        }
    }

    createPullPoint() {
        const id = "sub" + (++this.subCounter);
        const now = Date.now();
        // Seeded with the current state on every advertised topic so a fresh
        // subscriber learns it immediately instead of waiting for the next edge.
        this.subscriptions[id] = {
            created: now,
            termination: now + SUB_TIMEOUT_MS,
            queue: motionMessages(this.motionState, now),
        };

        const addr = this.camera.endpoints.eventsServiceUrl + "?sub=" + id;
        logger.info(`CreatePullPointSubscription for ${this.camera.name} -> ${id}`);

        return envelope(
            "<tev:CreatePullPointSubscriptionResponse>" +
            "<tev:SubscriptionReference><wsa:Address>" + addr + "</wsa:Address></tev:SubscriptionReference>" +
            "<wsnt:CurrentTime>" + iso(now) + "</wsnt:CurrentTime>" +
            "<wsnt:TerminationTime>" + iso(now + SUB_TIMEOUT_MS) + "</wsnt:TerminationTime>" +
            "</tev:CreatePullPointSubscriptionResponse>");
    }

    pullMessages(subId) {
        const now = Date.now();
        const sub = this.subscriptions[subId];
        const msgs = sub ? sub.queue.splice(0, 10) : [];

        // A pull is proof the subscriber is alive, so it extends the lifetime
        // exactly as Renew does. Without this only Renew would, and Protect
        // long-polls PullMessages continuously while renewing rarely — its
        // termination would fall past the reaper's cutoff and its subscription
        // would be dropped from under it, silently ending events for that
        // camera until it happened to resubscribe.
        if (sub) {
            sub.termination = now + SUB_TIMEOUT_MS;
        }

        if (msgs.length) {
            logger.debug("events", `PullMessages ${subId} for ${this.camera.name} -> ${msgs.length} message(s)`);
        }
        if (sub) {
            sub.termination = now + SUB_TIMEOUT_MS;
        }

        return envelope(
            "<tev:PullMessagesResponse>" +
            "<tev:CurrentTime>" + iso(now) + "</tev:CurrentTime>" +
            "<tev:TerminationTime>" + iso(now + SUB_TIMEOUT_MS) + "</tev:TerminationTime>" +
            msgs.join("") +
            "</tev:PullMessagesResponse>");
    }

    renew(subId) {
        const now = Date.now();
        if (this.subscriptions[subId]) {
            this.subscriptions[subId].termination = now + SUB_TIMEOUT_MS;
        }

        return envelope(
            "<wsnt:RenewResponse>" +
            "<wsnt:CurrentTime>" + iso(now) + "</wsnt:CurrentTime>" +
            "<wsnt:TerminationTime>" + iso(now + SUB_TIMEOUT_MS) + "</wsnt:TerminationTime>" +
            "</wsnt:RenewResponse>");
    }

    // Only queues on an actual change, so the caller can be as chatty as it
    // likes without flooding subscribers. Each edge fans out to every motion topic
    // advertised in GetEventProperties.
    setMotion(state) {
        if (state === this.motionState) {
            return { changed: false, subscribers: Object.keys(this.subscriptions).length };
        }

        this.motionState = state;
        const msgs = motionMessages(state, Date.now());
        const ids = Object.keys(this.subscriptions);
        for (const id of ids) {
            this.subscriptions[id].queue.push(...msgs);
        }

        logger.info(
            `Motion=${state} for ${this.camera.name} queued ${msgs.length} topic(s) to ${ids.length} subscriber(s)`
        );
        return { changed: true, subscribers: ids.length };
    }

    canHandleRequest(request) {
        if (!request.url) {
            return false;
        }
        const path = url.parse(request.url).pathname;
        return path === "/onvif/events_service" || path === "/trigger/motion";
    }

    handleRequest(request, response) {
        const parsed = url.parse(request.url, true);

        if (parsed.pathname === "/trigger/motion") {
            const state = String(parsed.query.state || "").toLowerCase();
            const on = (state === "on" || state === "true" || state === "1");
            const result = this.setMotion(on);
            response.writeHead(200, { "Content-Type": "application/json" });
            response.end(JSON.stringify({
                ok: true,
                motion: this.motionState,
                changed: result.changed,
                subscribers: result.subscribers,
                topics: activeTopics().map((t) => t.path),
            }));
            return;
        }

        let body = "";
        request.on("data", (c) => { body += c; });
        request.on("end", () => {
            const action = (body.match(
                /<[a-zA-Z0-9]*:?(GetEventProperties|CreatePullPointSubscription|PullMessages|Renew|Unsubscribe|GetServiceCapabilities)\b/
            ) || [])[1] || "UNKNOWN";

            logger.debug("events",
                `SOAP Events request received for ${this.camera.name}: ${action}` +
                (parsed.query.sub ? ` (${parsed.query.sub})` : ""));

            // Reap on every request, not only when a subscription is created.
            // Reaping only at creation left an abandoned subscription resident
            // until some client happened to subscribe again, which on an idle
            // camera is never. Doing it here is O(subscriptions) against a map
            // that holds one or two entries in practice, and it cannot run away
            // on its own, since no requests means no growth to reclaim.
            this.reapSubscriptions();

            if (action === "GetEventProperties") return send(response, getEventProperties());
            if (action === "CreatePullPointSubscription") return send(response, this.createPullPoint());
            if (action === "PullMessages") return send(response, this.pullMessages(parsed.query.sub));
            if (action === "Renew") return send(response, this.renew(parsed.query.sub));

            if (action === "Unsubscribe") {
                delete this.subscriptions[parsed.query.sub];
                return send(response, envelope("<wsnt:UnsubscribeResponse/>"));
            }

            if (action === "GetServiceCapabilities") {
                return send(response, envelope(
                    "<tev:GetServiceCapabilitiesResponse><tev:Capabilities " +
                    'WSSubscriptionPolicySupport="false" WSPullPointSupport="true" ' +
                    'WSPausableSubscriptionManagerInterfaceSupport="false" ' +
                    'MaxNotificationProducers="10" MaxPullPoints="10" ' +
                    'PersistentNotificationStorage="false"/></tev:GetServiceCapabilitiesResponse>'));
            }

            logger.warn(`Unhandled SOAP Events request for ${this.camera.name}: ${action}`);
            send(response, envelope("<tev:UnknownResponse/>"));
        });
    }
}

module.exports = EventsService;
