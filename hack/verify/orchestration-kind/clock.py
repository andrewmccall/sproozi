"""Compare Kind node clocks with the host before minting certificates.

The verifier signs its TLS certificates on the host, valid from the host's
current time. A node clock that lags the host (Docker Desktop's VM clock drifts
after the host sleeps) makes those certificates not yet valid inside the
cluster, and TLS then fails with "expired certificate".
"""
import subprocess
import time

# Seconds of difference tolerated. Measurement error is about one second
# (`date +%s` resolution plus `docker exec` latency).
MAX_SKEW_SECONDS = 10


def node_skew(node, now=time.time):
    """Return the node clock minus the host clock, in seconds."""
    before = now()
    stdout = subprocess.run(
        ["docker", "exec", node, "date", "+%s"],
        capture_output=True,
        text=True,
        timeout=30,
        check=True,
    ).stdout
    after = now()
    return int(stdout.strip()) - (before + after) / 2


def check_node_clocks(nodes, skew=node_skew, limit=MAX_SKEW_SECONDS):
    """Fail with a clear message when any node clock is off by over limit."""
    skews = {node: round(skew(node)) for node in nodes}
    for node, seconds in skews.items():
        if abs(seconds) > limit:
            direction = "behind" if seconds < 0 else "ahead of"
            raise RuntimeError(
                f"Kind node {node} clock is {abs(seconds)}s {direction} the host "
                f"(limit {limit}s). Certificates minted on the host would be "
                "rejected inside the cluster, typically as 'tls: expired "
                "certificate'. Resync the container VM clock (restart Docker "
                "Desktop) and rerun."
            )
    return skews
