# docker-events-to-loki

Streams `docker events` from a host's Docker daemon into Loki, so events
(container create/start/die/destroy, image pulls, network changes, etc.)
become a queryable, persistent log stream instead of vanishing once they
age out of dockerd's in-memory 256-event buffer.

Each event is pushed as its own log line (the raw JSON read straight off
the Docker Engine API's `/events` endpoint), labeled with:

- `job=docker-events`
- `host=<hostname>`
- `type=<event type>` (container, image, network, volume, ...)
- `action=<event action>` (create, start, die, destroy, ...)

Container name/image are left inside the JSON log line rather than used as
labels, to avoid blowing up Loki's label cardinality.

Implemented as a small Go program (standard library only - no docker-cli,
curl or jq) that talks to the Docker daemon directly over its Unix socket.
It compiles to a single static binary, and the image is `FROM scratch`:
just that binary plus a CA bundle for HTTPS Loki endpoints, no shell, no
package manager, no OS underneath.

## Build and run

```sh
docker compose up -d --build
```

Or without compose:

```sh
docker build -t docker-events-to-loki .

docker run -d --name docker-events-to-loki \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -e LOKI_URL=http://loki-host:3100/loki/api/v1/push \
  docker-events-to-loki:latest
```

## CI / published image

`.github/workflows/docker-publish.yml` has two jobs:

- `build` runs on every push, PR and manual trigger, and builds the image
  for `linux/amd64`/`linux/arm64` without pushing, to catch build
  breakage early.
- `push` runs only on pushes to `main`, `v*` tags, and manual dispatch,
  and pushes the built image to Docker Hub.

`push` is scoped to the `dockerhub` GitHub environment (Settings →
Environments), which holds the credentials below and has a deployment
branch policy limited to `main` and `v*` tags — so the token can't be
reached from an arbitrary branch or a PR, even via manual dispatch.

| Environment secret   | Value                                                      |
|-----------------------|-------------------------------------------------------------|
| `DOCKERHUB_USERNAME` | Docker Hub account/namespace the image is pushed under     |
| `DOCKERHUB_TOKEN`    | Docker Hub access token with Read & Write scope             |

The image is published as `<DOCKERHUB_USERNAME>/docker-events-to-loki`,
tagged `latest` (default branch), the branch name, the full commit SHA,
and `X.Y.Z` / `X.Y` for `vX.Y.Z` tags.

## Config

| Env var         | Default                                   | Purpose                                   |
|-----------------|--------------------------------------------|--------------------------------------------|
| `LOKI_URL`      | `http://localhost:3100/loki/api/v1/push`   | Loki push endpoint                        |
| `JOB_LABEL`     | `docker-events`                            | Loki `job` label                          |
| `HOST_LABEL`    | output of `hostname` in the container      | Loki `host` label                         |
| `RETRY_DELAY`   | `5`                                         | Seconds before reconnecting after the events stream ends |
| `DOCKER_SOCKET` | `/var/run/docker.sock`                     | Path to the Docker daemon's Unix socket   |

Set `HOST_LABEL` explicitly (e.g. to the Docker host's real hostname) if
you run this in a container, since otherwise it'll pick up the
container's own hostname/ID rather than the host's.

## Querying in Loki / Grafana

```logql
{job="docker-events"}
{job="docker-events", action="die"}
{job="docker-events", type="container"} | json | Actor_Attributes_name="my-container"
```

## Security note

This container mounts `/var/run/docker.sock`, which is effectively root
access to the host — anyone who can reach that socket from inside the
container can control every other container on the host. The mount here
is `:ro`, which stops this program from writing to the socket, but the
Docker API itself doesn't distinguish read/write at that layer for a
client with any access at all in the way a filesystem does; treat this
container as privileged and restrict who can reach it accordingly.

The image has no shell and no other binaries (`FROM scratch`), so there's
no `docker exec`-ing into it to reach the socket in the first place -
compromising this container's own process (e.g. via a bug in the program
or its dependencies) is the only route in.

## Caveat

This only captures events going forward, from whenever the container
starts. It does not recover events from before it was running (see the
buffer discussion this was built to work around).
