# Network contract

Status: binding contract. Unit and rootless Podman evidence is mandatory in CI
for implementation changes and protected publication paths; editorial-only
pull requests classified by the protected organization-ruleset workflow do not
replay unchanged runtime evidence. The strongest evidence and remaining
invariant gaps are indexed in `INDEX.md`.

The normative acceptance invariants are:

1. A base world has loopback as its only interface.
2. Exterior connects are genuinely unroutable except for an explicit
   host-operator `connect` to a named declared loopback interface.
3. No resolver answers and no UDP leaves.
4. With destinations, the only non-world-authored socket is `127.0.0.1:3128`.
5. CONNECT succeeds only for exact declared host-and-port pairs.
6. Each outward address is resolved by the proxy for that connection.
7. Direct dialing an allowed destination's IP remains unroutable.
8. Proxy death restores the base case without stopping world processes.
9. Ephemeral grants die by deadline or proxy death and removal closes connections.
10. Repeated application of one declaration is indistinguishable under 1–9.

Reapplication replaces the proxy's durable allowance set with the declaration
and clears ephemeral grants. This also restores a declaration-backed allowance
removed by `revoke`; `revoke` changes live policy, not declaration authority.

The invariants, rather than the internal mechanism, define network conformance.
Conforming mechanisms satisfy the same finite observation contract; Kenogram
makes no claim of equivalence beyond that contract. This engineering criterion
is informed by Kenogram's conceptual lineage; it is not a claim to implement
formal morphic bisimulation.

The mechanism uses a short-lived `nsenter` helper to create the listener inside
the world's user and network namespaces and transfer its descriptor over an
`AF_UNIX` socketpair. The helper exits; the host proxy retains the listener. The
proxy resolves per connection, bounds rate and concurrency, logs metadata only,
and closes connections when their grant is removed or expires.

The explicitly invoked `network-diagnostics` view distinguishes exact-policy
`refused` from admitted `dial_failed` attempts for the current proxy generation.
It is a bounded, ephemeral, metadata-only observation: UTC timestamp,
generation, outcome, host, and port. Destination metadata is sensitive. No
traffic content enters the view, and observation cannot alter policy. Both the
diagnostic recorder and the non-authoritative compatibility log use bounded
drop-on-pressure delivery so unavailable readers or storage cannot delay proxy
traffic; diagnostic loss is reported honestly by the command. Host is untrusted
world-authored request metadata. Outcome is a Kenogram-derived bounded
classification influenced by the attempted request and observed dial, but is
not authority. Invalid UTF-8 and Unicode format controls are rejected from the
request target before policy lookup or observation.

The private compatibility `proxy.log` is non-authoritative, drop-on-pressure,
and truncates before its next metadata line would exceed 1 MiB; it has no
retention guarantee.

Declared operator interfaces use the same namespace principle in the opposite
direction: a short-lived helper dials the exact declared loopback address inside
the authoritative generation and transfers the connected descriptor to
`kenogram connect`. It creates no listener in the host namespace, publishes no
container port, and supplies no general host-to-world address primitive.
