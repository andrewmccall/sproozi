# Simplified Sproozi local client onboarding

Status: largely implemented. [ADR 0025](../../adr/0025-local-provisioning-and-lifecycle.md) and [ADR 0026](../../adr/0026-kubernetes-release-assets-and-certificates.md) record what was built and how it differs from this proposal; the [local sessions guide](../../guides/local-sessions.md) and [Kubernetes guide](../../guides/install-kubernetes.md) describe current use. The text below is the original proposal.

Date: 20 October 2026

Audience: Sproozi maintainers, implementers and release reviewers

Repository baseline: [`andrewmccall/sproozi`, commit `25a44e65ce62cc9a803a6dc5a8fb5b8800bae826`](https://github.com/andrewmccall/sproozi/tree/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826)

Related proposal: [Managed harness configuration and trust](managed-harness-configuration-and-trust.md). That proposal covers protected configuration assembly, skills, diagnostics and future access requests. This PRD covers local provisioning and lifecycle. Shared configuration must use compatible precedence and trust rules; the broader dynamic-access work is not a prerequisite for local onboarding. Publication review against `67691286003978cf959b8586b67afbbe0cca7ca2` found only that related proposal and its index entry added since the architecture baseline above.

## 1. Product decision

An installed Sproozi CLI should prepare a usable local environment from a working Docker installation. It should resolve and pull images, build them when necessary, configure networking, generate and maintain its private TLS trust, start the trusted home service and launch the user's chosen native agent terminal. Users should not have to assemble these components from the repository.

All local agents use one `sproozi/agents` directory. It contains their persistent native state and shared configuration. Harness-specific files retain their native formats. Configuration profiles and separate agent homes are future work.

Kubernetes remains an operator-installed execution environment. This release simplifies its release manifests and quickstart, including optional Sproozi-managed certificates without cert-manager. No form of `sproozi init` reads cluster credentials or changes a cluster. A first public release may require a competent user to install and configure the Kubernetes components.

This document specifies product work. Statements labelled **present** describe inspected code or current documentation. Statements labelled **required** describe the proposed release. Command additions and defaults below are proposals, not claims about today's binary. Existing acceptance reports are evidence for their recorded scenarios; this PRD does not report a new live verification.

## 2. Problem and users

The CLI is already installable. The remaining work sits between installing it and starting an agent. Current local instructions ask the user to build client and native images, resolve Docker content IDs, create a bridge network, choose a reachable host address, generate certificates and private configuration, then start `sproozi serve`. This makes a local workflow depend on knowledge of Sproozi's internal deployment contract. [R1, R2, R3]

The primary user is a developer who has Docker running and wants to use Codex or Claude Code in a controlled local environment. They understand native agent terminals, repositories and service credentials. They should not need Go, Python, OpenSSL, Kubernetes or a Sproozi source checkout to start locally.

The secondary user is an operator who installs Sproozi in Kubernetes and publishes an approved remote executor to developers. They can review CRDs, RBAC, network enforcement and credential storage. They need a short, repeatable installation procedure and a usable certificate lifecycle.

## 3. Goals and non-goals

### Goals

- Reach a working native Codex or Claude Code terminal through a short local setup flow.
- Automate local image preparation, network selection, gateway setup, private configuration, service startup and certificate maintenance.
- Keep each stock harness's interface, conversation format and supported resume behaviour.
- Persist local agent settings and state under one shared `sproozi/agents` root.
- Preserve Sproozi's separation between agent execution, live access checks and upstream credentials.
- Retain recoverable session operations, original deadlines and usage accounting.
- Publish versioned Kubernetes installation resources with a no-model first check and an optional self-managed CA mode.
- Keep certificate and credential provisioning replaceable without changing harnesses or capability policy.
- State support limits and distinguish local convenience from isolation between mutually untrusted agents.

### Non-goals for this release

- Installing Docker, creating Kubernetes clusters, installing a CNI, or changing kubeconfig through the CLI.
- A cluster-mutating `init`, automatic CRD upgrades, or automatic remote policy/RBAC creation.
- Configuration profiles, per-project agent homes, multi-user local tenancy or isolation between agents sharing the local state root.
- A new agent loop, chat interface, custom tool protocol or rewritten conversation format.
- Importing personal Codex/Claude authentication stores, arbitrary existing sessions or whole native homes during onboarding.
- Cross-harness conversation conversion, broader portability for unsupported native features, or GUI/IDE attachment.
- A full enterprise PKI, public certificate service, Vault integration or general dynamic credential framework in the first release.
- New capability modules, a hard guarantee against provider spend, or a claim that Docker makes untrusted code harmless.

## 4. Present architecture and proposed changes

| Area | Present at the baseline | Required change |
| --- | --- | --- |
| Installation | Homebrew on macOS/Linux and downloadable platform binaries. The archive does not contain worker images. Homebrew currently builds from source. [R1] | Keep these paths. Release assets must include the metadata and build inputs needed for local provisioning. Prefer prebuilt CLI packages where supported. |
| `init` | Creates or validates a private directory. It does not configure services or credentials. [R2, R4] | Add an explicit local provisioning mode. Preserve the existing directory-only behaviour. |
| Home service | `sproozi serve` owns the local gateway, Docker placement, accounting and session coordinator. Control uses a private Unix socket or Windows named pipe. [R2] | Add managed startup, readiness checks and lifecycle commands around the existing service. |
| Docker placement | Requires prepared immutable image IDs, a bridge network and a fixed gateway IPv4/port. It creates separate worker, helper and network-anchor roles. [R3] | Provision those dependencies and verify reachability before activation. Keep role separation and immutable identities. |
| Network containment | The trusted anchor has `NET_ADMIN`; the worker has no capabilities and joins its network namespace. Worker traffic is limited to the selected gateway endpoint. [R3] | Automate address/port selection and verify actual IPv4/IPv6 denial behaviour on each supported platform. |
| Local state | Workers use fresh homes and a bounded managed workspace volume. Selected conversation and Git state form portable checkpoints. [R3, R9] | Introduce the shared persistent agents root. This requires an explicit mount, authority and checkpoint design change. |
| Harnesses | Managed interactive sessions support Codex and Claude Code. Kubernetes jobs also support OpenCode and Hermes. Remote retained tasks are currently Codex-only. [R5] | Simplify onboarding for the two interactive harnesses. Do not imply equal feature coverage across all harnesses. |
| Client versions | Repository images use integrity-checked lockfiles. A latest-client canary exists. Product activation does not require an exact client version. [R6] | Produce a tested release image manifest while retaining administrator image overrides and the existing canary. |
| Gateways | Kubernetes uses a shared authenticated gateway with model, semantic service, configured MCP and destination modules. Local home composes its own gateway and approved integrations. [R7, R8] | Reuse these components. Do not assume the local daemon is a Kubernetes control plane. |
| Kubernetes API | Four CRDs: `AgentRuntime`, `AgentPolicy`, `AgentTemplate`, `AgentRun`. Execution uses disposable Pods directly. [R7] | Keep this model. Publish CRDs separately from installation and optional certificate resources. |
| Kubernetes TLS | Installation already works without cert-manager. The guide manually creates an evaluation CA, a 30-day leaf, a TLS Secret and an immutable public trust ConfigMap. Renewal is operator-owned. [R10] | Supply optional automated bootstrap/renewal and a documented rotation procedure that respects immutable trust bundles. |
| Credentials | Gateways read private local files or trusted Kubernetes Secrets. GitHub App credentials, MCP token files and provider authentication have existing paths. Some credential/registration changes require restarts. [R2, R8] | Offer guided local credential storage and safe reload/restart. Retain a narrow future extension point for leased credentials. |

## 5. Target user experience

### 5.1 First local session

Docker is installed and running with Linux containers. The user has an ordinary Git repository to work on. Authentication happens through the setup flow; a no-credential connectivity check remains available.

Proposed flow:

```sh
# Existing installation route
brew install andrewmccall/tap/sproozi

# New local provisioning mode
sproozi init --local

# Proposed per-session harness selection
sproozi start --workspace "$PWD" --harness codex
```

`init --local` reports its local state location and shared agents directory, validates Docker, selects a supported harness/model, prepares images and network, creates private TLS material, obtains the selected provider credential, writes configuration and starts the home service. It reports readiness only after a verified gateway connection from the actual restricted Docker path.

The wizard explains the intended access before the first launch. It asks only for information it cannot derive safely, such as the provider account, model and optional external-service credentials. It offers explicit bounded execution/resource defaults. Users can change them without editing internal image IDs, certificate paths or network addresses.

Proposed starter limits are below. They require validation against both harnesses during M0 and are configurable within administrator ceilings. Model selection is explicit; setup does not invent provider pricing or promise a monetary cap when none is configured.

| Setting | Proposed starter value |
| --- | --- |
| Execution deadline | 30 minutes |
| Worker CPU / memory / processes | 1 vCPU / 1 GiB / 256 PIDs |
| Managed workspace / maximum checkpoint | 512 MiB / 8 MiB, with clear capacity errors and configurable increases |
| Model token allowance | 200,000 accounted tokens; current admission/usage limits still apply |
| Monetary allowance | Explicitly shown as unbounded unless the user selects a supported bound with administrator-supplied pricing |
| External capabilities | Model inference only until the user configures and grants another service |

`start` returns a session ID and opens the stock terminal by default. `--detach` becomes available for scripts and unattended use. Ctrl-] detaches; Ctrl-C reaches the harness. Detachment leaves the execution running within its existing deadline and allowance.

Starting another harness uses the same agents root:

```sh
sproozi start --workspace "$PWD" --harness claude-code
```

The second launch prepares any missing approved image automatically. It prompts for a missing provider credential through a trusted local input path. It never copies personal native authentication files into the worker. Anthropic API-key authentication is the first Claude onboarding path; subscription and cloud-provider variants require separate support work. [R5]

### 5.2 Daily operation

Existing session verbs remain recognisable. New service and diagnostic commands use the same installed executable:

```sh
sproozi sessions
sproozi attach SESSION_UUID
sproozi status SESSION_UUID
sproozi stop SESSION_UUID
sproozi resume SESSION_UUID
sproozi recover SESSION_UUID

# Proposed commands
sproozi doctor
sproozi service status
sproozi service start
sproozi service stop
sproozi credentials add openai
sproozi credentials add anthropic
```

`stop` keeps supported state after a graceful stop. `cancel` revokes access and forces a stop under the existing recovery contract. A service stop first handles active executions through an explicit drain or cancellation choice; stopping the daemon alone does not prove the containers stopped. If the daemon is absent, local session commands start the configured service or report the exact recoverable failure.

### 5.3 Images and certificates

Routine users need no image or certificate command. Advanced controls expose the same operations used by setup:

```sh
# All commands in this block are proposed
sproozi images status
sproozi images prepare --harness codex
sproozi images prepare --harness claude-code --build
sproozi images publish --registry ghcr.io/MY-ACCOUNT
sproozi certificates status
sproozi certificates renew
sproozi certificates rotate-ca
```

`--build` invokes the pinned release recipe through Docker. Publication requires an explicit registry destination and uses the user's registry authorisation. Setup does not publish to an external registry. Sproozi release automation publishes the standard images so normal onboarding pulls them.

### 5.4 Adding an operator-installed remote executor

Local setup works without a cluster or kubeconfig. A user can later register the HTTPS endpoint, public trust and principal credential supplied by an operator:

```sh
# Proposed registration command; changes local configuration only
sproozi executor add remote \
  --url https://approved-executor.example:8443 \
  --ca-file /absolute/path/public-ca.pem \
  --token-file /absolute/private/principal-token

# Existing workflow shape after registration
sproozi move SESSION_UUID --to remote
sproozi move SESSION_UUID --to local
```

Existing terms such as "profile service" name the current remote service architecture. They do not introduce user-selectable configuration profiles in this release. Remote moves still require compatible harnesses, approved instructions, current policy and checkpoint limits. Local shared state does not become remotely shared storage.

## 6. Detailed requirements

MUST marks a release requirement. SHOULD permits an exception recorded in release notes with its user impact. Future requirements are assigned to the later milestone explicitly.

### 6.1 Installation and configuration

| ID | Requirement |
| --- | --- |
| CLI-01 | The installed CLI MUST provision the local path without a source checkout or host Go, Python, OpenSSL, Node.js or Kubernetes tools. If Git is needed for capture, use detected compatible host Git or a packaged/trusted helper; document that choice and test its ignore/config semantics. |
| CLI-02 | `init --local` MUST be explicitly local. It MUST NOT invoke kubectl, discover cluster contexts, install CRDs, create namespaces, or contact a Kubernetes API. Existing `init --directory` behaviour MUST remain available. |
| CLI-03 | Setup MUST use a private per-user root, show its resolved absolute path, and store it for subsequent commands. New installations default to `~/sproozi`; an explicit root override is supported. Existing platform config paths remain discoverable. |
| CLI-04 | Setup MUST resume after interruption without duplicating networks, services, CAs or secrets. Every resource it owns has a stable installation identity and a journaled provisioning stage. Concurrent initialisers use one local ownership lock. |
| CLI-05 | Setup MUST preserve existing configuration. Existing installations receive a migration plan and backup. Image, mount, network or authority changes require safe drain/migration because current placement records reject changed contracts. |
| CLI-06 | Automation MUST support non-interactive settings, bounded timeouts and machine-readable results. Missing mandatory input fails with a specific next command. Secrets are accepted through a prompt, protected file or stdin, never a required command-line value. |
| CLI-07 | Local start MUST select Codex or Claude Code per session while keeping the harness immutable within that session. Selecting another harness starts a separate session. |
| CLI-08 | Diagnostics MUST separate image download, registry authentication, Docker availability, network verification, TLS verification, provider authentication, policy denial and retained-state recovery failures. |
| CLI-09 | Release documentation MUST identify supported host/engine combinations. macOS Docker Desktop and Linux Docker Engine are first targets; native Windows/Linux-container support requires its own passed gate before it is advertised for automatic setup. |

### 6.2 Image selection, build and publication

The image decision is per harness and per supported architecture. An official environment image, a Dockerfile example and a maintained CLI runtime image are different inputs.

[OpenAI's cloud environment documentation](https://learn.chatgpt.com/docs/environments/cloud-environment?translationFallback=es-419) describes a pullable `codex-universal` image. That establishes an official environment candidate, but does not establish that it contains the required CLI, meets Sproozi's privilege contract or provides compatible native session state. [Anthropic's development-container documentation](https://code.claude.com/docs/en/devcontainer) supplies a Dev Container Feature and describes its reference container as an example rather than a maintained base image. Neither source justifies assuming a ready-to-use Sproozi harness image exists. [U1, U2]

| ID | Requirement |
| --- | --- |
| IMG-01 | Each release MUST check for a provider-published, maintained image suitable for the native CLI. Record publisher, upstream source, release activity, maintenance policy, architectures, licence, CLI version and compatibility results. Recheck on each release and on upstream deprecation notices. |
| IMG-02 | Prefer an approved official image by digest when it passes the same tests as a Sproozi image. A broad cloud environment image MUST NOT win solely because it is official. Do not use unofficial third-party harness images as an automatic fallback. |
| IMG-03 | If no suitable official image passes, release CI MUST reproducibly build and publish a minimal Sproozi wrapper around the stock upstream CLI. The CLI executable and agent loop remain unmodified. Only runtime tools, native supervision/checkpoint helpers, trust/configuration wiring and necessary entrypoint handling are added. |
| IMG-04 | Keep worker, checkpoint-helper and network-anchor roles separate, whether they share an image digest or use separate images. The anchor's privileges MUST NOT become worker privileges. Do not import privileged upstream startup scripts blindly. |
| IMG-05 | A release manifest MUST resolve the product version to registry/index digest, platform digest, harness/package version, helper contract version, build-recipe hash and supported capabilities. Local setup pulls that reference, verifies it, then records the Docker content ID required by the adapter. |
| IMG-06 | Standard release images MUST be public-pullable on the selected registry, with `linux/amd64` and `linux/arm64` where supported. Registry naming and ownership are a release decision. Provide checksums, provenance and a software bill of materials for project-published images. |
| IMG-07 | Fallback build inputs MUST ship with the release, be verified against a trusted manifest and use pinned base images, packages and integrity hashes. Do not resolve `latest` or run an unpinned remote install script during a supported reproducible build. Build contexts MUST exclude workspaces, agent state, credentials and TLS keys. |
| IMG-08 | The CLI MUST pull by default, reuse verified cache entries and report progress. If a pull fails, explain why and offer the packaged build path. Do not silently build different inputs, substitute an unapproved image or introduce additional host toolchain dependencies. |
| IMG-09 | Explicit publication MUST push only selected built images and report registry digests usable in reviewed Kubernetes manifests. A local image ID is not a registry digest. No kubeconfig or cluster write is involved. |
| IMG-10 | Keep running sessions on their recorded images. Harness updates take effect for new sessions after compatibility checks. Runtime auto-updaters MUST NOT mutate an approved image's native executable in place. Support explicit rollback and cached/offline reuse. |
| IMG-11 | Retain the existing pinned-client/canary separation. Define reproducibility as repeatable declared inputs and equivalent verified artefacts; claim byte-identical OCI digests only if the release build demonstrates them. |

### 6.3 Networking, gateways and service lifecycle

| ID | Requirement |
| --- | --- |
| NET-01 | Setup MUST locate an approved local Docker endpoint and verify Linux container mode. Remote TCP Docker daemons are outside the automatic local path. It MUST NOT expose the Docker socket to an agent or repurpose the user's personal Docker credential configuration. |
| NET-02 | Setup MUST create or reuse an installation-owned network, select non-overlapping addressing and detect port conflicts. It MUST NOT assume a universal Docker Desktop host address such as `192.168.65.254`. |
| NET-03 | A platform-specific probe MUST establish the gateway IP/port reachable from the actual worker network namespace. Public trust and leaf SANs MUST match that endpoint and approved inspection authorities. Unsupported networking fails before activation. |
| NET-04 | Worker routing MUST enforce gateway-only traffic. Verify direct provider/API access, host/LAN/metadata endpoints, alternate ports, DNS bypass and IPv6 denial. Merely setting proxy environment variables is insufficient. |
| NET-05 | Only installation-owned trusted components may receive required network privileges. Worker containers remain non-root with a read-only root, dropped capabilities, no-new-privileges and explicit CPU/memory/PID/storage limits. |
| NET-06 | The gateway MUST bind only the required local interfaces. A wildcard bind requires equivalent host/network access restriction and an explicit diagnostic result. Provider requests retain upstream certificate and hostname verification. |
| SVC-01 | Setup MUST create a supervised per-user home service independent of an attached terminal. It MUST report readiness, log location and how to stop it. It SHOULD integrate with the platform service manager; boot/login persistence must be visible and configurable. |
| SVC-02 | There MUST be one authoritative service owner for each state root, gateway credential refresh stream and journal. Restarts reuse the same protected durable state. |
| SVC-03 | Service restart, upgrade and machine sleep/wake MUST preserve operation identities and usage uncertainty. A vanished connection or container MUST NOT become proof of a safe stop or zero consumption. |
| SVC-04 | `doctor` MUST be read-only by default. Repair is an explicit action limited to installation-owned resources. A successful check MUST include a real restricted-path TLS probe, plus optional provider validation with any charge disclosed before it runs. |
| SVC-05 | Service stop, uninstall and cleanup MUST retain checkpoints and journals by default. Deleting shared state or credentials requires an explicit destructive option and affected-session report. Never prune unrelated Docker resources. |

### 6.4 Shared `sproozi/agents` directory and native UX

The proposed root layout is logical; platform configuration paths may point to it without duplicating state:

```text
~/sproozi/
  config.json           # trusted installation and access configuration
  agents/               # one persistent local root used by every local agent
    codex/              # native Codex settings and supported local state
    claude/             # native Claude settings and supported local state
    shared/             # shared instructions and supported skills/config inputs
  credentials/          # gateway-owned upstream credentials; never worker-mounted
  pki/                  # private signing/leaf keys and public trust generations
  state/                # trusted journals, receipts and retained checkpoints
  logs/                 # private operational logs
```

Harness subdirectories avoid filename collisions. They are not configuration profiles or security boundaries. Every local agent uses this root, with supported native paths mapped by the launcher. Git workspaces remain managed per session; sharing the agents folder does not merge repositories or replace the current workspace capture contract.

| ID | Requirement |
| --- | --- |
| AGT-01 | All local harnesses MUST use the same configured `agents` root. Installation upgrades and ordinary container replacement preserve it. There is no profile selector or automatic per-project home. |
| AGT-02 | Native configuration, local history, supported instructions and skills MUST remain ordinary files in documented native layouts. Do not invent a universal transcript or skill format. The first implementation MUST publish a per-harness table of effective paths, supported files and precedence. |
| AGT-03 | Sproozi MUST distinguish editable native preferences from trusted enforcement. Users can keep supported native settings; gateway identity, service endpoints, immutable instructions, completion hooks and containment requirements take precedence. Unsupported hooks/plugins must be reported rather than silently trusted. |
| AGT-04 | The agents root MUST NOT contain Sproozi upstream credentials, signing keys, daemon control sockets or trusted journals. Workers receive only public trust and fresh scoped gateway identity. Native `auth.json`, `.credentials.json` or equivalent upstream login stores MUST be excluded or blocked in managed mode. |
| AGT-05 | Before introducing mounts, implement an explicit allowlist of shared paths. Reject symlink/path traversal and unsafe host-file mappings. Agents may read or alter state made available in the shared root; the UI and docs MUST state this shared trust assumption. |
| AGT-06 | Shared local state MUST NOT be interpreted as permission for another session. Bind every gateway request and allocation to the active execution. Agent-writable native history and configuration cannot grant capability, enlarge budgets or authorise service management. |
| AGT-07 | The first release MAY serialize local native writers if harness state is unsafe under concurrency. It MUST fail or queue clearly rather than corrupt shared indexes. Profiles and concurrency isolation are deferred. |
| AGT-08 | Checkpoints and remote moves MUST still select only the supported conversation and Git workspace. Never archive the entire agents root, local configuration, shared auth files or unrelated sessions. Imported state cannot overwrite shared settings or another conversation. |
| AGT-09 | Preserve stock terminal rendering, input, resize, signals and supported native resume. A Sproozi launcher may supervise the process; it MUST NOT replace the native loop with a headless prompt UI. Unsupported native features remain explicit support limits. |
| AGT-10 | Adding a shared state path MUST NOT silently broaden what native configuration gets loaded compared with today's managed launches. Validate intended settings/skills independently from portability and retain fixed authority-critical settings. |

## 7. Security and trust model

Local automatic setup trusts the signed/verified release metadata, selected images, Docker engine, host user, home service and gateway. The trusted service can control Docker and is therefore privileged relative to the worker. A compromised host or Docker administrator is outside the worker containment boundary.

Agents, repositories, native history, tool output and downloaded content remain untrusted. The shared agents directory deliberately permits local agents to influence shared preferences and exposed state. It provides no isolation between mutually untrusted users or agents. Users should treat it as the same shared local environment they would use when running their own native tools.

The release MUST meet these trust requirements:

1. Private directories are owner-only and private regular files use `0600` or stricter Unix modes. Windows uses equivalent restrictive ACLs. Validate ownership, file type and unsafe links before reading authority-bearing files.
2. Never mount the host's home directory, Docker socket, SSH directory, cloud credentials, kubeconfig, gateway credentials or CA private key into workers. Only the deliberate agents paths and managed workspace are exposed.
3. Upstream account selection comes from trusted gateway configuration. Worker placeholder keys and request headers cannot select another account or override policy.
4. Keep approved instructions separate from untrusted tasks and event context. Sharing a directory does not make its content administrator-approved instructions.
5. Restrict TLS inspection certificates to the selected gateway and enabled authorities. Do not automatically add Sproozi's CA to the host/browser/system trust store.
6. Keep gateway inspection trust separate from trust used to verify upstream services. A generated local CA must not become an upstream trust bypass.
7. Redact credentials, authorisation headers and private key material from diagnostics. Private transcripts and terminal logs can contain sensitive tool results; collection is bounded and opt-in for support export.
8. Preserve current deadline, allowance, fresh-admission and independent container-stop checks. Unknown consumption holds capacity. Onboarding must not reset accounting to repair an error.
9. Document current spend-estimation limits, native portability limits and Kubernetes network-enforcement exceptions. No setup success message implies production readiness. [R5, R7, R8, R9]

## 8. TLS and CA lifecycle

### 8.1 Ownership and defaults

Required local mode uses a private CA generated by the trusted service with an operating-system cryptographic random source. No external PKI tool is required. Private signing keys stay under `pki` and are available only to the certificate owner. Workers receive certificate-only trust bundles.

Proposed defaults are a one-year CA, 30-day leaf certificates, leaf renewal after two-thirds of their lifetime and CA rotation beginning 60 days before expiry. These are product defaults for validation, not measured operating limits. A leaf must never outlive its issuer. Status reports effective expiry times, next renewal, issuer fingerprint, trust generation and last successful probe.

Certificates cover the gateway's actual IP/DNS endpoint and enabled inspection authorities. Kubernetes can require `kubernetes.default.svc`, model authorities and configured egress/MCP authorities. The gateway already checks certificate coverage for enabled handlers. Extend that validation to automatically generated material. [R2, R8, R10]

Self-managed and externally supplied certificate modes are explicit. External mode never silently generates a replacement CA. The operator owns issuance; Sproozi validates files, expiry and name coverage and reports the renewal boundary.

### 8.2 Leaf renewal

1. The certificate owner checks remaining lifetime at startup, periodically while running and after sleep/wake.
2. It creates a new key and leaf under the current CA in a private staging location.
3. It validates the key pair, chain, names and lifetime, then publishes the files atomically.
4. It reloads the TLS listener if supported. Otherwise it performs a bounded, documented gateway restart or drain without losing journals or allocating a second owner.
5. It probes the serving certificate from the restricted worker path and records completion.

Existing TLS connections may retain the old leaf until they close. New connections must see a valid leaf. A failed renewal keeps the previous valid generation and retries with bounded backoff. Expired or invalid serving material denies affected connections; it never disables verification.

### 8.3 Routine CA rotation

CA rotation changes worker trust and cannot be implemented as a simple replacement of the gateway Secret.

| Stage | Required behaviour | Evidence before advancing |
| --- | --- | --- |
| Prepare | Create CA B and a new public bundle containing A and B. Keep the gateway serving a leaf signed by A. Persist the rotation stage. | Valid issuer/leaf tests and durable generation records. |
| Distribute | Give new launches the combined bundle. Update or drain/restart current trust consumers whose processes cannot reload it. | Every current consumer acknowledges B, or its old execution has independently stopped. |
| Switch | Issue and serve a B-signed leaf. Keep A and B trusted during the overlap. | Restricted-path probes validate B and representative harness requests succeed. |
| Retire | Remove A only after no consumer requires it and old sessions/connections are drained. Destroy retired private signing material according to the retention policy. | No live allocation, listener or configured service client depends on A. |

The overlap lasts at least the longest remaining admitted execution plus connection/cache drain, unless all consumers have explicit durable acknowledgements sooner. Time alone is not proof of distribution. If a consumer is unreachable, retain overlap or stop it safely; do not retire trust based on an assumed stop.

Some native TLS clients read their trust store only at process start. Zero interruption is therefore not a requirement for this release. A bounded drain and native resume is acceptable if state and accounting remain intact and the user sees the transition. Sleeping/offline clients may need trust refresh before reconnecting.

Rollback is allowed to the last valid serving generation while its trust remains distributed and no compromise is suspected. Rotation resumes from its journal after a crash. It never creates a new unrelated CA merely because a file was temporarily unavailable.

### 8.4 Expiry, loss and compromise

- After long offline periods, reconcile certificates before starting workers. Show whether a simple leaf renewal or trust redistribution is needed.
- A missing/corrupt signing key stops issuance and affected new launches. Report recovery steps; do not silently reset installation identity or session state.
- An untrusted clock or significant clock jump fails certificate validation visibly. Do not solve it with an enlarged arbitrary validity window.
- Suspected CA compromise requires an explicit emergency replacement, revocation of affected sessions/identities and fresh trust distribution. Do not overlap trust in a compromised CA as though this were routine renewal.
- Backups containing private signing material and credentials require protection separate from public configuration exports. A public-only export must be the default.
- Certificate renewal, gateway identity token refresh and upstream credential rotation are separate lifecycles. Rotating one cannot substitute for the others.

## 9. Kubernetes installation and optional certificate management

### 9.1 Manual installation boundary

Publish release-matched `crds.yaml`, installation manifests, sample runtime/policy/template/run resources and optional overlays. The operator downloads and reviews them, chooses a context, and applies them explicitly. Local `init`, image preparation and executor registration never apply these resources.

Proposed quickstart command shape, using downloaded release files:

```sh
kubectl config current-context

# CRDs are a separate release asset
kubectl apply --server-side --field-manager=sproozi-installer \
  -f ./sproozi-release/crds.yaml
kubectl wait --for=condition=Established --timeout=60s \
  crd/agentruntimes.sproozi.com crd/agentpolicies.sproozi.com \
  crd/agenttemplates.sproozi.com crd/agentruns.sproozi.com

# This reviewed overlay selects certificate mode and release image digests
kubectl apply -k ./sproozi-release/quickstart-self-managed-ca
kubectl rollout status -n sproozi-system deployment/sproozi-gateway
```

These are proposed release assets, not paths present at the baseline. Applying the self-managed overlay explicitly authorises its narrowly scoped bootstrap/renewal component to write its declared certificate objects. It does not delegate installation to local CLI init.

### 9.2 Kubernetes requirements

| ID | Requirement |
| --- | --- |
| K8S-01 | Keep CRD installation separate and show the Established wait before creating custom resources. Publish supported Kubernetes versions, upgrade ordering and a rollback/data-compatibility note. |
| K8S-02 | Ship digest-pinned controller, gateway, webhook and worker references. The quickstart MUST NOT require a user image build or public registry push. Custom image publication remains optional. |
| K8S-03 | The first example MUST use no model/GitHub credentials and demonstrate controller admission, gateway TLS, a permitted Kubernetes read, a denied read/direct path and cleanup. Add real-provider examples separately. |
| K8S-04 | Self-managed CA mode MUST work without cert-manager CRDs, controllers or APIs. It creates installation-specific keys, serving certificates and public worker trust with the correct SANs. |
| K8S-05 | External certificate mode MUST accept operator-supplied TLS and trust references and omit the Sproozi signer. cert-manager or an enterprise issuer may satisfy this contract without being required. |
| K8S-06 | Preserve namespace boundaries. Signing/upstream keys remain in trusted services; `sproozi-agents` contains certificate-only trust and workload identities. Do not place upstream or CA private-key Secrets there. |
| K8S-07 | A certificate helper MUST have only the RBAC needed for its declared named Secrets/trust resources and renewal signalling. It MUST NOT have wildcard Secret access, cluster-admin or authority to modify agent policies/templates. Prefer precreated target objects where that permits narrower update rights. |
| K8S-08 | Trust bundles MUST be versioned immutable ConfigMaps. CA rotation creates a new object and switches approved runtime references for new launches; it MUST NOT patch an immutable bundle in place or invalidate an active Pod's trust unexpectedly. |
| K8S-09 | Running Pods remain on their recorded trust generation until refreshed by a supported mechanism or safely drained. The rotation plan accounts for gateway clients, profile/task services and remote developer clients as well as workers. |
| K8S-10 | The certificate helper MUST be replaceable through the same key/certificate/trust references. Its absence in external mode MUST NOT change harness arguments, capabilities or the four-CRD execution model. |
| K8S-11 | Operators MUST verify their CNI and node/API bypass behaviour. The quickstart reports the tested network assumptions and does not install a CNI over an existing cluster network. |
| K8S-12 | The guide MUST cover private registry pulls, certificate renewal failure, immutable trust rotation, gateway reload/restart, durable audit collection and retention. Uninstall instructions identify deletion of CRDs and custom resource data as destructive. |

### 9.3 Replaceable implementation choice

A small trusted controller is the preferred initial option for publishing Kubernetes Secrets and versioned trust ConfigMaps. A gateway-side helper is acceptable if it satisfies the same contract with a single writer and narrow RBAC. Do not add a privileged signer sidecar to agent Pods.

Bootstrap and ongoing renewal must have one authority. A one-shot Job alone does not satisfy renewal. A helper must detect existing valid material, reuse it, reconcile after partial writes and expose expiry/renewal health. Keep the interface small: named issuer material, serving certificate/key, public trust generation and reload signal. A future issuer can replace the helper without becoming part of agent execution.

This implementation choice needs a short design decision before K8s work begins. The product requirement is the certificate lifecycle and replaceability, not a new general PKI subsystem.

## 10. External-service credentials

### Current release

Sproozi stores upstream credentials in protected local files or operator-managed Kubernetes Secrets and brokers approved requests through trusted gateways. The worker receives a scoped, short-lived Sproozi identity plus any harmless placeholder expected by its native client. Existing GitHub App installation-token minting and ChatGPT refresh behaviour remain separate provider mechanisms. [R2, R5, R8]

| ID | Requirement |
| --- | --- |
| CRED-01 | Guided credential setup MUST store values outside agent homes, workspaces, images and logs. Local input validates the selected service without printing the value. Kubernetes credential creation remains operator-owned. |
| CRED-02 | Configuration MUST refer to a credential source by a stable local identifier/reference rather than copying a secret into every harness configuration. Preserve existing provider-specific file/Secret paths behind that reference. |
| CRED-03 | Replacing credentials MUST use atomic publication and a supported reload or controlled restart. If a module only reads credentials at startup, the CLI MUST say so and perform the required lifecycle action safely. |
| CRED-04 | Removing/revoking a stored credential MUST deny subsequent affected requests and close/recheck active sessions according to the gateway contract. It cannot guarantee upstream cancellation of work already accepted. |
| CRED-05 | Policy authorisation MUST remain independent from credential availability. A broad stored credential does not grant broad service access. Audit records include source identity/version, never secret material. |

### Future short-lived credential sources

Reserve a narrow trusted-side resolution boundary so a later adapter can acquire scoped external credentials from HashiCorp Vault, workload identity or enterprise brokers. Do not ship an empty plugin framework in the first release.

The future contract must support acquisition, expiry, renewal, revocation and an opaque credential handle. Requests identify the authenticated execution, service, allowed operation/scope and required validity window. The gateway rechecks live policy before acquisition and use. Credentials remain in the trusted process unless a separately reviewed service protocol requires scoped credential delivery to the worker.

Lease caches are keyed by principal, service, scope and policy generation. Concurrent renewal has a single owner. Never reuse a credential across a broader scope, log its value or silently fall back to a long-lived credential when the broker fails. Expired/uncertain leases fail closed. Broker outages have explicit timeout and retry behaviour; expiry during streaming requests requires a service-specific policy.

Vault support needs its own implementation and acceptance milestone. Mentioning this boundary does not mean existing file/Secret readers already provide dynamic leases.

## 11. Session lifecycle, upgrades and recovery

The existing session model remains authoritative. A session identifies the native conversation; an execution has its own approval, deadline and allowance; each placement visit has fresh allocation and container identity. A move is not a new budget. A later resume can create a new approved execution. [R7, R9, R11]

Required lifecycle behaviour:

- Persist provisioning intent before changing Docker/service resources. Repeated setup converges on the same installation.
- Record image, trust generation, shared-state schema and native harness with each launch.
- Before moving or checkpointing, gate input, finish supported tools, revoke access, independently confirm all relevant writers stopped, finalise usage and capture the selected state.
- Do not start a destination when source stop, credential revocation or usage is uncertain. Preserve the recovery command and operation identity.
- On upgrade, validate the new image and configuration before switching new launches. Keep the old image and state format until active and retained sessions have a supported continuation or explicit migration path.
- Shared-state migrations require a backup and exclusive native writer access. Downgrades must not open an incompatible shared native database silently.
- Routine cleanup removes only owned stopped resources. Pending/uncertain operations retain the evidence required for recovery.
- Sleep, Docker restart, service crash and TLS renewal failure are recoverable states with specific diagnostics. Local execution cannot continue while its host or engine is unavailable; remote independence applies only after a completed remote placement.

## 12. Milestones and ownership

Milestones are dependency gates, not calendar promises. A named maintainer must own each before implementation starts. Existing Kubernetes preview work need not wait for every local automation phase; the label "simplified local onboarding" requires M1 through M4.

| Milestone | Accountable role | Deliverables | Exit gate |
| --- | --- | --- | --- |
| M0: lock the contracts | Product/technical lead | Confirm shared-folder semantics; per-harness path/precedence table; image-source assessment; CLI additions; platform matrix; certificate-helper decision; migration design. | Decisions have owners and a verification plan. No unresolved choice can expose credentials or change authority implicitly. |
| M1: distribute runnable images | Release maintainer | Public pinned images and manifest; verified packaged build inputs; build/publish commands; architecture checks; provenance/SBOM; canary integration. | A clean installed CLI can obtain both supported native harnesses without a checkout or manual Dockerfile work. |
| M2: provision a local installation | Local-runtime maintainer | Idempotent local init; network/address probes; private configuration; credential prompts; supervised home service; diagnostics. | A fresh supported machine reaches the native Codex terminal through the documented local flow. No cluster or kubeconfig is used. |
| M3: share agent state safely | Harness/runtime maintainer | Persistent agents root; per-session harness selection; supported native settings; single-writer handling; migration and selected-checkpoint regression checks. | Codex and Claude state survive restart/image replacement without exposing trusted material or exporting whole homes. |
| M4: maintain trust and recover | Security/runtime maintainer | Automatic leaf renewal; staged CA rotation; service recovery; upgrade/rollback; emergency replacement runbook. | Forced-expiry, crash-during-rotation and offline-return cases pass. Unknown stop/usage remains held. |
| M5: simplify manual Kubernetes | Kubernetes maintainer | Versioned CRDs/install assets; public images; no-model check; optional self-managed signer and external mode; renewal/rotation guide. | A competent operator installs from release assets without cert-manager or source builds and proves allowed/denied access. |
| M6: release evidence | Release lead | Fresh-machine walkthroughs; support matrix; redacted acceptance reports; present/planned docs; upgrade and uninstall instructions. | Applicable release gates below pass and documented exceptions have explicit scope. |
| Later: enterprise credential leases | Gateway/integration maintainer | Trusted resolver adapter, Vault/broker acquisition/renewal/revocation, scope-aware cache and outage tests. | Short-lived credentials expire/revoke correctly without exposing provider credentials to agents. |

A first public preview may ship with operator-managed Kubernetes setup and clearly labelled local setup gaps. It must not advertise this PRD's complete local experience until the corresponding gates pass. Simplified Kubernetes certificate automation can follow if its manual certificate lifecycle is documented honestly in the earlier preview.

## 13. Acceptance criteria and release evidence

Each scenario records product commit/version, host/engine version, architecture, exact image digests, selected harness, result and redacted evidence. Deterministic provider fixtures test failure paths. A separately authorised real-provider smoke test establishes actual authentication and native interaction. Neither substitutes for the other.

| ID | Scenario | Pass condition |
| --- | --- | --- |
| AC-01 | Fresh local install | On every advertised platform, a user with running Docker and a Git workspace installs the CLI, runs local init, supplies provider information and starts a stock terminal without a checkout, manual certificates, image IDs or network edits. |
| AC-02 | Cluster independence | The full local flow succeeds with no kubeconfig or Kubernetes tools. A deliberately configured unrelated cluster receives zero calls/writes from local init. |
| AC-03 | Repeated/interrupted init | Repeat setup and interrupt it after each external mutation. Recovery preserves the same CA, installation identity and owned resources, without duplicating or overwriting configuration. |
| AC-04 | Image resolution/fallback | Official-image selection and wrapper selection both follow the manifest. Pull/cache/build work on advertised architectures. Hash mismatch, unsupported architecture and failed registry auth cannot activate an unapproved worker. Explicit publication returns a usable immutable registry reference. |
| AC-05 | Native UX | Codex and Claude show their native UI; resize/signals/detach/reattach work. Supported same-harness stop/resume preserves the conversation. Known unsupported native features fail clearly. |
| AC-06 | Shared agent state | Both harnesses use the same configured agents root. Supported settings/history survive service and image replacement. Concurrent writes queue or reject safely. No hidden per-profile root appears. |
| AC-07 | Credential separation | Sentinel secrets placed in the credential store, private PKI and trusted journal are absent from worker mounts/environment, image layers, checkpoints and support exports. Placeholder keys cannot select another provider account. |
| AC-08 | Enforced network path | From the actual worker namespace, approved gateway traffic works and direct provider/API, host/LAN/metadata, alternate-port, DNS and IPv6 attempts fail as specified. Removing/altering anchor controls blocks activation/attachment. |
| AC-09 | Leaf renewal | Advance a test clock through the renewal threshold. A valid new leaf is served without manual commands; identity/accounting remain intact. Invalid replacement and issuer expiry fail closed. |
| AC-10 | CA rotation | Drive A-only, A+B, B-serving and B-only stages with active consumers. Restart the certificate owner at each stage. No consumer loses trust silently; stale/offline consumers refresh or drain before A retirement. |
| AC-11 | Emergency trust recovery | Missing/corrupt keys and suspected compromise produce the documented stop/re-enrolment path. No verification bypass or silent unrelated CA is introduced. |
| AC-12 | Service/crash recovery | Crash the service, restart Docker, interrupt a move and simulate lost replies. Recover the original operation without a second writer, deadline extension, budget refill or assumed stop. |
| AC-13 | Checkpoint boundary | Shared files, unrelated conversations, credentials, hooks and private keys do not enter a portable checkpoint. Supported Git changes and the selected conversation still survive an allowed local/remote round trip. |
| AC-14 | Kubernetes first check | A competent operator applies release CRDs and reviewed install resources, waits for readiness, runs a no-model example, observes permitted/denied access and confirms controlled cleanup. No developer source build is needed. |
| AC-15 | Kubernetes certificate modes | With cert-manager absent, explicit self-managed mode bootstraps and renews. External mode deploys no signer and accepts valid operator material. Root rotation creates new immutable trust objects and handles existing Pods safely. |
| AC-16 | Least privilege | The certificate helper cannot read unrelated Secrets, change policy or act as cluster-admin. Workers cannot reach signer keys or service control endpoints. Disabled capabilities need no unrelated credentials. |
| AC-17 | Upgrade/uninstall | Upgrade preserves approved retained work and records migration. Rollback follows documented compatibility. Default uninstall retains agents, checkpoints and credentials; explicit purge removes only owned data after an affected-session report. |
| AC-18 | Useful failure output | Port conflicts, unavailable Docker, invalid file permissions, wrong SAN, expired credentials, missing pricing, broker/provider denial and full storage each identify the failing component and an actionable next step without disclosing secrets. |

Proposed UX targets are at most two product commands after installation for a first attached session, excluding credential answers. On a declared cached-image reference machine, target readiness within 60 seconds, excluding interactive input and upstream latency. Cold setup must show download/build progress and bounded retries; it has no unconditional time promise. Measure these targets during acceptance and publish the actual machine/network conditions.

## 14. Risks and mitigations

| Risk | Consequence | Mitigation / release condition |
| --- | --- | --- |
| Shared native state permits cross-session changes | One agent can affect another agent's exposed settings/history. | State the shared trust model, keep trusted authority outside it, allowlist mounts and serialize unsafe writers. Separate profiles remain future work. |
| Shared folders broaden native settings discovery | Hooks/plugins could bypass assumptions about completion or loaded configuration. | Publish a per-harness precedence contract; keep authority-critical settings fixed and test hostile configuration. |
| Official images are unsuitable or stop being maintained | Onboarding depends on stale/privileged/incompatible artefacts. | Re-evaluate every release, retain reproducible wrapper recipes and select by compatibility evidence. |
| Native CLI updates change state/protocol formats | Resume or checkpoint capture fails. | Release pins, isolated canary, immutable running images and rollback/migration checks. |
| Platform networking differs | The gateway is unreachable or a direct route escapes containment. | Actual namespace probes per platform; explicit support matrix; fail before launching. |
| Trust rotation races with long-lived clients | Connections fail after switching issuer or old trust remains indefinitely. | Staged overlap, durable consumer generations, bounded drain and manual recovery. |
| Helper/controller privileges expand | Compromise exposes signing keys or unrelated Secrets. | Dedicated trusted component, narrow named-object rights, no worker signer sidecar and explicit RBAC tests. |
| Automatic rebuilds resolve mutable dependencies | Same product version executes different code. | Verified packaged recipes, exact package/base pins, provenance and no silent fallback. |
| Reconfiguration invalidates retained authority | Old sessions cannot recover or two owners run. | Migration/drain gate, durable journal identity and single-owner service control. |
| Credential handling appears broader than it is | Users expect subscription auth, Vault leases or immediate upstream cancellation. | Publish supported auth modes and reload semantics; keep future leasing clearly labelled. |
| Simplified setup is mistaken for production security | Users rely on unverified network/spend boundaries. | Preserve preview status and specific network, budget and native-feature limitations in setup and docs. |

## 15. Decisions and open questions

### Decisions fixed by this PRD

- Docker is assumed installed. The local CLI owns provisioning of Sproozi's local dependencies.
- Native harness UX and upstream agent loops remain intact.
- All local agents use one `sproozi/agents` root. Configuration profiles are deferred.
- Stored upstream credentials stay in trusted services and are brokered through existing enforcement points.
- Automatic TLS generation, leaf renewal and routine CA rotation are part of the complete local onboarding scope.
- Prefer maintained, suitable official images. Otherwise publish reproducible minimal wrappers and offer a packaged local build path.
- Kubernetes installation remains an explicit operator action. `init` never changes clusters.
- Self-managed Kubernetes CA mode is optional and has no cert-manager dependency. External certificate providers remain supported.
- Shared local homes do not change the selected-state portability or fresh-admission security contracts.
- Enterprise credential leasing is future work. A competent-user Kubernetes setup is acceptable for the first public preview.

### Proposed defaults requiring confirmation during M0

| Question | Proposed default | Owner / deadline |
| --- | --- | --- |
| Exact shared native paths and writable scope? | One agents root with harness-native subdirectories and a shared input directory; exclude authentication and authority files. Serialize native writers initially if needed. | Harness lead, before mount implementation. |
| Default local root and existing config migration? | `~/sproozi` for new installations; preserve platform config discovery and explicit private-root overrides. | CLI lead, before schema freeze. |
| Image registry and artefact signing trust? | Project-owned public OCI registry; authenticated release manifest with immutable digests and documented verification roots. | Release lead, before M1 publishing. |
| Are suitable official harness images available? | Treat current official candidates as unapproved until maintenance and Sproozi compatibility gates pass. Ship wrappers if they do not. | Release/harness leads, each release. |
| One combined native image or harness-specific images? | Prefer smaller harness-specific workers if they preserve the helper contract; sharing an image is acceptable initially. | Runtime/release leads, M1. |
| How is the local home service supervised on each platform? | Per-user service-manager integration, with an explicit foreground troubleshooting mode. | Platform lead, M2. |
| How are gateway certificates reloaded? | Implement atomic reload if practical; otherwise document and test bounded drain/restart. | Gateway lead, before M4. |
| Which component manages Kubernetes certificates? | A small replaceable trusted controller; a side helper is acceptable if it proves the same single-writer/RBAC contract. | Kubernetes/security leads, before M5. |
| Are the one-year CA and 30-day leaf suitable? | Use them as initial configurable defaults and validate overlap against all admitted lifetimes. | Security lead, M4 design review. |
| Should native Windows automatic setup be in the first supported matrix? | Gate on actual Linux-container networking, ACL, service and PTY acceptance; retain existing binary availability independently. | Platform/release leads, before M6. |
| How broad is local subscription authentication? | API keys first for both harnesses; preserve existing documented gateway-owned OpenAI flows where proven. Never import personal native login stores. | Gateway lead, M0. |
| What local credential protection ships beyond file permissions? | Existing private file storage initially; OS keychain support may follow without changing references. | Security/platform leads, M0. |
| Does the first public preview include Kubernetes automated renewal? | Prefer yes; if deferred, label it operator-managed and do not claim M5 acceptance. | Release lead, before scope announcement. |

## 16. Source notes

Repository links below are pinned to the inspected commit. Earlier project audit/implementation-plan files are historical context and were not used as evidence of current behaviour. The referenced conversation supplies product context; its cached assistant messages were opaque content references, so their technical claims were not assumed.

- R1: [CLI installation](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/guides/install.md) and [release workflow](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/.github/workflows/release.yml).
- R2: [Home service and configuration](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/cmd/sproozi/README.md) and [home configuration types](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/internal/session/home/config.go).
- R3: [Docker placement](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/internal/runtime/docker/README.md) and [current local preparation](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/guides/session-mobility.md).
- R4: [`init` implementation](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/cmd/sproozi/init.go) and [command declarations](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/cmd/sproozi/commands.go).
- R5: [Harness support and limits](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/reference/harnesses.md).
- R6: [Pinned client build process](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/hack/clients/README.md), [native image recipe](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/internal/runtime/docker/image/Dockerfile) and [ADR 0021](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/adr/0021-pinned-ci-inputs-and-latest-client-canary.md).
- R7: [System architecture](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/explanations/system-architecture.md).
- R8: [Secrets and configuration](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/reference/required-secrets-and-config.md) and [security limits](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/reference/security-hardening.md).
- R9: [Selected native/Git checkpoints, ADR 0008](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/adr/0008-selected-native-and-git-checkpoints.md) and [native completion, ADR 0011](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/adr/0011-native-session-compatibility-and-completion.md). Later amendments apply as described in current harness/client documentation.
- R10: [Manual Kubernetes installation and current CA handling](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/guides/install-kubernetes.md) and [current Kind walkthrough](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/guides/getting-started.md).
- R11: [Current session commands and identity model](https://github.com/andrewmccall/sproozi/blob/25a44e65ce62cc9a803a6dc5a8fb5b8800bae826/docs/reference/session-commands.md).
- U1: [Official OpenAI cloud environment documentation](https://learn.chatgpt.com/docs/environments/cloud-environment?translationFallback=es-419), checked 20 October 2026. It documents `codex-universal` as an environment image; Sproozi runtime suitability remains an engineering check.
- U2: [Official Claude Code development-container documentation](https://code.claude.com/docs/en/devcontainer), checked 20 October 2026. It distinguishes a Dev Container Feature from the reference container and describes version pinning and optional firewall privileges.
