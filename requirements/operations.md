# Operations contract

Status: implemented.

`kenogram up --dry-run <file>` parses, validates, resolves, and renders a plan
without changing runtime or durable world state. `--json` emits one JSON object.
The default text form is deterministic for identical semantics except that it
also reports the byte-sensitive declaration digest.

`up` renders the full successor plan, exact semantic changes, and workspace
drift before an interactive confirmation or `--yes`. It stages and verifies the
successor before recording it applied. `down`, `destroy`, `enter --repair`,
`status`, `allow`, and `worlds` operate only from host-side state. `version`
reports build provenance.

Parse, validation, or runtime failures use exit status 1. CLI usage or missing
confirmation uses status 2.
