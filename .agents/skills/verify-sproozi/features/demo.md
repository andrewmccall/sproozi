# AgentRun to pull request

The configured Kind demo lets stock Codex investigate the deliberately failing
workload and propose its correction in a real pull request.

## Sub-features

- `demo-inputs` checks configured credentials and the disposable repository.
- `demo-run` launches the documented `make demo` path in a new cluster.
- `demo-pr` proves the real PR and bounded file change through the existing live gate.
- `demo-denials` verifies allowed reads and exact policy and network denials.
- `demo-cleanup` proves run cleanup, then removes the owned cluster and scratch.

## How to get to it (user POV)

Complete the guide's GitHub repository, App and credential wizard steps. Run
`python3 hack/verify/quickstart.py demo --env-file "$PWD/.local/demo.env"`.

## Driving it with quickstart.py

Preconditions:

- The guide's disposable repository exists with `demo-workload.yaml` on `main`.
- The configured GitHub App is installed there with the documented permissions.
- Model credentials are valid. A ChatGPT session has no other refresh owner.

- Run `python3 hack/verify/quickstart.py doctor --env-file "$PWD/.local/demo.env"`.
  Require success without inference or browser consent.
- Run `python3 hack/verify/quickstart.py demo --env-file "$PWD/.local/demo.env"`.
  Require exit zero; use `--keep-cluster` if exploration is also requested.
- Inspect `live.json`. Require `outcome: passed`, nonempty `prURL` and `prCommit`,
  `observedSandbox: true`, `denialProbesVerified: true` and `cleanupVerified: true`.
- Open the reported PR through `gh pr view <prURL>` and inspect its diff through
  `gh pr diff <prURL>`. Require the intended correction in `demo-workload.yaml`.
- Read `result.json` for source hashes, feature coverage and cluster cleanup.

## Gotchas

- This path consumes provider usage and creates an external branch and PR.
- Host gh credentials inspect the result; the gateway uses the GitHub App.
- A PR alone does not prove Pod completion, denials or cleanup. Use the live report.
- Missing inputs leave this path unassessed. A dependency pass cannot replace it.
- The helper preserves the PR and the local Docker registry for review.
