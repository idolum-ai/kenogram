# Changelog

Notable user-visible and operational changes are recorded here.

## Unreleased

- Make repeated-plan comparisons insertion-aware while preserving order and
  redacting secret copy digests across insertion, removal, modification, and
  reorder evidence.
- Keep JSON dry-run output machine-clean and fail closed when candidate bytes or
  prior plan, declaration, state, history, staging, or canonical workspace
  evidence cannot be compared; revalidate the complete reviewed snapshot under
  the world lock before applying it.

## [v0.1.1] - 2026-07-17

- Accept Podman's `shareable` inspection label for a requested private IPC
  namespace only when live namespace file identity proves separation from
  Kenogram's ambient IPC namespace, restoring compatibility with rootless
  Podman 4.3.1 on Debian 12 without weakening verification.
- Keep each world's network proxy independent of the applying terminal and
  process group, make post-readiness ownership explicit, and prevent proxy
  leaks across integration cleanup and startup failure paths.
- Strengthen the OpenClaw Telegram, TUI, replacement, and network-door proofs
  while retaining failed structured test evidence in CI.
- Refine the Kenogram mark and require SVG changes to pass the full asset
  contract rather than the editorial-only gate.

## [v0.1.0] - 2026-07-14

- Verify the candidate-reviewed release head in private repositories without
  persisting checkout credentials.
- Add declared loopback interfaces, byte-transparent `connect`, and an optional
  SSH composition proven without a published host port.
- Pin CI and release builds to a supported Go security patch while retaining
  Go 1.22 as the module compatibility floor, and gate changes and releases on
  reachable vulnerability analysis.
- Add a complete read-only `doctor` command with stable JSON output for host prerequisites.
- Make release installation and first-world preparation standalone,
  checksum-covered paths that require no source checkout and pin the generated
  declaration to an exact local image ID.
- Add operator guides for the proven Engram, OpenClaw, and Hermes compositions.
- Clarify Kenogram's absence-first purpose, philosophical lineage, evidence
  posture, and proposed separation of provenance from world-pattern observations.
- Restart and re-establish services for a stopped authoritative successor during committed-transition recovery.
- Make confirmed destruction remove every generation in an unresolved transition without first reviving the world.
- Require proxy process identity and a control round trip before declaring the network door ready.
- Add an apply-ready first-world guide and keep local release artifacts out of version control.
- Add an experimental macOS launcher with shell-inert argv transport, explicit
  terminal handling, exact remote exit statuses, and graceful signal forwarding
  for operator-managed Apple container machines.
