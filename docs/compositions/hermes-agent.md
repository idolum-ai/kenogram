# Hermes Agent

Kenogram proves Hermes Agent release v2026.7.7.2 (agent version 0.18.2) from
`docker.io/nousresearch/hermes-agent@sha256:9c841866021c54c4596849f6135717e8a4d52ba510b7f52c50aef1de1a283973`.
The source commit and archive SHA-256 are recorded in
[`hermes-agent-v2026.7.7.2.lock.json`](../../internal/e2e/testdata/hermes-agent-v2026.7.7.2.lock.json).
The proof verifies commit
`9de9c25f620ff7f1ce0fd5457d596052d5159596` and source archive SHA-256
`f5d1022eed3763a768cf7b0f0844831f0170a35f54eb8d18223f2e93f503025e`
before acquisition.

## Direct agent boundary

[`writeHermesDeclaration`](../../internal/e2e/hermes_test.go) is the maintained
declaration generator. It uses the host UID/GID, a 2 CPU / 3 GiB / 768 PID
resource shape, `/workspace`, a mode-`0600` secret Hermes configuration, and
only the selected provider and Telegram destinations. The service copies that
configuration into the carried `/workspace/.hermes` locus and starts:

```text
HOME=/workspace/.hermes
HERMES_HOME=/workspace/.hermes
hermes gateway run --no-supervise --quiet
```

For real operation, replace the proof provider and fake Telegram URLs, keep the
API key and bot token only in the secret source, and admit their exact hosts and
ports. `make e2e-hermes` proves native Telegram, direct query and TUI paths,
replacement, restart, workspace carry, and absence observations.

The official image used by the proof does not supply the required host-matched
tmux surface. The generator therefore copies the host tmux executable and its
resolved libraries into the world. This is proof machinery, not a portable
production recipe: production operators should derive and digest-pin a reviewed
image containing a compatible tmux instead of copying host libraries.

## Through Engram

[`writeEngramHermesDeclaration`](../../internal/e2e/engram_hermes_test.go) adds
the verified Engram binary and secret environment, creates `main:hermes` with
`hermes --tui --ignore-rules`, then starts Engram. `make
e2e-hermes-composition` proves fake Telegram text through Engram into that TUI
and attachment ingestion into the carried workspace.

Hermes acquisition has an explicit 96 GiB free-space floor for an unmeasured
rootless `vfs` store. Read the capacity result before pulling; do not weaken or
bypass it.
