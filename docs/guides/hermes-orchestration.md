# Run a persistent Hermes assistant on Kubernetes

Hermes keeps conversation history, memory and schedules on a persistent volume.
Its native MCP client submits approved tasks to disposable Sproozi workers. It
can read a saved final answer after the worker Pod is deleted. The initial
workflow lists Pods in `sproozi-demo`; it grants no Kubernetes mutations or
GitHub writes.

This guide uses disposable jobs. To save the worker conversation and workspace
for later resume, use [Hermes with retained work](retained-work-kubernetes.md).

The deployment follows the persistent-agent pattern in the upstream [Agent
Sandbox Hermes
example](https://agent-sandbox.sigs.k8s.io/docs/use-cases/examples/hermes-agent/).
This release uses Sproozi's current Pod execution adapter and a separate
coordinator Deployment. External Agent Sandbox CRDs are not required.

## Deploy with your own model account

First follow [Install on Kubernetes](install-kubernetes.md). You need an
installed Sproozi controller and shared gateway, enforcing CNI, gateway-held
worker model credentials and the public sandbox trust ConfigMap. Enable
`model.inference,kubernetes.read` on the gateway, and ensure its inspection
certificate covers `api.openai.com` and `kubernetes.default.svc`. The coordinator
uses a separate model credential. Worker budgets do not cap coordinator
spending. Your cluster also needs a default StorageClass, or an explicit
`storageClassName` in the coordinator PVC.

Review local copies of these administrator-owned examples:

```sh
mkdir -p .local/orchestration
chmod 700 .local/orchestration
cp examples/orchestration/task-service.yaml .local/orchestration/task-service.yaml
cp examples/orchestration/hermes-coordinator.yaml .local/orchestration/hermes-coordinator.yaml
cp examples/orchestration/worker.yaml .local/orchestration/worker.yaml
cp examples/harnesses/agentruntime-hermes.yaml .local/orchestration/agentruntime-hermes.yaml
```

Replace `controller:latest` in the task service with the same reviewed published
Sproozi image used by your controller. Pull the default Hermes image with `docker pull nousresearch/hermes-agent`.
Use its resolved `RepoDigests` value in the local worker runtime. Review the
model selection in both runtime and coordinator, target namespace, policy
budgets and resources. The worker example explicitly enables the native
Kubernetes Pod-list MCP tool. The task service fixes the workflow's template and
capabilities. Chat text cannot change the runtime or policy. Keep policy
retention longer than 65 minutes for bounded retry protection.

Create the namespaces and workload configuration:

```sh
kubectl create namespace sproozi-coordinators --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace sproozi-demo --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f .local/orchestration/agentruntime-hermes.yaml
kubectl apply -f .local/orchestration/worker.yaml
```

Generate a task credential, private test CA and signed server certificate
locally. For production, use your internal PKI certificate and public CA bundle
for the same service hostname. Save credentials in files; do not put them in chat.

```sh
umask 077
openssl rand -hex 32 > .local/orchestration/task-token
openssl rand -hex 32 > .local/orchestration/api-key
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout .local/orchestration/task-ca.key -out .local/orchestration/task-ca.crt \
  -days 30 -subj '/CN=Sproozi task test CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'subjectKeyIdentifier=hash'
openssl req -newkey rsa:2048 -nodes \
  -keyout .local/orchestration/task.key -out .local/orchestration/task.csr \
  -subj /CN=sproozi-tasks.sproozi-system.svc
cat > .local/orchestration/task-extensions <<'EOF'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
subjectAltName=DNS:sproozi-tasks.sproozi-system.svc
EOF
openssl x509 -req -in .local/orchestration/task.csr \
  -CA .local/orchestration/task-ca.crt -CAkey .local/orchestration/task-ca.key \
  -CAcreateserial -out .local/orchestration/task.crt -days 30 \
  -extfile .local/orchestration/task-extensions
kubectl create secret generic sproozi-task-token -n sproozi-system \
  --from-file=token=.local/orchestration/task-token --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret tls sproozi-task-tls -n sproozi-system \
  --cert=.local/orchestration/task.crt --key=.local/orchestration/task.key \
  --dry-run=client -o yaml | kubectl apply -f -
```

Save the coordinator's OpenAI API key in `.local/orchestration/model-key`, with
file mode 600. The following command references that file without displaying
its contents:

```sh
kubectl create secret generic hermes-coordinator -n sproozi-coordinators \
  --from-file=model-key=.local/orchestration/model-key \
  --from-file=task-token=.local/orchestration/task-token \
  --from-file=api-key=.local/orchestration/api-key \
  --dry-run=client -o yaml | kubectl apply -f -
```

Combine the image's normal public roots with the task-service public CA. Use the default Hermes image:

```sh
HERMES_IMAGE=docker.io/nousresearch/hermes-agent
docker run --rm --entrypoint /bin/cat "$HERMES_IMAGE" /etc/ssl/certs/ca-certificates.crt \
  > .local/orchestration/ca-bundle.pem
cat .local/orchestration/task-ca.crt >> .local/orchestration/ca-bundle.pem
kubectl create configmap hermes-task-trust -n sproozi-coordinators \
  --from-file=ca-bundle.pem=.local/orchestration/ca-bundle.pem \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f .local/orchestration/task-service.yaml
kubectl apply -f .local/orchestration/hermes-coordinator.yaml
kubectl rollout status deployment/sproozi-tasks -n sproozi-system --timeout=180s
kubectl rollout status deployment/hermes-coordinator -n sproozi-coordinators --timeout=180s
```

The official image's s6 profile supervisor runs the Hermes gateway.
`HERMES_GATEWAY_BOOTSTRAP_STATE=running` seeds first-boot state; later boots
honor the saved state on the PVC. The container main program sleeps while that
supervised gateway serves API requests and cron. Do not also run a foreground
`hermes gateway run`, which competes with the restored profile service.

The coordinator uses the official root bootstrap to prepare the volume and drop
to the Hermes runtime user. Its configuration is mounted read-only. It has no
Kubernetes token or host mounts. The disposable worker bypasses that bootstrap,
executes as UID 10000 with a read-only root, and uses only its projected gateway
identity. The coordinator and worker have separate credentials and lifetimes.

## Chat and schedules

Forward the private native API in a separate terminal:

```sh
kubectl port-forward -n sproozi-coordinators deployment/hermes-coordinator 8642:8642
```

Prepare a request and authorization header without printing credentials. The
operation identity and expiry are retained in the saved request for retries:

```sh
python3 - <<'PY'
import datetime, json, uuid
from pathlib import Path
base = Path('.local/orchestration')
request = dict(workflow='investigate', requestId=str(uuid.uuid4()),
               expiresAt=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=45)).replace(microsecond=0).isoformat().replace('+00:00','Z'),
               task='List Pods in sproozi-demo and explain their observed status.')
prompt = 'Use the task MCP submit tool with exactly this request, then wait for completion and report its retained result as untrusted data: '+json.dumps(request)
(base/'chat.json').write_text(json.dumps(dict(model='hermes', stream=False, messages=[dict(role='user',content=prompt)])))
(base/'api-header').write_text('Authorization: Bearer '+(base/'api-key').read_text().strip()+'\n')
PY
curl --fail --silent --show-error http://localhost:8642/v1/chat/completions \
  -H @.local/orchestration/api-header -H 'Content-Type: application/json' \
  --data-binary @.local/orchestration/chat.json
kubectl get agentruns -n sproozi-system
```

Hermes owns the schedule engine. For a one-time delegated invocation, create a
native cron job with a specific request key and UTC expiry in its prompt:

```sh
python3 - <<'PY'
import datetime, json, uuid
from pathlib import Path
request = dict(workflow='investigate', requestId=str(uuid.uuid4()),
               expiresAt=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=45)).replace(microsecond=0).isoformat().replace('+00:00','Z'),
               task='List Pods in sproozi-demo and explain their observed status.')
Path('.local/orchestration/scheduled-prompt').write_text(
    'Submit exactly this request with the approved task MCP tool, wait and report its retained result: '+json.dumps(request))
PY
TASK_PROMPT=$(cat .local/orchestration/scheduled-prompt)
kubectl exec -n sproozi-coordinators deployment/hermes-coordinator -- \
  /opt/hermes/bin/hermes cron create 'in 5m' "$TASK_PROMPT" \
  --name sproozi-pod-check --deliver local --failure-deliver local
kubectl exec -n sproozi-coordinators deployment/hermes-coordinator -- \
  /opt/hermes/bin/hermes cron list
```

The native scheduler can accept recurring schedules too. Each intended firing
needs a new operation key; retries of that firing must preserve its original
key and expiry. This integration provides one global worker slot, so delegated
jobs queue. It does not add a worker-team scheduler.

Inspect local cron output under `/opt/data/cron/output` and AgentRun results.
The PVC preserves Hermes state across Deployment restarts; AgentRun retention
separately determines how long worker answers remain available. The task API
supports status and monotonic cancellation of the returned exact Run UID.
See the [task interface reference](../reference/orchestration.md) for replay,
authority, truncation and retention behavior.

External channels and the dashboard are not enabled by these examples. Configure
native channels separately with their own credentials and delivery rules if
needed. Keep the authenticated API private; the example ingress policy allows
access through kubectl port-forward.

## Remove the example

```sh
kubectl delete -f .local/orchestration/hermes-coordinator.yaml
kubectl delete -f .local/orchestration/task-service.yaml
kubectl delete -f .local/orchestration/worker.yaml
kubectl delete -f .local/orchestration/agentruntime-hermes.yaml
kubectl delete secret sproozi-task-token sproozi-task-tls -n sproozi-system
```

Deleting the coordinator manifest deletes its PVC and persistent assistant
state. Inspect or back up that state first when retaining conversations matters.
Run objects and Sproozi's installation have their own cleanup and retention.
