# SSH

SSH is the smallest direct operator composition: a declared daemon listens on
world loopback, and `kenogram connect` carries one byte stream to it without
publishing a host port. SSH still owns authentication, host verification,
encryption, terminals, and commands. Kenogram owns generation selection and
the absence of undeclared host reachability.

This is a trusted operator path, not an edge interpreter. Bytes from an SSH
client reach the world process unchanged. Do not use it to feed untrusted user
input to an agent when the intended boundary requires a separate, disposable
interpretation context.

## Prepare the composition

Build the optional image from a checkout matching your release as described in
[`../../images/ssh-world/README.md`](../../images/ssh-world/README.md). A
published release also includes checksum-covered `ssh-world.Containerfile`.
Record the exact local image ID and generate composition-specific client and
host keys:

```sh
image_id="$(podman image inspect --format '{{.Id}}' localhost/kenogram-ssh:local)"
ssh-keygen -t ed25519 -N '' -f host-key
ssh-keygen -t ed25519 -N '' -f client-key
cp client-key.pub authorized_keys
chmod 0600 host-key client-key authorized_keys
```

Write `sshd_config` with a high loopback-only port:

```text
Port 2222
ListenAddress 127.0.0.1
HostKey /home/agent/.ssh/host-key
AuthorizedKeysFile /home/agent/.ssh/authorized_keys
PidFile /tmp/kenogram-sshd.pid
PubkeyAuthentication yes
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
AllowUsers agent
PermitRootLogin no
DisableForwarding yes
StrictModes yes
```

In the declaration's existing `[world]` table, replace `base` with `image_id`
and `user` with the numeric output of `id -u` and `id -g`:

```toml
base = "sha256:..."
user = "1000:1000"
```

Then append the copied files, interface, and service:

```toml

[[copies]]
source = "./sshd_config"
target = "/home/agent/.ssh/sshd_config"
mode = "0600"
secret = false

[[copies]]
source = "./host-key"
target = "/home/agent/.ssh/host-key"
mode = "0600"
secret = true

[[copies]]
source = "./authorized_keys"
target = "/home/agent/.ssh/authorized_keys"
mode = "0600"
secret = false

[[interfaces]]
name = "ssh"
address = "127.0.0.1:2222"

[[services]]
name = "ssh"
command = ["/usr/sbin/sshd", "-D", "-e", "-f", "/home/agent/.ssh/sshd_config"]
autostart = true
restart = "on-failure"
```

Review and apply the complete declaration. Bind host verification to the
generated key, then connect through the named interface:

```sh
printf 'first %s\n' "$(cut -d' ' -f1,2 host-key.pub)" > known_hosts
kenogram up --dry-run ./world.toml
kenogram up --yes ./world.toml
ssh -o 'ProxyCommand=kenogram connect first ssh' \
  -F /dev/null -i ./client-key \
  -o BatchMode=yes -o IdentitiesOnly=yes \
  -o StrictHostKeyChecking=yes \
  -o GlobalKnownHostsFile=/dev/null \
  -o HostKeyAlias=first -o UserKnownHostsFile=./known_hosts agent@first
```

There is no `-p` or published port. `127.0.0.1:2222` belongs to the world's
network namespace, not the host. `connect` rejects addresses supplied by the
caller, undeclared interface names, stopped worlds, and runtime evidence that
does not match the authoritative generation.

If the first connection races daemon startup, retry it after a moment. Restrict
the host-source private key to its operator and dedicate it to one world. The
copied key is necessarily readable by `sshd` inside that world: `secret = true`
keeps its bytes out of rendered plans and history, but does not hide them from
world processes. Treat world compromise as key disclosure, then rotate the key
and matching `known_hosts` entry after disclosure or world destruction.

`make e2e-ssh` generates fresh client and host keys and proves the real OpenSSH
client path, a forced terminal on both remote standard streams, wrong client-
and host-key rejection, effective forwarding and authentication policy,
host-port absence, undeclared-interface rejection, replacement, restart, and
destruction. The Ubuntu base is pinned by digest, while its OpenSSH package is
resolved at image-build time; the proof logs the observed client and server
versions.

On macOS, the declaration, copied keys, image, and Kenogram state live inside
the selected Linux container machine. The launcher preserves the byte-stream
path, but a real Apple-machine SSH terminal round trip remains part of the
machine-boundary evidence backlog.
