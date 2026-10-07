# Exploration and cancellation

After acceptance, a newcomer can inspect the retained Kind cluster, submit another
manual run, and cancel it without merging a proposed correction.

## Sub-features

- `explore-policy` inspects the runtime, policy and template using kubectl.
- `explore-audit` reads the shared gateway's audit stream.
- `explore-run` submits the sample manual AgentRun.
- `explore-cancel` cancels that run and observes resource cleanup.
- `explore-teardown` removes the retained cluster while keeping the reports.

## How to get to it (user POV)

Follow the guide's inspect-and-explore commands after a full demo. Launch it using
`demo --env-file "$PWD/.local/demo.env" --keep-cluster`.

## Driving it with quickstart.py

Preconditions:

- The full demo passed and retained its owned cluster.
- Set `SPROOZI_QUICKSTART_EVIDENCE` to its printed directory and export
  `KUBECONFIG="$SPROOZI_QUICKSTART_EVIDENCE/cluster.kubeconfig"`.
- `python3 hack/verify/quickstart.py doctor --evidence "$SPROOZI_QUICKSTART_EVIDENCE"` succeeds.

- Run `kubectl get agentruntimes,agentpolicies,agenttemplates -n sproozi-system`
  and `kubectl get agentpolicy sre -n sproozi-system -o yaml`. Capture the objects
  in `exploration.log`; require the expected three capabilities and repository scope.
- Run `kubectl logs -n sproozi-system deployment/sproozi-gateway --tail=50`.
  Require audit events correlated with the accepted run UID.
- Run `kubectl create -f examples/sre-demo/manual-agentrun.yaml`, then
  `kubectl get agentrun manual-sre-remediation -n sproozi-system -o yaml`.
  Observe it become active and capture its run UID and sandbox name before cancellation.
- Run `kubectl patch agentrun manual-sre-remediation -n sproozi-system --type=merge -p '{"spec":{"cancel":true}}'`.
  Require terminal phase `Cancelled` and absence of that UID's Pod,
  ServiceAccount, NetworkPolicy and contract.
- Record the commands and observed states in `exploration.log` and a coverage
  result in `exploration.json`. A second run can consume model usage or create a PR.
- Run `python3 hack/verify/quickstart.py cleanup "$SPROOZI_QUICKSTART_EVIDENCE"`.
  Require removed cluster and scratch, and retained proof files.

## Gotchas

- The acceptance run itself has already been deleted. Exploration creates a new run.
- Pressing Ctrl-C on a kubectl watch does not cancel the AgentRun.
- Terminal handling clears identity status; capture the sandbox identity while active.
- If the second run finishes before cancellation, cancellation is unassessed.
- The failing demo workload remains broken because the guide proposes a PR rather
  than applying a repair to the cluster.
