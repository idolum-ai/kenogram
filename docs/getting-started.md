# First world

This guide starts from a released Kenogram binary, prepares one small local
image, materializes a world, enters it, stops and restarts it, then destroys it.
It is a local proof of the Kenogram lifecycle, not a production image recipe.

## Check the host

Kenogram currently requires Linux, cgroups v2, rootless Podman, `nsenter`, and
subordinate UID and GID ranges for the current user. Run the complete
non-world-mutating preflight before downloading an image or authoring a
declaration:

```sh
kenogram doctor
```

Every failed check includes its observation and remedy. `doctor` does not alter
a Kenogram world or its durable state; Podman may initialize its own rootless
runtime metadata while answering `podman info`. Do not work around a failure by
running Kenogram as root. Automation can consume the additive JSON report with
`kenogram doctor --json` and should select checks by `name`, not array position.

This is the **use** dependency set. Building Kenogram additionally needs Go,
Make, Git, and `rg`; replaying all composition evidence needs the much larger
tool and storage set in [`../CONTRIBUTING.md`](../CONTRIBUTING.md).

## Prepare the reference world

The commands below use the current release,
[`v0.1.1`](https://github.com/idolum-ai/kenogram/releases/tag/v0.1.1). Download
and inspect its standalone preparation script:

```sh
version=v0.1.1
curl --fail --location --proto '=https' --tlsv1.2 \
  --output prepare-first-world.sh \
  "https://github.com/idolum-ai/kenogram/releases/download/${version}/prepare-first-world.sh"
less prepare-first-world.sh
bash prepare-first-world.sh "${version}" world.toml
```

The script downloads the checksum-covered `reference-world.Containerfile` from
the same release, verifies it, and builds it for the current host UID/GID from
a digest-pinned Ubuntu base. It writes `world.toml` with Podman's exact local
image ID, not a mutable tag. It refuses to overwrite an existing declaration.

The resulting image contains only the common first-world surface: a shell,
`tail`, certificates, the host-bound user, and tmux. Custom images remain fully
supported. The image build has ordinary outbound package-manager access; the
materialized Kenogram world does not inherit that access.

## Plan, apply, and enter

Review the generated declaration, then run:

```sh
kenogram up --dry-run ./world.toml
kenogram up --yes ./world.toml
kenogram status first
kenogram enter first
```

Detach from tmux with `Ctrl-b d`. The workspace remains on the host under
Kenogram's state directory.

To inspect metadata-only carried-state drift from the first committed
generation without reading file contents:

```sh
kenogram inspect-workspace --baseline g1 --json first
```

Paths and file hashes are still sensitive operator metadata. The command is
bounded and read-only; it does not reset or migrate the workspace.

## Restart and remove it

```sh
kenogram down first
kenogram up --yes ./world.toml
kenogram enter first
kenogram destroy --yes first
```

An exact local image ID proves which built image was applied on this host; it
does not make separately built images byte-identical. Production declarations
should use an independently reviewed registry image pinned by repository digest
when the same realization must be shared across machines.

## Host preparation by distribution

Package names vary, but the contract does not:

- Debian and Ubuntu commonly provide Podman, `uidmap`, `fuse-overlayfs`, and
  `nsenter` through the `podman`, `uidmap`, `fuse-overlayfs`, and `util-linux`
  packages.
- Fedora commonly provides Podman, `shadow-utils` for subordinate IDs, and
  `util-linux`.
- On any distribution, confirm that `/etc/subuid` and `/etc/subgid` contain a
  range for the current user and migrate an older rootless Podman store if its
  mappings were created before those ranges.

Treat the distribution's maintained documentation as authoritative for package
installation. `kenogram doctor` remains the final observation boundary.
