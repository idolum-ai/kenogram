# Proven compositions

These guides transfer Kenogram's release-pinned E2E evidence into operator
recipes. They are compatibility records, not endorsements of an agent or model
provider and not a promise that only the pinned versions work.

| Composition | Version proven in CI | Automated boundary |
|---|---:|---|
| [SSH](ssh.md) | OpenSSH client + Ubuntu 24.04 server package | Declared loopback stream, key rejection, no host listener, lifecycle |
| [Engram](engram.md) | v0.3.0 | Release integrity, offline lifecycle, replacement, restart, destruction |
| [OpenClaw](openclaw.md) | 2026.6.11 | Native fake Telegram, TUI, isolation, replacement, absence |
| [Hermes Agent](hermes-agent.md) | v2026.7.7.2 (agent 0.18.2) | Native fake Telegram, TUI, isolation, replacement, absence |
| Engram + OpenClaw | versions above | Fake Telegram → Engram → tmux → OpenClaw, including attachment ingestion |
| Engram + Hermes | versions above | Fake Telegram → Engram → tmux → Hermes, including attachment ingestion |

All automated Telegram services are deterministic local fixtures. They prove
routing and isolation without sending a real message or exposing a credential.
The separate `make e2e-telegram-canary` path is operator-assisted, protected by
a GitHub environment, and currently covers OpenClaw through Engram.

## Shared trust model

- The host operator chooses the declaration, image digest, copied binaries,
  secrets, network destinations, resource limits, and carried workspace paths.
- Kenogram interprets none of the agent's prompts or output. It materializes
  the declared world and judges the runtime boundary.
- A `secret = true` copy is staged without recording its bytes in the applied
  declaration, history, or world description. The source must be mode `0600`
  (or a private tree) on the host.
- Telegram and model-provider access are independent declared destinations.
  Grant only the exact host and port used by the selected configuration.
- Engram is optional. It supplies a durable Telegram/terminal boundary; it is
  not part of Kenogram's ontology and direct agent Telegram remains valid.
- SSH is an optional trusted operator path. It is deliberately not a prompt-
  contamination boundary and is not present in the reference image.

## Capacity before acquisition

Run `kenogram doctor` first. Hermes lanes then require at least 96 GiB free in
the rootless Podman graph root because its observed expanded image footprint is
about 68 GiB. The OpenClaw and Engram lanes reject measured stores that cannot
fit their acquired artifacts but do not currently publish a fixed floor. E2E
cleanup removes only images it acquired and whose identity still matches; it
never force-removes an unrelated or in-use image.

## From proof to operation

The tests generate declarations at runtime so host UID/GID, fixture addresses,
temporary paths, and secret files are exact. Each guide names the generator and
the smallest changes needed for real services. Treat the generated structure as
the maintained recipe: copy it into an operator-owned declaration, replace only
the fixture endpoints and credentials, dry-run it, and review the full plan
before applying.
