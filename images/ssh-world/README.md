# SSH composition image

This optional image adds an SSH daemon to the reference-world surface. It is
not Kenogram's default image and opens no host port. Build it for the host
operator's identity:

```sh
podman build --pull=missing \
  --build-arg "USER_ID=$(id -u)" --build-arg "GROUP_ID=$(id -g)" \
  --tag localhost/kenogram-ssh:local images/ssh-world
```

Inspect the resulting immutable image ID and follow
[`../../docs/compositions/ssh.md`](../../docs/compositions/ssh.md). The
declaration supplies host keys, authorized keys, daemon configuration, and the
loopback interface explicitly; none are embedded in this image.
