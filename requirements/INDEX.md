# Kenogram Requirements Index

Status: draft and binding for implementation.

Requirements describe observable contracts. Each implemented contract is tested
directly or checked by `make check`. Planned documents constrain later work but do
not imply that behavior exists.

The project's name and its carefully limited relationship to Rudolf Kaehr's
work are documented in [`../docs/kenogrammatics.md`](../docs/kenogrammatics.md).

## Implemented contracts

1. [`declaration.md`](declaration.md) — accepted syntax, schema, and validation.
2. [`plan.md`](plan.md) — resolution, canonical encoding, provenance, and output.
3. [`operations.md`](operations.md) — current CLI behavior and honest failure modes.
4. [`security.md`](security.md) — input, path, secret, and runtime handling.

5. [`network.md`](network.md) — ten normative absence and proxy invariants.
6. [`lifecycle.md`](lifecycle.md) — materialization and binary replacement contract.
7. [`history.md`](history.md) — durable state, evidence, and hash-chain contract.

## Executable checks

- `make test` runs unit, contract, and parser fuzz-seed tests.
- `make integration` runs the rootless Podman boundary contract and is mandatory in CI.
- `make architecture` checks required files and package dependency direction.
- `make stdlib-only` rejects third-party Go modules.
- `make check` runs the complete local quality gate.
