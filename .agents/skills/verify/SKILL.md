---
name: verify
description: Prove no-mistakes through its maintained checks and real CLI runtime.
user_invocable: true
---

# /verify — prove the task before the PR

This is the independent runtime-proof layer for no-mistakes. It complements,
and never replaces, the existing Go tests, lint, build, no-mistakes review, or
CI gates.

Run it from this repository on a feature branch with committed changes:

```sh
scripts/verify.sh
```

The verifier first records a local machine inventory, then runs the maintained
race-enabled test suite, generated-skill lint check, and production build. It
finishes by invoking the built binary's real `--version`, `--help`, and
read-only `doctor` commands against a throwaway `NM_HOME`, so the smoke never
reads or writes shared app state and never reaches the shared daemon that other
lanes depend on. `doctor` exits `0` even when its own checks fail, so the
verifier gates on the verdict it prints rather than on its exit code.

Evidence is written under `.no-mistakes/evidence/`, which is gitignored. Keep
credentials, tokens, PHI, and raw provider payloads out of evidence and
reports. The verifier does not contact production services or push, open, merge,
or modify a PR.

A failing verifier is a failure even when another gate passes: fix the task,
commit it, and run `scripts/verify.sh` again from a fresh invocation. Report the
exact commands, result, first actionable failure, and evidence paths. Only
after this proof passes should `/no-mistakes` drive its authoritative review,
fix, push, PR, and CI gates with the complete task intent.
