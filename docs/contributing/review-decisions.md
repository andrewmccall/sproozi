# Review findings left open

Reviews on 2026-10-21 by Claude (Fable and Opus) and Codex (GPT-6-Astra and
GPT-6.1-Sol) raised findings. Most are fixed. This page records the ones left
open on purpose, so later reviews can check the reason rather than raise them
again. A finding moves off this page when its reason stops holding.

The IDs come from review reports that are not published in the repository, so
each row states the finding in full; read the row, not the ID.

## Deferred: worth doing, not yet done

| ID | Finding | Why it waits |
| --- | --- | --- |
| SEC-05 | `chatgpt-login` passes an ID-token hint to the browser launcher's arguments, visible to local `ps`. | The hint keeps a returning sign-in on the same account. Dropping it, or launching the browser without arguments, changes the provider flow and needs testing against the real sign-in. |
| SEC-08 | The inspection CA has no name constraints. | Inspected names include administrator-configured egress destinations, which change after the CA exists, so a constraint would force a CA rotation for every new destination. Rotation exists ([gateway certificates](../reference/gateway-certificates.md)) but replaces trust in every running sandbox. Revisit when destinations can be added without a rotation. |
| ENG-05 | The Docker and Kubernetes placements duplicate persistence helpers; `dupl` is off for internal packages. | A shared helper package is a refactor across both backends' durable state. It needs its own change and review. |
| ENG-06 | Some errors drop their cause. | A per-site review, not a count. The controller now wraps transition errors; other sites change as they are touched. |
| ENG-15 | Windows PID liveness is a stub, so a stale PID file reads as a running service. | Automatic setup does not run on Windows ([Windows guide](../guides/windows.md)), and the manual path tells users to stop the service themselves. It must be fixed before Windows lifecycle support is claimed. |
| ENG-20 | The CLI's JSON `next` field is cut from the message text. | Needs a structured field on the error type throughout the CLI. |
| DOC-03 (part) | The README has no comparison with other sandboxes. | Badges and the licence line are added; a fair comparison needs research. |
| DOC-08 (part) | `docs/guides/model-provider-auth.md` holds reference material. | Component references are now indexed; splitting the accounting sections is a larger edit. |
| Registry records | Session registry ConfigMaps are never removed automatically. | A coordinator may retry a stop after its run is gone, and the record answers it. Needs a bound on those retries ([ADR 0041](../adr/0041-gateway-state-namespace-and-retained-state-owners.md)). |

## Declined: intended behaviour

| ID | Finding | Why it stays |
| --- | --- | --- |
| SEC-06 | The local gateway may listen on all interfaces. | Only when Docker reaches neither loopback nor the bridge address, which are tried first. The listener requires TLS 1.3 and a per-execution token; [security hardening](../reference/security-hardening.md) recommends a host firewall for that case. |
| SEC-07 (part) | An existing sandbox Pod's scheduling fields are not compared. | Admission controllers set tolerations, node selectors and priority. Comparing them would reject the controller's own Pods. Security-relevant fields, DNS and host aliases are compared. |
| R3-SEC-01 | An existing sandbox keeps whatever gateway address it was created with. | Re-resolving made a resolver outage stop reconciliation. The NetworkPolicy allows only the gateway Pods, and harnesses verify the gateway's certificate by name ([ADR 0040](../adr/0040-sandboxes-reach-the-gateway-without-dns.md)). |
| ENG-13 | Status is written with whole-object updates. | Resource-version conflicts make a stale write fail and retry instead of overwriting. |
| ENG-16 | On Kubernetes, stopping a Claude session whose Pod already ended reports unknown. | Without Claude's acknowledgement the placement cannot prove the conversation is complete; it refuses to invent one ([ADR 0032](../adr/0032-stopping-a-visit-with-no-native-conversation.md)). |
| ENG-17 (part) | `make test` does not use the race detector. | `make verify-fast`, which AGENTS.md requires for Go changes and CI runs, does. |
| ENG-22 | Code identifiers such as `NativeKind` differ from glossary words. | The glossary allows code and wire names; renaming them is churn without a reader benefit. |
| ENG-23 (part) | `internal/harness/launch.go` panics on a missing embedded file. | It is a build-time invariant. The kubeconfig panic in `contract.go` now returns an error. |
| DOC-07 | A long requirements note sits in `docs/explanations/future-architecture/`. | It is labelled historical and links the current guides. |
| DOC-09 (part) | Most Go files have no licence header. | `LICENSE` covers the repository and the README names it. Headers are not required. |
| DOC-13 | British and American spellings are mixed. | Editorial; it never changes a term's meaning. |
| DOC-15 | The README is first person and its support table is dense. | A deliberate voice. |
