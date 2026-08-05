# Security contract

Status: binding contract. Evidence and open boundaries are indexed in `INDEX.md`.

The declaration is host-authored but still parsed fail-closed. It cannot select
arbitrary schema extensions. Planning never renders copied, mounted, or secret
bytes. Copied file and tree contents are deterministically digested into the
plan; live mounts are not. Secret bytes and their digests are never emitted in
plan output, logs, history, or generated projections. Host-private recovery
state may retain a source digest, but never copied bytes.

For governed one-shot jobs, declared read-only mounts are a narrower case: the
runtime snapshots them into Kenogram-owned scratch under the shared bounded
source-tree walker, proves an exact content-and-mode copy, then normalizes only
the private staged copy under `portable-readonly-v1`. Directories become `0555`;
files become `0444` plus execute bits for every contained identity only when the
source was executable; no staged node remains writable. Evidence retains both
the original authority digest and normalized delivered digest. Declared
writable source trees are descriptor-root inspected and reject every socket or
other special descendant before provider creation. Device/inode comparison also
rejects aliases of accessible known container-runtime endpoints; no claim is
made about aliases the host does not make inspectable.

Kenogram-owned ephemeral workspace roots use `portable-writable-v1`: the exact
bind root is `0777`, while every enclosing scratch directory remains `0700` and
host-private. Runtime evidence revalidates the root's identity and exact policy
before target admission and finalization. This projection is never applied to
operator-owned declared writable sources; those retain their authored modes
and remain target-visible authority by explicit declaration. An artifact root
is only a collection request. It must already exist or be target-creatable
inside declared write authority; Kenogram never makes an unrelated image path
writable to satisfy artifact collection.

Artifact collection distinguishes image-root paths from bind-mounted paths.
The deepest exact inspected mount boundary and its retained source inode bind
the read; workspace mounts must also resolve beneath the exact private scratch.
Cleanup persists a private owner/plan/declaration/scratch-bound inventory of
the exact workspace source devices and inodes while the stopped container is
still provable. After immutable container absence, the parent repeats the proof
immediately before namespace entry; the helper binds that ID to the authority
record without recursively invoking Podman and empties only the recorded
Kenogram workspace roots. Wait and Finalize workers must both join before
cleanup can mutate provider or filesystem authority. Failure
retains the record for an idempotent post-absence retry. The finalization worker
and every namespace-helper process group must be joined before cleanup can
advance. Symlinks and special nodes inside a workspace are unlinked as names,
not followed or interpreted. Declared writable sources are never cleanup
targets, even when the target made their contents inaccessible to the host.

Relative sources resolve against the declaration directory, not the caller's
working directory. Missing sources fail validation. Every file and directory in
a secret tree must have no group or other permission bits. Secret permission
validation uses the same descriptor-rooted, context-aware source-tree bounds as
planning and staging; cancellation or a bound violation fails before governed
provider preflight. Failed materialization removes staged bytes before returning.

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
