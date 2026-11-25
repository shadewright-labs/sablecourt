# Limits reference

Every numeric limit, ceiling, timeout and default in the daemon and the CLI, with the file that sets it. Sizes use KiB (1024 bytes) and MiB.

Status: built.

## HTTP server

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/operator_auth.go`.

| Limit | Value |
| --- | --- |
| Open connections on the listener | 256 |
| Read header timeout | 5 s |
| Read timeout | 60 s |
| Idle timeout | 60 s |
| Write timeout | none (streams stay open) |
| Shutdown wait | 5 s |
| Request body | 2 MiB; `/gate/model/` 16 MiB; voice transcribe 24 MiB; `POST /api/v1/councils` 1 MiB |
| Bearer header | 512 bytes |
| Login form body | 1 KiB |
| Login failures | 10 per minute per client, 1024 clients tracked |
| Session cookie lifetime | 30 min |
| Audit list | 50 rows; export 10000 rows |
| Memory and learning list | 100 entries |
| Server-sent state events | every 3 s (`/api/v1/stream`, observer); every 2 s (Conclave stream) |

## Tasks and reconciliation

Source: `internal/controlplane`, `internal/runtime`, `internal/domain/types.go`.

| Limit | Value |
| --- | --- |
| Concurrent task launches (`--max-concurrent`) | 4 |
| Reconcile pass | every 2 s |
| Observation staleness window | 2 min |
| Adapter call timeout | 15 s |
| Adapter start timeout | 30 s |
| Workload start breaker | opens after 3 failures, cools down 2 min |
| Task lease lifetime | 1 h |
| Coding task wall time | 15 min |
| Scry ring buffer per task | 64 KiB |
| Redaction pending buffer | 16 KiB |
| Agent turns in `sablecourt-agent` | 4 |
| Child budget ceiling | 4 KiB |

## Model Gate and budgets

Source: `internal/gate/model/gate.go`, `internal/store/budget.go`.

| Limit | Value |
| --- | --- |
| Request body | 16 MiB |
| Upstream client timeout | 5 min |
| Response captured for usage | 8 MiB |
| Budget reservation lifetime | 15 min |
| Reservation settlement | 3 attempts, 10 s each |
| Fallback trigger | status 429, 502, 503 or 504, for `openai` and `local` only |
| Daily budget window | UTC calendar day, per workload |

## MCP gate, actions and approvals

Source: `internal/gate/mcp`, `internal/approval`, `internal/policy/policy.go`.

| Limit | Value |
| --- | --- |
| Action request body | 2 MiB |
| Approval lifetime | 15 min |
| Out-of-band dialog timeout | 60 s (`sablectl approval approve` waits 65 s) |
| Action circuit breaker | opens after 3 failures, cools down 2 min |
| Upstream MCP client timeout | 45 s (parked) |
| Upstream MCP response | 4 MiB (parked) |
| Device pairing code lifetime | 5 min (pairing routes are disabled) |
| Notify client timeout | 8 s (parked) |
| Voice client timeout | 60 s (parked) |

## Conclaves

Source: `internal/domain/types.go`, `internal/store/conclave.go`, `internal/store/sqlite`.

| Limit | Value |
| --- | --- |
| Runs per room (`wards.maxRuns`) | 0 to 6, default 6 |
| Messages (`wards.maxMessages`) | default twice the vassal count |
| Ping-pong (`wards.maxPingPong`) | default twice the vassal count |
| Room age with a template | 15 min |
| Message body | 64 KiB |
| Relic name | 255 bytes |
| Relics per attempt | 16 |
| Turn input | 1 MiB |
| Dashboard studio form defaults | 40 messages, 4 ping-pong |

## Councils

Source: `cmd/sablecourtd/councils.go`, `internal/domain/types.go`, `internal/store/sqlite/council.go`.

| Limit | Value |
| --- | --- |
| Request body | 1 MiB |
| Question | 30 KiB |
| Encoded opening packet | 59 KiB |
| Council text per message | 64 KiB |
| Title | 512 bytes |
| Repo path | 4096 bytes |
| Ref | 256 bytes |
| `Idempotency-Key` | 256 characters |
| Rounds | default 2, at least 1 |
| Starts | default 6, 1 to 6 |
| Wall time | default 15 min |
| Wards for a council | `maxRuns` 6; `maxMessages` and `maxPingPong` each the larger of twice the member count and starts plus one (7 for the default panel) |
| Preflight timeout | 10 s |
| Opening packet and pre-check | 32 KiB packet; 16 KiB and 10 s for the pre-check; 16 KiB per peer |
| Launch input | 64 KiB |

## Council containers and gate

Source: `internal/runtime/coding/container.go`, `internal/runtime/docker/council_config.go`, `internal/gate/councilproxy`.

| Limit | Value |
| --- | --- |
| CPUs | 2 |
| Memory | 2 GiB, also the size of the `/scratch` tmpfs |
| Processes | 256 |
| Container wall time | the smaller of 15 min and the time left to the deadline |
| Setup timeout | 30 s |
| Root filesystem | read-only, all capabilities dropped, user 10001:10001, no log driver |
| Gate request body | 2 MiB |
| Gate connections | 32 |
| Gate header, read, write, idle timeouts | 5 s, 30 s, 30 s, 30 s; headers up to 32 KiB |
| Tunnels | 16 at once, 15 min each, 5 s to dial |
| Launch certificate lifetime | 2 h |
| Gate probe | drain 5 s; exit code 42 means unsupported |
| Cleanup timeout | 10 s |
| Proxy listen port in the container network | 8888 |

## Learnings

Source: `internal/store/learning.go`, `internal/domain/types.go`.

| Limit | Value |
| --- | --- |
| Statement | 4 KiB |
| Other text field | 512 bytes |
| Outcome summary | 2 KiB |
| Path globs per learning | 16, each up to 256 bytes |
| List size | default 100, at most 1000 |
| Review window (ADR-0012) | 90 days |

## sablectl

Source: `cmd/sablectl`.

| Limit | Value |
| --- | --- |
| HTTP response read | 8 MiB |
| MCP tool arguments | 256 KiB |
| Council and Conclave poll | 500 ms |
| Council question | 30 KiB |
| `approval approve` timeout | 65 s |

## In depth

- [Model gate](../how-it-works/model-gate.md), [approvals](../how-it-works/approvals.md), [council](../how-it-works/council.md) and [harness containers](../how-it-works/harness-containers.md) for why each limit exists.
- [Manifests](manifests.md) for the limits you set yourself, and [troubleshooting](../troubleshooting.md) for the refusals they cause.
