# AGENTS.md

Agent memory for this repo: invariants and pointers. Deep conditional detail lives in source comments, `docs/`, and regression tests - not here. Topic invariants moved to `.agents/skills/*/SKILL.md` (symlinked as `.claude/skills`).

Go CLI `no-mistakes`; entry `cmd/no-mistakes`; layout map: `internal/cli`, `internal/daemon`, `internal/pipeline` (+ `steps`), `internal/agent`, `internal/tui`, `internal/git`, `internal/ipc`, `internal/config`, `internal/db`, `internal/paths`, `internal/types`. Build/test/release: `Makefile`.

After non-trivial changes: `gofmt -w .`; `make lint`; `go test -race ./...` (e2e behind `e2e` tag); `make e2e` when touching agents/e2e/fixtures; `go build -o ./bin/no-mistakes ./cmd/no-mistakes`.

**Self-update channel manifest (`internal/update`)**

- `no-mistakes update` reads version metadata exclusively from `channels.json` on the GitHub release-asset CDN (`releases/download/channels/channels.json`), not `api.github.com`; a token is never required. Publisher: `cmd/publish-channels`, invoked from `.github/workflows/publish-channels.yml` (reusable `workflow_call`, plus `workflow_dispatch` / `on: release` backstops). `release.yml` calls it after `finalize` because GitHub does not cascade `GITHUB_TOKEN` `release` events, so release-please (pre)releases would otherwise leave the channel stale. Regressions: `internal/update/channels_test.go`, `workflow_publish_channels_test.go`, `TestReleaseWorkflowCallsPublishChannelsAfterFinalize`.

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
- Invalid analyzer output is not a fatal run error on the first attempt: `runTestAnalyzer` gives a fresh agent the rejected payload and validation errors for a bounded correction (`testAnalyzerMaxAttempts`); only exhausting that bound fails the step. The correction prompt must remain correction-only: no tools, scenario reruns, or external operations; preserve supported observations and downgrade unsupported pass/fail claims to untested. Do not weaken the pass/fail-must-be-live or untested-must-have-a-reason invariants to avoid the retry.
- Verdict policy (captain's call C2 = a, plus the 2026-09-07 no-surface ask-user decision) lives in `verdictFindings` (`internal/pipeline/steps/test.go`): `no-go` parks as an auto-fixable error, `inconclusive` parks as an ask-user warning (live surface, too little driven), `no-surface` parks as an ask-user warning when the change has nothing no-mistakes can drive live, a `fail` scenario with any other verdict is rejected as a contract violation, `no-surface` with any live or pass/fail scenario is rejected so a skipped live validation cannot masquerade as no-surface, and `untested` NEVER parks - it is listed on the PR so reporting it honestly stays cheaper than guessing a pass. Do not make untested blocking.
- Rendering is owned by `internal/pipeline/steps/prsummary_scenarios.go` (Testing section, Pipeline fold, and attestation `live_validation: {verdict, live, total}`). The attestation omits `live_validation` whenever the current head differs from the head Test validated. Pre-contract runs parse and render exactly as before.
- The evidence turn is unconditional: a green `commands.test` baseline is never enough on its own, so the step always drives scenarios afterwards. The step deliberately owns no capability inventory, scratch lab, or environment exports - the agent discovers what the machine can do for itself.
- `test.instructions` is the repository's live-validation runbook. It is trusted-default-branch-only, exactly like `document.instructions` and for the same reason (it steers the gate that validates the pushed branch), and is resolved from the repository only - never from global config, which has no repository to describe.
- Regressions: `TestTestStep_PromptDerivesScenariosAndMarksLive`, `TestTestStep_VerdictPolicy`, `TestTestStep_NoLiveSurfaceCIWorkflowAsksUser`, `TestTestStep_MissingScenarioContractFails`, `TestTestStep_InvalidAnalyzerPayloadTriggersCorrectionRound`, `TestTestStep_InvalidAnalyzerPayloadExhaustsCorrectionBound`, `TestTestStep_ValidMixedPayloadDoesNotRetry` (`internal/pipeline/steps/test_scenario_contract_test.go`), `internal/pipeline/steps/prsummary_scenarios_test.go`, `require_no_mistakes_live_validation_test.go`, `TestEffectiveRepoConfig_TestInstructionsTrustedOnly`.

**Review Schema Retry (`internal/pipeline/steps/review.go`)**

- When the agent adapter marks a review turn's final output as rejected against the schema (`agent.IsStructuredOutputRejected`: Pi when its final JSON fails validation, opencode once its own internal StructuredOutput retries run out), or `parseReviewAnalyzerOutput` refuses the output, that is a formatting slip, not a verdict: `ReviewStep.Execute` reruns a fresh, session-free review of the same prompt plus a note quoting the validation error, up to `reviewAnalyzerMaxAttempts` (3) attempts in total.
Claude Code's `--json-schema` re-prompts within its own limit and reports exhaustion as `error_max_structured_output_retries`; that result is deliberately not marked as a rejection and fails the step as before, so there is no second retry layer and Test's correction loop is untouched.
Findings come only from the attempt that validates; nothing else from a rejected attempt carries over, and fix-mode turns are never retried.
Exhausting the bound fails the step with `validate review analyzer findings after 3 attempts: <last error>`, so an unreadable review never passes (issue #703); the unconfigured "pass" meaning is unchanged.
- Every other failure returns at once, and so does a rejection from a turn its deadline or a cancellation cut short (`agentInvocationError` carries both the adapter's rejection and `errReviewAgentTimeout`). This is a rerun of the review, not a correction turn; Test's correction behaviour is separate and unchanged.
- Eval replay (`internal/eval/replay.go` `observedAgent`) sums token usage across every review attempt and reports none when any attempt lacks reported usage, matching the captured baseline, so a candidate that needed reruns never reads as cheaper.
- Regressions: `internal/pipeline/steps/review_schema_retry_test.go`, `TestReviewStep_UnrunAnalyzerDoesNotApprove`, `TestReplayTokensCoverEveryReviewAttempt`, e2e `TestAnalyzerEvidenceFailuresFailPipelineJourney`.

**CI Step Findings Model**

- `CIStep.Execute` has the review step's shape: a fix round (`sctx.Fixing`) first repairs the findings the executor selected (`repairFromFindings`, `internal/pipeline/steps/ci_fix.go`), then the monitor runs; every settled observation (all checks finished, every authorized transient rerun spent, at least one issue) returns one finding per issue with its action (`ciObservationFindings`, `ci_findings.go`) and the executor's shared loop decides what follows. The step never counts or limits fix rounds; `auto_fix.ci` is enforced by the executor from the round history, which survives `ResetStepsFrom` and daemon restarts. `step_results.ci_fix_attempts` is retired (column kept, never read).
- Classification reads provider structure only: a job verdict (fail bucket) and a merge conflict are `auto-fix` errors; an unresolved transient is `ask-user` (`unresolvedTransientFindings`); a check whose `scm.Check.App` (GitHub `checkSuite.app.slug`) is in `scm.ReviewBots` (Greptile: `greptile-apps`) becomes one `ask-user` warning per unresolved bot comment, bounded (`maxReviewBotCommentFindings`), so it never spends a round. An empty `App` is never a bot. The fix agent, not metadata, decides whether an unknown red is about the code: its `code_change_needed=false` conclusion parks the selected findings as `ask-user` (`ciRepairParkOutcome`).
- `types.Finding.Check` (display name), `types.Finding.CheckID` (provider identity), and the `FindingCategoryCI*` constants are the routing keys the fix half reads (`parseCIFixTargets`); `Finding.UnmarshalJSON` and `findingWire` copy fields explicitly, so a new field must be added there or it drops on every parse. Findings ride the IPC event stream, so per-comment findings stay bounded.
- A published repair keeps monitoring inside the same fix round and must call `sctx.MarkRunning()` (executor-owned: durable status back to `running` plus a `step_started` event), or the step stays `fixing` and neither the TUI nor `axi`'s `checks-passed` (`ciReadyToMerge` requires `running`) can surface. The post-repair stale-evidence guard is `lastRepairStillUnverified`: selected checks are snapshotted before a resumed repair, and targets that remain red with neither a newer completion/execution discriminator nor an observed pending state keep the monitor waiting instead of re-escalating. Findings deferred by a fix round survive pending observations and terminal monitor errors until a fresh settled issue set or an established clean result supersedes them.
- Regressions: `TestCIStep_GreptileOnlyRedParksAsAskUserAndSpendsNoAutoFixRound`, `TestCIStep_MixedGreptileAndTestFailureRoutesOnlyTheTestToAutoFix`, `TestCIStep_PublishedRepairResumesMonitoringAndWaitsForTheRerun`, `TestCIResumedRepairSnapshotsSelectedCheckCompletion`, `TestTerminalFailureFreshnessUsesExecutionIDWithoutCompletionTime`, `TestCIStep_PersistentCheckReadFailureParksAtAskUser`, `TestCIStep_UserFixOfTransientFindingIsHonored`, `TestCIObservationFindings_ClassifiesEachIssueByProviderStructure`, `TestExecutor_MarkRunningReturnsAFixingStepToRunning`, `TestExecutor_RestartsValidationFromRequestedStep`, `TestGetChecksCarriesCheckSuiteAppSlug`, `TestParseFindingsJSON_RoundTripsCICheckFields`. Step tests drive the loop with `stepstest.ExecuteWithAutoFix` (the executor's auto-fix loop in miniature); a test that calls `Execute` once sees only the observation.

**Rebase Step Integration Shape (`rebase.strategy`)**

- `rebase.strategy` (`rebase` default, or `merge`) selects how the rebase step integrates a moved base. Under `merge` each target is merged with `git merge --no-ff` instead of rebased onto, so the reviewed head stays on the branch as the merge commit's FIRST parent. Three things follow and are what the option exists for: `ciRepairContinuityGap`'s ancestry test then passes natively (no patch-id or content guess), publication is a fast-forward, and the merge commit's two parents keep a conflict resolution auditable from outside the pipeline afterwards. To match, `mergeWithAgent`'s resolver prompt requires an ADDITIVE resolution, which `rebaseWithAgent`'s "minimal necessary changes" prompt deliberately does not; do not unify the two prompts.
- `mergeWithAgent` PROVES the merge instead of trusting the agent's conclusion, and target ancestry alone is not that proof (a rebase satisfies it too). It snapshots the pre-merge head and resolves the target to a SHA BEFORE `git merge` runs - resolving by refname afterwards lets an agent that fetches mid-resolution fail a merge that actually landed - and afterwards requires HEAD to have moved off the pre-merge head with BOTH snapshots ancestors of it. `shouldSkipRebase` has already consumed every non-divergent case, so two divergent commits can only both be ancestors of HEAD through a merge commit; that admits a follow-up commit on top of the merge while still rejecting `merge --abort`, a rebase, and `reset --hard` onto the target, and an unrelated commit made after abandoning it. A conflicted rebase is the other mid-operation state the worktree can be left in and git sets no `MERGE_HEAD` for it, so `rebaseInProgress` is checked alongside `mergeInProgress` and a leftover rebase is aborted and rejected too. Every rejection routes through `restorePreMergeHead`, which resets the worktree back to the head that merge started from before returning so a rejected attempt is never left checked out as the branch's state, and is fail-closed: a restore that does not land exactly there, or that leaves a rebase in progress underneath it, is reported inside the returned error. Regressions: `TestRebaseStep_MergeStrategyUnconcludedMergeIsAbortedAndFails`, `TestRebaseStep_MergeStrategyConflictedRebaseLeftInProgressIsAbortedAndFails`, `TestRebaseStep_MergeStrategyAbortedMergeFails`, `TestRebaseStep_MergeStrategyRebasedInsteadOfMergedFails`, `TestRebaseStep_MergeStrategyUnrelatedCommitInsteadOfMergeFails`, `TestRebaseStep_MergeStrategyResetOntoTargetFails`, `TestRebaseStep_MergeStrategyFollowUpCommitAfterTheMergeIsAccepted`.
- The flag is trusted-default-branch-only regardless of `allow_repo_commands`, for the same reason `no_ci` is: a pushed branch must not opt its own integration out of the evidence shape the maintainer chose, in either direction. An unrecognized value fails the config closed rather than falling back.
- `forcePushDecision.fastForward` is a separate, unconditional path: an append-only update publishes with a plain push, no `--force`, so the remote rather than our lease bookkeeping enforces the no-rewrite property. It applies under either strategy.
- `test`/`ci` scope is deliberately untouched: both still run in full on the integrated head, and `ci_fix.go`'s merge-conflict repair still rebases and therefore still always revalidates.
- User-facing semantics are owned by `docs/src/content/docs/reference/repo-config.md` (`rebase.strategy`). Regressions: `internal/pipeline/steps/rebase_merge_test.go`, `internal/config/rebase_strategy_test.go`, `TestResolveForcePushDecision_FastForwardNeedsNoForce`, `TestResolveForcePushDecision_RewriteIsNotAFastForward`.

**Repo Config Trust Boundary (security)**

- `commands.{prepare,test,lint,format}` and `agent` from trusted default branch pinned SHA, never pushed SHA (`loadTrustedRepoConfig`, `assertGateTrustedConfigReadable`).
- Trusted-only regardless of `allow_repo_commands`: `document.instructions`, `review.path_instructions`, `gates`, `disable_project_settings`, `no_ci`, the whole `ci` block, `pr.template`, `pr.publish_intent`, `test.instructions`, `test.allow_approve_over_failure`. `allow_repo_commands` default false on trusted branch only.
- `review.path_instructions` match full changed-file set, not ignore-filtered subset. `reviewablePaths` in `common_diff.go`.
- Regressions: `TestLoadTrustedRepoConfig_*`, `TestEffectiveRepoConfig_*`, e2e repo-config and review-path journeys.
- `pr.base_branch` trusted-only but honors `allow_repo_commands` opt-in; existing PR forge base (`scm.PRBaseBranchReader`) wins for CI repair/tip monitoring; PR lookup by branch only. Semantics: `repo-config.md`.
- Per-run Pi pin (`--model`/`--effort`) is immutable (`runs.pi_profile`) and fully validated before superseding an active run (`ResolvePiProfile`, `validatePiProfileAgentsBeforeCancel`); `ApplyPiProfile` enforces after. Semantics: `global-config.md`.

**Gate-Agent Instruction Neutralization (`disable_project_settings`)**

- Under the trusted opt-out, `agent.EnsureGateNeutralized` refuses any gate agent that does not neutralize the TARGET repo's project instructions; only adapters whose suppression knob is empirically verified implement `NeutralizesGateInstructions()` and only while the knob is actually in effect (an operator override that defeats it reports false and fails closed). Verified: codex (`project_doc_max_bytes=0` + `--ignore-rules`), claude (`--setting-sources user`), pi (`--no-context-files`), and the `acp:omp` ACP target. Grok is deliberately refused (it still discovers native project instructions).
- `acp:omp` neutralization lives in `internal/agent/ompgate.go` and is applied by the `acpxAgent` (`internal/agent/acpx.go`): its default launch is switched to `--agent "omp acp --config <overlay> --no-rules --no-skills --no-extensions"`, where the overlay disables every omp context-file discovery provider AND mnemopi memory. omp has NO CLI flag for context files (`--no-rules` governs only RULES.md); the `--config` overlay is the highest settings layer, so a hostile repo `.omp/config.yml` cannot re-enable a disabled provider. Memory is disabled because a gate turn that reads the repo's AGENTS.md can otherwise auto-retain it and a later turn recall it around the provider suppression (a cross-turn vector omp has and pi does not). Only the default launch qualifies: an `acp_registry_overrides` entry for `omp` is an opaque custom command and fails closed, as does any non-omp `acp:<target>`. Empirically verified against omp 18.2.0 through the real acpx->omp path.
- Regressions: `internal/agent/gateneutralize_test.go` (`TestNeutralizesGateInstructions_OMPTargetUnderOptOut`, `TestOMPGateNeutralization_AppliesSuppressionOverlayAndFlags`), `internal/daemon/gateneutralize_test.go` (`TestNewPipelineAgent_OptOut_AdmitsOMPACPTarget`). User semantics: `docs/src/content/docs/reference/repo-config.md` (`disable_project_settings`).

**Repository-Declared Extra Gates (`gates`)**

- `steps.WithCustomGates` inserts each configured gate immediately after its core anchor. Gates can never skip, reorder, or replace a core step. Valid anchors are `rebase`, `review`, `test`, `document`, and `lint`; `intent` and the delivery tail are refused. User-facing semantics live in `docs/src/content/docs/reference/repo-config.md`.
- A gate step name is `gate.<anchor>.<label>`. `types.ValidCustomGateLabel` owns the path-safe label syntax. `GetStepsByRun` and the attestation use deterministic tie-breaks because gates share their anchor's step order.
- The daemon decides a run's gate list once, stores it in `runs.gates_json`, and reads the pin during recovery. An absent pin means the core pipeline. An unparseable pin fails recovery closed.
- `validReadableStep` accepts gate names for read-only logs. `validStep` still controls run mutations, so `--skip` and `no-mistakes.skip=` refuse gate names.
- Regressions: `internal/config/gates_test.go`, `internal/types/gates_test.go`, `internal/pipeline/steps/customgate_test.go`, `internal/daemon/gate_pin_test.go`, `TestGetStepsByRun_PlacesAGateAfterItsAnchor`, `TestBuildPipelineAttestation_ListsAGateAfterItsAnchor`, `TestAxiLogsRefusesAGateStepNameThatWouldEscapeTheLogDirectory`, `TestSkipPushOptionRefusesARepositoryGateStepName`.

**Parked / Awaiting-Agent Signal**

- `runs.awaiting_agent_since` set only at approval/fix_review gates; observability only.

**Approval Override Is Never a Silent Clean Pass (`pipeline.ApprovalOverrideVerifier`)**

- `CIStep` (still-failing checks) and `TestStep` (persisted failing `commands.test`) implement it; both `ActionApprove` sites route through `Executor.applyApprovalOverride`. Approval always proceeds; unresolved or verify-error marks `db.SetStepOverrideReason` (fail closed). Reason rides `run_completed` (`ipc.Event.CIOverrideReason`) for the TUI banner. Tests: `executor_approval_override_test.go`, `ci_approval_override_test.go`, `test_approval_override_test.go`.
- Test approval explanation lives in `step_results.approval_reason` (NULL = none, empty = no reason); only failing configured command, `no-go`/`inconclusive`, or a Test-agent budget cut qualifies as `passed-with-override` (`db.StepResult.TestOverrideReason`); never reuse `override_reason` for all Test approvals. Configured-command override goes on the attestation; `verify.py` refuses it unless trusted `test.allow_approve_over_failure`. Tests: `internal/e2e/test_approval_exception_test.go`.

**Stale Actions Event Replay (`require-no-mistakes`)**

- `verify.py` reads PR body/head SHA via live REST lookup (`live_pr_facts`) before `GITHUB_EVENT_PATH` when no explicit inputs; reruns replay stale archived events. Tests: `require_no_mistakes_action_test.go`.

**Pre-Push Pipeline Attestation**

- `publishRunHead` must write an existing PR attestation for the proposed head before pushing it on every supported provider with raw content reads. `docs/src/content/docs/reference/repo-config.md` (`pr.template`) owns provider-specific ownership/budget and visible Bitbucket metadata caveats. The protocol has single-publisher scope because the daemon enforces one active run per repository branch; do not add cross-publisher coordination without a separate requirement. Detailed behavior is owned by `docs/src/content/docs/reference/pipeline-steps.md`; local rationale lives in `attestHeadBeforePush` and `publishRunHead`. Regressions: `TestPushStep_AttestsHeadBeforePush`, `TestPushStep_AttestationWriteFailureAbortsBeforePush`, `TestPushStep_UnavailableSCMLeavesStaleAttestationFailingClosed`, `TestPushStep_PushFailureAfterAttestationLeavesBodyAhead`, and `TestCIStep_PublishRepairRebindsAttestationAcrossRepairPushes`.

**Private Mirror and Empty-Index Handoffs**

- `internal/gate` owns private-mirror reconciliation. The accepted Decision 41-A exception, containment and final-tree survival requirements, exact archive-before-delete contract, and descendant preservation are owned by `docs/src/content/docs/concepts/gate-model.md` (Private mirror reconciliation). Decision 41-A permits only a mirror head exactly equal to `Run.SubmittedHeadSHA` to bypass patch proof; it is an explicit exception, not a claim that ownership proves containment. `push.go` must settle shared gate/worktree refs only once. Behavioral coverage lives in `internal/gate/reconcile_test.go` and `internal/pipeline/steps/push_test.go`.
- The correction and CI repair handoff contract is owned by `docs/src/content/docs/reference/pipeline-steps.md`; `stagedChangesPresent` implements the index check in `internal/pipeline/steps/common_fix.go`.

**Custody Recovery (`internal/branchsync`)**

- Missing-head keep-local (#958): `missingHeadKeepLocalRuns` checked first; verified head absent from worktree+gate with compatible recovery refs offers `recover_custody` via `--recover --keep-local` (explicit discard); plain `--recover` refuses.
- Bound-archive keep-local (#954): `recoverySourceAvailable` returns `recoverySourceProof`; divergent head counts only via exactly one revalidated `recovery_archives` binding (`refs/heads/archive/*`); `recoverKeepLocalFromArchive` revalidates and rolls back gate branch on stamp failure. Any bad evidence fails closed without Git mutation. Regressions: `TestBoundArchive*`, `recover_test.go`, `sync_test.go`.

**AXI Run Resolution Is Branch-Scoped (`resolveRun`, `internal/cli/axi_query.go`)**

- `axi status`/`axi logs` implicit run: current branch's active, else most recent, else none; no repo-wide fallback; detached HEAD resolves nothing. Cross-branch: `--run <id>` renders `other_branch_run:` + `current_branch:`; explicit `--run` is observation-only. Regressions: `axi_status_branch_test.go`.

**Daemon Login-Shell Environment (`internal/shellenv`)**

- Service `PATH` (`managedServicePath`) is a bootstrap list; effective env comes from the startup login-shell probe (`ApplyToProcessWithShellRetryExcept`). Probe runs Setsid (`detachFromTerminal`), never via `ConfigureShellCommand`. Only a missing shell binary is retried, only at startup (#143). Regressions: `shellenv_retry_test.go`.

**Document+Lint Housekeeping**

- Empty `commands.lint`: document step does lint stash on `RunShared`; lint consumes or falls back. Placement policy: one owner per fact; no AGENTS postmortems; no corpus sweeps.

**Review Fix-Round Carry (`internal/pipeline/executor.go`, `internal/pipeline/findings.go`)**

- Review gate decides on an append-only outstanding set; a selected finding leaves only on positive coverage (trusted `reviewed_paths`, no re-report or other finding in that file) or explicit approve/skip/abort.
- Clean review certifies the head only when `reviewed_paths` covers every trusted reviewable path; otherwise `NeedsApproval` (`uncoveredReviewMessage`). Fake agent fills coverage via `withReviewCoverage`.
- Recovery keeps a selected ID only if its current finding matches the recorded identity (`retainFindingIDsByIdentity`).

**Pipeline Step Tests (CI latency)**

- Fake `gh`/`glab`/`git` (and the trailer-strip stand-in) are the non-race `internal/pipeline/fakecli` helper (`stepstest.Init`/`LinkFakeCLI`); never re-exec the race test binary. CI-monitor tests live in `steps/citest`; both run under `go test ./...`.
- Tests needing real Git use `internal/testgit.RealGit` (fixed absolute paths, not PATH).

**Telemetry**

- Read-only surfaces (`axi` home/status/logs, `status`, `runs`): NO remote telemetry; never reintroduce `trackCommand`/`trackAxiSurface`/`ReadSurfaceGate` there. Perf local-only (`agent_invocations`); remote terminal run event counts only. `internal/agent/invocationmetrics.go` owns fidelity fields.
- Token/fidelity fields are nullable: not-reported (incl. failed/cancelled invocations) stored as NULL, never zero.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
