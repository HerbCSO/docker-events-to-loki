# syntax=docker/dockerfile:1

FROM golang:1.27.1-alpine3.24 AS builder

# For the CA bundle only - not present in the final image.
RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/docker-events-to-loki .

# Final image: just the static binary and CA certs, nothing else - no
# shell, no package manager, no OS.
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/docker-events-to-loki /usr/local/bin/docker-events-to-loki

ENTRYPOINT ["/usr/local/bin/docker-events-to-loki"]
