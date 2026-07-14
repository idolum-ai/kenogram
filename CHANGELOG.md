# Changelog

Notable user-visible and operational changes are recorded here.

## Unreleased

## [v0.1.0] - 2026-07-14

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
