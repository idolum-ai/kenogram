<p align="center">
  <img src="docs/assets/kenogram-mark.svg" alt="Kenogram: two different fields of marks preserve one relational pattern across a warm white field" width="760">
</p>

# Kenogram

Kenogram lets you give an agent a whole small computer without giving it your
computer. One declaration says what the world contains. Everything inside
belongs to its inhabitant; everything else is absent.

Anything admitted into an AI's context can change what follows. Kenogram
therefore treats absence—not concealment behind another approval prompt—as the
first boundary: an undeclared capability should not be present in the agent's
world at all. Declared worlds still rely on isolation, validation, resource and
network limits, provenance, and recovery; absence does not replace those
controls.

Idolum's projects repeatedly separate speech from authority, representation
from truth, and capability from ambient context. Kenogram gives that posture an
environmental form. An agent can work freely inside a faithful world, while the
human retains authority over what may exist in that world at all.

## Install and check the host

Kenogram's released binaries support Linux on amd64 and arm64. Download the
standalone installer for the release you intend to trust, inspect it, then run
it:

```sh
version=vX.Y.Z
curl --fail --location --proto '=https' --tlsv1.2 \
  --output install-release.sh \
  "https://github.com/idolum-ai/kenogram/releases/download/${version}/install-release.sh"
less install-release.sh
bash install-release.sh "${version}"
kenogram doctor
```

The installer verifies the release checksum and embedded version before
installing to `~/.local/bin`. `doctor` is read-only and reports every missing
host prerequisite in one run; `doctor --json` is stable automation output. It
does not inspect a future world image: every image still needs `/usr/bin/tail`,
`/bin/sh`, the declared user, and its declared service binaries. Normal
`kenogram enter` additionally expects `/usr/bin/tmux` and a `main` session;
`enter --repair` needs only `/bin/sh`.

To build, enter, restart, and destroy a minimal live world, follow the
[first-world guide](docs/getting-started.md). It includes the rootless host
preflight and uses the release-covered `prepare-first-world.sh` to produce a
small host-bound image and apply-ready declaration.

After authoring and dry-running a real declaration, the lifecycle follows this
illustrative sequence:

```sh
kenogram up --yes ./world.toml
kenogram status engineering
kenogram enter engineering       # or: kenogram enter --repair engineering
kenogram down engineering
kenogram up --yes ./world.toml   # restart or reconcile
kenogram destroy --yes engineering
```

Replace `engineering` with the declaration's name. Durable state lives
under `$XDG_DATA_HOME/kenogram/worlds` (normally
`~/.local/share/kenogram/worlds`); tests and automation may set
`KENOGRAM_STATE_DIR`.

The contracts in [`requirements/`](requirements/) are binding. Their
[evidence table](requirements/INDEX.md#evidence-and-known-limits) separates
what is proven from the next proof and says whether each limit constrains v0.x,
a future stable claim, or an experimental surface. See the [declaration
schema](requirements/declaration.md), [operations and
recovery](requirements/operations.md), and [contributor contract](CONTRIBUTING.md).

The name is a deliberate but limited adaptation of the kenogrammatic lineage
begun by Gotthard Günther and developed by Rudolf Kaehr and Thomas Mahler: the
project privileges observable patterns over the identity of their realization,
without claiming to implement a morphogrammatic calculus.
[`docs/kenogrammatics.md`](docs/kenogrammatics.md) records that lineage, the
engineering analogy, and its limits.

Kenogram is pre-release and uses the Go standard library exclusively. Its
proven runtime is Linux; it requires
rootless Podman on cgroups v2, `nsenter`, and configured subordinate UID/GID
ranges. `make integration` verifies the real namespace boundary; it is mandatory
in CI and intentionally fails rather than weakening isolation when those host
prerequisites are absent.

An [experimental Apple container-machine launcher](docs/apple-container-machine.md)
can carry an encoded Linux operation from macOS into an operator-managed
machine. It preserves argv across Apple's shell-mediated machine command and
retains the Podman checks rather than treating Apple's container CLI as an
equivalent isolation backend. The launcher is unit-tested and cross-compiled,
but still needs real Apple-silicon proof before release support.

`make e2e` runs the release-pinned composition proofs. Kenogram isolates
OpenClaw `2026.6.11` with deterministic fake Telegram and model services,
Hermes Agent `v2026.7.7.2` with the same hermetic boundaries, and accepts the
Engram `v0.3.0` release. Separate proofs cover each agent's native Telegram
path and fake-Telegram → Engram → tmux → agent path. Pull requests require
both isolation and Engram composition proofs.

The [composition guides](docs/compositions/README.md) turn those proofs into
operator-facing version, trust, secret, network, and capacity guidance for
Engram, OpenClaw, and Hermes Agent.

The operator-assisted `make e2e-telegram-canary` is deliberately separate. It
uses a protected canary bot to prove the real Telegram path and never runs on a
pull request. Exact commands and secret requirements are in
[`CONTRIBUTING.md`](CONTRIBUTING.md#composition-proofs). Security reports belong
in GitHub's private vulnerability-reporting flow.

Contributors build and replay evidence from a checkout as described in
[`CONTRIBUTING.md`](CONTRIBUTING.md). The reviewed candidate and immutable
publication contract is documented in
[`docs/release-strategy.md`](docs/release-strategy.md).
