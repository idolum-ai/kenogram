# Security contract

Status: binding contract. Evidence and open boundaries are indexed in `INDEX.md`.

The declaration is host-authored but still parsed fail-closed. It cannot select
arbitrary schema extensions. Planning never renders copied, mounted, or secret
bytes. Copied file and tree contents are deterministically digested into the
plan; live mounts are not. Secret bytes and their digests are never emitted in
plan output, logs, history, or generated projections. Host-private recovery
state may retain a source digest, but never copied bytes.

Relative sources resolve against the declaration directory, not the caller's
working directory. Missing sources fail validation. Every file and directory in
a secret tree must have no group or other permission bits; failed materialization
removes staged bytes before returning.

Symlinked host source paths are rejected and copied trees reject symlink nodes.
Declared mounts cannot contain or overlap Kenogram state or known container
runtime control sockets. Runtime evidence must match the exact declared mount
set and bind-source filesystem identity; image-authored volumes are ignored.
Host-specific mount safety is checked during dry-run and apply. A replacement
also rejects a new source beneath a predecessor-writable host mount.
Podman evidence must confirm rootless operation, cgroups v2, private none-network
mode, active seccomp filtering, provenance labels, declared mounts, and resource
limits before any service starts. Kenogram requests `--ipc private`. For Podman
versions that report the resulting mode as `shareable`, Kenogram accepts that
label only when the live holder's IPC namespace identity differs from
Kenogram's ambient namespace. This proves separation from the IPC namespace
ambient to the Kenogram process, not that a trusted host process cannot join
the holder's namespace. No container-runtime control socket is mounted into a
world. Kenogram protects the host only to the extent provided by the
kernel, rootless runtime, and its own correctness; declared rw mounts and secrets
remain world-owned input by design.

Named interfaces are trusted host-operator capability. Kenogram verifies the
declaration and generation but does not authenticate, encrypt, authorize, or
interpret relayed bytes; the composed protocol must do so. An interface is not
an input-sanitization or prompt-contamination boundary.

`network-diagnostics` deliberately reveals exact destination host and port as
sensitive operator metadata only after explicit local invocation. Both fields
are untrusted world-authored request metadata: a world can choose the port and
encode bounded prose in a valid hostname, so the destination must not be
interpreted as authority or supplied unsanitized to automation or AI. Outcome is a Kenogram-derived
bounded classification influenced by that request and the observed dial, not
host-authored authority. Invalid UTF-8 and Unicode format controls are rejected
from the request target; opaque non-authority HTTP field values remain outside
this evidence rule. Text output ASCII-quotes destinations. The view never
captures payloads, headers, credentials, complete URLs, paths, query strings,
environment values, or application output beyond that bounded request
metadata. Its ephemeral observations are not copied into status, history,
generated projections, or message channels and do not authorize a declaration
or temporary grant.

## Trust boundary

The host operator and host-authored declaration are trusted authority. World
processes are untrusted relative to the host. The Linux kernel and rootless
Podman are dependencies whose isolation Kenogram observes but does not
independently establish. Declared writable mounts and secrets intentionally
cross the boundary. Kenogram does not claim to harden a multi-tenant host.

Test credentials remain outside the declaration and durable world state.
Hermetic composition uses canary values and local fake APIs. The optional live
Telegram canary requires a dedicated bot and account, receives credentials only
through the protected `live-telegram-canary` environment, scans Kenogram state
for the bot token, and destroys its world after the proof. It is never executed
for untrusted pull-request code.
