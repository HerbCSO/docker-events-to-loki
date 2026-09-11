// Command docker-events-to-loki streams `docker events` from a host's
// Docker daemon into Loki's push API, so events (container create/start/die,
// image pulls, network changes, etc.) become a queryable, persistent log
// stream instead of vanishing once they age out of dockerd's in-memory
// 256-event buffer.
//
// It talks to the Docker Engine API directly over the Unix socket (no
// docker-cli needed) and uses only the Go standard library, so it builds
// into a single static binary with no runtime dependencies.
//
// Env vars:
//
//	LOKI_URL       Loki push endpoint (default: http://localhost:3100/loki/api/v1/push)
//	JOB_LABEL      Loki "job" label value (default: docker-events)
//	HOST_LABEL     Loki "host" label value (default: hostname)
//	RETRY_DELAY    Seconds to wait before reconnecting after the events stream ends (default: 5)
//	DOCKER_SOCKET  Path to the Docker daemon's Unix socket (default: /var/run/docker.sock)
//
// Run with a single "-healthcheck" argument to check liveness instead of
// streaming: exits 0 if connected to the Docker events stream recently,
// 1 otherwise. This doubles as the image's Docker HEALTHCHECK, since the
// scratch image has no shell/curl/wget to run one any other way.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	// healthFile is touched while connected to the Docker events stream,
	// and read back by "-healthcheck". Scratch has no /tmp by default, so
	// it's created at startup.
	healthFile = "/tmp/health-ok"
	// healthcheckInterval is how often the heartbeat is refreshed while
	// connected, including on hosts with no events to report.
	healthcheckInterval = 15 * time.Second
	// healthMaxAge is how stale the heartbeat can be before "-healthcheck"
	// reports unhealthy - a generous multiple of healthcheckInterval so a
	// single missed tick under load doesn't flap the container's status.
	healthMaxAge = 3 * healthcheckInterval
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), fmt.Sprintf(format, args...))
}

// dockerEvent captures just the fields we need out of a Docker events API
// message; everything else is forwarded to Loki untouched via rawEvent.
type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
}

// runHealthcheck reports whether the heartbeat file was touched recently
// enough to consider the process still connected to the Docker events
// stream. Returns a process exit code (0 healthy, 1 unhealthy).
func runHealthcheck() int {
	info, err := os.Stat(healthFile)
	if err != nil {
		return 1
	}
	if time.Since(info.ModTime()) > healthMaxAge {
		return 1
	}
	return 0
}

// touchHealthFile records that the process is currently connected to the
// Docker events stream. Failures are logged but non-fatal - health
// reporting is secondary to the actual event forwarding.
func touchHealthFile() {
	content := []byte(time.Now().UTC().Format(time.RFC3339) + "\n")
	if err := os.WriteFile(healthFile, content, 0o644); err != nil {
		logf("warn: failed to update health file: %v", err)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthcheck())
	}

	if err := os.MkdirAll("/tmp", 0o755); err != nil {
		logf("warn: failed to create /tmp for health file: %v", err)
	}

	lokiURL := getenv("LOKI_URL", "http://localhost:3100/loki/api/v1/push")
	jobLabel := getenv("JOB_LABEL", "docker-events")
	dockerSocket := getenv("DOCKER_SOCKET", "/var/run/docker.sock")

	hostLabel := os.Getenv("HOST_LABEL")
	if hostLabel == "" {
		h, err := os.Hostname()
		if err != nil {
			h = "unknown"
		}
		hostLabel = h
	}

	retryDelay := 5 * time.Second
	if v := os.Getenv("RETRY_DELAY"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid RETRY_DELAY %q: %v\n", v, err)
			os.Exit(1)
		}
		retryDelay = time.Duration(secs) * time.Second
	}

	dockerClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", dockerSocket)
			},
		},
	}
	pushClient := &http.Client{Timeout: 10 * time.Second}

	logf("forwarding docker events to %s (job=%s, host=%s)", lokiURL, jobLabel, hostLabel)

	for {
		streamDockerEvents(dockerClient, pushClient, lokiURL, jobLabel, hostLabel)
		logf("docker events stream ended, reconnecting in %s", retryDelay)
		time.Sleep(retryDelay)
	}
}

// streamDockerEvents connects to the Docker daemon's /events endpoint and
// pushes each event to Loki until the stream errors or ends.
func streamDockerEvents(dockerClient, pushClient *http.Client, lokiURL, jobLabel, hostLabel string) {
	req, err := http.NewRequest(http.MethodGet, "http://docker/events", nil)
	if err != nil {
		logf("error: building docker events request: %v", err)
		return
	}

	resp, err := dockerClient.Do(req)
	if err != nil {
		logf("error: connecting to docker events stream: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logf("error: docker events stream returned status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
		return
	}

	// Connected: mark healthy now, and keep refreshing on a timer for as
	// long as the connection stays open - a quiet host with no events to
	// report isn't unhealthy, so the heartbeat can't depend on events
	// actually arriving.
	touchHealthFile()
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	defer stopHeartbeat()
	go func() {
		ticker := time.NewTicker(healthcheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				touchHealthFile()
			}
		}
	}()

	dec := json.NewDecoder(resp.Body)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if err != io.EOF {
				logf("warn: docker events stream error: %v", err)
			}
			return
		}
		pushEvent(pushClient, lokiURL, jobLabel, hostLabel, raw)
	}
}

// pushEvent forwards one raw Docker event to Loki as a single log line,
// labeled with job/host/type/action.
func pushEvent(client *http.Client, lokiURL, jobLabel, hostLabel string, raw json.RawMessage) {
	var ev dockerEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		logf("warn: failed to parse event JSON: %v", err)
	}
	evType := ev.Type
	if evType == "" {
		evType = "unknown"
	}
	action := ev.Action
	if action == "" {
		action = "unknown"
	}

	ts := strconv.FormatInt(time.Now().UnixNano(), 10)

	payload := map[string]any{
		"streams": []map[string]any{
			{
				"stream": map[string]string{
					"job":    jobLabel,
					"host":   hostLabel,
					"type":   evType,
					"action": action,
				},
				"values": [][2]string{{ts, string(raw)}},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		logf("warn: failed to marshal loki payload: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, lokiURL, bytes.NewReader(body))
	if err != nil {
		logf("warn: failed to build loki push request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		logf("warn: failed to push event to %s: %v", lokiURL, err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logf("warn: failed to push event to %s (status %d)", lokiURL, resp.StatusCode)
	}
}
