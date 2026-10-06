#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

forbidden=( 'bin/' '.idea/' '.vscode/' 'go.work' '.engineering/' '.codex/' '.aws/' '.kube/' '.agents/skills/' '.claude/skills/' 'skills-lock.json' 'CONTEXT.md' 'docs/PRD.md' 'docs/implementation-plan.md' 'docs/superpowers/' 'docs/impl/' '.DS_Store' 'cover.out' )
is_forbidden() {
  local path=$1 prefix
  for prefix in "${forbidden[@]}"; do
    [[ "$path" == "$prefix" || "$path" == "$prefix"* ]] && return 0
  done
  case "/$path" in
    */.local/*|*/__pycache__/*|*/.pytest_cache/*|*/.ruff_cache/*|*/.venv/*|*/venv/*)
      return 0 ;;
    */.env.example|*/.env.template)
      return 1 ;;
    */.DS_Store|*/.env|*/.env.*|*.py[cod]|*.test|*.out|*.log|*.prof|*.bundle|*.kubeconfig|*.key|*.pem|*.p12|*.pfx|*.secret.yaml|*.secret.yml)
      return 0 ;;
  esac
  return 1
}
bad=()
# Use Git's NUL-delimited form so a deliberately hostile filename cannot hide
# a forbidden path by introducing a newline into the report.
while IFS= read -r -d '' path; do
  if is_forbidden "$path"; then bad+=("$path"); fi
done < <(git ls-files --cached -z)
if ((${#bad[@]})); then
  printf 'Forbidden public-tree paths are tracked:\n' >&2
  printf '  %s\n' "${bad[@]}" >&2
  exit 1
fi

# Check the index itself, rather than only the working tree.  In particular,
# an ignored file can still be staged and must fail the release export gate.
if ! git diff --cached --check; then
  echo 'Cached changes contain whitespace errors' >&2
  exit 1
fi

# Materialise exactly the index into a temporary export and scan that export
# too.  This catches path handling differences (notably symlinks) between the
# index listing and the archive/release tooling.  Never inspect the whole
# checkout: private local files are intentionally allowed to remain there.
export_dir="$(mktemp -d)"
trap 'rm -rf "$export_dir"' EXIT
git checkout-index --all --prefix="$export_dir/"
while IFS= read -r -d '' path; do
  path=${path#./}
  if is_forbidden "$path"; then
    echo "Forbidden path present in index export: $path" >&2
    exit 1
  fi
done < <(cd "$export_dir" && find . -mindepth 1 -print0)

for required in LICENSE CONTRIBUTING.md SECURITY.md CODE_OF_CONDUCT.md README.md docs/README.md \
  docs/explanations/system-architecture.md docs/explanations/capabilities-and-gateways.md \
  docs/explanations/run-lifecycle.md \
  docs/reference/agent-contract.md docs/reference/client-compatibility.md \
  docs/guides/manual-pr-demo.md; do
  git ls-files --cached --error-unmatch -- "$required" >/dev/null 2>&1 || {
    echo "Missing required public file from index: $required" >&2
    exit 1
  }
done
for private in .local/verification/probe.json test/acceptance/.local/verification/probe.json \
  .engineering/TODO.md hack/demo/__pycache__/probe.pyc .env private-key.pem recovery.bundle go.work.sum; do
  if ! git check-ignore --no-index -q "$private"; then
    echo "Expected local artifact to be ignored: $private" >&2
    exit 1
  fi
done
echo 'Public-tree checks passed'
