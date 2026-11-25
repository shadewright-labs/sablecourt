# Limits reference

Limits, ceilings and timeouts that refuse, cut or time out a request, a task or a stored value. Each row names the Go constant that sets it, as `<package directory>.<Name>`.

Status: built.

The rows are checked by `TestLimitsReferenceMatchesConstants` in `internal/refdocs`, which `go test ./...` runs. It finds each named constant in the source, evaluates it, and fails when the value or the unit differs from the Value column, or when the constant no longer exists. A time unit needs a `time.Duration` constant; sizes use B, KiB (1024 bytes), MiB and GiB. A limit that is a literal in the code, with no constant, is not listed here; the [how it works](../how-it-works/index.md) pages describe those where they matter.

## HTTP server

| Limit | Constant | Value |
| --- | --- | --- |
| Open connections on each listener (the TCP listener and the Unix socket each) | `cmd/sablecourtd.maxListenerConns` | 256 |
| Idle keep-alive connection timeout | `cmd/sablecourtd.daemonIdleTimeout` | 60 s |
| Request body of a write, except under `/gate/model/` and `POST /api/v1/voice/transcribe` | `cmd/sablecourtd.maxOperatorWriteBody` | 2 MiB |
| Request body of `POST /api/v1/voice/transcribe` | `cmd/sablecourtd.voiceTranscribeMaxBytes` | 24 MiB |
| Request body of `POST /api/v1/councils`, read inside the general write cap | `cmd/sablecourtd.maxCouncilOpenBody` | 1 MiB |
| Request body of each Shadewright integration route | `internal/integrations.maxRequestBodyBytes` | 2 MiB |
| Status text of a Shadewright verification, in characters | `internal/integrations.maxStatusRunes` | 64 |
| Operator token length, `op_` and 64 hex digits | `internal/operatortoken.size` | 67 |
| Observer bearer token, after `Bearer ` | `cmd/sablecourtd.maxBearerSize` | 512 B |
| Login form body | `cmd/sablecourtd.loginBodyLimit` | 1 KiB |
| Failed logins per client address in one window | `cmd/sablecourtd.loginMaxFailures` | 10 |
| Login failure window | `cmd/sablecourtd.loginFailureWindow` | 1 min |
| Client addresses the login limiter tracks | `cmd/sablecourtd.loginLimiterMaxClients` | 1024 |
| Session cookie lifetime | `cmd/sablecourtd.sessionTTL` | 30 min |
| Dashboard Relic upload, file content | `internal/ui.maxRelicUploadBytes` | 1 MiB |
| Dashboard Relic upload, whole form | `internal/ui.maxRelicFormBytes` | 1280 KiB |

## Tasks and reconciliation

| Limit | Constant | Value |
| --- | --- | --- |
| Task launches in progress at once in the reconciler | `internal/controlplane.maxParallelLaunches` | 4 |
| Timeout of each runtime adapter call except start | `internal/controlplane.adapterCallTimeout` | 15 s |
| Timeout of a runtime adapter start call | `internal/controlplane.adapterStartTimeout` | 30 s |
| Task lease lifetime | `internal/controlplane.taskLeaseTTL` | 1 h |
| Persisted runtime handle | `internal/controlplane.maxPersistedHandleBytes` | 16 KiB |
| Workflow step final message; a longer one is not used | `internal/controlplane.maxFinalMessage` | 64 KiB |
| Scry ring buffer per task | `internal/attach.defaultRingSize` | 64 KiB |
| Redaction pending buffer of a live session | `internal/attach.maxPendingBytes` | 16 KiB |
| Agent turns in `sablecourt-agent` | `cmd/sablecourt-agent.maxAgentTurns` | 4 |
| Child budget ceiling, as JSON | `internal/store.maxChildBudgetCeilingBytes` | 4 KiB |

## Model Gate and budgets

| Limit | Constant | Value |
| --- | --- | --- |
| Request body read by the Model Gate | `internal/gate/model.maxRequestBodyBytes` | 16 MiB |
| Upstream response kept to read token usage | `internal/gate/model.maxCapturedResponseBytes` | 8 MiB |
| Budget reservation lifetime | `internal/store.defaultBudgetReservationTTL` | 15 min |
| Reservation settlement attempts | `internal/gate/model.settleReservationAttempts` | 3 |
| Time limit for all settlement attempts together | `internal/gate/model.settleReservationTimeout` | 10 s |
| Reserved tokens per request in any one dimension (2^40) | `internal/gate/model.maxReservedTokens` | 1099511627776 |

## Conclaves

| Limit | Constant | Value |
| --- | --- | --- |
| Runs per room (`wards.maxRuns`), and the value of `0` | `internal/domain.ConclaveRunCeiling` | 6 |
| Room age of a Conclave opened from a template | `internal/store.ConclaveWallTime` | 15 min |
| Message body | `internal/conclave.maxMessageBody` | 64 KiB |
| Turn input of a Conclave task | `internal/store.MaxConclaveTurnInput` | 1 MiB |
| Dashboard studio form default for `maxMessages` | `internal/ui.defaultWardMaxMessages` | 40 |
| Dashboard studio form default for `maxPingPong` | `internal/ui.defaultWardMaxPingPong` | 4 |

## Councils

| Limit | Constant | Value |
| --- | --- | --- |
| Question, checked by the daemon | `cmd/sablecourtd.maxCouncilQuestion` | 30 KiB |
| Question, checked by `sablectl council` before sending | `cmd/sablectl.maxCouncilQuestionBytes` | 30 KiB |
| Encoded opening packet | `internal/contextpack.MaxCouncilOpeningEncodedPacketBytes` | 59 KiB |
| Council text per message | `internal/store.MaxCouncilText` | 64 KiB |
| Relic name in a council | `internal/store.MaxCouncilRelicName` | 255 B |
| Relics per attempt | `internal/store.MaxCouncilRelicsPerAttempt` | 16 |
| Context packet for a council turn | `internal/contextpack.maxPacketBytes` | 32 KiB |
| Context packet for a council turn, JSON-encoded | `internal/contextpack.MaxCouncilEncodedPacketBytes` | 63 KiB |
| Peer excerpts in a council turn's packet | `internal/contextpack.councilPeerBudget` | 16 KiB |
| Default rounds in `sablectl council` | `cmd/sablectl.defaultCouncilRounds` | 2 |
| `sablectl council` poll interval | `cmd/sablectl.defaultCouncilPollEvery` | 500 ms |
| Council role name at launch | `internal/runtime/coding.maxCouncilRoleBytes` | 64 B |

## Council containers and gate

| Limit | Constant | Value |
| --- | --- | --- |
| CPU of a council container, in billionths of a CPU (2 CPUs) | `internal/runtime/coding.defaultCouncilNanoCPUs` | 2000000000 |
| Memory of a council container | `internal/runtime/coding.defaultCouncilMemoryBytes` | 2 GiB |
| Processes in a council container | `internal/runtime/coding.defaultCouncilPIDs` | 256 |
| Council container setup timeout | `internal/runtime/coding.defaultCouncilSetupTimeout` | 30 s |
| Output kept per council container | `internal/runtime/docker.maxCouncilOutput` | 1 MiB |
| Gate request body | `internal/gate/councilproxy.MaxGateBodyBytes` | 2 MiB |
| Gate connections | `internal/gate/councilproxy.MaxGateConns` | 32 |
| Gate read header timeout | `internal/gate/councilproxy.GateReadHeaderTimeout` | 5 s |
| Gate read timeout | `internal/gate/councilproxy.GateReadTimeout` | 30 s |
| Gate write timeout | `internal/gate/councilproxy.GateWriteTimeout` | 30 s |
| Gate idle timeout | `internal/gate/councilproxy.GateIdleTimeout` | 30 s |
| Gate request headers | `internal/gate/councilproxy.GateMaxHeaderBytes` | 32 KiB |
| `CONNECT` tunnels open at once | `internal/gate/councilproxy.MaxTunnels` | 16 |
| Lifetime of one tunnel | `internal/gate/councilproxy.tunnelLifetime` | 15 min |
| Tunnel dial timeout | `internal/gate/councilproxy.dialTimeout` | 5 s |
| Launch certificate lifetime | `internal/gate/councilproxy.launchCertLifetime` | 2 h |
| Gate probe drain timeout | `internal/gate/councilproxy.probeDrainTimeout` | 5 s |
| Gate probe exit code that means unsupported | `internal/gate/councilproxy.ProbeSocketUnsupportedExitCode` | 42 |
| Gate cleanup timeout | `internal/gate/councilproxy.cleanupTimeout` | 10 s |

## Learnings

| Limit | Constant | Value |
| --- | --- | --- |
| Statement | `internal/store.MaxLearningStatementBytes` | 4 KiB |
| Other text field | `internal/store.MaxLearningFieldBytes` | 512 B |
| Outcome summary | `internal/store.MaxLearningOutcomeSummaryBytes` | 2 KiB |
| Path globs per learning | `internal/store.MaxLearningPathGlobs` | 16 |
| Length of one path glob | `internal/store.MaxLearningPathGlobBytes` | 256 B |
| Cluster, namespace and workload kind selectors, each | `internal/store.MaxLearningSelectorBytes` | 128 B |
| Symptom | `internal/store.MaxLearningSymptomBytes` | 256 B |
| Default list size | `internal/store.DefaultLearningListLimit` | 100 |
| Largest list size | `internal/store.MaxLearningListLimit` | 1000 |
| Review window ([ADR-0012](../adr/0012-learning-evidence-and-expiry.md)) | `internal/domain.LearningReviewTTL` | 90 days |

## Coding adapter

| Limit | Constant | Value |
| --- | --- | --- |
| Launch input, and the prompt and context inside it | `internal/runtime/coding.maxLaunchInput` | 64 KiB |
| Checkout ref | `internal/runtime/coding.maxCheckoutRefBytes` | 256 B |
| Conclave ID | `internal/runtime/coding.maxConclaveIDBytes` | 128 B |
| Session output line | `internal/runtime/coding.maxSessionLine` | 1 MiB |
| Session log kept | `internal/runtime/coding.maxSessionLog` | 1 MiB |
| Wall time of a coding task or council container when the adapter sets none | `internal/runtime/coding.defaultWallTime` | 15 min |
| Grace period before a kill | `internal/runtime/coding.processGracePeriod` | 1 s |
| Diagnostic tail kept from a failed harness | `internal/runtime/coding.maxDiagnosticTail` | 4 KiB |
| Context pack pre-check timeout | `internal/contextpack.precheckTimeout` | 10 s |
| Context pack pre-check output kept | `internal/contextpack.maxPrecheckBytes` | 16 KiB |

## sablectl

| Limit | Constant | Value |
| --- | --- | --- |
| HTTP response read | `cmd/sablectl.maxHTTPResponseBytes` | 8 MiB |
| `mcp-server` tool arguments | `cmd/sablectl.maxMCPArgsBytes` | 256 KiB |
| Encoded council opening packet, checked before sending | `cmd/sablectl.maxCouncilEncodedPacketBytes` | 59 KiB |

## In depth

- [Model gate](../how-it-works/model-gate.md), [approvals](../how-it-works/approvals.md), [council](../how-it-works/council.md) and [harness containers](../how-it-works/harness-containers.md) for why each limit exists.
- [Manifests](manifests.md) for the limits you set yourself, and [troubleshooting](../troubleshooting.md) for the refusals they cause.
