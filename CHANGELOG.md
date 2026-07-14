# Changelog

Notable user-visible and operational changes are recorded here.

## Unreleased

- Pin CI and release builds to a supported Go security patch while retaining
  Go 1.22 as the module compatibility floor, and gate changes and releases on
  reachable vulnerability analysis.
- Restart and re-establish services for a stopped authoritative successor during committed-transition recovery.
- Make confirmed destruction remove every generation in an unresolved transition without first reviving the world.
- Require proxy process identity and a control round trip before declaring the network door ready.
- Add an apply-ready first-world guide and keep local release artifacts out of version control.
- Add an experimental macOS launcher with shell-inert argv transport, explicit
  terminal handling, exact remote exit statuses, and graceful signal forwarding
  for operator-managed Apple container machines.
