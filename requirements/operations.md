# Operations contract

Status: M1 implemented; lifecycle commands planned.

`kenogram up --dry-run <file>` parses, validates, resolves, and renders a plan
without changing runtime or durable world state. `--json` emits one JSON object.
The default text form is deterministic for identical semantics except that it
also reports the byte-sensitive declaration digest.

At M1, `up` without `--dry-run` exits unsuccessfully after planning and states
that materialization is not implemented. It must never suggest that a world was
created. `version` reports build provenance. `help` identifies implemented and
planned commands.

Parse or validation failures use exit status 1. CLI usage failures use status 2.
