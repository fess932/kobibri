# Building with CGO_ENABLED=0 is the point of the cgo-free SQLite driver: the
# result is one static binary that runs on a NAS or a Raspberry Pi without a
# matching C toolchain.
# The build stage runs on the host's own architecture and cross-compiles, so a
# multi-arch image costs one build rather than one emulated build per platform.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not refetch them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/kobibri ./cmd/kobibri

FROM alpine:3.21

# ca-certificates is needed to reach the Kobo store when proxying is on;
# tzdata so timestamps in the interface match the operator's clock.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 kobibri \
 && mkdir -p /data && chown 10001:10001 /data

COPY --from=build /out/kobibri /usr/local/bin/kobibri

# The database, the converted books and the scaled covers live here — and the
# only copy of anything uploaded by hand or imported from the web. Mount a
# volume, or it all goes with the container.
#
# /data is created owned by the unprivileged user *before* it is declared a
# volume, which is the only way a named volume comes out writable: Docker
# initialises a fresh one from the image, ownership and all. A bind mount does
# not inherit that — the host directory's own ownership wins — so a bind mount
# has to be chowned to 10001 on the host.
# debug rather than info: in a container the log is the only way to see what a
# reader actually asked for, and the interesting failures here are silent ones —
# a device that caches a bad endpoint map and simply stops talking leaves nothing
# at info. Override it with KOBIBRI_LOG_LEVEL=info once a deployment is settled.
ENV KOBIBRI_DATA_DIR=/data \
    KOBIBRI_LISTEN=0.0.0.0:8078 \
    KOBIBRI_LOG_LEVEL=debug
VOLUME /data

# Calibre libraries are mounted read-only from the host, e.g.
#   -v /srv/calibre:/library:ro
# They only need to be readable by uid 10001, which is what a library with the
# usual 0755 directories already is.
EXPOSE 8078
USER 10001:10001

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8078/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/kobibri"]
CMD ["serve"]
