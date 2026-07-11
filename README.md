<p align="center">
  <img src="docs/assets/kenogram-mark.svg" alt="Kenogram: graphite declarations terminate at a black marked place in a warm white field" width="760">
</p>

# Kenogram

Kenogram materializes small, faithful Linux computers from a declaration.
Everything visible inside a world belongs to its inhabitant. Everything outside
the world is absent.

The repository implements the v3.1 lifecycle: strict declarations and semantic
plans, rootless Podman generations, binary replacement with rollback, carried
workspace digest trees, regenerated configuration, runtime evidence, a
hash-chained history, a network-absent namespace with a host-held loopback door,
and memory-only ephemeral grants.

```sh
make check
go run ./cmd/kenogram up --dry-run ./kenogram.toml
go run ./cmd/kenogram up --yes ./kenogram.toml
```

The requirements in [`requirements/`](requirements/) are binding. The complete
design is recorded in [`docs/design.md`](docs/design.md); implementation status
is stated separately so unfinished behavior is never implied by the charter.

Kenogram is Linux-only and uses the Go standard library exclusively. It requires
rootless Podman on cgroups v2, `nsenter`, and configured subordinate UID/GID
ranges. `make integration` verifies the real namespace boundary; it is mandatory
in CI and intentionally fails rather than weakening isolation when those host
prerequisites are absent.
