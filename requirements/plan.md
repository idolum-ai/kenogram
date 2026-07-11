# Plan contract

Status: implemented at M1.

Planning resolves source paths relative to the declaration directory and cleans
all absolute target paths. A semantic plan is encoded as JSON from fixed-order
struct fields. Declaration formatting and comments cannot affect this encoding.

The plan digest is lowercase SHA-256 over the canonical semantic JSON followed by
one newline. The declaration digest is lowercase SHA-256 over the exact input
bytes. Both are printed by dry-run and present in JSON output.

Unpinned base images are rejected unless `allow_unpinned = true`; an admitted
unpinned image produces a prominent plan warning. Plans never print file content
or secret material.
