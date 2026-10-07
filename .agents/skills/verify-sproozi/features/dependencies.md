# Cluster dependencies

A newcomer can check the Kind deployment and enforcing network before configuring
model credentials or a GitHub App.

## Sub-features

- `deps-host` confirms host tools, Docker health and compatible kubectl.
- `deps-controller` deploys the actual controller in isolated Kind.
- `deps-crds` observes the four Sproozi CRDs and the default Calico CRDs.
- `deps-network` proves a connection works, deny-egress blocks it, and removal restores it.
- `deps-cleanup` removes the owned cluster and scratch while retaining proof.

## How to get to it (user POV)

Follow the guide's optional dependency check after preparing your computer:
`python3 hack/verify/quickstart.py dependencies`.

## Driving it with quickstart.py

Preconditions:

- Docker is running and the guide's local tools are available.
- `python3 hack/verify/quickstart.py doctor` succeeds.

- Run `python3 hack/verify/quickstart.py dependencies`. Require exit zero.
- Read the printed directory's `commands.log`. Require the real Kind gate and
  its controller and NetworkPolicy tests to pass. Optional metrics may be skipped.
- Read `crds-observed.json`. Require `agentruns.sproozi.com`,
  `agentruntimes.sproozi.com`, `agentpolicies.sproozi.com` and
  `agenttemplates.sproozi.com`, plus Calico's `crd.projectcalico.org` entries.
  The pinned manifest also supplies `AdminNetworkPolicy` and
  `BaselineAdminNetworkPolicy` under `policy.networking.k8s.io`.
- Read `result.json`. Require `outcome: passed`, `features.dependencies: passed`,
  `clusterRemoved: true`, `scratchRemoved: true` and `fullDemo: not_run`.
- Confirm the evidence files remain readable after teardown.

## Gotchas

- The gate builds the actual controller image. It is not an offline manifest check.
- Default Kind networking does not enforce the required NetworkPolicy boundary.
- This path explicitly skips cert-manager and optional metrics. It neither
  configures nor proves model inference, the shared gateway or GitHub PR creation.
- The current Pod adapter needs no external Agent Sandbox CRDs or controller.
