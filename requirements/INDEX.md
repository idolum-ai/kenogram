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

## Evidence and open boundaries

| Contract | Strongest automated evidence | Boundary still open |
|---|---|---|
| Declaration and plan | Unit, parser seeds, scheduled fuzzing, canonical digest, strict names, and staged-byte recheck | Snapshot-grade handling of an adversarially mutating source tree |
| Operations | Signal-aware CLI, transition recovery tests, Engram v0.3.0 lifecycle E2E, and OpenClaw/Hermes compositions | Exhaustive CLI fault matrix and concurrent mutation stress |
| Security | Exact Podman argv/mount-inode/seccomp evidence, rootless preflight, secret failure canaries, OpenClaw/Hermes absence checks, runtime-socket E2E | Seccomp profile identity and a supported Podman/kernel matrix |
| Network | Multi-megabyte CONNECT, per-connection resolution, removal/expiry closure, Git/TLS fixture, and rootless integration | Full ten-invariant replay after every adoption path |
| Lifecycle | Durable rollback/commit transition, recovery unit tests, service acknowledgement, Engram E2E, and isolated OpenClaw/Hermes replacement | Every-action/write failpoint matrix and SIGKILL-at-each-phase campaign |
| History | Tamper/truncated-tail unit tests plus E2E tombstone outcomes | Power-loss testing on multiple filesystems |

## Executable checks

- `make test` runs unit, contract, and parser fuzz-seed tests.
- `make test-evidence` retains structured test events and a coverage profile.
- `make integration` runs the rootless Podman boundary contract and is mandatory in CI.
- `make e2e-release` proves the checksum-pinned Engram `v0.3.0` lifecycle.
- `make e2e-openclaw` proves checksum-pinned OpenClaw `2026.6.11` isolation and TUI use.
- `make e2e-composition` proves fake Telegram text through Engram and the
  isolated OpenClaw TUI, plus attachment ingestion into its workspace.
- `make e2e-hermes` proves checksum-pinned Hermes Agent `v2026.7.7.2` isolation, native fake
  Telegram, lifecycle, and TUI use.
- `make e2e-hermes-composition` proves fake Telegram text through Engram and the
  isolated Hermes TUI, plus attachment ingestion into its workspace; `make e2e`
  runs all five.
- `make e2e-telegram-canary` is an operator-assisted, protected-environment
  proof of the real Telegram path and is never a pull-request gate.
- `make architecture` checks required files and package dependency direction.
- `make stdlib-only` rejects third-party Go modules.
- `make check` runs the fast local quality gate; runtime proofs remain separate.
