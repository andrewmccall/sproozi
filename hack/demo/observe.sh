#!/usr/bin/env bash
set -euo pipefail

kubectl_bin="${KUBECTL:-kubectl}"
namespace="${1:-sproozi-system}"
name="${2:-manual-sre-remediation}"
timeout_seconds="${SPROOZI_DEMO_TIMEOUT_SECONDS:-1800}"
interval_seconds="${SPROOZI_DEMO_POLL_SECONDS:-5}"

if ! [[ "$timeout_seconds" =~ ^[1-9][0-9]*$ ]]; then
  echo "SPROOZI_DEMO_TIMEOUT_SECONDS must be a positive integer" >&2
  exit 1
fi
if ! [[ "$interval_seconds" =~ ^[1-9][0-9]*$ ]]; then
  echo "SPROOZI_DEMO_POLL_SECONDS must be a positive integer" >&2
  exit 1
fi

deadline=$((SECONDS + timeout_seconds))
last_phase=""
while (( SECONDS < deadline )); do
  phase="$("$kubectl_bin" get agentrun "$name" -n "$namespace" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  if [[ "$phase" != "$last_phase" ]]; then
    printf 'AgentRun %s/%s phase: %s\n' "$namespace" "$name" "${phase:-Pending}"
    last_phase="$phase"
  fi

  case "$phase" in
    Succeeded)
      printf 'Demo completed successfully.\n'
      printf 'Audit events (redacted fields only):\n'
      "$kubectl_bin" logs deployment/sproozi-gateway -n "$namespace" --tail=500 2>/dev/null | \
        grep '"runUID"' | tail -n 20 || true
      exit 0
      ;;
    Failed|Cancelled)
      "$kubectl_bin" describe agentrun "$name" -n "$namespace" >&2 || true
      echo "Demo AgentRun ended in $phase" >&2
      exit 1
      ;;
  esac
  sleep "$interval_seconds"
done

"$kubectl_bin" describe agentrun "$name" -n "$namespace" >&2 || true
echo "Timed out waiting for AgentRun $namespace/$name after ${timeout_seconds}s" >&2
exit 1
