# Home-ops demo runbook

**Audience:** operators, evaluators

This runbook is the planned end-to-end demo path for showing Sproozi on a real
`home-ops` cluster. It is not a claim that this acceptance has run in the
current checkout:

1. build and publish Sproozi
2. install it into `home-ops`
3. introduce a broken workload through GitOps
4. let monitoring trigger the SRE agent
5. show the proposed remediation PR
6. prove the sandbox stays bounded

## Outcome

If the live acceptance succeeds, it will show:

- Sproozi installed as a bounded control-plane service
- a broken deployment landing through GitOps
- Alertmanager creating an `AgentRun`
- the trusted services provisioning a sandbox in `sproozi-agents`
- the agent proposing a fix against `andrewmccall/home-ops`
- the sandbox staying inside its RBAC, filesystem, and network bounds

## Demo prerequisites

You need:

- this repository checked out locally
- access to your `home-ops` repository
- a reachable cluster managed by `home-ops`
- Flux already reconciling `home-ops`
- Prometheus and Alertmanager already installed in the cluster
- a GitHub App installation that can open PRs against `andrewmccall/home-ops`
- the real Sproozi trusted-service secrets stored in `home-ops` with SOPS

## 1. Build and publish Sproozi

Follow the release-artifact steps in the [home-ops playbook](home-ops-playbook.md).
Use the pinned image and generated install bundle for this scenario.

## 2. Update home-ops

Follow the playbook's app layout, certificate/trust and credential requirements.
Keep monitoring-owned rules and routing under `cluster/apps/monitoring/alerts/`.
The runbook below covers execution and observation after those inputs exist.

## 3. Configure the Sproozi objects

Use the same object model as the runnable example:

- one `AgentRuntime`
- one `AgentPolicy`
- one `AgentTemplate`

For the demo, keep the policy aligned with the current SRE example:

- `kubernetes.read` limited to `sproozi-demo`
- `github.pull_request` limited to `andrewmccall/home-ops`
- `model.inference` enabled
- no generic egress or package capability is needed for the first proof

The template should:

- use `sproozi.com/webhook-receiver=alertmanager`
- use `sproozi.com/webhook-secret=<secret-name>`
- point at the `sre` policy
- point at the `codex` runtime
- instruct the agent to edit `cluster/apps/sproozi/demo-workload.yaml` and create the PR through bounded `gh api` REST

Use one runtime workload container named `agent`. AgentRun success is container
exit zero; independently verify the resulting PR, commit and path scope.
The local disposable-repository acceptance gate cannot be used against home-ops
unchanged.

## 4. Wire the alert path

In `home-ops`, add:

1. a `PrometheusRule` that fires on the demo workload
2. an `AlertmanagerConfig` route to a notification converter/HMAC signer
3. that adapter's signed POST to the Sproozi webhook

The converter/signer is still required work. It must produce `{id,eventContext}`
and the timestamp/signature headers in the
[webhook contract](../reference/webhook-contract.md). A receiver URL alone is
insufficient to connect native Alertmanager notifications.

Receiver target:

```text
http://sproozi-webhook.sproozi-system.svc.cluster.local:8083/webhook/sproozi-system/alertmanager
```

If you enforce the webhook ingress label contract, ensure the sender namespace has:

```yaml
metadata:
  labels:
    sproozi.com/webhook-client: enabled
```

## 5. Reconcile the baseline install

Commit the `home-ops` changes and let Flux apply them.

Useful checks:

```sh
kubectl get deploy -n sproozi-system
kubectl get pods -n sproozi-system
kubectl get agentruntimes,agentpolicies,agenttemplates -n sproozi-system
kubectl get ns sproozi-agents sproozi-demo
```

Expected result:

- controller-manager is running
- shared gateway and webhook are running
- Sproozi CRs exist in `sproozi-system`
- `sproozi-agents` exists but does not yet contain a sandbox for this demo

## 6. Seed the broken deployment

For the cleanest story, introduce the fault **through `home-ops` GitOps**, not by patching the live cluster.

Use a visible one-line break in `cluster/apps/sproozi/demo-workload.yaml`, for example:

```yaml
image: busybox:does-not-exist
```

or:

```yaml
command:
  - /bin/sh
  - -c
  - exit 1
```

The first option is easier to explain on screen because `ImagePullBackOff` is obvious in `kubectl get pods`.

Commit that broken change to the branch Flux reconciles.

## 7. Watch the failure land

Open four panes or terminals:

### Pane 1: workload and alert path

```sh
kubectl get pods -n sproozi-demo -w
kubectl get events -n sproozi-demo --sort-by=.lastTimestamp
```

### Pane 2: Sproozi control plane

```sh
kubectl get agentruns.sproozi.com -n sproozi-system -w
kubectl get pods -n sproozi-agents -w
```

### Pane 3: webhook logs

```sh
kubectl logs -n sproozi-system deploy/sproozi-webhook -f
```

### Pane 4: controller logs

```sh
kubectl logs -n sproozi-system deploy/sproozi-controller-manager -f
```

What you want to see:

1. the demo workload goes unhealthy
2. Prometheus fires the rule
3. Alertmanager calls the webhook
4. the webhook creates an `AgentRun`
5. the controller provisions a sandbox pod in `sproozi-agents`

## 8. Show the proposed fix / PR

Use the GitHub UI or `gh` in a separate browser window or fifth pane:

```sh
gh pr list --repo andrewmccall/home-ops --limit 5
```

In the PR, highlight:

- the change is proposed against `home-ops`, not applied directly to the cluster
- the repository target matches `AgentPolicy.githubPullRequest.repositories`
- the fix is narrow and auditable
- a human still reviews and merges it

If you want a deterministic demo, prepare the expected good state in advance so the likely remediation is obvious:

- broken state: `busybox:does-not-exist`
- proposed fix: restore the known-good image tag

## 9. Show the sandbox is bounded

This segment is important because it turns the demo from “AI that writes PRs” into “AI inside a constrained system.”

### 9.1 Show the sandbox pod security context

```sh
kubectl get pod -n sproozi-agents -o yaml
```

Call out:

- `runAsNonRoot: true`
- `runAsUser: 65532`
- `allowPrivilegeEscalation: false`
- `readOnlyRootFilesystem: true`

### 9.2 Show writable mounts and read-only root

If the runtime image includes `/bin/sh`, run:

```sh
RUN_NAME=<active-demo-run>
RUN_UID=$(kubectl get agentrun "$RUN_NAME" -n sproozi-system -o jsonpath='{.metadata.uid}')
POD=$(kubectl get pod -n sproozi-agents -l "sproozi.com/agentrun-uid=$RUN_UID" -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n sproozi-agents "$POD" -c agent -- /bin/sh -lc 'id && touch /workspace/probe /tmp/probe /home/agent/probe && if touch /usr/sproozi-probe; then echo "Unexpected writable root"; exit 1; fi'
```

If your runtime image does not include a shell, skip this step and rely on the pod spec plus the run-scoped NetworkPolicy and RBAC checks below.

Expected story:

- writing under the bounded `/workspace`, `/tmp` and `/home/agent` mounts works
- writing under the image root filesystem fails

### 9.3 Show direct RBAC and gateway authorization

Select the ServiceAccount from the active Run rather than the first account in
the namespace:

```sh
RUN_SA=$(kubectl get agentrun "$RUN_NAME" -n sproozi-system -o jsonpath='{.status.identity.serviceAccountName}')
kubectl auth can-i get secrets --as="system:serviceaccount:sproozi-agents:$RUN_SA" -n sproozi-demo
kubectl auth can-i get pods --as="system:serviceaccount:sproozi-agents:$RUN_SA" -n sproozi-demo
```

Both answers should be `no`. The sandbox has no direct Kubernetes grants.
During the run, verify an authenticated gateway Pod read returns 200, then a
Secrets read returns 403. The positive control proves the client and identity
work; direct RBAC denial alone does not prove gateway authorization. Capture
those probes and their audit decisions in the home-ops acceptance report.

### 9.4 Show network traffic is forced through the shared gateway

Inspect the created network policy:

```sh
kubectl get networkpolicy -n sproozi-agents
kubectl describe networkpolicy -n sproozi-agents
```

Narrate that:

- the sandbox does not receive raw GitHub or OpenAI credentials
- Kubernetes reads, model inference and PR actions go through one shared authenticated gateway
- the policy grants only cluster DNS and shared gateway egress; no Sproozi proxy sidecar is injected
- verify direct API/provider denial on the actual cluster, including sandbox node placement

Standard NetworkPolicy has exceptions for traffic involving the Pod's own node.
API isolation therefore needs a real probe, especially when the API server runs
on that node. See [Kubernetes NetworkPolicy semantics](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

### 9.5 Show the controller is not letting the run widen its own scope

Show the `AgentRun`, `AgentTemplate`, and `AgentPolicy`:

```sh
kubectl get agentrun -n sproozi-system -o yaml
kubectl get agenttemplate sre-remediation -n sproozi-system -o yaml
kubectl get agentpolicy sre -n sproozi-system -o yaml
```

Point out:

- the run references the template
- the template references the runtime and policy
- the repository scope is fixed
- the Kubernetes namespace scope is fixed

## 10. Close the loop

After the PR is created:

1. review it
2. merge it
3. let Flux reconcile the fix
4. show the workload recovering
5. show the alert clearing

Useful checks:

```sh
kubectl get pods -n sproozi-demo -w
kubectl get agentruns.sproozi.com -n sproozi-system
gh pr view --repo andrewmccall/home-ops <pr-number>
```

## Suggested 90-second recording flow

If you want a short video or GIF, use four panes plus a browser window:

1. `kubectl get pods -n sproozi-demo -w`
2. `kubectl get agentruns -n sproozi-system -w` and `kubectl get pods -n sproozi-agents -w`
3. `kubectl logs -n sproozi-system deploy/sproozi-webhook -f`
4. `kubectl logs -n sproozi-system deploy/sproozi-controller-manager -f`
5. GitHub PR page in a browser

Suggested narration:

1. “Here is the broken GitOps deployment landing in `sproozi-demo`.”
2. “Prometheus and Alertmanager turn that into a signed webhook event.”
3. “Sproozi creates a bounded `AgentRun` and a sandbox in `sproozi-agents`.”
4. “The agent proposes a PR against `home-ops` rather than mutating the cluster directly.”
5. “The sandbox is non-root, read-only, and limited to run-scoped identity, semantic capabilities, and the shared gateway.”

## Optional terminal-only capture

If you want a text-first animation:

```sh
asciinema rec sproozi-home-ops-demo.cast
```

Run the demo, then optionally convert it with whatever cast-to-GIF or cast-to-video tool you already use.

## Demo reset

After the demo:

- merge or revert the broken `home-ops` commit
- let Flux reconcile the good state
- remove demo `AgentRun` objects if you want a clean slate

Example cleanup:

```sh
kubectl delete agentrun "$RUN_NAME" -n sproozi-system
kubectl get pods -n sproozi-demo
kubectl get pods -n sproozi-agents
```

## Notes and caveats

- keep GitHub branch protection enabled; the capability proxy is not the only line of defense
- keep `sproozi-agents` reserved for Sproozi-managed identities
- use the shared inspected-TLS gateway and record actual home-ops evidence before calling this integration accepted
- budget and replay state use Kubernetes ConfigMaps; ChatGPT refresh still requires one active gateway process
- model reservations can undercount total usage, and interrupted provisioning can evade cleanup; see [security hardening](../reference/security-hardening.md)
