# Apple container-machine launcher

Status: experimental transport; not part of Kenogram's proven release matrix.

Kenogram can forward its complete command from macOS into one persistent Linux
environment managed by Apple's `container machine`. The inner Linux Kenogram
continues to use rootless Podman and the binding evidence checks. Apple's
ordinary per-container CLI is not silently treated as Podman: it does not expose
the same inspect evidence or Kenogram's host-side Linux network-namespace door.

This design follows Apple's distinction between a persistent Linux
[container machine](https://github.com/apple/container/blob/main/docs/container-machine.md)
and its one-VM-per-container
[runtime](https://github.com/apple/container/blob/main/docs/technical-overview.md).

## Prerequisites

Use an Apple-silicon Mac with macOS 26 and a current `container` release. Before
selecting the launcher, the operator must:

1. Start Apple's container system and create a dedicated machine from an OCI
   image containing `/sbin/init`.
2. Install a Linux Kenogram binary with exactly the same version, commit, build
   date, and Go toolchain identity as the macOS launcher, plus `nsenter` and
   rootless Podman, in that machine.
3. Configure cgroups v2 and subordinate UID/GID ranges for the machine's mapped
   user. `podman info --format json` must report rootless operation, cgroups v2,
   and mappings larger than one ID.
4. Put declarations and copied inputs somewhere visible inside the machine.

Apple documents that `container machine run` boots a stopped machine. Kenogram
therefore requires an existing machine but does not create, configure, stop, or
remove it. In particular, `kenogram down` and `kenogram destroy` affect inner
Podman worlds only.

Select the transport explicitly:

```sh
export KENOGRAM_RUNTIME=apple-container-machine
export KENOGRAM_CONTAINER_MACHINE=kenogram
# Optional when the Linux binary is not on PATH:
export KENOGRAM_MACHINE_KENOGRAM=/opt/kenogram/bin/kenogram

kenogram up --dry-run /path/visible/in/the/machine/world.toml
kenogram up --yes /path/visible/in/the/machine/world.toml
```

Every invocation performs these fail-closed checks before forwarding the user's
argv without a shell:

```text
container machine inspect <machine>
container machine run -n <machine> -- /usr/bin/env -u KENOGRAM_RUNTIME -u KENOGRAM_CONTAINER_MACHINE -u KENOGRAM_MACHINE_KENOGRAM -- <linux-kenogram> version
container machine run -n <machine> -- /usr/bin/env -u KENOGRAM_RUNTIME -u KENOGRAM_CONTAINER_MACHINE -u KENOGRAM_MACHINE_KENOGRAM -- podman info --format json
```

The final command uses the same prefix and the original Kenogram arguments.
Unsetting the selector variables prevents recursive forwarding if a machine
login profile happens to define them. The launcher also requires the inner and
outer `kenogram version` output to match exactly, preventing an older Linux
implementation from silently executing newer host intent. A failed inspect,
identity check, or Podman preflight prevents the requested Kenogram operation.

## Security boundary and open proof

The inner world retains Kenogram's rootless-Podman evidence contract: no runtime
socket mount, no network route by default, private namespaces, empty capability
bounding set, `no-new-privileges`, resource limits, and exact declared mounts.
Network grants are created by the Linux Kenogram inside the machine, where
`/proc` and namespace file descriptors are available.

The outer machine is a separate, broader trust boundary. Kenogram does not yet
verify its image, kernel, init system, network attachment, persistent disk, or
home-mount setting. Apple's default `home-mount=rw` exposes the operator's home
directory to the machine and is materially broader than a Kenogram world. A
dedicated machine with `home-mount=none` is the safer posture; declarations,
inputs, state, and the Linux binary must then live on the machine's persistent
filesystem. If host sharing is necessary, review `home-mount=ro` or `rw` as an
explicit authority grant.

Automated tests currently prove selector validation, exact argv, environment
scrubbing, prerequisite ordering, failure before the user command, non-ownership
of machine lifecycle, native Linux regressions, and a Darwin/arm64 cross-build.
They do not prove that a released Apple machine supplies nested rootless Podman,
cgroup delegation, user namespaces, bind-mount flags, network-door transfer, or
interactive TTY behavior.

Before this transport can be called supported, a real Apple-silicon CI or
operator proof must exercise: create/adopt/replace/restart/destroy; `enter` TTY;
workspace carry; copied and secret inputs; all network absence/grant/revoke
invariants; post-start inspect evidence; stopped-machine auto-boot; and both
`home-mount=none` and deliberately shared-home configurations.
