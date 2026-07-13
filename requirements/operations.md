# Operations contract

Status: binding contract. Evidence and open boundaries are indexed in `INDEX.md`.

`kenogram up --dry-run <file>` parses, validates, resolves, and renders a plan
without changing runtime or durable world state. `--json` emits one JSON object.
The default text form is deterministic for identical semantics except that it
also reports the byte-sensitive declaration digest.

| Command | Effect | Mutates |
|---|---|---|
| `up --dry-run <file>` | validate and render intent | no |
| `up --yes <file>` | reconcile and apply one generation | yes |
| `down <world>` | stop the active generation | yes |
| `destroy --yes <world>` | remove runtime and tombstone history | yes |
| `enter [--repair] <world>` | attach to the world | world processes may |
| `status <world>` / `worlds` | report recorded and observed state | no |
| `allow … --for <duration>` | grant temporary destination access | yes |
| `revoke <world> <destination>` | remove access and close admitted connections | yes |
| `repair-history --yes <world>` | remove one proven truncated final fragment | yes |

`up` renders the full successor plan, exact semantic changes, and workspace
drift before an interactive confirmation or `--yes`. It stages and verifies the
successor before recording it applied. `down`, `destroy`, `enter --repair`,
`status`, `allow`, and `worlds` operate only from host-side state. `version`
reports build provenance.

`status` names the authoritative generation and, while a durable transition
exists, the candidate generation and recovery phase separately. `enter
--repair` resolves authority from the same transition record. Applying an
unchanged declaration reconciles the live network door to its declared
destinations and clears temporary grants; process liveness alone is not policy
evidence. Destinations accepted by `allow` and `revoke` use canonical
`host:port` syntax, including brackets around IPv6 addresses, with no URL
userinfo, path, query, or fragment.

`allow`, `revoke`, and `repair-history` reject mutation while a durable
transition is unresolved. The operator must first recover it with `up`, `down`,
or `destroy`. `status --json` preserves the `state` and `runtime_evidence`
aliases while also reporting authoritative and candidate observations; during
recovery, its declaration and state provenance is `transition.json`.

Parse, validation, or runtime failures use exit status 1. CLI usage or missing
confirmation uses status 2.

Durable state is under `$XDG_DATA_HOME/kenogram/worlds`, normally
`~/.local/share/kenogram/worlds`; `KENOGRAM_STATE_DIR` overrides it. After an
interrupted `up`, run the same `up` again: the transition record is reconciled
under the world lock before new work begins. If reconciliation cannot establish
one authority, Kenogram stops and preserves the record rather than guessing.
Use `status`, then `enter --repair`, to inspect a world; never edit state files
while an operation is running.
