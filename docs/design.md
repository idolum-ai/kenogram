# Kenogram design

Status: consolidated draft v3.1, binding as a charter; features remain
unimplemented until the requirements index marks them implemented.

Kenogram writes worlds; it never decides them. A world is a rootless Linux
environment materialized from one host-authored declaration. The inhabitant owns
everything visible within it. Undeclared paths, processes, devices, credentials,
routes, and names are absent.

The declaration is the sole authority input. Requests emitted through a terminal
are prose, not protocol. A person decides by editing the declaration on the host
and invoking Kenogram. Replacement is the universal change mechanism: workspace
data is carried and digested; configuration is regenerated.

Networking begins with a namespace containing loopback and no exterior route or
resolver. Declared destinations add one visible object: a host-held TCP proxy
socket bound on the world's loopback. The proxy resolves and dials exact declared
name-and-port pairs. This is an implementation target, not an M1 claim.

The implementation advances only through observable contracts. In particular,
no world is called applied until runtime evidence has been inspected, and no
network mechanism is accepted until all invariants in `requirements/network.md`
pass against the real rootless runtime boundary.
