# Governed job contract

Status: binding pre-implementation contract. The schemas and independent Go
semantic validators are implemented; `kenogram job` and `kenogram verify-job`
are deliberately unavailable until the execution and evidence-publication
boundary earns real-runtime proof.

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
  that produced the observation.

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
failure. Exit 1 means a typed `refused` or `incomplete` result was sealed. Exit
2 means the invocation or request was invalid before a trustworthy job identity
could be established and need not emit a job result. Diagnostics use stderr
and are never part of the machine result.

Until execution lands, all three command forms remain unavailable and no
consumer may treat the existence of these schemas as execution evidence.

## Request authority and bounds

The caller supplies a request no larger than 1 MiB. It contains:

- a 1–63 byte portable job identifier;
- an absolute declaration path and exact lowercase SHA-256 binding;
- 1–128 nonempty UTF-8 argv values, each at most 4,096 bytes;
- an absolute, canonical container working directory;
- at most 128 unique environment names, each bound either to an explicitly
  retained public value or an in-world declaration-owned secret-file path;
- a target timeout from 1 ms through 24 hours;
- a finalization timeout from 1 ms through 10 minutes;
- independent stdout and stderr capture limits up to 64 MiB each; and
- optionally, an absolute container artifact root bounded to 10,000 entries and
  1 GiB of ordinary-file content.

`public_value` is retained authority and therefore MUST NOT contain a secret.
`secret_file` names an absolute in-world file delivered by a declaration-owned
secret copy; the executor must prove that binding before reading it, inject the
exact non-NUL file bytes up to 16 KiB, and never duplicate those bytes into the
request, plan, result, output metadata, or provenance.
Target-owned stdout, stderr, and artifacts can disclose bytes available to the
target; retaining those outputs is an explicit caller decision, not a claim
that Kenogram can stop a target from printing its own secrets.

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

The target result is exactly one of `exited`, `signaled`, or `not_started`.
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
runtime-evidence, and executable-provenance identities. Missing observed
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

Optional target artifacts are copied into the host-owned evidence tree only
after target execution has ended. Manifest entries are unique and strictly
ordered by relative path. Each carries a kind, byte size, and lowercase
SHA-256. The manifest separately binds the request digest, result digest, and a
canonical content-root digest. It is at most 8 MiB and contains no more than
10,032 entries.

`verify-job` is an offline verifier. It reopens only descriptor-owned regular
files, recomputes every entry and content-root digest, validates all four
documents, and cross-checks job/request/result/provenance identities. It never
starts a target, contacts a provider, or upgrades runtime-reported fields to
host-observed facts.

Ergograph and other consumers must independently parse and verify the retained
bytes. They do not import Kenogram packages, and Kenogram does not import their
model, ledger, qualification, or release code.
