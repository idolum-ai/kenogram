# Apple container-machine launcher

Status: experimental transport; not part of Kenogram's proven release matrix.

Kenogram can carry a complete command from macOS into one persistent Linux
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
   date, and Go toolchain report as the macOS launcher, plus `/usr/bin/env`,
   `nsenter`, and rootless Podman, in that machine.
3. Configure cgroups v2 and subordinate UID/GID ranges for the machine's mapped
   user. `podman info --format json` must report rootless operation, cgroups v2,
   and mappings larger than one ID.
4. Put declarations and copied inputs somewhere visible inside the machine.

Build macOS launchers with Go 1.24 or newer. Go 1.24 made the Mach-O `LC_UUID`
load command part of linker output; macOS 26 rejects older Go binaries that omit
it. The native CI lane pins Go 1.26.4, and release automation should build both
the outer macOS launcher and inner Linux binary with the same pinned toolchain
so their version reports match.

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

The inner binary setting must be a shell-inert absolute path or command name:
letters, digits, dot, underscore, slash, and hyphen only. This is narrower than
a general Unix path because Apple's machine init mediates explicit commands
through a shell.

Every invocation performs these fail-closed checks before forwarding the user's
operation:

```text
container machine inspect <machine>
container machine run -n <machine> -- /usr/bin/env -u KENOGRAM_RUNTIME -u KENOGRAM_CONTAINER_MACHINE -u KENOGRAM_MACHINE_KENOGRAM -- <linux-kenogram> version
container machine run -n <machine> -- /usr/bin/env -u KENOGRAM_RUNTIME -u KENOGRAM_CONTAINER_MACHINE -u KENOGRAM_MACHINE_KENOGRAM -- podman info --format json
```

Apple's current `machine run` implementation invokes explicit commands through
`/sbin.machine/init -s`; Apple's own integration suite demonstrates shell
expansion. Kenogram therefore never places raw user arguments on that command
line. The final command contains a versioned bridge name followed by one
nonempty base64url token per argument. The Linux Kenogram accepts only the
canonical shell-inert alphabet, decodes the tokens back to their original bytes,
and dispatches the restored argv. Empty arguments, whitespace, quotes, Unicode,
newlines, variables, substitutions, globs, and control operators are covered by
round-trip tests. See Apple's
[`MachineRun.swift`](https://github.com/apple/container/blob/main/Sources/ContainerCommands/Machine/MachineRun.swift)
and its
[`testRunCommandInShell`](https://github.com/apple/container/blob/main/Tests/IntegrationTests/Machine/TestCLIMachineRuntimeSerial.swift).

Apple constructs a machine-local environment for explicit commands and does not
inherit host variables unless they are requested with `--env`; Kenogram requests
none. The `/usr/bin/env -u` prefix is defense in depth against recursive selector
variables, not proof of general environment inheritance or scrubbing. Machine
state therefore follows the machine's `HOME`/`XDG_DATA_HOME`; host
`KENOGRAM_STATE_DIR` and other configuration do not cross automatically.

The launcher requires inner and outer `kenogram version` reports to match
exactly. This prevents an accidental old/new pairing, but it is not binary
attestation: development builds can share placeholder metadata, and an
operator-controlled executable can report arbitrary text. Supported packaging
must give the paired binaries non-placeholder provenance and a trusted install
path. A failed inspect, version check, or Podman preflight prevents the requested
operation.

The final operation explicitly opens stdin. `enter` and an unconfirmed `up`
request an Apple machine TTY when the host has one; other operations retain
separate stdout and stderr, including machine-readable output. `enter` fails
before preflight if no host terminal exists. Inner exit statuses, including
usage status 2 and conventional signal statuses, are returned unchanged. On
host cancellation Kenogram forwards the originating interrupt or termination
signal to Apple's CLI, waits five seconds for its remote forwarding path, and
only then escalates to `SIGKILL`.

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

Automated tests currently prove selector validation, canonical adversarial argv
encoding/decoding, shell-inert outer tokens, explicit stdin/TTY flags,
machine-local environment assumptions, exit-status preservation, graceful
signal forwarding with bounded escalation, prerequisite ordering, failure
before the user command, non-ownership of machine lifecycle, native Linux
regressions, a Darwin/arm64 cross-build, and a native Apple-silicon build and
fail-closed CLI smoke test on GitHub's `macos-26` runner.

That hosted runner cannot prove the machine boundary. GitHub documents that its
[arm64 macOS runners do not support nested virtualization](https://docs.github.com/en/actions/reference/runners/github-hosted-runners#limitations-for-arm64-macos-runners),
while Apple's runtime is built on the macOS Virtualization framework. A real
machine proof therefore requires a dedicated self-hosted Apple-silicon Mac on
macOS 26 (or an equivalent bare-metal Mac provider), with a version-pinned
`container` installation and a pre-provisioned Kenogram machine. The workflow
should be scheduled and manually dispatchable, not a required pull-request gate
until runner capacity and cleanup are reliable.

The current matrix does not prove that a released Apple machine supplies nested
rootless Podman, cgroup delegation, user namespaces, bind-mount flags,
network-door transfer, or end-to-end interactive TTY and signal behavior.

Before this transport can be called supported, a real Apple-silicon CI or
operator proof must exercise: adversarial argv arrival; piped stdin; `up`
confirmation; `enter` TTY; interruption during both `up` and `enter`; exact exit
statuses; create/adopt/replace/restart/destroy; workspace carry; copied and
secret inputs; all network absence/grant/revoke invariants; post-start inspect
evidence; stopped-machine auto-boot; and both `home-mount=none` and deliberately
shared-home configurations.
