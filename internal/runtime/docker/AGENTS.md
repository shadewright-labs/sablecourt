This file adds to the root AGENTS.md and does not repeat it.

# internal/runtime/docker

- `councilOptions` in `council_config.go` is the one function that builds the create options for a container that runs a council harness seat, including the interactive login run (`login` true). A new harness seat container goes through it. Do not build seat options anywhere else.
- Other builders exist and are exceptions, not a pattern to copy: the login probe (`probeLogin` in `council_preflight.go`), the tool install (`council_tool.go`), and the socket probe and proxy container (`council_proxy.go`). All four use only `councilBaseEnv` for the environment and drop all capabilities with no-new-privileges. The probe and install run as 10001:10001 with a read-only root and no network. The socket probe also has no network and a read-only root, and the proxy joins the per-launch internal network. Their user comes from the proxy spec. In `CreateProxy` the read-only root, the capability drop and no-new-privileges apply only when the spec flags are set, which `internal/gate/councilproxy/socket.go` does. A new builder keeps the same hardening.
- `councilOptions` must keep: a non-root user, a read-only root filesystem, all capabilities dropped, no-new-privileges, and an environment built from `councilBaseEnv` plus named additions.
- Network access is only through the per-launch proxy on the internal network. Never add a direct route, host networking or a published port.
- The task lease never appears in create options, environment or labels. `TestBootstrapKeepsLeaseOutsideProcessEnvironment` holds this.
- `councilWorkspace` refuses a checkout that is the filesystem root, contains the home directory, or contains the runtime socket (direct or through symlinks). `TestCouncilRefusals` in `council_test.go` is its table test.
- Do not loosen these guards or remove table rows. A new guard adds a row in the same change.
- Basis: ADR-0003 and the ADR-0011 amendment of 2026-11-27 (container-only harnesses for 0.1.0).
