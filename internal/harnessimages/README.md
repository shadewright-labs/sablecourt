# Local council images

Run these commands from a Sablecourt source checkout:

```sh
sablectl harness build claude
sablectl harness build codex
sablectl harness build agy
```

Each command builds the gate proxy and the selected CLI image through the
current Docker context. The supported image platform is `linux/arm64`.
Use `--source` to select another checkout containing the gate proxy implementation.
The proxy source and explicit build entry point from issue #76 must be present.
Missing source stops the command before it calls Docker.

The local Go toolchain must support Go 1.27.4. The command compiles the proxy
entry point, its gate and socket code, and the state directory helper in a temporary module,
with module downloads, workspace configuration, cgo, and inherited Go flags
disabled. It removes the temporary module after success or failure.

`Lookup` publishes the role, local image tag, state volume, and login argv for
admission and launch. It returns fresh argument slices on each call.

| Role | CLI version | State volume | Login argv |
| --- | --- | --- | --- |
| claude | 2.1.332 | sablecourt-harness-claude | claude auth login |
| codex | 0.172.0 | sablecourt-harness-codex | codex login --device-auth |
| agy | 1.5.17 | sablecourt-harness-agy | agy |

`StateDir` is the volume mount destination and `User` is the non-root identity.
`ProxyImage` publishes the matching proxy image tag. Tags include a digest of
the embedded image definitions. Admission must resolve each tag to its local
image ID and launch that ID, since tags can be reassigned. No login or launch
command should pull an image.

During a council turn, `SABLECOURT_DATA_GATE_URL` points to the per-turn proxy.
A gate client reads its bearer token from the private scratch file named by
`SABLECOURT_TASK_TOKEN_FILE`. The launcher delivers the token over attached
stdin and disables Docker logs for the harness, socket probe, and proxy.

All targets derive from the spike's digest-pinned Debian base. A dated Debian
snapshot fixes the package index. The CLI installation steps select exact
versions and reject unexpected version output. Installation downloads happen
only during an explicit build. The proxy is compiled locally from source.

The Docker build context contains only the embedded Dockerfile and compiled
proxy. It contains no source checkout, client configuration, or login volume.
The Docker subprocess receives Docker connection settings and basic process
settings, with no provider keys or task leases. Child diagnostics are discarded
so they cannot expose build credentials in command output.

The proxy is built first. If the CLI build fails, its successful proxy image and
Docker build cache remain available for the next attempt. The command creates
no runtime container, network, or login volume. Repeating it uses the same tags
and does not depend on temporary state from an earlier attempt.

On an arm64 Mac with Docker available, run the native acceptance test explicitly:

```sh
SABLECOURT_DOCKER_TEST=1 go test -race ./internal/harnessimages -run '^TestDockerImages$' -v
```

It builds all three roles and checks their exact versions and default non-root
identity under a read-only root, dropped capabilities, and no network. It also
checks the proxy executable and default identity. Test containers use private
tmpfs state and scratch, with cleanup registered before launch. The test skips
with a reason when Docker is unavailable or image builds are not enabled.
