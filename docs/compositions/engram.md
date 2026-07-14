# Engram

Kenogram proves Engram v0.3.0 as a checksum-pinned copied binary, not as ambient
host software. The direct lifecycle proof is
[`TestEngramReleaseInsideKenogram`](../../internal/e2e/engram_release_test.go);
the shared materialization helper used by both agent compositions is
[`materializeEngramRelease`](../../internal/e2e/engram_openclaw_test.go).

## Acquire and verify

The proof lock is
[`engram-v0.3.0.lock.json`](../../internal/e2e/testdata/engram-v0.3.0.lock.json).
It identifies the official Linux/amd64 release archive and SHA-256. Verify the
archive before extracting `engram`; do not curl a moving release URL directly
into a world.

```text
f8ef677d964ba8b86394d2e535fc195da206cbdf84029c9fc7593ebe3675c677  engram-v0.3.0-linux-amd64.tar.gz
```

The checked declaration shape is:

```toml
[[copies]]
source = "/operator/verified/engram"
target = "/usr/local/bin/engram"
mode = "0755"

[[copies]]
source = "/operator/private/engram.env"
target = "/etc/engram.env"
mode = "0600"
secret = true

[[services]]
name = "engram"
command = ["/usr/local/bin/engram", "run", "--env", "/etc/engram.env"]
autostart = true
restart = "never"
```

The environment file owns `TELEGRAM_BOT_TOKEN`, the allowed user and chat IDs,
the model-provider credential, `ENGRAM_HOME=/workspace/.engram`, the workdir,
and `ENGRAM_TMUX_SESSION=main`. Keep it mode `0600`. Declare the Telegram and
model-provider host/port pairs separately; do not allow the wider internet.

## Terminal contract

Engram sends terminal input to the declared `main` tmux session. The agent guide
must therefore create that session before Engram starts polling. OpenClaw and
Hermes paint different TUIs, so their guides own the session command while this
guide owns the shared Engram process and secret boundary.

For deterministic local proof run `make e2e-release`; the two complete
compositions are `make e2e-composition` and `make e2e-hermes-composition`.
Real Telegram belongs only in the protected canary path described in
[`CONTRIBUTING.md`](../../CONTRIBUTING.md#composition-proofs).
