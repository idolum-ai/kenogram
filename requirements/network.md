# Network contract

Status: implemented. Unit contracts always run; all observable invariants run
against rootless Podman in the mandatory Linux integration job.

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

The mechanism uses a short-lived `nsenter` helper to create the listener inside
the world's user and network namespaces and transfer its descriptor over an
`AF_UNIX` socketpair. The helper exits; the host proxy retains the listener. The
proxy resolves per connection, bounds rate and concurrency, logs metadata only,
and closes connections when their grant is removed or expires.
