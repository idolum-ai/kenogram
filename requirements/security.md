# Security contract

Status: M1 portion implemented.

The declaration is host-authored but still parsed fail-closed. It cannot select
arbitrary schema extensions. Planning reads metadata only: copied and mounted
file contents are neither rendered nor incorporated into the M1 plan digest.

Relative sources resolve against the declaration directory, not the caller's
working directory. Missing sources fail validation. Secret files must have no
group or other permission bits; secret bytes are never printed.

Container isolation, race-safe descriptor-based source traversal, and runtime
evidence are planned lifecycle contracts. M1 makes no host-isolation claim.
