This file adds to the root AGENTS.md and does not repeat it.

# internal/runtime/docker

- `councilOptions` in `council_config.go` is the one function that builds the create options for a council harness container. Every new harness container goes through it. Do not build container options anywhere else.
- It must keep: a non-root user, a read-only root filesystem, all capabilities dropped, no-new-privileges, and an environment built from `councilBaseEnv` plus named additions.
- Network access is only through the per-launch proxy on the internal network. Never add a direct route, host networking or a published port.
- The task lease never appears in create options, environment or labels. `TestBootstrapKeepsLeaseOutsideProcessEnvironment` holds this.
- `councilWorkspace` refuses a checkout that is the filesystem root, contains the home directory, or contains the runtime socket (direct or through symlinks). `TestCouncilRefusals` in `council_test.go` is its table test.
- Do not loosen these guards or remove table rows. A new guard adds a row in the same change.
- Basis: ADR-0003 and the ADR-0011 amendment of 2026-11-27 (container-only harnesses for 0.1.0).
