# AGENTS.md

Agent memory for this repo: invariants and pointers. Deep conditional detail lives in source comments, `docs/`, and regression tests - not here.

Go CLI `no-mistakes`; entry `cmd/no-mistakes`; layout map: `internal/cli`, `internal/daemon`, `internal/pipeline` (+ `steps`), `internal/agent`, `internal/tui`, `internal/git`, `internal/ipc`, `internal/config`, `internal/db`, `internal/paths`, `internal/types`. Build/test/release: `Makefile`.

After non-trivial changes: `gofmt -w .`; `make lint`; `go test -race ./...` (e2e behind `e2e` tag); `make e2e` when touching agents/e2e/fixtures; `go build -o ./bin/no-mistakes ./cmd/no-mistakes`.

**Fork Routing**

- `repos.upstream_url` is the PR base parent; `repos.fork_url` is optional GitHub fork push target.
- `no-mistakes init --fork-url <url>` expects `origin` at the parent and `<url>` at the fork; plain `init` preserves fork URL on idempotent refresh.
- Push and CI auto-fix push must use `resolvePushURL` (`internal/pipeline/steps/common_git.go`). Non-fork paths recover the credentialled upstream from the worktree `origin` at run time because DB `upstream_url` is redacted. `Repo.PushURL()` is for fork-only callers (e.g. `rebase.go`).
- GitHub PR: `--repo` parent; `--head <fork_owner>:<branch>` when `fork_url` set. Existing PR lookup: bare branch + filter head owner; never `gh pr list --head <owner>:<branch>`.
- GitLab/Bitbucket fork MR routing is out of scope; legacy `fork_url` on those hosts must skip PR creation, not self-PR.
- Each run best-effort refreshes upstream/fork via `gate.RefreshRepoURLs` (origin authority; fork needs one matching clone remote; atomic DB replace; failures log bounded reason and keep old registration; never rewrite clone/gate remotes). `Repo.URLsVerified` is run-scoped trust for refreshed DB URLs.

**Credential Redaction in Stored URLs and Errors (security)**

- `gate.InitWithFork` redacts upstream via `safeurl.Redact` before every DB persist and gate-init log; bare gate `origin` stays credentialled via `provisionGate`. Push/CI must recover credentials from worktree `origin` (`resolvePushURL`/`resolveUpstreamURL`), never `Repo.UpstreamURL`/`Repo.PushURL()`.
- Step failures and Bitbucket resolve-repo errors use `safeurl.RedactText`/`safeurl.Redact`. New redaction sites: `internal/safeurl` only.
- Regressions: `TestInitRedactsCredentialURL`, `TestResolveUpstreamURL_*`, `TestResolvePushURL_ForkWinsOverCredential`.

**Documentation**

- `README.md` stays high-level; facts live in `docs/`. Config keys: `docs/src/content/docs/reference/global-config.md`, `repo-config.md`; env/telemetry split: `environment.md`; daemon model: `docs/src/content/docs/concepts/daemon.md`. Guides link; they do not duplicate tables.
- `document.instructions` in `.no-mistakes.yaml` owns placement policy for the document step.

**Agent-Guidance Surfaces**

- Generated skill: `internal/skill/skill.go` body, then `make skill`; never edit `skills/no-mistakes/SKILL.md` directly.
- Driving guidance: skill body + live `axi` strings (`internal/cli/axi*.go`); `docs/src/content/docs/guides/agents.md` pins sentences via `axi_guidance_test.go`.
- `internal/testguidance` default test-quality rule: task-first skill and pipeline roles that author/repair/review tests only.
- Review auto-fix defaults off (`auto_fix.review: 0`); qualify skill, axi notes, and docs if you change it.

**Context, Concurrency, and Processes**

- Thread `context.Context`; `exec.CommandContext`; timeouts on networked work.
- Long-lived cancellable children: `shellenv.ConfigureShellCommand` (process group + `cmd.Cancel`). One-shots: `RunShellCommand`/`OutputShellCommand`/`CombinedOutputShellCommand`, or `StartShellCommand` + `TerminateShellCommandGroup` - `cmd.Cancel` alone does not reap on clean exit (#357 OOM). `WaitDelay` 5s backstop. Regressions: `TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit`, `TestRunShellCommandWithEnv_*`, `TestTerminateShellCommandGroup_*`.
- `internal/procreap`: cwd-under-worktree identity reaping; spares pending/running runs; startup + per-run sweeps. Windows: job objects. Regressions: `internal/procreap`, sweep tests.
- Windows console children: `winproc.Harden(cmd)` (`shellenv` already calls it).

**Recursive Gate-Execution Containment**

- `internal/gatecontext` classifies nested pipeline control (gate common-dir + IPC peer ancestry); `NO_MISTAKES_GATE` diagnostic only. Read-only axi status/logs, help, doctor stay available.
- Phase boundary: `internal/gateguidance` in prompts and generated skill; step agents never control push/PR/CI.

**Filesystem and Paths**

- `filepath.Join`; `NM_HOME` for state; `0o755` dirs / `0o644` files. macOS: resolve symlinks for comparisons.

**Git on Bare Gate Repos (`safe.bareRepository`)**

- Gate git via `git.Run` (`--git-dir` on bare); never bare discovery via `cmd.Dir`/`-C` (#362). Migration DB-authoritative; `git.RunBare` on legacy paths. Regressions: `TestRunOnBareRepoUnderSafeBareRepositoryExplicit`, migration tests.

**`gh` From Bare Gate (`internal/scm/github`)**

- Name PR explicitly (`prSelector`: number, else URL, else fail closed). Empty positional infers `main` and misses feature PRs.
- Regressions: `TestGetChecksTargetsKnownPRByURLWhenNumberMissing`, `TestPRTargetingReadsFailClosedWithoutIdentity`, `TestUpdatePR*`.

**Post-Receive Hook (`internal/git/hook.go`)**

- `--gate` must be absolute (not `$(pwd)` / `.` #269). `normalizeNotifyGatePath` in `daemon_cmd.go` second layer.

**Daemon Singleton Lock (`internal/daemon/lock.go`)**

- One daemon per `NM_HOME`: exclusive `daemon.lock` first in `RunWithOptions`, before recovery/socket; held for life.
- Readiness = IPC health, not PID alone; stop waits for process exit (releases lock). Socket: dial-before-unlink; bounded dial (`daemon_connect_timeout`). `daemon run --root` explicit only.
- Never remove worktrees with `pending`/`running` runs. User model: `docs/src/content/docs/concepts/daemon.md`.

**Bounded Logging and Event-Driven AXI**

- `internal/logstore` bounds daemon/managed/bootstrap logs. Read IPC at DEBUG; mutations INFO; failures WARN.
- `internal/cli/run_reconciler.go` owns subscribe-first axi driving; no fixed `get_run` polling.

**Bounded Loss-Aware Event Subscriptions**

- `ipc/events.go` `ClassOf` taxonomy; `daemon/eventmailbox.go` overflow (64 events, 1 MiB; activity evictable; `stream_gap` sticky).
- `StateRev` on state + snapshots; subscriptions open gapped. Step diff on demand: `ipc.MethodGetStepDiff` (512 KiB cap), not stream.

**Destructive Daemon Lifecycle Guard (`internal/lifecycle/guard.go`)**

- `daemon stop`/`restart`/`update` refuse with active runs unless `--force`; log caller to `<NM_HOME>/logs/cli.log`.

**Testing Conventions**

- E2e for cross-boundary behavior (`e2e` tag; `make e2e` / `scripts/e2e.sh`). Real git temp dirs over mocks.
- `internal/e2edaemon` owns test daemons; never reap shared `~/.no-mistakes`.
- Git packages: unset `GIT_CONFIG_COUNT` in `TestMain`. Daemon-touching packages: isolated `NM_HOME`/`HOME` in `TestMain`.
- `paths.New()` refuses default root under `go test` unless `NO_MISTAKES_ALLOW_DEFAULT_ROOT_IN_TESTS=1`.
- Windows CI: git-heavy shard split; `*_windows_test.go` suffix is GOOS-only. macOS `-race` git segfault: fork pre-exec child, not repo bug.

**Repo Config Trust Boundary (security)**

- `commands.{test,lint,format}` and `agent` from trusted default branch pinned SHA, never pushed SHA (`loadTrustedRepoConfig`, `assertGateTrustedConfigReadable`).
- Trusted-only regardless of `allow_repo_commands`: `document.instructions`, `review.path_instructions`, `disable_project_settings`, `no_ci`, `ci.rerun_transient`. `allow_repo_commands` default false on trusted branch only.
- `review.path_instructions` match full changed-file set, not ignore-filtered subset. `reviewablePaths` in `common_diff.go`.
- Regressions: `TestLoadTrustedRepoConfig_*`, `TestEffectiveRepoConfig_*`, e2e repo-config and review-path journeys.

**CI Monitor Lifecycle**

- `ci_timeout` idle; re-arms on default-branch tip via `timeoutAnchor` only extends. Semantics: `config.go` + `global-config.md`.
- Empty check list not green without proof; trusted `no_ci` with zero checks OK (`ci.go`, `cimonitor`).
- `axi abort --run <id>` reaps orphan monitor without daemon. Terminal PR state monotonic; completes active runs.
- Cancelled checks: rerun before CI fix; budget per name; no rerun after head != `runs.head_sha`. `ci_transient.go` owns classification/retirement.
- Terminal cancel bucket != pending (#628). Live rollup head, not recorded SHA only.
- Regressions: `TestCIStep_*`, `TestChecksPassed_PR607RealLogSequence`, `TestEffectiveRepoConfig_NoCITrustedOnly`.

**Parked / Awaiting-Agent Signal**

- `runs.awaiting_agent_since` set only at approval/fix_review gates; observability only.

**Review-Loop Sessions (`internal/pipeline/sessions.go`)**

- One fixer session per run; every review turn session-free (rereview never resumes prescriber session). Fail-safe: cold on unsupported adapter; failed resume retries fresh; `session_reuse: false` forces cold.
- Codex resume narrower flags; fakeagent parses both argv shapes.

**Uncertified Review (`internal/pipeline/uncertified.go`)**

- Review fixer commit without completed rereview persists per-branch range; next initial review binds `fixRoundProvenanceClause`. Clear only on completed review at/after `to_sha`; rebase remaps SHAs.

**Review Fixer Verification (`internal/pipeline/steps/review.go`)**

- Fix round: focused verification in changed area only; no full-repo test/lint in fix prompt (Test/Lint steps own gates).

**Local Test (`internal/pipeline/steps/test.go`)**

- Targeted validation only; broad regression is CI (`go test -race ./...` in `.github/workflows/ci.yml`). Empty `commands.test` here by design; do not add full suite locally.

**Intent Provenance (`internal/pipeline/steps/intent_prompt.go`)**

- Explicit `--intent` -> agent source (authoritative criteria); inferred keeps hint framing. Agent-source conformance contradictions -> `ask-user` at review. Pre-push review strips deferred pipeline-delivery-only findings (`pipeline_delivery.go`). Missing finding `action` -> `ask-user` (`ActionOrDefault`).

**Test Evidence (orphan branch)**

- Evidence in `StepContext.EvidenceDir`, never on code branch. `internal/evidence` fail-closed publish; links pin commit; `test.evidence.branch` trusted-only.

**Scratch Paths**

- Evidence under `<NM_HOME>/evidence/<runID>` (`paths.EvidenceDir`), never `os.TempDir()`. Executor resolves once into `StepContext.EvidenceDir`. Eval replay sandboxes stay in system temp (held scope).

**Document+Lint Housekeeping**

- Empty `commands.lint`: document step does lint stash on `RunShared`; lint consumes or falls back. Placement policy: one owner per fact; no AGENTS postmortems; no corpus sweeps.

**Local Eval (`internal/eval`)**

- Global-only keys in `config.yaml` (not env); auto-capture after terminal outcome. Gold by recorded fix/skip + merge; `diversified` pinned gold-only. Shared object pools, not per-case bundles. Details: `docs/src/content/docs/reference/eval.md`.

**Telemetry**

- Read surfaces: no pageview; `ReadSurfaceGate` on fingerprint. Perf local-only (`agent_invocations`); remote terminal run event counts only. `internal/agent/invocationmetrics.go` owns fidelity fields.

**Branch Sync (`internal/branchsync`)**

- `sync`/`axi sync`/TUI `u`: guarded FF or equivalent-diverged reset; `--recover` and `--keep-local` rules in `Recover` doc comment in `sync.go`. Push bindings persist SHA/fingerprint/generation. Cancellation unmoved head -> `user_owned`, not recoverable custody.

**Post-Review Head Continuity and Push**

- Post-review steps call `assertPipelineHeadContinuity`. Push uses `review_approved_head_sha` only (exact or descendant), not mutable `HEAD`/`head_sha`.

**Rebase and Force-Push Safety (`internal/pipeline/steps/forcepush.go`)**

- Prefer refuse over clever recovery. Rebase bases from fresh remote refs; unpushed local default commits park (#283).
- `resolveForcePushDecision`: live remote read; allow only safe anchors; fetch failure fails closed.
- `lastSeenSHA` is last observed head, not live tip before push (#281); rebase refreshes `origin/<branch>` on normal push only, not force push.

**macOS Release Signing**

- Developer ID `com.kunchenguid.no-mistakes`, Team `9T2J7MNUP9` permanent. `.github/workflows/release.yml`; `TestReleaseWorkflow*`.

**GitLab Backend (`internal/scm/gitlab`)**

- `glab v1.5x` drift traps in `gitlab.go` comments (host-scoped auth, no `--state opened`, jobs via API).

**When Making Changes**

- New dependencies: check docs, discuss. TDD for bugs and features.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
