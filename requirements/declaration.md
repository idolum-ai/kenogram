# Declaration contract

Status: binding contract. Evidence and open boundaries are indexed in `INDEX.md`.

Kenogram reads exactly one UTF-8 declaration. The accepted TOML subset contains
double-quoted strings with TOML-compatible basic escapes, booleans, signed decimal
integers, homogeneous single-line scalar arrays, bare keys, tables, arrays of tables, and
comments. Inline tables, floats, dates, multiline strings, dotted assignment
keys, and quoted keys are rejected.

Unknown keys and tables are errors. Duplicate keys and table declarations are
errors. Array elements must have one scalar type. Integer overflow, invalid UTF-8,
trailing material, and malformed escapes are errors with line attribution.

Schema version 1 is the only accepted version. World, service, and interface names are
unique, targets and workspace paths are absolute and clean, reserved paths cannot
be covered, mount targets cannot overlap, resources are positive, network ports
are 1–65535, restart is `never`, `on-failure`, or `always`, and declared source
paths must exist. Secret file sources must not grant group or other permission.
Interface addresses are canonical `127.0.0.1:port` endpoints: wildcard, host,
URL, non-loopback, noncanonical, and caller-selected addresses are rejected.

The world `name` is its stable operational address; changing it addresses a
different world. This namespace rule does not claim that names determine
behavioral or ontological identity.

## Version 1 schema

| Section | Keys | Multiplicity |
|---|---|---|
| root | `version`, `name`, `allow_unpinned` | once |
| `[world]` | `hostname`, `base`, `workdir`, `user` | once |
| `[resources]` | `cpus`, `memory_bytes`, `pids` | once |
| `[workspace]` | `paths` | once |
| `[[copies]]` | `source`, `target`, `mode`, `secret` | zero or more |
| `[[mounts]]` | `source`, `target`, `mode` | zero or more |
| `[[network.allow]]` | `host`, `port` | zero or more |
| `[[interfaces]]` | `name`, `address` | zero or more |
| `[[services]]` | `name`, `command`, `autostart`, `restart` | zero or more |

The parser, not this summary, is authoritative about required keys and defaults.
Start from [`../kenogram.example.toml`](../kenogram.example.toml) and use
`kenogram up --dry-run` as the validation boundary. The example proves planning,
not image compatibility. `world.base` is immutable when expressed as either a
registry reference ending in `@sha256:<64 hex digits>` or an exact local image
ID `sha256:<64 hex digits>`; any tag requires the explicit `allow_unpinned =
true` escape hatch. Before start, the materialized world must contain
`/usr/bin/tail`, `/bin/sh`, the declared user, and every executable named by an
autostart service; executables may come from the base or declared copies. Normal
`enter` additionally requires `/usr/bin/tmux` and a `main` session.
