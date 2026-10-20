# Verified native session mobility

On 12 October 2026, stock Codex 0.161.0 and Claude Code 2.1.27 completed real
interactive Docker-to-Kubernetes-to-Docker sessions through the production
home CLI, now named `sproozi`. The remote placement used a dedicated two-node Kind cluster with
Calico 3.30.4 and matching `10.244.0.0/16` Pod/CNI pools. Local execution used
Docker Desktop Linux containers. Deterministic TLS providers supplied actual
Responses or Messages streams; no paid-provider request occurred.

The [machine-readable evidence](session-mobility-evidence.json) records the
native/control image digests, logical and native UUIDs, original deadlines and
accounting outcomes. Both proofs used the final managed writer-fence implementation.
These were working-tree proofs based on
`1369ad4926fe466eedbcba20ddf17e678d836ceb`, before the delivery commit.

The clients pinned in [`hack/clients`](../../../hack/clients/README.md) have
since moved on to Codex 0.163.0 and Claude Code 2.1.302. These round trips have
not been repeated with those versions, so this report doesn't show that
movement works with the current pins.

| Observable behavior | Codex | Claude Code |
| --- | --- | --- |
| Genuine native terminal and same UUID after both-direction moves | Passed, five completed turns | Passed, three completed turns |
| Actual tool execution and restored file effect | `exec_command` through native tool calls | Foreground Bash |
| Git history, refs, index, staged/unstaged and untracked bytes | Passed at each checkpoint | Passed at each checkpoint |
| Explicitly selected ignored file retained; other ignored files excluded | Passed | Passed |
| Original deadline and fresh per-visit authority | Passed | Passed |
| Local and remote ungranted Kubernetes/MCP calls | Explicit gateway denials | Explicit gateway denials |
| Local and remote direct IPv4, IPv6 and domain HTTP reachability | Denied | Denied |
| Sandbox firewall mutation | Denied, UID 1000 | Denied, UID 1000 |
| Move while a native reply is held, with new input gated | Passed | Native prerequisite passed; complete placement scenario unassessed |
| Forced cancellation of a held move, with no destination created | Passed | Separate stuck-return cancellation passed; held placement scenario unassessed |
| Final cancellation and finalized visit usage | Passed | Passed |

The administrator probes ran inside the exact native container with its observed
process environment. They checked visit (`allocationId`) and label bindings locally and actual
Run/Pod UIDs remotely before and after probing. Direct domain HTTP failure is
not proof that all DNS queries are blocked. Kubernetes policy grants cluster DNS;
these probes do not remove NetworkPolicy node-traffic exceptions or establish
isolation in another deployment.

Codex completed 165 fixture token units before an interrupted final inference.
Forced cancellation stopped its exact agent, helper and anchor in approximately
0.403 seconds in this run, within the verifier's 15-second bound. The unresolved
reservation was finalized conservatively: 200,000 units charged, zero remaining.
Those 200,000 units describe retained budget liability, not measured completed
provider consumption. Releasing the old stream and recovering the cancellation
did not amend its receipts or create a destination.

Claude made eight completed fixture requests, including native auxiliary calls.
Each reported 40 token units. Four stopped visits finalized exactly 320 units and
left 199,680 of the original 200,000-unit allowance. An earlier retained native transcript captured a recovered gateway 429 retry. The pinned adapter accepts that bounded retry
shape only with a later successful assistant reply and live completion evidence;
a transcript ending at the retry cannot reuse an earlier completed leaf.

## Reproduce and assess the limits

Follow the [complete isolated fixture setup](../../../hack/verify/session-mobility/README.md).
The public `extended.py` driver exercises Codex tools, held movement, denials and
forced cancellation. For Claude, use `claude.py verify --admin-probes` with the
explicit dedicated kubeconfig to include both-placement denial checks. Without
that switch, its verifier uses the developer's session socket and checkpoint CLI.
Raw requests, transcripts, private TLS material and diagnostic logs stay in
protected local evidence directories.

Required unit/envtest, scoped race, lint, protocol and fast verification checks
passed locally. Hosted CI is separate evidence and must be inspected on the PR.
The fixture does not establish paid-provider compatibility, arbitrary native
versions, all tools or failure modes, background/subagent continuity, compacted
histories, existing-session adoption, GUI/IDE ownership, or other platforms.

[Move a live session between Docker and Kubernetes](../../../docs/guides/session-mobility.md) gives the user-facing commands.
[ADR 0010](../../../docs/adr/0010-managed-session-authority-and-admission.md) records authority
and lifecycle choices; [ADR 0011](../../../docs/adr/0011-native-session-compatibility-and-completion.md)
records native compatibility, completion and accepted availability costs.
