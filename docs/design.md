# Kenogram design

Status: binding design. Observable implementation status is recorded in the
requirements index.

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
name-and-port pairs. The implementation transfers that listener descriptor from
a short-lived namespace helper to the host proxy; no route or in-world forwarder
is created.

The implementation advances only through observable contracts. In particular,
no world is called applied until runtime evidence has been inspected, and no
network mechanism is accepted until all invariants in `requirements/network.md`
pass against the real rootless runtime boundary.

## Name and conceptual lineage

Kenogram takes its name from Rudolf Kaehr's account of kenograms and
morphograms, where the identities of particular marks recede and their pattern
of differences is what matters. Kenogram does not implement that formalism. It
adapts one methodological posture: a world is specified by an observable
pattern, while any mechanism that preserves the required observations is an
acceptable realization of that pattern.

In this analogy, a declaration describes a world-pattern and a generation is
one material inscription of it. Replacement may change the inscription without
changing the observations that define the world. Names, declaration digests,
plan digests, and workspace digests remain essential operational evidence, but
they record addressing and provenance rather than an ontology of sameness.

"Absence precedes denial" belongs to Kenogram's own security design. It is not
attributed to Kaehr. The complete lineage, vocabulary, and boundary of this
adaptation are recorded in [`kenogrammatics.md`](kenogrammatics.md).
