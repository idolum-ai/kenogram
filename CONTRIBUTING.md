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

## Composition proofs

All release inputs are URL- and checksum-locked under `internal/e2e/testdata`.
Local archive variables avoid repeated downloads without weakening digest
verification.

| Command | Evidence |
|---|---|
| `make e2e-release` | Engram v0.3.0 materialization, replacement, restart, and destruction |
| `make e2e-openclaw` | OpenClaw 2026.6.11 isolation, native fake-Telegram and TUI round-trips, replacement, and absence claims |
| `make e2e-composition` | Fake Telegram methods and files through Engram v0.3.0 into the isolated OpenClaw TUI |
| `make e2e-hermes` | Hermes Agent v2026.7.7.2 integrity, isolation, native fake-Telegram and TUI round-trips, lifecycle, and absence claims |
| `make e2e-hermes-composition` | Fake Telegram methods and files through Engram v0.3.0 into the isolated Hermes TUI |
| `make e2e` | All deterministic proofs above |

The real Telegram canary is manual and must use a dedicated bot and account:

```sh
export KENOGRAM_TELEGRAM_BOT_TOKEN='...'
export KENOGRAM_TELEGRAM_ALLOWED_USER_ID='...'
export KENOGRAM_TELEGRAM_CHAT_ID='...'
export KENOGRAM_TELEGRAM_CANARY_NONCE="$(date +%s)"
make e2e-telegram-canary
```

The canary sends its three operator commands through the bot, waits three
minutes for them, proves the resulting model request and Telegram delivery from
Engram's audit record, then destroys the world. CI stores these secrets only in
the protected `live-telegram-canary` environment.
