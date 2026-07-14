# Operations contract

Status: binding contract. Evidence and known limits are indexed in `INDEX.md`.

`kenogram up --dry-run <file>` parses, validates, resolves, and renders a plan
without changing runtime or durable world state. `--json` emits one JSON object.
The default text form is deterministic for identical semantics except that it
also reports the byte-sensitive declaration digest.

| Command | Effect | Mutates |
|---|---|---|
| `up --dry-run <file>` | validate and render intent | no |
| `up --yes <file>` | reconcile and apply one generation | yes |
| `down <world>` | stop the active generation | yes |
| `destroy --yes <world>` | remove all recorded generations and tombstone history | yes |
| `enter [--repair] <world>` | attach to the world | world processes may |
| `status <world>` / `worlds` | report recorded and observed state | no |
| `allow … --for <duration>` | grant temporary destination access | yes |
| `revoke <world> <destination>` | remove access and close admitted connections | yes |
| `repair-history --yes <world>` | remove one proven truncated final fragment | yes |
| `doctor [--json]` | report all host prerequisites and image-entry notes | no |

`up` renders the full successor plan, exact semantic changes, and workspace
drift before an interactive confirmation or `--yes`. It stages and verifies the
successor before recording it applied. `down`, `destroy`, `enter --repair`,
`status`, `allow`, and `worlds` operate only from host-side state. `version`
reports build provenance.

`doctor` observes Linux, cgroups v2, the rootless Podman executable and info
surface, subordinate ID mappings, `nsenter`, state storage, and the rootless
container graph root. It reports every check even when one fails and exits 1 if
any required observation fails. Informational checks distinguish the `/bin/sh`
repair-entry surface from the tmux-backed normal-entry surface without claiming
to inspect an image that has not been declared. `doctor --json` emits the stable
`ready` boolean and ordered `checks` objects with `name`, `status`, `observed`,
and optional `remediation` fields.

`status` names the authoritative generation and, while a durable transition
exists, the candidate generation and recovery phase separately. `enter
--repair` resolves authority from the same transition record. Applying an
unchanged declaration reconciles the live network door to its declared
destinations and clears temporary grants; process liveness alone is not policy
evidence. Destinations accepted by `allow` and `revoke` use canonical
`host:port` syntax, including brackets around IPv6 addresses, with no URL
userinfo, path, query, or fragment.

`allow`, `revoke`, and `repair-history` reject mutation while a durable
transition is unresolved. `up` and `down` first converge the recorded authority;
confirmed `destroy` instead removes every generation named by the transition.
`status --json` preserves the `state` and `runtime_evidence`
aliases while also reporting authoritative and candidate observations; during
recovery, its declaration and state provenance is `transition.json`.

Parse, validation, or runtime failures use exit status 1. CLI usage or missing
confirmation uses status 2.

Durable state is under `$XDG_DATA_HOME/kenogram/worlds`, normally
`~/.local/share/kenogram/worlds`; `KENOGRAM_STATE_DIR` overrides it. After an
interrupted `up`, run the same `up` again: the transition record is reconciled
under the world lock before new work begins. If reconciliation cannot establish
one authority, Kenogram stops and preserves the record rather than guessing.
Use `status`, then `enter --repair`, to inspect a world. If the world is no
longer wanted, `destroy --yes` remains available without recovery. Never edit
state files while an operation is running.
