# Readiness wrapper semantic reference

Kenogram currently commits an autostart service after its supervisor reports
`supervised` or the command exits successfully. That is acknowledgement, not
application readiness. This page records proposed semantic evidence for a
possible later **transactional cutover readiness** contract. It does not add a
declaration field, status field, health monitor, or supported readiness API.

The motivating K-Live observation is concrete: an OpenClaw gateway was already
supervised and its loopback `/readyz` surface answered before its messaging
adapter reported connected. A later channel-status action reported connected
without another replacement. Premature cutover is therefore a demonstrated
state distinction, although message loss from that distinction has not been
established.

## What the fixture fixes in place

[`TestReadinessWrapperSemanticReference`](../../internal/app/readiness_semantics_test.go)
inserts today's polling-wrapper technique at Kenogram's real
`services-started` checkpoint. The existing `App`, transition record, runtime
fixture, network-door process, rollback, commit, and SIGKILL recovery paths then
perform the authority handoff:

1. Supervisor acknowledgement does not satisfy a declared action. A delayed
   provider-facing action succeeds after the real `startServices` call and
   before successor verification and commit. Because every configured service
   has already been started at this checkpoint, the fixture does not prove
   ordering or gating between services.
2. A never-ready successor is removed; the predecessor and its network door are
   restored through the ordinary rollback path. Because there is no supported
   readiness gate yet, the fixture routes a negative result into the next real
   successor-verification failure. This proves the selected pre-commit placement
   and rollback cleanup, not production wiring from an action result to an
   `App.Up` error.
3. The proposed rule is that success before commit is not durable authority. If
   the applying process dies at that point, recovery restores the predecessor
   **without** rerunning the action. A later explicit apply reruns it against the
   new candidate. This is selected design evidence, not behavior exposed by a
   readiness API today.
4. The action's policy analogue derives its immutable exact `host:port` set from
   the candidate's resolved `network.allow` plan. Asking for an undeclared
   destination reaches no provider and cannot add an allowance. This is not
   traffic through Kenogram's real proxy; proxy enforcement remains separate
   rootless integration evidence. The rollback cases independently prove that
   App restores its existing proxy-door process for the predecessor.
5. The reference wrapper has a 150 ms total timeout, 5 ms retry cadence, five-
   attempt ceiling, and 192-byte retained diagnostic-text ceiling. Deadline or
   parent cancellation wins over a simultaneously available success, including
   on the final attempt. The complete serialized observation is not claimed to
   fit within 192 bytes. These values make the proof fast; they establish the
   need for hard bounds, not production defaults. The in-memory fixture action
   honors context cancellation; it does not prove production process-tree
   termination or bounded stdout/stderr ingestion.
6. The exact operator-declared command is preserved in the result. Provider
   invocation mutates a counter deliberately: this is a bounded action that may
   affect provider or world state, never a claim of passive observation.
7. No action follows today's real acknowledgement-only path. Existing
   declarations acquire no new lifecycle condition.

Run the hermetic proof with:

```sh
make proof-readiness
```

The provider and declaration-derived policy decision are deterministic local
fixtures. The transaction, proxy-door lifecycle, and process-death portions
reuse Kenogram's existing lifecycle fixtures; this proof does not duplicate the
rootless Podman proxy-enforcement evidence already indexed for implemented
boundaries.

## Deliberately open design questions

- Whether readiness belongs to one service, a relation among services, or the
  whole candidate generation.
- Declaration syntax, command representation, production bounds, and whether
  retries are fixed or operator-selected within hard limits.
- How multiple actions compose and which failures are retryable.
- Whether and how one service's readiness gates another service's startup.
- Whether any bounded historical result belongs in durable state or `status`.
  A past success must never be rendered as current health.
- Redaction rules for action output and whether output should be retained at all.
- Process-group termination and bounded stdout/stderr ingestion when an action
  ignores cancellation or forks descendants.
- Adoption, stopped-generation restart, and manual-service behavior.

Continuous health monitoring, automatic reaction to later degradation, and
application-specific interpretation remain outside this design.
