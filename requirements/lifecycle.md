# Lifecycle contract

Status: planned.

Generations are named `kenogram-<world>-g<N>`. A successor is staged before the
predecessor stops; they never run concurrently over one workspace. The successor
starts and is verified from backend evidence before it is recorded as applied. On
failure the predecessor is restarted and no hybrid state remains.

Workspace data is host-side, carried, and represented by a deterministic digest
tree. Configuration is regenerated from the declaration. Confirmation surfaces
workspace drift. Rootless operation, private namespaces, capability reduction,
seccomp, device allowlisting, cgroups v2, and absence of the runtime socket are
mandatory.
