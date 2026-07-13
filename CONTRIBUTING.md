# Contributing

Kenogram accepts small changes that strengthen an observable contract.

1. Read the relevant file in `requirements/` and keep contract, evidence, and
   implementation claims distinct.
2. Add the smallest test that would fail without the change. Prefer standard
   library mechanisms and exact postconditions over test frameworks.
3. Run `make check` for every change.
4. Run `make integration` for namespace, proxy, Podman, mount, or runtime
   evidence changes.
5. Run the relevant composition proof below for lifecycle, generated
   configuration, Engram, OpenClaw, Hermes Agent, or destruction changes.
6. Include failure-path evidence for lifecycle mutations. At most one
   generation may own a writable workspace after any injected failure.

Do not add third-party Go dependencies, generic runtime abstractions, or new
integration fixtures unless they prove a boundary not already covered. Report
security issues privately as described in [`.github/SECURITY.md`](.github/SECURITY.md).

Release preparation uses a short-lived `release/vX.Y.Z` branch and the process
in [`docs/release-strategy.md`](docs/release-strategy.md). Maintainers review
candidate binaries, checksums, embedded source identity, and release text before
merge; ordinary contributors never need publication credentials.

## Composition proofs

All release inputs are URL- and checksum-locked under `internal/e2e/testdata`.
Local archive variables avoid repeated downloads without weakening digest
verification.

Local runtime proofs need Go 1.22, `rg`, Linux/amd64 for Engram compositions,
rootless Podman with `uidmap`, `fuse-overlayfs`, `nsenter`, host tmux for Hermes,
outbound artifact/image access, and substantial temporary disk. Run the target
relevant to your change; reserve `make e2e` for a complete 10–20 minute-per-lane
replay.

Every container-heavy proof uses a random world identity and refuses a
pre-existing container name. Cleanup verifies Kenogram's world/generation
labels, removes containers by immutable ID newest-first, and preserves label
mismatches. Images absent from the pre-test snapshot are removed only after the
test claims and re-verifies their immutable ID. Image removal is never forced;
an image used by another workload survives as a visible cleanup failure.

Before artifact downloads or image builds, rootless Podman `vfs` stores require
lane-specific free-space floors: 24 GiB for the Engram release proof, 64 GiB for
OpenClaw proofs, and 96 GiB for Hermes proofs. These are conservative fail-fast
budgets, not image-size claims; the Hermes floor adds transient headroom to an
observed 68 GiB expanded footprint. Rootless `overlay` is not subject to the
amplification guard or its override. Operators with a measured local footprint
may set `KENOGRAM_E2E_VFS_MIN_FREE_GIB` to a positive whole-GiB threshold. Use
`df -h <graph-root>` for free capacity and `podman system df` for attribution.
The setting does not delete or prune storage.

| Command | Evidence |
|---|---|
| `make e2e-release` | Engram v0.3.0 materialization, replacement, restart, and destruction |
| `make e2e-openclaw` | OpenClaw 2026.6.11 isolation, native fake-Telegram and TUI round-trips, replacement, and absence claims |
| `make e2e-composition` | Fake Telegram text through Engram v0.3.0 and the isolated OpenClaw TUI, plus attachment ingestion into its workspace |
| `make e2e-hermes` | Hermes Agent v2026.7.7.2 integrity, isolation, native fake-Telegram and TUI round-trips, lifecycle, and absence claims |
| `make e2e-hermes-composition` | Fake Telegram text through Engram v0.3.0 and the isolated Hermes TUI, plus attachment ingestion into its workspace |
| `make e2e` | All deterministic proofs above |

The real Telegram canary is manual and must use a dedicated bot and account:

```sh
export KENOGRAM_TELEGRAM_BOT_TOKEN='...'
export KENOGRAM_TELEGRAM_ALLOWED_USER_ID='...'
export KENOGRAM_TELEGRAM_CHAT_ID='...'
export KENOGRAM_TELEGRAM_CANARY_NONCE="$(date +%s)"
make e2e-telegram-canary
```

The canary notifies the operator with two commands, waits three minutes for
them, proves the resulting model request and Telegram delivery from
Engram's audit record, then destroys the world. CI stores these secrets only in
the protected `live-telegram-canary` environment.
