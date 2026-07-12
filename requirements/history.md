# History contract

Status: binding contract. Evidence and open boundaries are indexed in `INDEX.md`.

Each world has an append-only, fsync'd `history.jsonl`. Records use UTC RFC3339,
carry plan and declaration digests, image digests, workspace root digest, action,
and observed outcome, and form a SHA-256 hash chain. This is operator-facing
tamper evidence, not an authority ledger.

Verification fails closed. `repair-history --yes` may remove only a malformed,
non-newline-terminated final fragment after the complete prefix verifies; it
cannot heal a complete hash mismatch or interior corruption, and records the
repair as the next chained event.

Digests identify the exact evidence carried by a record. They establish
provenance; they are not a claim that byte equality exhausts the meaning or
behavior of a world.

`applied.toml` is written only after runtime evidence verifies the generation.
Restart recovery combines the declaration, verified history, and observed runtime
state; uncertain state is surfaced rather than invented.

Lockfiles record both PID and kernel process start time. A crash-stale lock is
reclaimed without confusing a reused PID for the original mutator.
