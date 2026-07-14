# SSH composition image

This optional image adds an SSH daemon to the reference-world surface. It is
not Kenogram's default image and opens no host port. From a checkout matching
your Kenogram release, build it for the host operator's identity:

```sh
podman build --pull=missing \
  --build-arg "USER_ID=$(id -u)" --build-arg "GROUP_ID=$(id -g)" \
  --tag localhost/kenogram-ssh:local images/ssh-world
```

Published releases also attach the checksum-covered source as
`ssh-world.Containerfile`; it can be built with the same arguments using
`podman build -f ssh-world.Containerfile .`.

Inspect the resulting immutable image ID and follow
[`../../docs/compositions/ssh.md`](../../docs/compositions/ssh.md). The
declaration supplies host keys, authorized keys, daemon configuration, and the
loopback interface explicitly; none are embedded in this image. The Ubuntu
base is digest-pinned, but the OpenSSH package is resolved from Ubuntu 24.04 at
build time. The resulting image ID is immutable; independent builds need not be
byte-identical.
