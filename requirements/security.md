# Security contract

Status: binding contract. Evidence and open boundaries are indexed in `INDEX.md`.

The declaration is host-authored but still parsed fail-closed. It cannot select
arbitrary schema extensions. Planning never renders copied, mounted, or secret
bytes. Copied file and tree contents are deterministically digested into the
plan; live mounts are not. Secret bytes and their digests are never emitted in
plan output, logs, history, or generated projections. Host-private recovery
state may retain a source digest, but never copied bytes.

Relative sources resolve against the declaration directory, not the caller's
working directory. Missing sources fail validation. Secret files must have no
group or other permission bits; secret bytes are never printed.

Symlinked host source paths are rejected and copied trees reject symlink nodes.
Podman evidence must confirm rootless operation, cgroups v2, private none-network
mode, provenance labels, declared mounts, and resource limits. The runtime socket
is never mounted. Kenogram protects the host only to the extent provided by the
kernel, rootless runtime, and its own correctness; declared rw mounts and secrets
remain world-owned input by design.

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
