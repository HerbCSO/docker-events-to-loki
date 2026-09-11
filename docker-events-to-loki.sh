#!/usr/bin/env sh
# Streams `docker events` into Loki's push API.
#
# Env vars:
#   LOKI_URL     Loki push endpoint (default: http://localhost:3100/loki/api/v1/push)
#   JOB_LABEL    Loki "job" label value (default: docker-events)
#   HOST_LABEL   Loki "host" label value (default: `hostname`)
#   RETRY_DELAY  Seconds to wait before reconnecting after the events stream ends (default: 5)

set -eu

LOKI_URL="${LOKI_URL:-http://localhost:3100/loki/api/v1/push}"
JOB_LABEL="${JOB_LABEL:-docker-events}"
HOST_LABEL="${HOST_LABEL:-$(hostname)}"
RETRY_DELAY="${RETRY_DELAY:-5}"

log() {
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

push_event() {
    line="$1"
    ts=$(date +%s%N)

    type=$(printf '%s' "$line" | jq -r '.Type // "unknown"')
    action=$(printf '%s' "$line" | jq -r '.Action // "unknown"')

    payload=$(jq -nc \
        --arg ts "$ts" \
        --arg line "$line" \
        --arg job "$JOB_LABEL" \
        --arg host "$HOST_LABEL" \
        --arg type "$type" \
        --arg action "$action" \
        '{
            streams: [
                {
                    stream: {job: $job, host: $host, type: $type, action: $action},
                    values: [[$ts, $line]]
                }
            ]
        }')

    if ! curl -sS -o /dev/null -w '%{http_code}' -m 10 \
        -X POST "$LOKI_URL" \
        -H 'Content-Type: application/json' \
        -d "$payload" | grep -q '^2'; then
        log "warn: failed to push event to $LOKI_URL"
    fi
}

log "forwarding docker events to $LOKI_URL (job=$JOB_LABEL, host=$HOST_LABEL)"

while true; do
    docker events --format '{{json .}}' | while IFS= read -r line; do
        [ -n "$line" ] && push_event "$line"
    done
    log "docker events stream ended, reconnecting in ${RETRY_DELAY}s"
    sleep "$RETRY_DELAY"
done
