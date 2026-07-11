# Network contract

Status: planned; no networking is implemented at M1.

The normative acceptance invariants are:

1. A base world has loopback as its only interface.
2. Exterior connects are genuinely unroutable.
3. No resolver answers and no UDP leaves.
4. With destinations, the only non-world-authored socket is `127.0.0.1:3128`.
5. CONNECT succeeds only for exact declared host-and-port pairs.
6. Each outward address is resolved by the proxy for that connection.
7. Direct dialing an allowed destination's IP remains unroutable.
8. Proxy death restores the base case without stopping world processes.
9. Ephemeral grants die by deadline or proxy death and removal closes connections.
10. Repeated application of one declaration is indistinguishable under 1–9.

The reference mechanism is a host process holding a listener created inside a
rootless, none-network namespace. The mechanism is non-normative; the invariants
must be tested against the real supported runtime before this requirement becomes
implemented.
