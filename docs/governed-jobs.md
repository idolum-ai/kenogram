# Governed jobs

Status: experimental Linux execution surface; release-candidate replay pending.

`kenogram job` runs one noninteractive command in a fresh rootless Podman
container, retains a create-only evidence bundle, stops the container, and
proves exact owned-container absence. It does not reuse persistent worlds or
mount a Docker/Podman control socket.

Create an ordinary Kenogram declaration with an immutable `world.base`, then
write a request conforming to `kenogram.job-request.v1`:

```json
{
  "schema": "kenogram.job-request.v1",
  "job_id": "example-qualification",
  "declaration": {
    "path": "/absolute/machine-local/path/kenogram.toml",
    "sha256": "sha256:<lowercase digest of the exact declaration bytes>"
  },
  "command": {
    "argv": ["/usr/local/bin/qualify", "--json"],
    "working_directory": "/workspace",
    "environment": [
      {"name": "MODE", "public_value": "qualification"},
      {"name": "TOKEN", "secret_file": "/run/qualification-token"}
    ]
  },
  "limits": {
    "timeout_ns": 30000000000,
    "finalize_timeout_ns": 10000000000,
    "stdout_max_bytes": 1048576,
    "stderr_max_bytes": 1048576
  }
}
```

`secret_file` must equal exactly one declaration copy target marked
`secret = true`. Kenogram revalidates that regular source and sends its non-NUL
bytes to the contained launcher over stdin. The value never enters provider
argv, provider environment, plan evidence, or runtime evidence. Target output
and requested artifacts are target-controlled and can disclose target-visible
values; retain them only when that is acceptable.

Run and independently verify:

```sh
kenogram job --request /absolute/request.json \
  --evidence-dir /absolute/new-evidence-directory

kenogram verify-job \
  --evidence-dir /absolute/new-evidence-directory
```

Exit 0 means a complete sealed observation, including a target nonzero exit.
Exit 1 means a sealed refusal/incomplete observation or a post-identity
publication failure. Exit 2 means invocation authority was invalid before
semantic job identity. Always inspect the JSON `status`, `target`, `cleanup`,
and `reasons` fields rather than treating the shell status as the target result.
The provider client's exit status is never used as the target's status. The
contained Kenogram helper records target-local launch, exit or signal, and
monotonic duration in an HMAC-authenticated lifecycle file whose key is passed
only over the bounded launcher stdin protocol. A missing, modified, or
provider-only observation is `unknown`.

The current direct provider enforces `network=none`. A declaration containing
`network.allow` is refused until a job-scoped egress proxy has its own proof.
Commands must be absolute container paths, and the requested working directory
must equal the declared world workdir so provider inspection can bind it.
Read-only/read-write mounts, world
user, CPU, memory, PID, capabilities, seccomp, namespaces, exact image identity,
and the absence of extra mounts are inspected before target admission. The
public `kenogram.podman-runtime-observation.v1` documents retain a closed,
strictly decoded cross-phase proof; `verify-job` re-derives its bindings rather
than accepting arbitrary provider JSON.

The Kenogram executable also acts as the image-independent holder and target
launcher. It must therefore be a self-contained Linux binary for images that do
not provide a compatible dynamic loader. The hosted integration builds it with
`CGO_ENABLED=0`; release-candidate packaging must prove the distributed Linux
binary has the same property. An incompatible development binary fails after
container admission and yields incomplete/unknown evidence, never a fallback.

On macOS, local execution makes no container-isolation claim. With
`KENOGRAM_RUNTIME=apple-container-machine`, the complete command is forwarded
to the configured Linux machine; request, declaration, source, and evidence
paths are interpreted there. Without that explicit handoff the job fails
closed. See [Apple container-machine launcher](apple-container-machine.md).

The normative limits, schemas, evidence inventory, and remaining proof boundary
are defined in [the governed-job contract](../requirements/jobs.md).
