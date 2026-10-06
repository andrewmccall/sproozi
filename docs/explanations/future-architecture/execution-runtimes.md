# Multiple execution runtimes

**Status:** future direction. Kubernetes is the current and first runtime;
local Docker is the key next runtime. See the [overview](README.md).

Docker should provide a low-friction way to run a familiar coding harness in a
disposable local environment with explicitly granted capabilities. The same
execution intent should also be runnable on Kubernetes when policy permits it.
That would test whether Sproozi's execution model can stand independently of its
first placement.

This is more than launching a container. The environment still needs identity,
capability mediation, bounded resources, workspace rules, an observable outcome
and cleanup. A Docker proof is useful only if those requirements remain part of
the execution boundary.

An illustrative future CLI experience is:

```text
cd my-repository
sproozi codex
sproozi codex --runtime kubernetes
```

These commands describe the intended experience, not an available launcher or a
committed flag specification. Trusted policy would determine whether either
placement is permitted.

## Separate policy, placement and mechanism

| Responsibility | Decision |
| --- | --- |
| Execution policy | What authority, resource bounds, lifetime, filesystem access and isolation guarantees are required? Which placements are acceptable? |
| Placement | Which permitted location can satisfy the execution and its capability dependencies? |
| Runtime adapter | How are those approved requirements provisioned, observed, stopped and released there? |

For production investigation, trusted policy might require a designated cluster
and forbid local execution. For repository development, it might permit either
local Docker or an approved Kubernetes environment. A caller's runtime preference
can narrow that choice. It cannot turn the production policy into local policy.

Portability means preserving intent and declared guarantees. It does not mean
identical implementation or that every policy works everywhere. Runtime support
must include the mechanisms needed to enforce the requested boundary. An adapter
that cannot enforce a required filesystem or network restriction should reject
the run, with an explanation, rather than ignore it or downgrade assurance.

## Evolve the current Kubernetes boundary

The current [AgentRuntime](../../reference/agentruntime.md) fixes an
administrator-owned Pod template. `internal/kubernetes/sandbox.go` supplies
creation, observation, exit status and deletion through `SandboxClient`.
The controller also provisions ServiceAccounts, NetworkPolicies and contract
ConfigMaps, and admission reads container resource limits from the Pod template.
Replacing only the Pod creation call would leave those dependencies in place.

A portable boundary should describe an approved execution environment and its
lifecycle without requiring other backends to impersonate Pods, ServiceAccounts
or ConfigMaps. Kubernetes should translate those requirements into its existing
mechanisms. Docker should translate them into its own containment, storage,
network and identity mechanisms. Trusted capability enforcement remains above
both.

The Kubernetes API can remain the operator's entry point. A future local CLI
should not require a cluster merely to use Docker, or embed a second policy model
that grants different authority for the same intent. How policy and run state
are stored locally is an implementation decision still to be made.

Do not make today's Pod template a universal portable schema or rename existing
CRDs as part of documenting this direction. Preserve the working operator path
while a second backend reveals which responsibilities need a common boundary.
External Kubernetes Agent Sandbox integration remains a separate planned way
to provision cluster environments, as the current reference explains.

## Local Docker experience

A developer should be able to select a harness, task, approved environment and
capabilities from the current repository. Sproozi prepares the sandbox, configures
its mediated clients, and launches the harness with its local tool executor
inside that environment. Interactive use attaches a terminal; bounded automation
uses the harness's non-interactive mode.

The execution setup should make these choices explicit:

- Workspace source and write policy, including whether changes persist in the
  host repository or leave as exported artefacts. A writable bind mount is
  deliberate host write authority and is not rolled back by container deletion.
- Approved image and dependencies, resource bounds and writable storage. Policy
  must cover build or preparation steps too, not begin only after the agent starts.
- Run identity and access to trusted capability enforcement, including local
  gateway placement and containment against direct upstream access.
- Deadline, cancellation, outcome evidence and disposal of temporary state.

The agent should not inherit the user's home directory, credential stores,
Docker socket, host namespaces or unrestricted local-network access. The trusted
launcher may operate the container engine; agent code should not receive that
control channel. A harness's own executor must not create a second unrestricted
host environment or silently redirect commands outside the selected sandbox.

Repository environment descriptions, including Dev Container configuration,
may supply dependencies or workspace suggestions in a future integration.
They remain untrusted inputs subject to policy review, not authority to add
mounts, secrets, privileged containers or unrestricted network routes.

## Preserve the execution guarantees

Kubernetes tokens and CNI policies cannot be copied literally to Docker. The
local adapter needs a run-bound identity accepted by the trusted broker and an
actual network boundary. Setting proxy environment variables alone is client
configuration, not containment. Local gateway credentials and state also need
protection from the untrusted workload.

Likewise, process exit, loss of a container, cancellation and deadline expiry
need consistent meaning across backends. Stop the workload and revoke its
external authority; preserve isolation until stopping is confirmed. Recover
resources by run ownership after a launcher or controller restart. See the
[current lifecycle](../run-lifecycle.md) for the baseline and its limitations.

Resource and storage limits can differ by host and engine configuration. Keep
those differences visible in admission and evidence. Choosing Docker is not a
claim that every local host has the same isolation as the verified cluster.
The first version can support a narrow environment if it states and proves the
bounds it enforces.

## First proof and later options

Run the same harness with the same requested capability scope locally and on
Kubernetes. Demonstrate allowed work, an out-of-scope denial, direct-access
denial, cancellation and cleanup in both environments. Verify workspace
persistence separately from sandbox disposal. Include an unsupported-policy
case that is rejected before execution.

This proves Docker is an execution adapter under policy, not a separate product
with a looser local mode. Add interactive attachment once the basic bounded run
is established; it should not require managed harness integration.

Later backends could use existing sandbox providers, stronger container
isolation or virtual machines. Reuse those implementations where possible.
Decide interfaces, placement discovery and backend support reporting from the
Docker/Kubernetes comparison before committing to a general runtime plugin
system or scheduling platform.
