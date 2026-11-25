# Limits reference

The limits, ceilings, timeouts and defaults that refuse, cut or time out a request, a task or a stored value, in the daemon, the CLI and the agent program, with the file that sets each. Display-only truncation (dashboard previews, dialog text, diagnostic text) and values used only in tests are left out. Sizes use KiB (1024 bytes) and MiB.

Status: built.

## HTTP server

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/operator_auth.go`, `cmd/sablecourtd/host.go`, `internal/webhook/github.go`, `internal/integrations/shadewright.go`, `internal/ui/audit.go`, `internal/ui/memory.go`.

| Limit | Value |
| --- | --- |
| Open connections | 256 on the TCP listener, and 256 more on the Unix socket |
| Read header timeout | 5 s |
| Read timeout | 60 s |
| Idle timeout | 60 s |
| Write timeout | none (streams stay open) |
| Shutdown wait | 5 s |
| Request body | 2 MiB on any write; `/gate/model/` 16 MiB; `POST /api/v1/voice/transcribe` 24 MiB. `POST /api/v1/councils` 1 MiB, and the GitHub webhook and the two Shadewright routes 2 MiB each, inside the general 2 MiB |
| Operator token length | exactly 67 characters (`op_` and 64 hex digits) |
| Observer bearer token | 512 bytes, counted after `Bearer ` |
| Host name in the `Host` header | 253 characters, each label 63, port 1 to 65535 |
| Login form body | 1 KiB |
| Login failures | 10 per minute per client address (an IPv4 address or an IPv6 /64), 1024 per minute across all clients; at most 1024 client addresses tracked |
| Session cookie lifetime | 30 min |
| Audit list | 50 rows; export 10000 rows; dashboard audit page 250 rows |
| Memory and learning list | 100 entries (API and dashboard) |
| Dashboard Relic upload | 1 MiB of content (form up to 1.25 MiB), name at most 200 bytes |
| Server-sent state events | every 3 s (`/api/v1/stream`, observer); every 2 s (Conclave stream) |

## Tasks and reconciliation

Source: `internal/controlplane`, `internal/runtime`, `internal/attach`, `internal/store/budget.go`, `internal/gate/model/gate.go`, `cmd/sablecourt-agent/main.go`.

| Limit | Value |
| --- | --- |
| Concurrent task launches (`--max-concurrent`) | 4 |
| Reconcile pass | every 2 s |
| Observation staleness window | 2 min |
| Adapter call timeout | 15 s |
| Adapter start timeout | 30 s |
| Task lease lifetime | 1 h |
| Coding task wall time | 15 min |
| Persisted runtime handle | 16 KiB |
| Workflow final message | 64 KiB; a longer one is dropped |
| Scheduler record of fired workflow minutes | 24 h |
| Scry ring buffer per task | 64 KiB |
| Redaction pending buffer | 16 KiB |
| Agent turns in `sablecourt-agent` | 4 |
| `sablecourt-agent` gate calls | 10 s to read a thread or post a message, 15 s for an action, 90 s for a model call; model response read 4 MiB, action response read 1 MiB, `max_tokens` 1024 |
| Child budget ceiling | 4 KiB of JSON |

## Model Gate and budgets

Source: `internal/gate/model/gate.go`, `internal/store/budget.go`.

| Limit | Value |
| --- | --- |
| Request body | 16 MiB |
| Upstream client timeout | 5 min |
| Response captured for usage | 8 MiB |
| Budget reservation lifetime | 15 min |
| Reservation settlement | 3 attempts inside one 10 s limit |
| Fallback trigger | status 429, 502, 503 or 504 or a failed connection, for `openai` and `local` only, and only to the other of the two when the workload lists it |
| Reserved tokens per request | at most 2^40 in any one dimension |
| Daily budget window | UTC calendar day, per workload |

## MCP gate, actions and approvals

Source: `internal/gate/mcp`, `internal/approval`, `internal/policy/policy.go`, `internal/notify`, `internal/voice/voice.go`, `internal/providers`, `cmd/sablecourtd/main.go`.

| Limit | Value |
| --- | --- |
| Action request body | 2 MiB (read; the rest is cut off) |
| Approval lifetime | 15 min |
| Out-of-band dialog timeout | 60 s (`sablectl approval approve` waits 65 s) |
| Action circuit breaker | per `action:` capability: opens after 3 failures in a row, cools down 2 min, a success resets it |
| Upstream MCP client timeout | 45 s (parked) |
| Upstream MCP response | 4 MiB (parked) |
| Device pairing code lifetime | 5 min (pairing routes are disabled) |
| Notify client timeout | 8 s (parked) |
| Voice client timeout | 60 s (parked) |
| Speech-to-text response read | 4 MiB (parked) |
| Text-to-speech response read | 16 MiB (parked) |
| Text-to-speech input | 4000 bytes, cut at that length (parked) |
| Provider model-list client timeout | 10 s |
| GitHub device-flow client timeout | 15 s; polling every 5 s, slower when the server asks (parked) |
| Manifests Git sync interval | 60 s (`--manifests-git-interval`, parked) |

## Conclaves

Source: `internal/domain/types.go`, `internal/store/conclave.go`, `internal/conclave/broker.go`, `internal/ui/conclave.go`.

| Limit | Value |
| --- | --- |
| Runs per room (`wards.maxRuns`) | 0 to 6, default 6 |
| Messages (`wards.maxMessages`) | default twice the vassal count |
| Ping-pong (`wards.maxPingPong`) | default twice the vassal count |
| Room age with a template | 15 min |
| Message body | 64 KiB |
| Relic body and name | no size check of their own beyond the 2 MiB request body (a council has its own, see below) |
| Turn input | 1 MiB |
| Dashboard studio form defaults | 40 messages, 4 ping-pong |

## Councils

Source: `cmd/sablecourtd/councils.go`, `internal/domain/types.go`, `internal/store/council.go`, `internal/store/sqlite/council.go`, `internal/contextpack/council.go`, `internal/runtime/coding/launch.go`.

| Limit | Value |
| --- | --- |
| Request body | 1 MiB |
| Question | 30 KiB |
| Encoded opening packet | 59 KiB |
| Council text per message | 64 KiB |
| Relic name in a council | 255 bytes, and not `digest` |
| Relic body in a council | 1 MiB |
| Relics per attempt | 16 |
| Title | 512 bytes |
| Repo path | 4096 bytes |
| Ref | 256 bytes |
| `Idempotency-Key` | 256 characters |
| Rounds | default 2, at least 1 |
| Starts | default 6, 1 to 6 |
| Wall time | default 15 min |
| Wards for a council | `maxRuns` 6; `maxMessages` and `maxPingPong` each the larger of twice the member count and starts plus one (7 for the default panel) |
| Preflight timeout | 10 s |
| Context packet for a council turn | 32 KiB, at most 63 KiB once JSON-encoded; peer excerpts 16 KiB |
| Launch input | 64 KiB |

## Council containers and gate

Source: `internal/runtime/coding/container.go`, `internal/runtime/coding/council.go`, `internal/runtime/docker/council_config.go`, `internal/runtime/docker/streams.go`, `internal/gate/councilproxy`.

| Limit | Value |
| --- | --- |
| CPUs | 2 |
| Memory | 2 GiB, also the size of the `/scratch` tmpfs |
| Processes | 256 |
| Container wall time | the smaller of 15 min and the time left to the deadline |
| Setup timeout | 30 s |
| Root filesystem | read-only, all capabilities dropped, user 10001:10001, no log driver |
| Prompt, task token | 64 KiB prompt, 4096 bytes task token |
| Output kept per container | 1 MiB |
| Gate request body | 2 MiB |
| Lease header at the gate | 512 bytes |
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
| Cluster, namespace and workload kind selectors | 128 bytes each |
| Symptom | 256 bytes |
| List size | default 100, at most 1000 |
| Review window (ADR-0012) | 90 days |

## Coding adapter

Source: `internal/runtime/coding`, `internal/attach`.

| Limit | Value |
| --- | --- |
| Launch input | 64 KiB |
| Checkout ref | 256 bytes |
| Conclave ID | 128 bytes |
| Session output line | 1 MiB |
| Session log kept | 1 MiB |
| Harness reply read | 64 KiB |
| `git-diff` artifact | 32 KiB, then cut |
| `session-log` artifact | the last 16 KiB |
| Seatbelt probe | 5 s |
| Stop call | 15 s |
| Grace period before a kill | 1 s |

## Storage and runtimes

Source: `internal/store/sqlite/sqlite.go`, `internal/store/postgres/postgres.go`, `internal/runtime/k8s/k8s.go`, `internal/harnessimages/compiler.go`.

| Limit | Value |
| --- | --- |
| SQLite busy timeout | 5 s |
| PostgreSQL connection lifetime | 30 min (parked) |
| Kubernetes Job time to live after it finishes | 1 h (parked) |
| Kubernetes log stream | 30 min (parked) |
| Council proxy source file | 1 MiB (`sablectl harness build`) |

## sablectl

Source: `cmd/sablectl`.

| Limit | Value |
| --- | --- |
| HTTP response read | 8 MiB |
| MCP tool arguments | 256 KiB |
| Council poll | 500 ms |
| Council question | 30 KiB |
| Request timeout, reads | 5 s for `task list`, `task inspect`, `task cancel`, `task diff`, `workload list`, `budget status`, `audit tail`, `approval list`, `provider list`, `conclave list`, `conclave inspect`, `conclave watch`, `conclave relic get`, `device list`, `device revoke`, `pair`, `voice status`, `memory list`, `memory set`, `memory delete` |
| Request timeout, writes | 10 s for `task submit`, `task suspend`, `task resume`, `task retry`, `task takeover`, `apply`, `audit verify`, `approval deny`, `promote`, `conclave open`, `conclave decree`, `conclave relic put` |
| Other request timeouts | `doctor` 4 s; `conclave close` 5 s; `provider sync` and the council open request 15 s; `voice speak` 30 s; `approval approve` 65 s |
| No request timeout | `task scry`, `audit export`, and the stream half of `task takeover`, `conclave watch` and `council` |
| `mcp-server` tool calls | 5 s, 10 s or 15 s, by tool |

## In depth

- [Model gate](../how-it-works/model-gate.md), [approvals](../how-it-works/approvals.md), [council](../how-it-works/council.md) and [harness containers](../how-it-works/harness-containers.md) for why each limit exists.
- [Manifests](manifests.md) for the limits you set yourself, and [troubleshooting](../troubleshooting.md) for the refusals they cause.
