# Patched emberstonel/onvif-virtual-camera adding an ONVIF Events service, so
# Protect can subscribe via WS-PullPoint and file Nest events on the timeline.
#
# Pinned by digest, not :latest. This container carries the device identity
# Protect keys adoption off, so a base image change must be a deliberate act.
# Digest of the :latest tag as deployed and verified on 2026-08-30.
FROM ghcr.io/emberstonel/onvif-virtual-camera@sha256:54a68b294fd75b00f14c7be45a13c8feb72e96b22447642fe19165436fc5ae92

COPY events-service.js /app/src/events-service.js
COPY patch-onvif-server.js /usr/local/bin/patch-onvif-server.js

# The patch exits non-zero if any anchor fails to match exactly once, which
# fails the build here rather than producing an image that starts cleanly and
# silently serves no events. node --check catches a patch that applied but left
# the file unparseable, which would otherwise only show at container start.
# The patch script also loads the patched WSDL through node-soap and asserts the
# audio operations are exposed, which node --check cannot see.
RUN node /usr/local/bin/patch-onvif-server.js /app/src \
    && node --check /app/src/onvif-server.js \
    && node --check /app/src/camera-manager.js \
    && node --check /app/src/config-loader.js \
    && node --check /app/src/services/device-service.js \
    && node --check /app/src/services/media-service.js \
    && node --check /app/src/events-service.js
