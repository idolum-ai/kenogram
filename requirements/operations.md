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
| `connect <world> <interface>` | relay stdin/stdout to one declared loopback stream | world processes may |
| `status <world>` / `worlds` | report recorded and observed state | no |
| `allow … --for <duration>` | grant temporary destination access | yes |
| `revoke <world> <destination>` | remove access and close admitted connections | yes |
| `repair-history --yes <world>` | remove one proven truncated final fragment | yes |
| `doctor [--json]` | report all host prerequisites and image-entry notes | no |

`up` renders the full successor plan and exact semantic changes before an
interactive confirmation or `--yes`. A quiescent predecessor also renders exact
workspace drift. A verified running predecessor is reported as live authority:
its workspace may advance while Kenogram stages the successor, so its stable
handoff tree is captured only after it stops. `up` stages and verifies the
successor before recording it applied. A predecessor is new only when state,
applied artifacts, authoritative history, runtime-proxy artifacts, recorded
digests, staged generation artifacts, and carried workspace entries are all
absent. Empty `0700` mount roots derived from the candidate's declared workspace
paths are deterministic Kenogram scaffolding, not carried entries. Failure-only
history, including a recorded repair of its proven truncated tail, is retained
without inventing authority. Unreadable
or inconsistent plan, state, declaration, history, or canonical workspace-digest
evidence fails before confirmation. The
reviewed predecessor evidence is revalidated under the world mutation lock
before application; if that lock guards transition recovery, the recovered
authority must reproduce the reviewed changes and workspace classification
before application continues. Immediately before successor start, after any
predecessor has stopped, Kenogram must capture and fsync a stable canonical
workspace tree. New and inactive worlds must preserve every reviewed workspace
entry exactly; only missing empty mount roots for the reviewed candidate may be
added before cutover. A verified active predecessor may advance its workspace
until stop; that final tree is authoritative handoff evidence, not a claim that
its bytes were frozen at confirmation. Operators requiring byte-exact review first run
`down`, then review and apply. `down`, `destroy`, `enter --repair`,
`status`, `allow`, and `worlds` operate only from host-side state. `version`
reports build provenance.

`doctor` observes Linux, the required cgroups v2 CPU/memory/PID controllers,
the rootless Podman executable, user namespace, and info surface, subordinate
ID mappings, `nsenter`, effective state-directory access and free space, and the
rootless container graph root. The user-namespace observation executes exactly
`podman unshare true`. It reports every named check even when an earlier
observation blocks it and exits 1 if
any required observation fails. Informational checks distinguish the `/bin/sh`
repair-entry surface from the tmux-backed normal-entry surface without claiming
to inspect an image that has not been declared. It does not mutate Kenogram
worlds or durable state, although Podman may initialize its own rootless runtime
metadata while answering `podman info`. `doctor --json` emits `ready` and an
additive set of `checks` objects with stable `name`, `pass`/`fail`/`info`
`status`, `observed`, and optional `remediation` fields. Consumers select checks
by name; order and the complete set of names are not a versioned API in v0.x.

`status` names the authoritative generation and, while a durable transition
exists, the candidate generation and recovery phase separately. `enter
--repair` resolves authority from the same transition record. Applying an
unchanged declaration reconciles the live network door to its declared
destinations and clears temporary grants; process liveness alone is not policy
evidence. Destinations accepted by `allow` and `revoke` use canonical
`host:port` syntax, including brackets around IPv6 addresses, with no URL
userinfo, path, query, or fragment.

`connect` writes no framing or status text to stdout. It validates the applied
authoritative plan and exact runtime evidence under the world lock, acquires a
connected descriptor inside that generation's user and network namespaces,
then releases the lock before relaying bytes. It accepts a declared name only;
the caller cannot supply a socket address. Errors go to stderr. On macOS the
existing container-machine launcher carries the operation into Linux.

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
