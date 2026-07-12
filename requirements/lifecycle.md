# Lifecycle contract

Status: implemented; the real runtime boundary is continuously verified by the
mandatory Linux integration job.

Generations are named `kenogram-<world>-g<N>`. A successor is staged before the
predecessor stops; they never run concurrently over one workspace. The successor
starts and is verified from backend evidence before it is recorded as applied. On
failure the predecessor is restarted and no hybrid state remains.

Workspace data is host-side, carried, and represented by a deterministic digest
tree. Configuration is regenerated from the declaration. Confirmation surfaces
workspace drift. Rootless operation, private namespaces, capability reduction,
seccomp, device allowlisting, cgroups v2, and absence of the runtime socket are
mandatory.

A generation is one material inscription of the declared world-pattern, not the
persistent substance of the world. Replacement is correct when provenance is
preserved, carried state is handled explicitly, and the successor satisfies the
same observable contracts.

Rootless Podman, cgroups v2, and subordinate UID/GID mappings are hard
preflight requirements. Generated `/KENOGRAM.md`, `world.json`, and service
supervisors are configuration state. Matching surviving generations are adopted;
matching stopped generations are restarted; missing or mismatched runtime state
is replaced. A failed cutover restores predecessor services and its door.
