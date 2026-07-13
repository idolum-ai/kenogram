# Lifecycle contract

Status: binding contract. Successful composition is verified at the real
runtime boundary; failure-transition evidence remains explicitly open in
`INDEX.md`.

Generations are named `kenogram-<world>-g<N>`. A successor is staged before the
predecessor stops; they never run concurrently over one workspace. The successor
starts and is verified from backend evidence before it is recorded as applied. On
failure the predecessor is restarted and no hybrid state remains.

Before the first cutover mutation, `up` fsyncs a transition record that retains
both declarations and identifies the authoritative recovery direction. Before
durable successor state is written, that record advances from rollback to
commit. The next `up` completes either direction idempotently before planning a
new generation; an unrecoverable observation leaves the record intact.

The transition phase defines authority for every recovery-facing command.
During rollback the predecessor remains authoritative and the successor is a
candidate; during commit the successor is authoritative and the predecessor is
the displaced candidate. `status` reports both roles and `enter --repair`
attaches only to the authoritative generation. If rollback has no predecessor,
Kenogram reports that no authoritative generation exists rather than entering
the candidate.

Workspace data is host-side, carried, and represented by a deterministic digest
tree. Configuration is regenerated from the declaration. Confirmation surfaces
workspace drift. Rootless operation, private namespaces, capability reduction,
seccomp, device allowlisting, cgroups v2, and absence of the runtime socket are
mandatory. Exact mount identity and active seccomp mode are observed before the
network door or any declared service starts.

A generation is one material inscription of the declared world-pattern, not the
persistent substance of the world. Replacement is correct when provenance is
preserved, carried state is handled explicitly, and the successor satisfies the
same observable contracts.

The unit suite kills a replacement process at fourteen lifecycle boundaries:
after the rollback record, predecessor stop, successor start, both evidence
checks, service start, commit record, each durable authority write, history
append, predecessor removal, and transition removal. A fresh process must then
converge on one running successor with one applied history record per
generation. This is process-crash evidence, not a claim about storage surviving
kernel or power loss on every filesystem.

Rootless Podman, cgroups v2, and subordinate UID/GID mappings are hard
preflight requirements. Generated `/KENOGRAM.md`, `world.json`, and service
supervisors are configuration state. Matching surviving generations are adopted;
matching stopped generations are restarted; missing or mismatched runtime state
is replaced. A failed cutover restores predecessor services and its door.

Autostart service wrappers report `starting`, `running <pid>`, and `exited
<status>` under `/run/kenogram/services`. Restart policies wait one second
between attempts. A successful command may intentionally daemonize; its zero
exit is an acknowledgement, not a general application-health claim.
