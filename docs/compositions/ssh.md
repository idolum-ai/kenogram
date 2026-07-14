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

Build the optional image as described in
[`../../images/ssh-world/README.md`](../../images/ssh-world/README.md), then
record its exact local image ID and generate a host key:

```sh
image_id="$(podman image inspect --format '{{.Id}}' localhost/kenogram-ssh:local)"
ssh-keygen -t ed25519 -N '' -f host-key
cp "$HOME/.ssh/id_ed25519.pub" authorized_keys
chmod 0600 host-key authorized_keys
```

Write `sshd_config` with a high loopback-only port:

```text
Port 2222
ListenAddress 127.0.0.1
HostKey /home/agent/.ssh/host-key
AuthorizedKeysFile /home/agent/.ssh/authorized_keys
PidFile /workspace/sshd.pid
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
AllowUsers agent
StrictModes yes
```

Add these sections to a first-world declaration. Replace `base` with
`image_id` and the numeric user with `id -u` and `id -g`:

```toml
[world]
base = "sha256:..."
user = "1000:1000"

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
  -o HostKeyAlias=first -o UserKnownHostsFile=./known_hosts agent@first
```

There is no `-p` or published port. `127.0.0.1:2222` belongs to the world's
network namespace, not the host. `connect` rejects addresses supplied by the
caller, undeclared interface names, stopped worlds, and runtime evidence that
does not match the authoritative generation.

`make e2e-ssh` generates fresh client and host keys and proves the real OpenSSH
client path, wrong-key rejection, host-port absence, undeclared-interface
rejection, replacement, restart, and destruction.
