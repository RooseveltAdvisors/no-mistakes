# AGENTS.md

Agent memory for this repo: invariants and pointers. Deep conditional detail lives in source comments, `docs/`, and regression tests - not here. Topic invariants moved to `.agents/skills/*/SKILL.md` (symlinked as `.claude/skills`).

Go CLI `no-mistakes`; entry `cmd/no-mistakes`; layout map: `internal/cli`, `internal/daemon`, `internal/pipeline` (+ `steps`), `internal/agent`, `internal/tui`, `internal/git`, `internal/ipc`, `internal/config`, `internal/db`, `internal/paths`, `internal/types`. Build/test/release: `Makefile`.

After non-trivial changes: `gofmt -w .`; `make lint`; `go test -race ./...` (e2e behind `e2e` tag); `make e2e` when touching agents/e2e/fixtures; `go build -o ./bin/no-mistakes ./cmd/no-mistakes`.

**Self-update channel manifest (`internal/update`)**

- `no-mistakes update` reads version metadata exclusively from `channels.json` on the GitHub release-asset CDN (`releases/download/channels/channels.json`), not `api.github.com`; a token is never required. Publisher: `cmd/publish-channels`, invoked from `.github/workflows/publish-channels.yml`. Regressions: `internal/update/channels_test.go`.

**GitLab Backend (`internal/scm/gitlab`)**

- `glab v1.5x` drift traps in `gitlab.go` comments (host-scoped auth, no `--state opened`, no `--yes` on `mr update`, jobs via API).

**Gitea Backend (`internal/scm/gitea`)**

- Verified empirically against a real `tea 0.15.1` CLI and a real Gitea 1.27.2 + Actions instance (Docker/Podman `gitea/gitea` + `gitea/act_runner`), not guessed from docs. `tea whoami` has no `--login` flag (unlike every other tea entity subcommand), so `Host.Available` scopes its check through `tea api --login <name> /user` instead - the same host-scoping purpose as glab's `--hostname`.
- `tea actions runs view --jobs --output json` does NOT emit clean structured JSON: `--output json` renders the run header as plain text and only the trailing jobs array is real JSON, and that array carries `status` (queued/in_progress/completed) but no `conclusion` (success/failure/...) at the job level. Job-level pass/fail is read from the REST endpoint (`GET /repos/{owner}/{repo}/actions/runs/{run}/jobs`, which does carry `status` + `conclusion`) reached through `tea api`, which reuses tea's own stored login/token - no separate HTTP client or credential needed.
- `tea pulls list --output json` renders every field (including `index` and `mergeable`) as a JSON *string*, while `tea pulls <idx> --output json` (single-PR view) renders the same fields with native JSON types (`index` an int, `mergeable`/`hasMerged` bools). The two response shapes are genuinely different structs in `gitea.go`; do not unify them.
- `tea pulls create` has no `--output json` flag and echoes the PR body into its human-readable stdout, so a body containing an `http(s)://` URL can defeat a naive "first URL line" scrape. `CreatePR` re-lists the PR by head branch via `tea pulls list` for a structured result instead of parsing create's own output; scraping stdout is only a last-resort fallback.
- `tea pulls edit` has no `--base` flag (unlike `tea pulls create --base`). Retargeting uses `tea api --method PATCH --field base=<branch>` on `/repos/{owner}/{repo}/pulls/{index}`; omitting `--method` would POST because a body defaults tea api to POST. `FindPR` copies `item.Base` onto `scm.PR.BaseBranch` so a per-run override can see the live forge base.
- tea infers "which Gitea instance" from the current directory's git remote, which the daemon's detached bare-gate repo never has, so every invocation carries `--login <name>` explicitly. The login name is resolved from tea's own `config.yml` by host (`scm.ResolveGiteaLogin`), mirroring `glabKnowsHost`/`ghKnowsHost` for detection.
- `Capabilities().MergeableState` is declined (matching Bitbucket): Gitea's PR `mergeable` field has a documented upstream bug (go-gitea/gitea#25849) that can stick `false` after a conflict is actually resolved.
- `tea actions runs list`'s array order is not documented as newest-first, and a branch can have more than one run sharing the same head SHA (e.g. a manual UI re-run), so `GetChecks`/`FetchFailedCheckLogs` select the run via `mostRecentRun` (highest numeric run ID) rather than trusting list order or index `[0]`.
- The comments in `internal/scm/gitea/gitea.go` own the full rationale for each trap.

**GitHub user-attachments (`internal/scm/github/attachments.go`)**

- GitHub.com/GHEC image and video evidence is uploaded at PR render time via the unofficial `POST https://uploads.github.com/user-attachments/assets` endpoint gh 2.99.0 uses. The comments in `attachments.go` own the request shape, token-class allowlist, GHES refusal, and size/type rules. Fail closed: any upload error keeps today's PR rendering. Collection and the orphan evidence branch are unchanged. Setting: `test.evidence.attach_media` (default true), OR `store_in_repo` (both links when both apply). Regressions: `internal/scm/github/attachments_test.go`, `internal/pipeline/steps/pr_attach_media_test.go`.

**OpenCode Adapter Failure Reporting (`internal/agent/opencode*.go`)**

- opencode reports a failed turn on `info.error` with an HTTP 200 and no parts, and serializes every named error as `{"name": ..., "data": {...}}` - the payload fields are nested under `data`, never at the top level. Decoding only the flat shape silently blanks the message, and ignoring non-`StructuredOutputError` variants drops the cause entirely so the run reports the undiagnosable `opencode returned no text output`. `opencodeMessageError` owns the wire shape and `opencodeMessageFailure` owns the surfaced error.
- Retry is opencode's call, not ours: `classifyOpencodeTransient` trusts `data.isRetryable` (status class only as a fallback) and deliberately does not fall through to the shared substring matching, so a 400 quoting a provider's rate-limit prose is never retried. Regressions: `TestOpencodeAgent_FailedTurnSurfacesProviderErrorInsteadOfEmptyOutput`, `TestOpencodeAgent_StructuredOutputErrorReadsNestedErrorData`, `TestOpencodeAgent_RetriesRetryableProviderErrorThenSucceeds`, `TestClassifyOpencodeTransient`.
- A retryable verdict is necessary but not sufficient: `runOnce` creates a FRESH session per attempt, so retrying a turn that already invoked a tool replays its side effects with no memory of them. `classifyOpencodeTransient` withdraws the retry when `opencodeTurnRanTools` saw a `tool` part on the SSE stream (`opencodeStreamState.toolInvoked`, durable - not the cosmetic `pendingStepSeparator` that `step-finish` sets and every emit clears) or in the message response body. Resuming the session instead was rejected: opencode's message endpoint only appends another user message, so whether the model re-runs the tool stays its judgement rather than a guarantee. `step-finish` alone is never tool activity - every turn emits one - and the blip this retry exists for kills the turn before any tool runs, so it is unaffected. Regressions: `TestOpencodeAgent_RetryableFailureAfterToolActivityIsNotRetried`, `TestOpencodeAgent_RetryableFailureWithoutToolActivityStillRetries`, `TestOpencodeAgent_ToolPartInTheMessageResponseAlsoBlocksRetry`.
- The general `info.error` branch in `runOnceWithFormat` must stay AFTER the `isThinkingToolChoiceConflict` check: that conflict is the trigger for the prompt-only structured-output fallback, and a general failure claiming it first silently disables the fallback.
- That fallback is a second attempt in a fresh session too, and a `session.error` can arrive at any point in a turn, so it carries the same gate: `thinkingConflict` annotates the trigger with `errOpencodeToolsAlreadyRan` when the turn ran a tool and `runOnce` then reports the conflict instead of falling back. Regression: `TestOpencodeAgent_ThinkingConflictAfterToolActivityDoesNotFallBack`.
- `opencodeMessageError.rawData` keeps the verbatim `data` payload beside the typed `Data`, because thinking/tool_choice detection scans the payload as text (`providerText`) and a decode into the typed struct drops every provider-specific extra it has no field for.

**Live-Validation Contract (Test step)**

- The Test step's evidence turn returns `scenarios` (name, result pass/fail/untested, `live`, evidence, reason) plus a `verdict` (go/no-go/inconclusive/no-surface); both are REQUIRED by `testFindingsSchema` and validated in `unmarshalRequiredTestFindings`. `live: true` means driven against the real running product in that run - a unit test, stub, or fixture is not live.
- Verdict policy (captain's call C2 = a, plus the 2026-09-07 no-surface ask-user decision) lives in `verdictFindings` (`internal/pipeline/steps/test.go`): `no-go` parks as an auto-fixable error, `inconclusive` parks as an ask-user warning (live surface, too little driven), `no-surface` parks as an ask-user warning when the change has nothing no-mistakes can drive live, a `fail` scenario with any other verdict is rejected as a contract violation, `no-surface` with any live or pass/fail scenario is rejected so a skipped live validation cannot masquerade as no-surface, and `untested` NEVER parks - it is listed on the PR so reporting it honestly stays cheaper than guessing a pass. Do not make untested blocking.
- Rendering is owned by `internal/pipeline/steps/prsummary_scenarios.go` (Testing section, Pipeline fold, and attestation `live_validation: {verdict, live, total}`). The attestation omits `live_validation` whenever the current head differs from the head Test validated. Pre-contract runs parse and render exactly as before.
- The evidence turn is unconditional: a green `commands.test` baseline is never enough on its own, so the step always drives scenarios afterwards. The step deliberately owns no capability inventory, scratch lab, or environment exports - the agent discovers what the machine can do for itself.
- `test.instructions` is the repository's live-validation runbook. It is trusted-default-branch-only, exactly like `document.instructions` and for the same reason (it steers the gate that validates the pushed branch), and is resolved from the repository only - never from global config, which has no repository to describe.
- Regressions: `TestTestStep_PromptDerivesScenariosAndMarksLive`, `TestTestStep_VerdictPolicy`, `TestTestStep_NoLiveSurfaceCIWorkflowAsksUser`, `TestTestStep_MissingScenarioContractFails` (`internal/pipeline/steps/test_scenario_contract_test.go`), `internal/pipeline/steps/prsummary_scenarios_test.go`, `require_no_mistakes_live_validation_test.go`, `TestEffectiveRepoConfig_TestInstructionsTrustedOnly`.

**Repo Config Trust Boundary (security)**

- `commands.{prepare,test,lint,format}` and `agent` from trusted default branch pinned SHA, never pushed SHA (`loadTrustedRepoConfig`, `assertGateTrustedConfigReadable`).
- Trusted-only regardless of `allow_repo_commands`: `document.instructions`, `review.path_instructions`, `disable_project_settings`, `no_ci`, the whole `ci` block, `test.instructions`. `allow_repo_commands` default false on trusted branch only.
- `review.path_instructions` match full changed-file set, not ignore-filtered subset. `reviewablePaths` in `common_diff.go`.
- Regressions: `TestLoadTrustedRepoConfig_*`, `TestEffectiveRepoConfig_*`, e2e repo-config and review-path journeys.
- `pr.base_branch` trusted-only but honors `allow_repo_commands` opt-in; existing PR forge base (`scm.PRBaseBranchReader`) wins for CI repair/tip monitoring; PR lookup by branch only. Semantics: `repo-config.md`.

**Parked / Awaiting-Agent Signal**

- `runs.awaiting_agent_since` set only at approval/fix_review gates; observability only.

**Approval Override Is Never a Silent Clean Pass (`pipeline.ApprovalOverrideVerifier`)**

- Only `CIStep` implements it; both `ActionApprove` sites route through `Executor.applyApprovalOverride`. Approval always proceeds; unresolved or verify-error marks `db.SetStepOverrideReason` (fail closed). Reason rides `run_completed` (`ipc.Event.CIOverrideReason`) for the TUI banner. Tests: `executor_approval_override_test.go`, `ci_approval_override_test.go`.

**Stale Actions Event Replay (`require-no-mistakes`)**

- `verify.py` reads PR body/head SHA via live REST lookup (`live_pr_facts`) before `GITHUB_EVENT_PATH` when no explicit inputs; reruns replay stale archived events. Tests: `require_no_mistakes_action_test.go`.

**Pre-Push Pipeline Attestation**

- `publishRunHead` must write an existing GitHub PR attestation for the proposed head before pushing it. The protocol has single-publisher scope because the daemon enforces one active run per repository branch; do not add cross-publisher coordination without a separate requirement. Detailed behavior is owned by `docs/src/content/docs/reference/pipeline-steps.md`; local rationale lives in `attestHeadBeforePush` and `publishRunHead`. Regressions: `TestPushStep_AttestsHeadBeforePush`, `TestPushStep_AttestationWriteFailureAbortsBeforePush`, `TestPushStep_UnavailableSCMLeavesStaleAttestationFailingClosed`, `TestPushStep_PushFailureAfterAttestationLeavesBodyAhead`, and `TestCIStep_PublishRepairRebindsAttestationAcrossRepairPushes`.

**Custody Recovery (`internal/branchsync`)**

- Missing-head keep-local (#958): `missingHeadKeepLocalRuns` checked first; verified head absent from worktree+gate with compatible recovery refs offers `recover_custody` via `--recover --keep-local` (explicit discard); plain `--recover` refuses.
- Bound-archive keep-local (#954): `recoverySourceAvailable` returns `recoverySourceProof`; divergent head counts only via exactly one revalidated `recovery_archives` binding (`refs/heads/archive/*`); `recoverKeepLocalFromArchive` revalidates and rolls back gate branch on stamp failure. Any bad evidence fails closed without Git mutation. Regressions: `TestBoundArchive*`, `recover_test.go`, `sync_test.go`.

**AXI Run Resolution Is Branch-Scoped (`resolveRun`, `internal/cli/axi_query.go`)**

- `axi status`/`axi logs` implicit run: current branch's active, else most recent, else none; no repo-wide fallback; detached HEAD resolves nothing. Cross-branch: `--run <id>` renders `other_branch_run:` + `current_branch:`; explicit `--run` is observation-only. Regressions: `axi_status_branch_test.go`.

**Document+Lint Housekeeping**

- Empty `commands.lint`: document step does lint stash on `RunShared`; lint consumes or falls back. Placement policy: one owner per fact; no AGENTS postmortems; no corpus sweeps.

**Pipeline Step Tests (CI latency)**

- Fake `gh`/`glab`/`git` (and the trailer-strip stand-in) are the non-race `internal/pipeline/fakecli` helper (`stepstest.Init`/`LinkFakeCLI`); never re-exec the race test binary. CI-monitor tests live in `steps/citest`; both run under `go test ./...`.

**Telemetry**

- Read-only surfaces (`axi` home/status/logs, `status`, `runs`): NO remote telemetry; never reintroduce `trackCommand`/`trackAxiSurface`/`ReadSurfaceGate` there. Perf local-only (`agent_invocations`); remote terminal run event counts only. `internal/agent/invocationmetrics.go` owns fidelity fields.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
