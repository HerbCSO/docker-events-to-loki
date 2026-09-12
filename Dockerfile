# syntax=docker/dockerfile:1

FROM golang:1.27.1-alpine3.24 AS builder

# For the CA bundle only - not present in the final image.
RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/docker-events-to-loki .

# Nothing but the compiled binary - CI extracts this stage with
# `docker buildx build --target binary -o type=local` to publish it as a
# release asset alongside the Docker image. Not used by the image itself
# (see the "runtime" stage below, which stays last so it's still the
# default build target).
FROM scratch AS binary
COPY --from=builder /out/docker-events-to-loki /docker-events-to-loki

# Final image: just the static binary and CA certs, nothing else - no
# shell, no package manager, no OS.
FROM scratch AS runtime

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/docker-events-to-loki /usr/local/bin/docker-events-to-loki

# Exec form, not shell form - there's no /bin/sh to run a shell-form CMD in
# this image. The binary checks its own heartbeat file; see "-healthcheck"
# in main.go.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/docker-events-to-loki", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/docker-events-to-loki"]
