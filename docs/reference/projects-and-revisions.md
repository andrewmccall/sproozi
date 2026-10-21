# Projects and workspace revisions

A Project groups retained sessions and limits who can access them. An
administrator configures its template, capabilities and members. Projects are
optional; sessions without a Project remain valid.

Each configured executor uses one fixed template, harness, instruction digest
and placement. Use separate executors when Projects need different templates.
See [core concepts](../explanations/core-concepts.md) for the session model.

## Owner configuration

Add these fields to the private `session-profile` configuration alongside its
existing allowances and approved repositories:

```json
{
  "templateName": "portable-codex",
  "repositories": {
    "sproozi": {
      "url": "https://github.com/andrewmccall/sproozi",
      "ref": "main",
      "reference": "https://github.com/andrewmccall/sproozi"
    }
  },
  "projects": {
    "sproozi-dev": {
      "template": "portable-codex",
      "workspace": "sproozi",
      "capabilities": ["model.inference", "github.pull_request"],
      "members": ["developer"]
    },
    "home-ops": {
      "template": "portable-codex",
      "capabilities": ["model.inference", "kubernetes.read"],
      "members": ["developer"]
    }
  }
}
```

This is a configuration fragment. Every selected capability must already exist
in the owner's allowances and fit live Kubernetes policy. Project selection copies
that subset of numeric ceilings; it never enlarges a grant. Project IDs contain
1–63 lowercase letters, digits or hyphens and start with a letter. Members are
explicit authenticated **installation principals**, such as the profile's
`principal`; they are not usernames inferred from Hermes messages.

`workspace` is optional. When present it names an approved repository. A caller
may omit `--repository` and use that workspace; a conflicting explicit repository
is rejected. Without a workspace the owner prepares an empty controlled Git
repository. Snapshot-backed Project repositories require a credential-free
`reference`; URL sources default to their configured URL. HTTPS, SSH and URN
identities are descriptive provenance. They contain no embedded user credentials,
query or fragment and are never used as authorization.

## Run and discover work

```sh
sproozi run --remote --context engineering --project sproozi-dev --harness codex \
  --task 'Investigate the failing package check and propose a change'
sproozi sessions --context engineering --project sproozi-dev
sproozi sessions --context engineering --project home-ops
sproozi status ID --context engineering
```

Equivalent MCP inputs add `project` to `sproozi_run` and optionally to
`sproozi_sessions`. MCP session listing returns `{"sessions": [...]}`; REST
`/work/list` retains its array response. Unfiltered discovery includes accessible
Project sessions and standalone work. A filter returns only that Project; an
unavailable Project returns an empty list, including across separate executors.
Direct-ID operations, exact submission replay, export, cancellation and recovery
also check current membership. Changing a submitted Project or revision source
conflicts with the original operation's immutable intent.

Project capability definitions select new submission grants. Editing a definition
does not rewrite an already admitted remote visit; its saved grant remains
subject to current placement policy. Configuration is loaded on service startup.
After membership removal, public
reads/admission are denied. The trusted owner still cancels unresolved writers
on restart, independently of caller membership. Keep its fixed template,
instructions, placement and principal compatible with the retained catalogue;
a different template cannot reinterpret existing work. Local resume, recovery and
placement movement recheck current Project approval before preparing or activating
a writer. If an interrupted execution's immutable grant exceeds narrowed approval,
cancel it before starting a fresh resume. Recovering a stop or cancellation
still stops the agent and finalizes usage, even without approval. Configuration removal
is not a storage deletion or an accounting refund.

## Independent local resume approval

Pull authenticates the source and imports saved work without starting an agent.
The checkpoint records its Project association. The home must independently
approve the next execution. Add this fragment to the private home configuration,
using the exact configured source name:

```json
{
  "projectApprovals": [
    {
      "source": "remote",
      "project": "sproozi-dev",
      "capabilities": ["model.inference"]
    },
    {
      "source": "remote",
      "project": "home-ops",
      "capabilities": ["model.inference", "kubernetes.read"]
    }
  ]
}
```

These capability subsets use the home's current usage limits. They do not copy
the remote grant or remaining allowance. Missing approval produces an actionable
resume error before preparing a sandbox. An approval for a different source with
the same Project name does not match. Project/template/repository association
remains visible after local resume and stop; `sproozi sessions --project ID`
filters this machine's sessions too.

## Conversation checkpoint and Git revision

Retained status exposes independent identities:

- `threadId`: the harness conversation ID.
- `checkpointDigest`: SHA256 of the complete stopped conversation/workspace artifact.
- `repositoryReference`: immutable descriptive repository identity from owner configuration.
- `checkpointInfo.workspace.revision`: full Git HEAD captured with the selected workspace.
- `checkpointInfo.workspace.objectFormat`: Git object format.
- `checkpointInfo.workspace.sha256`: digest of the complete selected workspace archive,
  including supported staged, working and selected untracked state.

Local status reports the last committed checkpoint while a writer is active;
this is not a live filesystem revision. Bounded inspection validates manifest
metadata. Restore also verifies the actual Git objects before starting an agent.
A configured fetch instruction or repository label alone does not prove an
upstream checkout.

Native resume restores the same conversation and complete supported unfinished
workspace. To start a **fresh conversation** at the clean committed revision:

```sh
sproozi run --remote --context engineering --project sproozi-dev --harness codex \
  --workspace-from SOURCE_SESSION_UUID \
  --checkpoint-digest EXACT_STOPPED_CHECKPOINT_SHA256 \
  --revision FULL_RECORDED_GIT_HEAD \
  --task 'Run the package checks against this recorded commit'
```

All three source flags are required together. The source must be in `retained`,
`exporting` or `pulled` state, with confirmed stop and finalized usage
records. The current principal must have access. Its approved repository alias
and reference must match the target. The full requested revision must equal that
source checkpoint's recorded HEAD; a branch, short commit, arbitrary ancestor or
moving ref is rejected.

The owner creates a clean detached checkout from saved Git objects in private
staging. It saves the input record before preparing the destination. Dirty and
untracked changes and harness conversation history are excluded from this fresh
workspace-only execution. No network fetch or Git service is needed. The source
checkpoint, receipt and ownership remain unchanged, even for unpublished
commits. The destination has a fresh harness conversation ID (`threadId`), visit, deadline and
independent grant. This workflow neither publishes commits nor gives permission
to push them.

See [ADR 0013](../adr/0013-configured-projects-and-workspace-revisions.md) for
alternatives, accepted costs and revisit conditions, and the
[Hermes with remote work](../guides/retained-work-kubernetes.md) guide for setup.
