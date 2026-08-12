# Building with CGO_ENABLED=0 is the point of the cgo-free SQLite driver: the
# result is one static binary that runs on a NAS or a Raspberry Pi without a
# matching C toolchain.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not refetch them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kobibri ./cmd/kobibri

FROM alpine:3.21

# ca-certificates is needed to reach the Kobo store when proxying is on;
# tzdata so timestamps in the interface match the operator's clock.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 kobibri

COPY --from=build /out/kobibri /usr/local/bin/kobibri

# The database, the converted books and the scaled covers live here. Mount a
# volume, or everything is lost with the container.
ENV KOBIBRI_DATA_DIR=/data \
    KOBIBRI_LISTEN=0.0.0.0:8078
VOLUME /data

# Calibre libraries are mounted read-only from the host, e.g.
#   -v /srv/calibre:/library:ro
EXPOSE 8078
USER kobibri

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8078/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/kobibri"]
CMD ["serve"]
