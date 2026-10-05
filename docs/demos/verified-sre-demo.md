# Recorded SRE acceptance

Two real provider-backed runs passed on 4 October 2026 in a dedicated Kind
cluster with an enforcing CNI. A stock Codex agent used ordinary `kubectl`,
`git` and `gh api` through the shared inspected-TLS gateway to investigate a
workload configured to exit with code 1. It proposed a one-line correction to
`demo-workload.yaml` on a run-owned branch in a disposable GitHub repository.

| Recorded result | First run | Refactored gateway run |
| --- | --- | --- |
| PR | [PR #1](https://github.com/andrewmccall/sproozi-demo/pull/1) | [PR #2](https://github.com/andrewmccall/sproozi-demo/pull/2) |
| Run | `sproozi-live-20261004t163448z-70205` | `sproozi-live-20261004t220331z-81198` |
| Run UID | `c501126d-53dd-4e8d-a3f1-50639ad4912e` | `65231c08-c461-4b86-b206-5589b1fd6bc3` |
| PR commit | `0652af6c23ca92f2583c66d6f2f1b8a18127d3c9` | `a98ebfdd594657b17c497de6ce4b8df6ef02ea8b` |
| Successful model calls | 7 | 9 |
| Verified total tokens | 96,020 | 124,732 |
| Local report | `.local/verification/sre-rest-pr-20261004.json` | `.local/verification/architecture-20261004.json` |

The later run observed controller/gateway image
`sha256:c3fd925f6ad3b415af89382128c440476837de86994d2765493e62ec0834182c`.
Both builds used dirty working trees over base commit
`2ae40a285e448e13f369974d4d6199d61e61adf3`; that commit alone does not reproduce
the built images. The full reports record the agent image and tool identities.

Both reports record observed sandbox security, authenticated allowed reads,
denial probes and scoped cleanup. The probes verified:

- direct Kubernetes API access timed out without the proxy;
- a token with the wrong gateway audience received 401;
- access to an unapproved GitHub repository received 403;
- a Kubernetes Secrets read received 403.

The separate acceptance suite read the real PR back and checked its branch,
commit and file scope. AgentRun success itself came from container exit code 0.
See the [manual guide](../guides/manual-pr-demo.md) to run the current path.

## Scope of this evidence

These are historical observations from dirty working trees, not a clean release
candidate or a fresh test of today's checkout. The redacted reports are local,
gitignored artifacts and are not included in the public repository. The demo
repository may require access. The acceptance runs did not merge or deploy the
PRs, so they do not prove workload recovery or home-ops integration.

The runs exercised semantic Kubernetes, GitHub and model endpoints. They did
not exercise a standalone protocol-tier integration, package installation or
generic destination access. Successful usage accounting on these requests does
not establish hard model-spending ceilings.

Cleanup checks covered run-labelled sandbox and invocation resources. They did
not cover durable budget ConfigMaps or recovery after lost provisioning status.
The current implementation has limitations in model admission, interrupted
cleanup and container completion. See
[security hardening](../reference/security-hardening.md). Release verification
and lint were not established as passing by these acceptance reports.
