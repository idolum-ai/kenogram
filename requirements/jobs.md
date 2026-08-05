# Governed job contract

Status: implemented provider-independent core and direct Linux provider. The
schemas, independent Go semantic validators, create-only publisher, bounded
executor, offline verifier, and direct one-shot Podman CLI adapter are active.
Finite declared egress is implemented through a namespace-pinned, host-owned
job proxy while the container retains `network=none`. Darwin either hands the exact invocation to an explicitly configured
Linux Apple container machine or fails closed without namespace claims.

A governed job is one noninteractive, bounded target execution inside a fresh
Kenogram generation. It is distinct from the persistent-world service model.
The job request is host-authored authority. Target output, target artifacts,
and runtime-reported fields are observations and never become authority merely
because a producer calls the job successful.

The versioned language-neutral documents are:

- `kenogram.job-request.v1`, which binds the exact declaration, target command,
  public environment, work and output bounds, and optional artifact inventory;
- `kenogram.job-result.v1`, which keeps the target outcome, finalization, and
  cleanup as separate observations;
- `kenogram.job-evidence-manifest.v1`, which seals one closed create-only
  evidence inventory; and
- `kenogram.executable-provenance.v1`, which identifies the Kenogram executable
  that produced the observation; and
- `kenogram.podman-runtime-observation.v1`, which closes the public K5 runtime
  proof over immutable container, image, enforcement, and mount identities; and
- `kenogram.job-egress-evidence.v1`, which conditionally binds the declared
  allowlist, pinned namespace listener, bounded outcome counters, revocation,
  active-tunnel closure, and proxy join.

Their JSON Schemas are under [`../schemas/`](../schemas/). The schemas are
closed and collection/value bounded. Kenogram additionally limits the encoded
document bytes before decoding, rejects duplicate object keys, trailing JSON,
invalid UTF-8, unsafe operator paths, semantically duplicated names, and
inconsistent cross-field outcomes. JSON Schema `maxLength` counts characters;
Kenogram's equal numeric limits count UTF-8 bytes and are intentionally at
least as restrictive.

## Reserved CLI

The public command shape is reserved as:

```text
kenogram job --request REQUEST.json --evidence-dir NEW_DIRECTORY
kenogram verify-job --evidence-dir DIRECTORY
kenogram version --json
```

This contract does not authorize an implementation to synthesize those
commands by composing `up`, detached services, and `destroy`. The job executor
must own the attached target process, bounded streams, target process group,
runtime generation, proxy, evidence directory, and cleanup through terminal
observation.

Once implemented, a semantically valid request produces exactly one JSON result
on stdout. Exit 0 means the execution was completely observed and sealed; the
target's own nonzero exit remains a target result and does not become recorder
failure. Exit 1 means a typed `refused` or `incomplete` result was sealed, or
that a publication failure occurred after semantic job identity was established
but could not itself be sealed. Exit 2 means the invocation, request, retained
declaration, or declaration-owned secret binding was invalid before trustworthy
semantic job identity could be established and need not emit a job result.
Diagnostics use stderr and are never part of the machine result.

The provider-independent core is not evidence that a real provider satisfies
the contract. A consumer must require a sealed bundle produced through the K5
adapter and must independently run `verify-job`.

The core wraps admission, terminal observation, finalization, artifact opens
and reads, and cleanup in caller-owned deadlines. A provider that ignores its
context cannot make the caller wait without bound or produce a complete result.
Cleanup is deferred immediately before provider admission and is therefore not
skipped by later publication or artifact failures; a late admission response
also triggers a bounded cleanup attempt.

## Request authority and bounds

The caller supplies a request no larger than 1 MiB. The CLI opens that path as
a regular-file descriptor, reads at most 1 MiB plus one detection byte, and
rejects directories and special inputs before semantic parsing. It contains:

- a 1–63 byte portable job identifier;
- an absolute declaration path and exact lowercase SHA-256 binding;
- 1–128 nonempty UTF-8 argv values, each at most 4,096 bytes;
- an absolute, canonical container working directory;
- at most 128 unique environment names, each bound either to an explicitly
  retained public value or an in-world declaration-owned secret-file path;
- at most 512 runtime mounts in total, including every declared mount,
  workspace, and the two Kenogram-owned helper and lifecycle mounts;
- a target timeout from 1 ms through 24 hours;
- a finalization timeout from 1 ms through 10 minutes;
- independent stdout and stderr capture limits up to 64 MiB each; and
- optionally, an absolute container artifact root bounded to 10,000 entries and
  1 GiB of ordinary-file content.

The artifact root is collection authority, not provisioning authority.
Kenogram does not create it or make an arbitrary image path writable. The
caller must select a root that already exists in the image or that the target
can create beneath declared write authority, normally a Kenogram-owned
workspace such as `/workspace/artifacts`. Missing or unwritable roots fail the
target or collection honestly; they never broaden a mount.

`public_value` is retained authority and therefore MUST NOT contain a secret.
`secret_file` names an absolute in-world file delivered by a declaration-owned
secret copy; the executor must prove that binding before reading it, inject the
exact non-NUL file bytes up to 16 KiB, and never duplicate those bytes into the
request, plan, result, output metadata, or provenance.
Target-owned stdout, stderr, and artifacts can disclose bytes available to the
target; retaining those outputs is an explicit caller decision, not a claim
that Kenogram can stop a target from printing its own secrets.

Request authority cannot set any case variant of `HTTP_PROXY`, `HTTPS_PROXY`,
`ALL_PROXY`, or `NO_PROXY`. For declared egress Kenogram injects the upper- and
lowercase proxy variables itself only after the exact listener is ready;
`NO_PROXY` is never injected into the target.

Every `secret_file` must match exactly one copy target whose declaration marks
it `secret = true`. The retained plan replaces that copy's source digest with
`<redacted>` and binds the projection through `evidence_digest`; secret bytes
and their content digests are not needed by the offline verifier. At execution,
Kenogram opens the source once, bounds and reads that descriptor, derives the
canonical plan-copy digest from exactly those bytes and that descriptor's mode,
compares it with the operational plan digest, and delivers those same bytes.
Replacing the source pathname after open cannot substitute the delivered value.

The request never inherits ambient command, environment, timeout, output, or
artifact authority. A consumer may refuse a request it cannot implement; it
must not silently fall back to a persistent service or direct host execution
while retaining a Kenogram claim.

## Execution result

Target lifecycle and observer lifecycle are separate:

```text
target start ───────── target exit/signal
                         │
                         └─ finalization ─ output/artifact close
                                           └─ cleanup ─ proof of absence
```

The target result is exactly one of `exited`, `signaled`, `not_started`, or
`unknown`. `unknown` means the target was admitted but its terminal outcome was
not observed; it is always incomplete and never invents an exit status or
signal. `not_started` is reserved for refusal before target admission.
Observed targets carry wall-clock start and finish times plus a monotonic
duration. Finalization has its own timestamps and duration. Cleanup is complete
only when the owned container, proxy, and target process group are all observed
absent. Forced cleanup is reported independently and does not by itself weaken
a result when absence is proven.

Stream evidence carries the retained path, exact digest, captured byte count,
total observed byte count, and truncation. A truncated stream cannot appear in
a `complete` result. `incomplete` and `refused` results carry one or more stable
uppercase reason codes; `complete` carries none. A refusal cannot invent a
target start.

The result binds declaration, plan, generation, declared and observed image,
runtime-evidence, optional egress-evidence, and executable-provenance identities. Missing observed
identity is permitted only on a refusal or incomplete execution and must remain
empty rather than being copied from declaration authority.

## Evidence publication and verification

The evidence directory leaf MUST NOT exist before execution. Kenogram owns it
descriptor-relatively, never mounts it into the target, creates every entry
without replacement, fsyncs completed files, and publishes `manifest.json`
last. Failure before that final publication leaves no seal and can never be
interpreted as a complete job.

The mandatory inventory is:

```text
declaration.toml
plan.json
provenance.json
request.json
result.json
runtime-before.json
runtime-after.json
stdout.bin
stderr.bin
manifest.json          # written last; does not list itself
```

`egress.json` is mandatory for a complete result when the independently
reprojected plan has a nonempty allowlist and forbidden for a networkless plan.
A refusal before proxy identity exists may omit it; that absence can never be
upgraded to complete. Invalid or unrequested runtime egress output is not
retained as `egress.json`; the sealed result is incomplete with a stable reason
and remains independently replayable. When present, the artifact's digest is
bound by the result identity and manifest content root.

The retained outcome counters are producer observations. Exact diagnostic
events remain bounded and ephemeral because they contain target-authored
destination metadata. Kenogram does not retain a digest of that discarded
snapshot: without the preimage such a value would be an unverifiable producer
claim, not an independently replayable evidence commitment.

Optional target artifacts are copied into the host-owned evidence tree only
after target execution has ended. Manifest entries are unique and strictly
ordered by relative path. Each carries a kind, byte size, and lowercase
SHA-256. The manifest separately binds the request digest, result digest, and a
canonical content-root digest. It is at most 8 MiB and contains no more than
10,032 entries.

When artifacts are requested, the runtime returns open-once readers and
relative paths to the core. The core validates count, aggregate byte, path,
and duplicate bounds, writes them beneath `target-artifacts/`, and publishes a
`target-inventory.json` binding the requested container root and every artifact
digest. Unrequested runtime artifacts make the result incomplete.

The Podman collector freshly re-proves the exact stopped container and resolves
the requested container root against the longest matching runtime mount target.
For a matched bind, it requires the source device and inode retained before
target admission; a workspace source must additionally equal Kenogram's
deterministic private workspace projection. It descriptor-opens that source
inside `podman unshare`. With no matching bind it uses the stopped storage root.
Symlink traversal and ambiguous mount boundaries fail closed. Collection never
changes a declared mount source.

The runtime evidence digest is SHA-256 over the length-prefixed exact
`runtime-before.json` and `runtime-after.json` byte strings. The manifest
content root is SHA-256 over its sorted entries encoded one per line as:

```text
path NUL kind NUL decimal-size NUL sha256-digest LF
```

`verify-job` is an offline verifier. It reopens only descriptor-owned regular
files, recomputes every entry and content-root digest, validates every
versioned document, and cross-checks job/request/result/provenance identities. It never
starts a target, contacts a provider, or upgrades runtime-reported fields to
host-observed facts. For a complete result it strictly decodes both runtime
phases, requires `before` running and `after` stopped, re-derives containment
and resource constraints, cross-binds plan/result/provider identity, and
requires stable facts and mount identities to agree across phases. Generic
JSON cannot substitute for the runtime contract. For declared egress it also
re-derives the canonical allowlist digest, binds the immutable container and
generation, and cross-checks the listener, proxy owner, PID/start identity, and
pinned user/network namespace device and inode identities against the distinct
pre-target runtime admission. It requires the exact system environment key
inventory and `network=none`, and checks readiness/revocation against target
and finalization intervals. Runtime claims cannot strengthen an incomplete
proxy lifecycle into a complete result.

The verifier never adopts a manifest entry size or kind as allocation or work
authority. It classifies the fixed inventory first, parses `request.json` under
its schema byte bound, and only then applies fixed document limits and the
request's stdout, stderr, artifact-count, and artifact-byte limits. Unknown
paths and kinds fail closed before payload reads.

`plan.json` cross-binds its declaration digest and recomputable public evidence
digest. The result retains the declared image reference separately from the
provider-observed immutable image digest. A pinned-reference mismatch is
incomplete evidence and can never verify as complete.

Before publishing the seal, Kenogram fsyncs every descriptor-opened artifact
directory, writes and fsyncs `manifest.json` without replacement, revalidates
that leaf against the opened file identity, and fsyncs the descriptor-owned
evidence root. Pathname replacement cannot redirect the durability proof.

Ergograph and other consumers must independently parse and verify the retained
bytes. They do not import Kenogram packages, and Kenogram does not import their
model, ledger, qualification, or release code.

## K5 direct provider

The direct adapter implements `job.Runtime` / `job.Process` without invoking
the persistent `App.Up`/`Destroy` lifecycle or contacting a Docker-compatible
daemon API. It invokes the Podman CLI with exact argv and proves:

- a fresh bounded generation and immutable observed image digest;
- attached target admission with stdout and stderr connected directly to the
  core's bounded writers;
- an error without a `Process` only before admission; after admission the
  adapter must return a `Process` even when its first observation is malformed,
  so the core never confuses an admitted target with `not_started`;
- an observed terminal result, or `unknown` when that observation is lost;
- runtime-before and runtime-after JSON from independent provider inspection;
- open-once artifact readers after the target is terminal;
- cancellation followed by bounded forced escalation; and
- post-cleanup absence of the container, proxy, and target process group.

The declared image never supplies the inert holder or environment launcher.
The executing Kenogram binary is mounted read-only at the declaration-reserved
`/etc/kenogram/job-exec` path and supplies both. Public and secret environment
values and a one-use lifecycle MAC key cross to that launcher only over a
bounded binary stdin protocol. The key is retained only in host memory and is
not passed to the target environment. The helper launches the target as its
child and writes an authenticated target-local start, finish, duration, and
exit/signal observation. The Podman client envelope is never reported as target
time; absent or invalid lifecycle evidence produces `unknown`. Secret
bytes are read from the uniquely bound declaration-owned regular source after
content revalidation; they never enter provider argv, the provider environment,
or retained evidence. Copy staging is removed immediately after provider copy;
ordinary scratch is removed only after container absence so uncertain ownership
cannot redirect cleanup. The launched target receives exactly the requested
environment and an already-consumed stdin.

Because that helper executes inside the declared image, the Linux Kenogram
binary must be self-contained when the image has no compatible dynamic loader.
The real integration builds with `CGO_ENABLED=0`; release-candidate packaging
must separately prove the distributed Linux artifact is equally self-contained.

Requested artifacts are collected only after the target is terminal and the
container is stopped. A second staged Kenogram helper enters `podman unshare`,
re-proves the immutable container ID and owner label, mounts the stopped root
inside that user namespace, opens the mounted root descriptor-first, rejects
symlinks in every requested-root component, and copies only descriptor-opened regular files
under the requested count, byte, traversal, and caller deadline bounds. It
always attempts an unmount before returning. The adapter does not use an
unbounded `podman cp` as artifact authority.

Every container uses `network=none`. For a nonempty `network.allow`, Kenogram
pins the verified holder's user and network namespace descriptors, revalidates
the immutable ID, owner, PID, and process-start identity, creates one loopback
listener through that pinned authority, and serves it from a host-owned exact
destination proxy. Policy is checked before per-connection DNS resolution;
direct destination routes remain absent. At target termination the proxy policy
is revoked, the listener and active tunnels are closed, and the serve loop joins
before output/artifact finalization. Missing or contradictory `egress.json`
cannot verify a complete result. The target command is absolute and its requested
working directory must equal the declared world workdir, keeping the inspected
configuration and execution authority identical. Declared bind mounts have
their file-or-directory type retained in the plan. Every declared read-only
source is copied under bounded entry and byte limits into a create-only,
Kenogram-owned snapshot beneath a host-private `0700` staging parent. Kenogram
first proves that the source before and after copying and the exact staged copy
share one content-and-mode digest. It then applies `portable-readonly-v1` only
to the staging copy: directories are `0555`; regular files are `0444` plus
`0111` exactly when any source execute bit was present; no projected node has a
write bit. Provider mounts and runtime facts refer to the normalized projection
through a stable `kenogram-snapshot:sha256:...` semantic source rather than its
temporary host pathname. `authority_source` binds the declaration path,
`authority_sha256` binds the exact original content and mode, `sha256` binds the
normalized delivered projection, and `permission_policy` names the transform.
Only declared read-only mounts carry authority and delivered-content digests
for a projection. Each Kenogram-owned ephemeral workspace root is made exact
`0777` beneath the host-private `0700` scratch boundary and carries
`permission_policy = portable-writable-v1`. This grants every declared
contained user authority over the workspace without changing the mode or bytes
of any operator-owned declared writable source. Other mounts carry no
permission policy.
Declared writable mount `source` equals that exact authority path. Workspace
and lifecycle sources use deterministic Kenogram-owned semantic references.
The normalized snapshot and exact workspace-root permission policy are
revalidated immediately before target admission and again before finalization.
A declared writable source may not be the same inode
as, or canonically overlap, any declared read-only source. Target-writable
workspaces intentionally carry no unchanged-content claim and are never
recursively hashed during finalization. The lifecycle channel is one exact
precreated regular file, not a writable directory. Offline verification requires
helper and lifecycle mounts to be files, workspace mounts to be directories,
and each declared mount type to equal the type retained in the plan. The
admitted and retained runtime inventories share the same 512-mount bound.

Before Finalize or cleanup mutates descriptors, processes, provider objects, or
the private scratch, the core joins the required Wait lifecycle. Before cleanup
it also joins the required Finalize lifecycle. An unjoined worker makes cleanup
incomplete and leaves its authority intact.
Cleanup then freshly proves the exact stopped container, owner label,
plan/declaration labels, private scratch identity, and complete workspace mount
inventory. It writes the sorted target/source/device/inode bindings to a
create-only mode-`0600` authority record inside the unmounted scratch root. The
container is re-proved, destroyed by immutable ID, and proved absent. The
parent repeats that immutable-ID absence proof immediately before the exact
staged Kenogram helper enters `podman unshare`; the helper does not recursively
invoke the provider from inside its user namespace. It authenticates the same
ID in the authority record and descriptor-removes immediate
child names from those exact workspace roots. The helper accepts no arbitrary
deletion source and never traverses or mutates a declared writable mount. A
failed namespace pass retains the record and scratch so a later Cleanup call
can retry after container absence. Provider namespace helpers run in a dedicated
process group which is killed and joined at deadline. Cleanup is complete only
after the scratch path is explicitly proved absent. No shell participates.
Planning, content digests, copy staging, read-only snapshotting, and writable
source inspection share one descriptor-rooted, cancellation-aware walker capped
at 20,000 entries, 1 GiB, depth 128, and 4,096 relative-path bytes. Writable
source directories are recursively inspected before provider creation and fail
closed on every socket or other special node. For accessible known Podman and
Docker endpoint paths, device/inode aliases are also rejected. Kenogram does not
claim it can detect an inaccessible bind alias that the host does not expose;
the stronger enforceable policy is that no socket descendant is admitted.
Mounts retain their exact read-only/read-write mode and cannot overlap known
Podman or Docker control sockets. Source and target paths containing comma,
equals, quote, or backslash `--mount` grammar metacharacters are refused before
provider contact. Runtime memory, CPU, PID, user, namespace, capability,
seccomp, image, mount, and ownership facts are independently inspected before
target admission. Cleanup re-inspects both the immutable container ID and the
random ownership label before every destructive stop, kill, unmount, or
removal. Every post-create provider call, cleanup check, and absence proof is
addressed by immutable container ID; a later rename cannot hide an owned
container. A canceled or reply-lost create is reconciled under a fresh bounded context,
and Kenogram never deletes a name whose identity or ownership has changed.

Podman reserves exit statuses for provider/invocation failures, so the adapter
does not interpret its client's status as a target status. The authenticated
target-local lifecycle record preserves the full 0–255 target exit range and
signals separately. A provider/client failure or unauthenticated lifecycle
record remains `unknown`.

The core treats every returned field as untrusted: malformed target, cleanup,
runtime, artifact, or identity evidence is refused or downgraded. Unit tests
prove provider-hostile behavior without platform claims. The opt-in Linux
integration owns real rootless Podman enforcement evidence for exact image,
success and nonzero exit, timeout and orphan cleanup, network-none, read-only
mounts, secret delivery, artifact extraction from declared target write
authority, portable workspace writes as mapped root and the keep-id user, and
runtime-socket absence.
Darwin compilation and CLI handoff are not evidence of local macOS namespace
enforcement; without a configured Linux machine those facts remain unknown and
the adapter refuses admission.
