# First world

This guide builds one small local image, materializes a world, enters its tmux
session, stops and restarts it, then destroys it. It is a local proof of the
Kenogram lifecycle, not a production image recipe.

## Host preflight

Kenogram currently requires Linux, cgroups v2, rootless Podman, `nsenter`, and
subordinate UID and GID ranges for the current user. Verify the runtime before
creating any files:

```sh
podman info --format json >/dev/null
podman unshare true
test "$(podman info --format '{{.Host.Security.Rootless}}')" = true
test "$(podman info --format '{{.Host.CgroupVersion}}')" = v2
grep -q "^$(id -un):" /etc/subuid
grep -q "^$(id -un):" /etc/subgid
command -v nsenter >/dev/null
```

Install or configure the missing host prerequisite if any command fails. Do not
work around the preflight by running Kenogram as root.

## Build the local base

From the Kenogram repository root, create a temporary authoring directory:

```sh
make build
export KENOGRAM="$(pwd)/bin/kenogram"
mkdir -p /tmp/kenogram-first-world
cd /tmp/kenogram-first-world
```

Create `Containerfile`:

```Dockerfile
FROM docker.io/library/ubuntu:24.04

ARG USER_ID
ARG GROUP_ID

RUN apt-get update \
    && apt-get install --no-install-recommends -y ca-certificates tmux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --non-unique --gid "${GROUP_ID}" agent \
    && useradd --non-unique --uid "${USER_ID}" --gid "${GROUP_ID}" --create-home agent

ENV HOME=/home/agent
```

Build it for the current host identity:

```sh
podman build \
  --build-arg USER_ID="$(id -u)" \
  --build-arg GROUP_ID="$(id -g)" \
  --tag localhost/kenogram-first-world:latest \
  .
```

## Declare and enter the world

Create `world.toml`. The local image is deliberately admitted as unpinned so
this first run does not pretend to be reproducible:

```sh
cat > world.toml <<EOF
version = 1
name = "first"
allow_unpinned = true

[world]
hostname = "first"
base = "localhost/kenogram-first-world:latest"
workdir = "/workspace"
user = "$(id -u):$(id -g)"

[resources]
cpus = 1
memory_bytes = 536870912
pids = 128

[workspace]
paths = ["/workspace"]

[[services]]
name = "tmux"
command = ["/bin/sh", "-c", "/usr/bin/tmux new-session -d -s main && exec /usr/bin/tmux wait-for kenogram-stop"]
autostart = true
restart = "never"
EOF
```

Plan, apply, inspect, and enter it using the binary from the repository:

```sh
"$KENOGRAM" up --dry-run ./world.toml
"$KENOGRAM" up --yes ./world.toml
"$KENOGRAM" status first
"$KENOGRAM" enter first
```

Detach from tmux with `Ctrl-b d`. The workspace remains on the host under
Kenogram's state directory.

## Restart and remove it

```sh
"$KENOGRAM" down first
"$KENOGRAM" up --yes ./world.toml
"$KENOGRAM" enter first
"$KENOGRAM" destroy --yes first
```

For a durable declaration, publish or otherwise obtain the base image by digest
and replace the local tag with an immutable `name@sha256:...` reference. Then
remove `allow_unpinned = true`.
