# Transactional cutover readiness

Status: design proposal for review; not a binding contract, accepted declaration
surface, or implementation commitment.

A cutover-readiness action is a host-declared, bounded command that runs inside
one candidate world after its autostart services acknowledge and before that
generation becomes authoritative. Success is historical cutover evidence, not
current health or truth.

## Why this belongs to the generation

The [readiness semantic reference](compositions/readiness-wrapper.md) places a
provider-facing action after the complete `startServices` step and before
successor verification and commit. It does not prove service dependencies or
inter-service startup gating. The motivating OpenClaw observation is likewise a
relation among a gateway and its provider adapter, not a property of one
process.

The proposed gate therefore belongs to the candidate generation. It decides
only whether Kenogram may transfer authority from the predecessor to that
candidate. It does not:

- delay one service until another is ready;
- monitor the generation after commit;
- prevent effects produced by the candidate before commit; or
- imply that a past observation remains true.

"Transactional" describes Kenogram's authority handoff. A readiness command
may change provider or world state; Kenogram cannot roll those external effects
back.

## Proposed declaration surface

Readiness is proposed for a new declaration schema version so that an older
binary rejects the new authority before mutation instead of silently ignoring
it:

```toml
version = 2
name = "openclaw"

[[cutover.readiness]]
name = "adapter-connected"
command = ["/usr/local/bin/openclaw-channel-ready"]
timeout_ms = 30000
interval_ms = 500
```

A binary that supports version 2 would continue to accept version 1 with
unchanged semantics and canonical plan bytes. Each declared readiness action is
required; absence of `[[cutover.readiness]]` means there is no readiness gate.
There is no `required = false` mode.

Each action has:

- a unique portable `name`;
- a non-empty direct argument vector in `command`, never implicit shell text;
- a total `timeout_ms`; and
- a retry `interval_ms`.

An operator who needs shell behavior must place a reviewed executable script in
the world and declare that script directly. Actions run sequentially, in
declaration order, inside the candidate world as `world.user`, from
`world.workdir`, with closed stdin, no TTY, and the world's existing environment.
They gain no readiness-specific mount, secret, environment variable, network
destination, interface, capability, or privilege.

Exit status zero says only that the declared predicate held for that attempt.
A non-zero status is retryable within the declared bounds. Failure to start the
command or observe its exit is an infrastructure failure, not a negative
predicate result.

## Mandatory bounds

The exact numbers remain reviewable, but a production contract must impose
non-expandable ceilings:

| Quantity | Proposed accepted range | Hard ceiling |
|---|---:|---:|
| Actions per generation | 1–8 | 8 |
| Timeout per action | 1,000–120,000 ms | 120 s |
| Retry interval | 100–10,000 ms | 10 s |
| Attempts per action | derived from time and interval | 128 |
| Combined stdout and stderr read | fixed | 64 KiB |
| Operator-visible failure excerpt | fixed | 4 KiB |

The first attempt is immediate; later attempts wait the declared interval. An
action ends at the first zero exit, deadline, 128th attempt, parent
cancellation, infrastructure failure, or output limit. Deadline or cancellation
wins over a simultaneously observable success, including on the final attempt.
Exceeding the stream limit is an `output_limit` failure, never a truncated
success.

Kenogram must terminate the complete readiness process group. If it cannot
prove termination, it must stop and remove the candidate before restoring the
predecessor. The ceilings themselves, rather than merely their defaults, are
part of the contract.

## Authority and information flow

The host-authored declaration authorizes the command. Its stdout and stderr are
untrusted world-authored bytes: they may contain provider data, credentials,
instructions, terminal control bytes, or invalid UTF-8. They never become
authority.

The normal `up` result retains only the action name, outcome, attempt count,
byte count, truncation state, and a cryptographic hash of observed output. Raw
output does not enter state, transition records, history, generated files,
`status`, composition output, or ordinary JSON.

An explicit apply-time diagnostic option may expose at most 4 KiB. Text output
uses visible ASCII byte escapes; JSON uses base64 plus an untrusted-data label.
The option affects presentation only and cannot enlarge the read ceiling or the
durable record.

Readiness inherits the candidate's declared network policy. It cannot add a
destination. A real-proxy integration test must prove that an action cannot
reach an undeclared endpoint.

## Lifecycle placement

For a replacement, the proposed sequence is:

1. Parse, validate, resolve, and confirm the version 2 plan.
2. Persist the rollback transition and workspace evidence.
3. Stop the predecessor.
4. Start the candidate and verify its runtime boundary and network door.
5. Start every autostart service and receive the existing acknowledgement.
6. Run the declared readiness actions.
7. Reinspect the candidate runtime and network door.
8. Persist the commit transition, including a bounded readiness summary.
9. Write successor authority and clean up the predecessor.

Any readiness failure enters the existing rollback path. Neither the command
nor its output may edit host authority.

## Crash, recovery, and rerun rules

Recovery restores durable authority; it never calls a provider-facing readiness
action.

| Event | Recovery result | Automatic rerun |
|---|---|---|
| Cancellation or process death while an action is pending | Restore predecessor | No |
| Death after success but before commit is durable | Restore predecessor | No |
| Death after commit is durable | Restore successor | No |
| Ordinary readiness failure | Restore predecessor | No |
| Later explicit apply creates or reactivates a candidate | Normal apply semantics | Yes |
| Adoption of an unchanged, verified running generation | Keep authority | No |
| Supervisor restart in the authoritative generation | Keep authority | No |
| Recovery restarts the predecessor | Restore authority | No |

This favors at-most-once execution within a single interrupted transaction. It
does not claim exactly-once external effects.

## Durable evidence and status

A success before commit exists only in memory. Once commit becomes durable, the
transition and state may contain a bounded summary such as:

```json
{
  "scope": "historical-cutover",
  "generation": 4,
  "outcome": "satisfied",
  "observed_at": "2026-07-19T00:00:00Z",
  "actions": [
    {"name": "adapter-connected", "outcome": "ready", "attempts": 3}
  ],
  "current_health": "unknown"
}
```

The timestamp is a host observation, not a plan input. Raw output is absent.
`status` may show the summary only for the authoritative generation and must
label it exactly as historical cutover evidence with current health unknown. It
must never rerun an action. A version 1 generation reports readiness as
`not_declared`; a failed candidate never replaces the predecessor's status.
Failure history may retain action names and coarse outcomes but not output.

## Canonical plans and compatibility

Version 1 parsing, plan bytes, plan digests, dry-run output, and lifecycle
behavior must remain byte-for-byte unchanged. In version 2, readiness is an
ordered plan structure; every accepted field affects the canonical plan,
digest, and diff. An older binary must reject version 2 before mutation, and a
newer binary must fail closed on unsupported readiness evidence. Golden and
downgrade tests must enforce both directions.

## Non-goals

This proposal does not introduce:

- continuous health monitoring or reaction to later degradation;
- a service dependency graph or inter-service startup gating;
- application-specific output parsing in Kenogram;
- readiness-specific capabilities, secrets, mounts, or network grants;
- automatic execution during recovery, adoption, or `status`;
- rollback of external provider effects; or
- a claim that readiness proves safety, correctness, or present health.

## Evidence required before acceptance

An implementation is not complete until it proves:

1. strict version 2 parsing, canonical ordering and diffs, downgrade rejection,
   and unchanged version 1 golden bytes;
2. delayed success at the real lifecycle checkpoint;
3. every time, attempt, output, and cancellation bound, including precedence at
   boundary races;
4. exact predecessor runtime, workspace, service, and proxy restoration after
   every readiness failure class;
5. process death while pending, after success, during commit, and after each new
   durable write;
6. output floods, invalid UTF-8, terminal controls, and forked descendants
   without secret or instruction leakage;
7. undeclared-destination refusal through the real rootless proxy;
8. ordered multiple actions and partial success without claiming external
   rollback;
9. the complete rerun table above;
10. bounded deterministic text, JSON, history, and status presentation; and
11. unchanged behavior for declarations without readiness.

## Proposed implementation sequence

The architecture is intentionally separable into reviewable changes:

1. schema version 2, plan representation, compatibility, and golden evidence;
2. bounded process executor and adversarial stream/process-tree evidence;
3. lifecycle, state, rollback, and crash-recovery integration;
4. `up`, diagnostic, history, and `status` presentation; and
5. real rootless network and composition evidence.

This document becomes binding only after review resolves the schema version,
numeric ceilings, diagnostic opt-in, and durable summary. Until then, the
version 1 declaration and lifecycle requirements remain the complete supported
contract.
