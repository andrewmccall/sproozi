# Verify retained remote work

This fixture drives the production task owner, managed Kubernetes placement,
stock Codex 0.161.0, home daemon and Docker continuation. It uses the existing
public deterministic TLS provider. The authenticated MCP caller follows Hermes's
native client protocol; it does not establish a fresh Hermes harness run or a
paid-model software contribution. Personal infrastructure is never selected.

First follow the [mobility setup](../session-mobility/README.md) through image
resolution, Codex configuration, TLS Secret and enforcing two-node Kind
installation. Use its explicit private `work`, `codex` and `cluster` variables.
Start the provider, but leave the home and profile processes stopped. If you
already started them, stop their saved PIDs gracefully first. Retain all journals
and original operation IDs when recovering a failed run.

Extend the generated fixture before starting its owner:

```sh
set -e
python3 hack/verify/portable-workflows/configure.py \
  --fixture "$codex" --manifest "$codex/kubernetes.yaml" \
  --output "$codex/workflows.yaml"
kubectl --kubeconfig "$work/kubeconfig" apply --server-side -f "$codex/workflows.yaml"
KUBECONFIG="$work/kubeconfig" "$work/bin/session-profile" \
  --config "$codex/profile-config.json" >"$codex/profile.log" 2>&1 &
codex_profile_pid=$!
python3 hack/verify/portable-workflows/verify.py \
  --home-config "$codex/home-config.json" --profile-config "$codex/profile-config.json" \
  --session-binary "$work/bin/sproozi" --pty-helper "$work/bin/native-pty" \
  --output-directory "$codex/workflow-proof"
```

The configurator creates a separate private task token and a small stopped Git
snapshot. The verifier submits and replays an authenticated MCP task while the
home socket is absent, waits for genuine native completion and sealed retention,
then starts the home, pulls/claims the checkpoint, resumes the same UUID under a
fresh execution and completes a genuine local follow-up. It stops to retained
state without another destination. `report.json` records identities, stages and
limits; private terminal/provider logs retain native evidence. `--request-id`
retries the original submission instead of manufacturing a replacement.

## Real external MCP

For the keyless public DeepWiki integration, gracefully stop the profile and home,
then repeat configuration with `--deepwiki`. The configurator uses the same
[authored registry](../../../examples/portable-workflows/deepwiki-servers.json)
for a private local copy and the remote ConfigMap, and adds the
[bounded tool policy](../../../examples/portable-workflows/deepwiki-policy.yaml).
Apply the output and restart the gateway to snapshot the changed registry:

```sh
python3 hack/verify/portable-workflows/configure.py \
  --fixture "$codex" --manifest "$codex/kubernetes.yaml" \
  --output "$codex/deepwiki.yaml" --deepwiki
kubectl --kubeconfig "$work/kubeconfig" apply --server-side -f "$codex/deepwiki.yaml"
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-system rollout restart deployment/sproozi-gateway
kubectl --kubeconfig "$work/kubeconfig" -n sproozi-system rollout status deployment/sproozi-gateway --timeout=180s
```

Restart profile and home with the same protected directories, then resume a
retained product session with `sproozi resume SESSION_ID --config FILE --detach`.
The fresh grant has ten `mcp.docs` calls. Run `mcp-probe.js` through the exact
fixture's active agent, first with `docker exec -i CONTAINER_ID node`, then with
`kubectl --kubeconfig FILE -n sproozi-agents exec -i POD -c agent -- node` after a
live move to `remote`. Feed the script on stdin. Select the container/Pod from the
trusted execution record, never from an unrelated context or name match.

The administrator probe uses the actual native process's proxy environment
without printing it. It calls the real `read_wiki_structure` tool for
`modelcontextprotocol/go-sdk`, then verifies wrong-repository, wrong-tool and
direct-provider denials. The calls are transport probes under real worker
identity; they do not establish autonomous model selection of that tool. Move
back locally and stop. Inspect the retained sealed receipts for actual admitted
call units and immutable remaining allowance. Duplicate allowed probes consume
additional units; denied calls do not.

Stop saved fixture process PIDs and delete only this dedicated Kind cluster using
the mobility cleanup instructions. Keep reports and protected journals until you
have reviewed failed-operation stop and accounting evidence. A missing Pod or
expired deadline does not establish a refund.
