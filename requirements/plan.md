# Plan contract

Status: binding contract. Evidence and open boundaries are indexed in `INDEX.md`.

Planning resolves source paths relative to the declaration directory and cleans
all absolute target paths. A semantic plan is encoded as JSON from fixed-order
struct fields. Declaration formatting and comments cannot affect this encoding.
Copied source files and trees are deterministically content-digested into the
plan; changing configuration bytes changes its exact fingerprint. Live mounts
and carried workspace are evidenced separately because their bytes intentionally
drift.

Named loopback interfaces are semantic plan fields. Changing a name or address
therefore changes the plan digest and requires ordinary generation replacement.

Plan comparison preserves repeated-field order and uses deterministic exact
anchors with local substitution alignment. Insertions therefore do not make
unchanged successors appear modified, in-place edits remain modifications, and
reorders remain visible as removal and insertion evidence. Duplicate elements
are matched by occurrence. The comparison uses resulting indices for additions
and modifications and prior indices for removals; when its global alignment
budget is exhausted, it reports an exact redacted array-level replacement. If
that replacement prevents safe positional attribution of changed secret-copy
digests, `copies[*].source_digest` records a redacted change in either the global
secret-digest multiset or a digest multiset attached to a stable `(source,
target)` identity, without claiming which position changed. Pure reorder and
source-only or target-only edits do not produce that marker.

The plan digest is lowercase SHA-256 over the canonical semantic JSON followed by
one newline. The declaration digest is lowercase SHA-256 over the exact input
bytes. Both are printed by dry-run and present in JSON output.

These digests establish provenance and conservative operational equality. They
do not define behavioral or ontological identity: different realizations may
satisfy the same observable contracts even when their fingerprints differ.

Unpinned base images are rejected unless `allow_unpinned = true`; an admitted
unpinned image produces a prominent plan warning. Plans never print file content
or secret material.
