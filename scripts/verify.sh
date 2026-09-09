#!/usr/bin/env bash
set -Eeuo pipefail

repo=$(git rev-parse --show-toplevel)
test "$repo" = "$PWD"
branch=$(git branch --show-current)
test -n "$branch"
test "$branch" != main
test "$branch" != master
test -z "$(git status --porcelain)"

for tool in git go make; do
  command -v "$tool" >/dev/null 2>&1 || {
    printf 'missing required command: %s\n' "$tool" >&2
    exit 1
  }
done

evidence=.no-mistakes/evidence
mkdir -p "$evidence"
git check-ignore -q "$evidence/verify.log"
exec > >(tee "$evidence/verify.log") 2>&1

printf '== inventory ==\n'
printf 'repo: %s\nbranch: %s\ncommit: %s\n' "$repo" "$branch" "$(git rev-parse --short HEAD)"
git --version
go version
uname -a

printf '\n== race-enabled tests ==\n'
go test -race ./...

printf '\n== lint and generated-skill check ==\n'
make lint

printf '\n== production build ==\n'
make build

printf '\n== real CLI smoke ==\n'
# Throwaway NM_HOME: the smoke never reads or writes the shared app state, and
# never reaches the shared daemon that other lanes depend on.
nm_home=$(mktemp -d)
trap 'rm -rf "$nm_home"' EXIT
export NM_HOME="$nm_home" NO_MISTAKES_NO_UPDATE_CHECK=1 NO_MISTAKES_TELEMETRY=off
./bin/no-mistakes --version
./bin/no-mistakes --help >/dev/null
doctor_out=$(./bin/no-mistakes doctor)
printf '%s\n' "$doctor_out"
# doctor exits 0 even when its own checks fail, so gate on the verdict it prints.
# ponytail: string match on the summary line; replace when doctor grows an exit code.
case "$doctor_out" in
  *'some checks failed'*)
    printf 'doctor reported failing checks\n' >&2
    exit 1
    ;;
esac

printf '\nverification passed; evidence: %s\n' "$evidence/verify.log"
