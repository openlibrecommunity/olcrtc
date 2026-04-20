# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.22

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/olcrtc ./cmd/olcrtc

FROM alpine:${ALPINE_VERSION} AS runtime

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S olcrtc && \
    mkdir -p /usr/share/olcrtc /var/lib/olcrtc && \
    adduser -S -D -h /var/lib/olcrtc -s /sbin/nologin -G olcrtc olcrtc && \
    chown -R olcrtc:olcrtc /usr/share/olcrtc /var/lib/olcrtc

COPY --chown=olcrtc:olcrtc data /usr/share/olcrtc
COPY --from=build /out/olcrtc /usr/local/bin/olcrtc
COPY script/docker/olcrtc-entrypoint.sh /usr/local/bin/olcrtc-entrypoint
COPY script/docker/olcrtc-healthcheck.sh /usr/local/bin/olcrtc-healthcheck

RUN chmod 0755 /usr/local/bin/olcrtc /usr/local/bin/olcrtc-entrypoint /usr/local/bin/olcrtc-healthcheck

USER olcrtc:olcrtc
WORKDIR /var/lib/olcrtc

ENV OLCRTC_MODE=srv \
    OLCRTC_PROVIDER= \
    OLCRTC_DATA_DIR=/usr/share/olcrtc \
    OLCRTC_DNS=1.1.1.1:53 \
    OLCRTC_KEY_FILE=/var/lib/olcrtc/key.hex

VOLUME ["/var/lib/olcrtc"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/olcrtc-healthcheck"]

ENTRYPOINT ["/usr/local/bin/olcrtc-entrypoint"]
