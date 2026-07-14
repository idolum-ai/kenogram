# Reference world source

This Containerfile is the minimal first-run image source shipped with each
Kenogram release. It is intentionally host-bound: the preparation script builds
the declared user with the invoking host's UID/GID, then records the exact local
image ID in the generated declaration.

It contains only Ubuntu's shell and `tail` surface plus certificates and tmux.
It is not a production image, agent image, or universal binary artifact.

For every base update:

1. resolve and review the multi-architecture registry manifest digest;
2. build on Linux/amd64 and Linux/arm64;
3. run `kenogram doctor`, generate a world, and prove dry-run, apply, normal
   entry, repair entry, restart, and destruction;
4. inspect the installed package manifest and review known vulnerabilities;
5. update the digest only in a normal pull request and record the change in the
   changelog.

The release checksum covers this source. The resulting apt packages are
resolved at build time, so independently prepared images may differ. Their
exact local IDs remain immutable inputs on their respective hosts.
