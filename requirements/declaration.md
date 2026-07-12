# Declaration contract

Status: implemented.

Kenogram reads exactly one UTF-8 declaration. The accepted TOML subset contains
double-quoted strings with TOML-compatible basic escapes, booleans, signed decimal
integers, homogeneous single-line scalar arrays, bare keys, tables, arrays of tables, and
comments. Inline tables, floats, dates, multiline strings, dotted assignment
keys, and quoted keys are rejected.

Unknown keys and tables are errors. Duplicate keys and table declarations are
errors. Array elements must have one scalar type. Integer overflow, invalid UTF-8,
trailing material, and malformed escapes are errors with line attribution.

Schema version 1 is the only accepted version. Names and service names are
unique, targets and workspace paths are absolute and clean, reserved paths cannot
be covered, mount targets cannot overlap, resources are positive, network ports
are 1–65535, restart is `never`, `on-failure`, or `always`, and declared source
paths must exist. Secret file sources must not grant group or other permission.

The world `name` is its stable operational address; changing it addresses a
different world. This namespace rule does not claim that names determine
behavioral or ontological identity.
