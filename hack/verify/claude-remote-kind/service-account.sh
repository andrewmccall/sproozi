# Sourced by run.sh (and service_account_test.py).
#
# Kubernetes creates a namespace's "default" ServiceAccount a moment after the
# namespace itself, and admission refuses a Pod applied in between (serviceaccount
# "default" not found), even a Pod that mounts no token. Deployments hide this
# because their ReplicaSet controller retries; a bare Pod does not.

# Seconds to wait for a namespace's default ServiceAccount (normally well under
# one second).
SERVICE_ACCOUNT_WAIT_SECONDS="${SERVICE_ACCOUNT_WAIT_SECONDS:-60}"

# wait_for_default_service_account NAMESPACE: returns once NAMESPACE's default
# ServiceAccount exists; otherwise names what did not appear and fails.
wait_for_default_service_account() {
  local namespace="$1"
  kubectl wait --for=create serviceaccount/default --namespace "$namespace" \
    --timeout="${SERVICE_ACCOUNT_WAIT_SECONDS}s" >/dev/null || {
    echo "ServiceAccount default did not appear in namespace $namespace within ${SERVICE_ACCOUNT_WAIT_SECONDS}s;" \
      "Pods applied there would be refused" >&2
    return 1
  }
}
