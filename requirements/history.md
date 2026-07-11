# History contract

Status: implemented.

Each world has an append-only, fsync'd `history.jsonl`. Records use UTC RFC3339,
carry plan and declaration digests, image digests, workspace root digest, action,
and observed outcome, and form a SHA-256 hash chain. This is operator-facing
tamper evidence, not an authority ledger.

`applied.toml` is written only after runtime evidence verifies the generation.
Restart recovery combines the declaration, verified history, and observed runtime
state; uncertain state is surfaced rather than invented.

Lockfiles record both PID and kernel process start time. A crash-stale lock is
reclaimed without confusing a reused PID for the original mutator.
