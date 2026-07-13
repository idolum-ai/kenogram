# Changelog

Notable user-visible and operational changes are recorded here.

## Unreleased

- Restart and re-establish services for a stopped authoritative successor during committed-transition recovery.
- Make confirmed destruction remove every generation in an unresolved transition without first reviving the world.
- Require proxy process identity and a control round trip before declaring the network door ready.
- Add an apply-ready first-world guide and keep local release artifacts out of version control.
