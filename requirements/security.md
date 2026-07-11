# Security contract

Status: implemented, subject to the kernel/runtime limits stated here.

The declaration is host-authored but still parsed fail-closed. It cannot select
arbitrary schema extensions. Planning reads metadata only: copied and mounted
file contents are neither rendered nor incorporated into the semantic plan digest.

Relative sources resolve against the declaration directory, not the caller's
working directory. Missing sources fail validation. Secret files must have no
group or other permission bits; secret bytes are never printed.

Symlinked host source paths are rejected and copied trees reject symlink nodes.
Podman evidence must confirm rootless operation, cgroups v2, private none-network
mode, provenance labels, declared mounts, and resource limits. The runtime socket
is never mounted. Kenogram protects the host only to the extent provided by the
kernel, rootless runtime, and its own correctness; declared rw mounts and secrets
remain world-owned input by design.
