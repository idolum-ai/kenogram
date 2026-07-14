# OpenClaw

Kenogram proves OpenClaw 2026.6.11 from the official image
`docker.io/openclaw/openclaw@sha256:3814fb1f62f9cfc5944de088c5817c68c88b5d721feebe36420b666a90a61ce7`
and checksum-locks the corresponding npm artifact. The identity record is
[`openclaw-2026.6.11.lock.json`](../../internal/e2e/testdata/openclaw-2026.6.11.lock.json).
The locked npm archive SHA-256 is
`3b3165508391b82b38e62189979df589a45a2d8019a8ef7910fccc554649ce7b`;
the proof verifies it before using the artifact.

## Direct agent boundary

[`writeOpenClawDeclaration`](../../internal/e2e/openclaw_test.go) is the
maintained declaration generator. It uses the host UID/GID, a 2 CPU / 2 GiB /
512 PID resource shape, `/workspace`, a mode-`0600` secret OpenClaw config, and
only two network destinations: the selected model provider and Telegram API.
The gateway service runs with:

```text
HOME=/workspace/home
OPENCLAW_HOME=/workspace/home
OPENCLAW_STATE_DIR=/workspace/.openclaw
OPENCLAW_CONFIG_PATH=/etc/openclaw.json
OPENCLAW_WORKSPACE_DIR=/workspace/openclaw
OPENCLAW_PROXY_URL=http://127.0.0.1:3128
OPENCLAW_DISABLE_BONJOUR=1
openclaw gateway --port 18789 --verbose
```

For real operation, replace the proof provider and fake Telegram base URL in
the generated config, keep both tokens in the secret source, and declare only
their exact hosts and ports. Do not add a host Docker/Podman socket or mount an
ambient home directory. `make e2e-openclaw` proves native Telegram polling,
model routing, TUI use, replacement, restart, workspace carry, and absence of
host/runtime authority.

## Through Engram

[`writeEngramOpenClawDeclaration`](../../internal/e2e/engram_openclaw_test.go)
adds the verified Engram binary and secret environment, starts the gateway,
waits for readiness, creates `main:openclaw` with
`openclaw tui --url ws://127.0.0.1:18789`, and finally starts Engram. Keep that
ordering: polling before the TUI is ready turns an environmental race into an
apparent interoperability failure.

Run `make e2e-composition` for the hermetic Telegram/text/attachment proof.
Run `make e2e-telegram-canary` only with the protected environment and dedicated
canary bot; it sends real messages and is never a pull-request gate.
