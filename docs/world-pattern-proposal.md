# Observation-profile proposal

Status: design proposal; not a binding contract or public data format.

Kenogram currently retains two exact provenance digests: declaration bytes and
the resolved plan. They answer “which intent was applied?” They deliberately do
not answer the different question suggested by the project's name: “which
promised observations remain the same across different material
inscriptions?”

This proposal separates those questions before introducing another digest.

## Proposed observation model

An observation profile is an ordinary Boolean, canonical record projected from
an already validated plan and its verified runtime evidence. It is not a
kenogrammatic structure or an identity criterion. Its first version should
contain only relations for which Kenogram already has a contract:

- workspace loci and which loci are carried across replacement;
- visible mount targets, their read/write and executable posture, but not host
  source paths;
- resource shape: CPU, memory, and PID bounds;
- service names, autostart/restart relations, and executable targets, excluding
  host staging paths and transient process IDs;
- exact declared destination host/port pairs and the observation that all other
  egress is absent;
- required runtime isolation observations: no ambient network, private IPC/PID/
  UTS namespaces, dropped capabilities, no-new-privileges, device absence, and
  no runtime socket;
- the declared workspace/workdir and world-visible copy targets, excluding
  secret bytes and source locations.

The projection must exclude world name, generation number, container ID,
process ID, timestamps, declaration path, temporary paths, proxy socket path,
and implementation/backend labels. Those are inscription or provenance facts,
not promised relations inside the world.

## Canonical form and digest

If the model earns implementation, define a versioned structure, sort every set
by its canonical tuple, encode it with the same deterministic discipline as the
plan, and compute:

```text
world-observation/v1:<sha256(canonical observations)>
```

Keep this value adjacent to—not in place of—the declaration and plan digests.
Never use it to adopt runtime state until the observation model has a stronger
equivalence proof than the exact plan digest it would replace.

## Required proofs before a public format

1. Two sequential generations with different container and process identities
   produce the same profile when their promised observations are unchanged.
2. Renaming a world and its generation changes provenance but not the projected
   observation profile.
3. Changing one mount mode, resource limit, service relation, carried locus, or
   network destination changes the profile.
4. Changing a secret value, host source path, timestamp, or staging location
   does not reveal or accidentally fingerprint that value in the profile.
5. Two runtime mechanisms may receive the same conformance result only after
   both produce the complete required boundary evidence; absence of evidence is
   not equivalence.
6. Canonical encoding and digest fixtures remain stable across map iteration,
   architecture, and supported Go versions.

## First operational use

The first use should be explanatory: `status --json` may eventually report the
profile beside exact provenance so an operator can see that replacement changed
the inscription while preserving the promised world. It must not become an
authorization shortcut, generic backend interface, or claim of formal morphic
bisimulation.

Implementation should wait for a second real realization or another concrete
operation that needs this distinction. Until then, the existing contracts and
exact digests remain authoritative.

The broader term *world-pattern* remains a methodological analogy for the
relational posture described in [`kenogrammatics.md`](kenogrammatics.md). It is
reserved here until Kenogram has a model of loci and relations that earns a
stronger use.
