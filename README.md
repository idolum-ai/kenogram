# Kenogram

Kenogram materializes small, faithful Linux computers from a declaration.
Everything visible inside a world belongs to its inhabitant. Everything outside
the world is absent.

This repository is at milestone M1. It implements the declaration and planning
boundary: a strict TOML subset, schema validation, canonical semantic plans,
and both plan and declaration digests. It does **not** yet create containers.

```sh
make check
go run ./cmd/kenogram up --dry-run ./kenogram.toml
```

The requirements in [`requirements/`](requirements/) are binding. The complete
design is recorded in [`docs/design.md`](docs/design.md); implementation status
is stated separately so unfinished behavior is never implied by the charter.

Kenogram is Linux-only and uses the Go standard library exclusively.
