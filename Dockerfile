FROM alpine:3.24

RUN apk add --no-cache docker-cli curl jq

COPY docker-events-to-loki.sh /usr/local/bin/docker-events-to-loki.sh
RUN chmod +x /usr/local/bin/docker-events-to-loki.sh

ENTRYPOINT ["/usr/local/bin/docker-events-to-loki.sh"]
