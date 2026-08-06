# Executable provenance contract

Status: packaged release provenance implemented. The closed document, semantic
validator, exact executing-file digest, machine JSON output, full-SHA and
canonical-UTC packaging, and independent packaged-binary cross-binding are
live. Canonical release-manifest publication and external attestation remain
later work.

`kenogram.executable-provenance.v1` is a bounded identity report for one exact
Kenogram executable. It contains:

- `build_kind`: `development` or `release`;
- product version;
- full 40-character source commit or the explicit development placeholder
  `unknown`;
- canonical UTC source date or the explicit development placeholder `unknown`;
- Go toolchain string;
- runtime GOOS and GOARCH; and
- the lowercase SHA-256 of the executing file.

A release document accepts no placeholders. Its version is a canonical
`vMAJOR.MINOR.PATCH` Semantic Version, optionally with a prerelease suffix; its
commit is the full source revision; and its source date is canonical UTC
RFC3339 with optional fractional seconds. Development provenance remains
honest by naming placeholders rather than imitating release identity.

The report is no larger than 64 KiB, is strict JSON with no unknown or duplicate
keys, and is independently digest-bound by each governed-job manifest.
Self-reported provenance proves byte and metadata consistency only when an
independent consumer also authenticates the expected release coordinate and
executable digest. It is not a signature and does not make the executable its
own qualification authority.

Candidate, ordinary pull-request, and publication workflows independently bind
the packaged Linux executable's build kind, version, full source commit,
canonical source date, platform tuple, and SHA-256 to verifier-owned expected
values, then run governed-job integration with that exact executable.

Release packaging will later publish a canonical manifest binding every supported
asset to the full source commit, source date, Go toolchain, platform tuple, and
asset digest. A feature-branch or development executable may support contract
testing, but it cannot produce release-qualified evidence.
