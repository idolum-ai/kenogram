# Release strategy

Kenogram releases are small, reviewed snapshots of `main`. Publication does
not create, replace, or restart a world.

## Contract

- Tags use Semantic Versioning with a `v` prefix, such as `v0.1.0`.
- The product version and declaration schema version are independent.
- Release branches are named `release/vX.Y.Z` and contain a dated matching
  heading in `CHANGELOG.md`.
- A release PR body has `Summary`, `Compatibility`, and `Validation` sections.
- Candidate artifacts are reviewed before merge. Publication verifies that the
  merged tree is exactly the candidate-reviewed tree.
- Tags and published assets are create-only. Corrections use a new version.

Before `v1.0.0`, minor releases may deliberately change compatibility when the
release notes say so plainly. Patch releases are reserved for compatible fixes.
Build metadata is not accepted in tags.

## Release path

1. Branch `release/vX.Y.Z` from current `main`.
2. Move relevant `Unreleased` entries into
   `## [vX.Y.Z] - YYYY-MM-DD`, then restore an empty `Unreleased` section.
3. Open a release PR. Describe observable changes, compatibility or migration
   needs, validation, and known limits in its required sections.
4. Review normal CI and the release-candidate artifact. The candidate workflow
   runs the full gate and real runtime integration, builds both archives,
   verifies embedded identity, and records checksums and release-note preview.
5. Merge only when notes, source, and candidate evidence agree.
6. The release workflow proves the merged tree equals the reviewed branch head,
   reruns validation, and rebuilds artifacts without write credentials.
7. A separately approved `release` environment grants `contents: write` only to
   the publication job. It creates the tag atomically, verifies a draft release
   and every uploaded byte, then publishes it.

Maintainers do not create ordinary release tags manually.

## Assets

Kenogram is Linux-only. Each release contains:

- `kenogram-vX.Y.Z-linux-amd64.tar.gz`
- `kenogram-vX.Y.Z-linux-arm64.tar.gz`
- `install-release.sh`
- `prepare-first-world.sh`
- `reference-world.Containerfile`
- `ssh-world.Containerfile`
- `checksums.txt`

Every archive contains `kenogram`, `README.md`, and `LICENSE`. Binaries are
static, built with `-trimpath`, and report their version, source commit, build
date, and Go version through `kenogram version`. Archives use source-commit time
and normalized ownership for reproducibility.

`checksums.txt` covers both archives and all four standalone onboarding
assets. Checksums detect corruption or asset substitution when the checksum
file is obtained from the same release. They are not code signing or
independent provenance, and this process does not claim either.

`scripts/install-release.sh` downloads the matching host archive and checksum,
checks exact archive contents and embedded identity, then atomically installs
the binary under `~/.local/bin` by default. It does not install Podman or host
prerequisites and does not touch a running world.

`prepare-first-world.sh` verifies the release's
`reference-world.Containerfile`, builds it for the current host identity from a
registry-digest-pinned base, and writes a declaration naming the resulting
exact local image ID. It neither publishes an image nor claims that independent
package-manager builds are byte-identical.

## Repository settings

Maintainers must verify and preserve these settings for every release:

- protected `main` requires current CI and review;
- an organization ruleset requires `.github/workflows/ci.yml` from its protected
  source revision before path-aware pull-request results replace full per-job
  requirements;
- `PATH_AWARE_CI_ENABLED=true` is set only after that ruleset workflow passes in
  Evaluate mode, is activated, and proves pull-request events plus merge-group
  events when a merge queue is enabled; leaving it unset preserves the full
  suite;
- the `release` Actions environment requires maintainer approval;
- immutable GitHub Releases are enabled;
- a `v*` tag ruleset denies update and deletion while permitting the release
  environment's Actions token to create a tag;
- Actions are restricted to reviewed, full-SHA-pinned actions.

`PATH_AWARE_CI_ENABLED` is an Actions **repository variable** under **Settings →
Secrets and variables → Actions → Variables**, not an environment variable.
Without the organization workflow rule, enabling it and requiring only the
aggregate `required` job is a trusted-contributor optimization, not a security
boundary, and must not be used for public fork contributions.

The workflow fails closed around existing tags and assets, but repository
settings are the enforcement boundary after publication.

## Review checklist

- [ ] Branch, changelog heading, proposed tag, and notes agree.
- [ ] The release branch is current with `main` and contains only preparation.
- [ ] Compatibility, migration, and known limits are explicit.
- [ ] `make check`, `make test-race`, and `make integration` pass.
- [ ] Both Linux candidate archives exist and report the intended identity.
- [ ] `checksums.txt` covers exactly both archives and the four standalone onboarding assets.
- [ ] No declaration, credential, state, transcript, or runtime artifact ships.
- [ ] Protected environment, immutable releases, and tag rules are enabled.

On failure, fix the release branch before merge. After publication, preserve the
immutable release and publish a correcting version.
