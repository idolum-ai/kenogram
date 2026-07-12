<p align="center">
  <img src="docs/assets/kenogram-mark.svg" alt="Kenogram: two different fields of marks preserve one relational pattern across a warm white field" width="760">
</p>

# Kenogram

Kenogram materializes small Linux computers from a declaration. Everything
visible inside a world belongs to its inhabitant; everything else is absent
unless the declaration deliberately carries it across the boundary.

```sh
make build
./bin/kenogram up --dry-run ./kenogram.example.toml
```

This first command is read-only. Before applying a real declaration, replace the
example image digest and review its mounts and destinations. Durable state lives
under `$XDG_DATA_HOME/kenogram/worlds` (normally
`~/.local/share/kenogram/worlds`); tests and automation may set
`KENOGRAM_STATE_DIR`.

The contracts in [`requirements/`](requirements/) are binding. Their
[evidence table](requirements/INDEX.md#evidence-and-open-boundaries) separates
what is proven from what remains open. See the [declaration
schema](requirements/declaration.md), [operations and
recovery](requirements/operations.md), and [contributor contract](CONTRIBUTING.md).

The name is a deliberate but limited adaptation of Rudolf Kaehr's
kenogrammatics: the project privileges observable patterns over the identity of
their realization, without claiming to implement a morphogrammatic calculus.
[`docs/kenogrammatics.md`](docs/kenogrammatics.md) records that lineage, the
engineering analogy, and its limits.

Kenogram is pre-release, Linux-only, and uses the Go standard library
exclusively. It requires
rootless Podman on cgroups v2, `nsenter`, and configured subordinate UID/GID
ranges. `make integration` verifies the real namespace boundary; it is mandatory
in CI and intentionally fails rather than weakening isolation when those host
prerequisites are absent.

`make e2e` runs the release-pinned composition proofs. Kenogram isolates the
OpenClaw `2026.6.11` TUI with a deterministic fake model, accepts the Engram
`v0.2.0` release, and proves the hermetic fake-Telegram → Engram → tmux →
OpenClaw path, including Bot API file download routing. Pull requests require
the OpenClaw isolation proof; the full composition runs on `main` and nightly.

The operator-assisted `make e2e-telegram-canary` is deliberately separate. It
uses a protected canary bot to prove the real Telegram path and never runs on a
pull request. Exact commands and secret requirements are in
[`CONTRIBUTING.md`](CONTRIBUTING.md#composition-proofs). Security reports belong
in GitHub's private vulnerability-reporting flow.
