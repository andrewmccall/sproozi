"""Wait for a new namespace's default ServiceAccount.

Kubernetes creates it a moment after the namespace, and admission refuses a
Pod applied in between (serviceaccount "default" not found), even one that
mounts no token. A Deployment hides this because its ReplicaSet retries; a bare
Pod does not. Same wording and bound as the Claude remote proof's
hack/verify/claude-remote-kind/service-account.sh.
"""

# Seconds to wait; normally well under one.
SERVICE_ACCOUNT_WAIT_SECONDS = 60


def wait_for_default_service_account(k, namespace, seconds=SERVICE_ACCOUNT_WAIT_SECONDS):
    """Returns once namespace's default ServiceAccount exists. k runs kubectl
    and raises RuntimeError on failure (verify.k); otherwise this raises,
    naming what did not appear."""
    try:
        k("wait", "--for=create", "serviceaccount/default", "-n", namespace, "--timeout=%ds" % seconds)
    except RuntimeError as error:
        raise RuntimeError("ServiceAccount default did not appear in namespace %s within %ds; "
                           "Pods applied there would be refused\n%s" % (namespace, seconds, error)) from error
