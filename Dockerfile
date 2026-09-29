# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# The UI is embedded, so the runtime image needs no Node or nginx.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X github.com/nonlog/ClinePassProxy/internal/version.Version=${VERSION} -X github.com/nonlog/ClinePassProxy/internal/version.Commit=${COMMIT} -X github.com/nonlog/ClinePassProxy/internal/version.BuildTime=${BUILD_TIME}" \
      -o /out/clinepassproxy ./cmd/clinepassproxy

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -S clinepassproxy && \
    adduser -S -G clinepassproxy -h /var/lib/clinepassproxy clinepassproxy && \
    mkdir -p /var/lib/clinepassproxy && \
    chown -R clinepassproxy:clinepassproxy /var/lib/clinepassproxy && \
    chmod 700 /var/lib/clinepassproxy

COPY --from=build /out/clinepassproxy /usr/local/bin/clinepassproxy

USER clinepassproxy
VOLUME ["/var/lib/clinepassproxy"]
EXPOSE 8788

ENV CLINEPASSPROXY_DATA_DIR=/var/lib/clinepassproxy \
    CLINEPASSPROXY_LISTEN=0.0.0.0:8788

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8788/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/clinepassproxy"]
