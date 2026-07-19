# Kenogram Requirements Index

Status: binding contract index.

Requirements describe observable contracts. Evidence is recorded separately
from the contract so code existence, test coverage, and real-boundary proof are
never collapsed into one status word.

The project's name and its carefully limited relationship to Rudolf Kaehr's
work are documented in [`../docs/kenogrammatics.md`](../docs/kenogrammatics.md).

## Binding contracts

1. [`declaration.md`](declaration.md) — accepted syntax, schema, and validation.
2. [`plan.md`](plan.md) — resolution, canonical encoding, provenance, and output.
3. [`operations.md`](operations.md) — current CLI behavior and honest failure modes.
4. [`security.md`](security.md) — input, path, secret, and runtime handling.

5. [`network.md`](network.md) — ten normative absence and proxy invariants.
6. [`lifecycle.md`](lifecycle.md) — materialization and binary replacement contract.
7. [`history.md`](history.md) — durable state, evidence, and hash-chain contract.

## Evidence and known limits

The final column says how an unproved boundary constrains the promise. **Accepted
for v0.x** means it is useful next evidence, not an incomplete advertised
feature. **Before stable** means the project should earn it before making a
production-stability claim. **Experimental** means the surface is available for
evaluation but outside the supported Linux runtime promise.

| Contract | Strongest automated evidence | Known limit / next proof | Release posture |
|---|---|---|---|
| Declaration and plan | Unit, parser seeds, scheduled fuzzing, canonical digest, strict names, and staged-byte recheck | Snapshot-grade handling of an adversarially mutating source tree | Accepted for v0.x |
| Operations | Signal-aware CLI, complete host doctor, fail-closed bounded workspace inspection, transition recovery tests, ownership-aware E2E image cleanup, rootless-vfs capacity policy, SSH loopback composition, Engram v0.3.0 lifecycle E2E, and OpenClaw/Hermes compositions | Historical locus attribution across plan changes, exhaustive CLI fault matrix, and concurrent mutation stress | Before stable |
| Security | Exact Podman argv/mount-inode/seccomp evidence, rootless preflight, secret failure canaries, OpenClaw/Hermes absence checks, runtime-socket E2E | Seccomp profile identity and a supported Podman/kernel matrix | Before stable |
| Network | Multi-megabyte CONNECT, per-connection resolution, removal/expiry closure, declaration reconciliation, identity-bound proxy readiness, bounded current-generation refusal/dial-failure diagnostics with privacy canaries, Git/TLS fixture, declared loopback SSH without a host listener, and rootless integration | Full ten-invariant replay after every adoption path | Before stable |
| Lifecycle | Durable rollback/commit transition, persisted-runtime 15-boundary SIGKILL recovery-only matrix, stopped-commit restart, terminal transition destruction, replay-safe service acknowledgement, Engram E2E, and isolated OpenClaw/Hermes replacement | Syscall-granular power-loss testing and exhaustive non-`up` action failpoints | Before stable |
| History | Tamper/truncated-tail unit tests plus E2E tombstone outcomes | Power-loss testing on multiple filesystems | Before stable |
| Experimental Apple transport | Canonical shell-inert argv envelope, explicit stdin/TTY flags, remote exit-status preservation, graceful signal forwarding, Darwin/arm64 cross-build, and native macOS launcher smoke test | Real Apple machine argv/TTY/signal proof, nested rootless Podman, and the full lifecycle/network matrix | Experimental |

### Design evidence outside the binding contract

| Question | Executable semantic evidence | Deliberately not implemented |
|---|---|---|
| Transactional cutover readiness | `make proof-readiness` inserts a polling action with bounded time, attempts, cadence, and diagnostic text at the real `services-started` lifecycle checkpoint; it exercises delayed success before dependent behavior and commit, a negative-result placement followed by ordinary rollback cleanup and proxy-door restoration, pre-commit SIGKILL recovery and explicit rerun, a declaration-derived policy analogue, and acknowledgement-only compatibility | Real-proxy traffic in the readiness fixture (covered separately by network integration), production wiring from a negative action result to `App.Up`, declaration schema, durable readiness fields, status presentation, production bounds, and continuous health |

## Executable checks

- `make test` runs unit, contract, and parser fuzz-seed tests.
- `make test-evidence` retains structured test events and a coverage profile;
  CI uploads the available evidence even when the evidence command fails.
- `make integration` runs the rootless Podman boundary contract and is mandatory
  in CI for implementation changes, pushes to `main`, and releases. Editorial-
  only pull requests retain the architecture, documentation-freshness, secret,
  and workflow gates without replaying unchanged runtime evidence. That
  classification is authoritative only in the organization-ruleset workflow,
  whose policy comes from its protected workflow revision.
- `make proof-readiness` runs proposed semantic evidence for issue #26 through
  the existing lifecycle fixture. It is not evidence that declarations
  currently accept readiness fields.
- `make e2e-ssh` proves a key-authenticated SSH stream to a declared world-
  loopback interface without a published host port.
- `make e2e-release` proves the checksum-pinned Engram `v0.3.0` lifecycle.
- `make e2e-openclaw` proves checksum-pinned OpenClaw `2026.6.11` isolation and TUI use.
- `make e2e-composition` proves fake Telegram text through Engram and the
  isolated OpenClaw TUI, plus attachment ingestion into its workspace.
- `make e2e-hermes` proves checksum-pinned Hermes Agent `v2026.7.7.2` isolation, native fake
  Telegram, lifecycle, and TUI use.
- `make e2e-hermes-composition` proves fake Telegram text through Engram and the
  isolated Hermes TUI, plus attachment ingestion into its workspace; `make e2e` runs all six.
- `make e2e-telegram-canary` is an operator-assisted, protected-environment
  proof of the real Telegram path and is never a pull-request gate.
- Container-heavy E2Es lease random world names and snapshot image references
  and IDs. They verify identities before removal or exact untagging, never force image removal,
  collect bounded cleanup failures, and reject undersized
  rootless Podman `vfs` stores before pulling. Unmeasured `vfs` lanes require an
  explicit local floor; unit contracts use fake responses and capacity probes.
- `make architecture` checks required files and package dependency direction.
- `make stdlib-only` rejects third-party Go modules.
- `make check` runs the fast local quality gate; runtime proofs remain separate.
