# Build the bridge binary and ship it on a minimal base.
#
# The runtime image needs CA certificates: every SDM call is HTTPS to Google,
# and a scratch image would fail certificate verification on the first request
# with an error that does not obviously point at missing roots.

FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/nest-bridge ./cmd/nest-bridge

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

# Unprivileged: the bridge only makes outbound connections and reads its config.
# It does need to write tokens.json, so that mount must be owned by this uid.
RUN adduser -D -u 10001 bridge
USER bridge

COPY --from=build /out/nest-bridge /usr/local/bin/nest-bridge

ENTRYPOINT ["/usr/local/bin/nest-bridge"]
CMD ["-config=/config/config.yaml", "-tokens=/config/tokens.json", "serve"]
