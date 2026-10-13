# Projects and workspace revisions

Projects group retained work under administrator configuration. They reference
independent execution templates; each configured executor still owns one fixed
template, harness, instruction digest and placement. Use separate executors when
projects need different templates. Existing sessions without a Project remain valid.

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
sproozi run --executor remote --project sproozi-dev --harness codex \
  --task 'Investigate the failing package check and propose a change'
sproozi sessions --executor remote --project sproozi-dev
sproozi sessions --executor remote --project home-ops
sproozi status SESSION_UUID --executor remote
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
does not rewrite an already admitted remote allocation; its saved grant remains
subject to current placement policy. Configuration is loaded on service startup.
After membership removal, public
reads/admission are denied. The trusted owner still cancels unresolved writers
on restart, independently of caller membership. Keep its fixed template,
instructions, placement and principal compatible with the retained catalogue;
a different template cannot reinterpret existing work. Local resume, recovery and
placement movement recheck current Project approval before preparing or activating
a writer. If an interrupted execution's immutable grant exceeds narrowed approval,
cancel it before starting a fresh resume. Stop/cancellation recovery still fences
and accounts without an approval. Configuration removal
is not a storage deletion or an accounting refund.

## Independent local continuation approval

Pull remains an inert, authenticated import. A Project association in a checkpoint
supplies history; the home must approve the next execution itself. Add this fragment
to the private home configuration, using the exact configured source name:

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

These subsets use the home's current allowance ceilings. They do not copy remote
remaining authority. Missing approval produces an actionable resume error before
preparing a writer. An approval for a different source with the same Project name
does not match. Project/template/repository association remains visible after local
resume and stop; local filtering uses `sproozi sessions --executor docker --project ID`.

## Conversation checkpoint and Git revision

Retained status exposes independent identities:

- `threadId`: the pinned harness's native conversation UUID.
- `checkpointDigest`: SHA256 of the complete stopped conversation/workspace artifact.
- `repositoryReference`: immutable descriptive repository identity from owner configuration.
- `checkpointInfo.workspace.revision`: full Git HEAD captured with the selected workspace.
- `checkpointInfo.workspace.objectFormat`: Git object format.
- `checkpointInfo.workspace.sha256`: digest of the complete selected workspace archive,
  including supported staged, working and selected untracked state.

Local status reports the last committed checkpoint while a writer is active;
this is not a live filesystem revision. Bounded inspection validates manifest
metadata. Inert restore additionally verifies the actual Git objects. A configured
fetch instruction or repository label alone does not prove an upstream checkout.

Native resume restores the same conversation and complete supported unfinished
workspace. To start a **fresh conversation** at the clean committed revision:

```sh
sproozi run --executor remote --project sproozi-dev --harness codex \
  --workspace-from SOURCE_SESSION_UUID \
  --checkpoint-digest EXACT_STOPPED_CHECKPOINT_SHA256 \
  --revision FULL_RECORDED_GIT_HEAD \
  --task 'Run the package checks against this recorded commit'
```

All three source flags are required together. The source must be retained,
exporting or home-owned with stopped/sealed evidence, accessible to the current
principal, and have the same approved repository alias and reference as the target.
The full requested revision must equal that source checkpoint's recorded HEAD;
a branch, short commit, arbitrary ancestor or moving ref is rejected.

The owner reconstructs a clean detached checkout from retained objects in private
staging and durably records its input before preparing the destination. Dirty and
untracked changes and native conversation history are excluded from this fresh
workspace-only execution. No network fetch or Git service is needed. The source
checkpoint, receipt and ownership remain unchanged, even for unpublished commits.
The destination has a fresh native UUID, allocation, deadline and independent grant.
This workflow neither publishes commits nor gives permission to push them.

See [ADR 0013](../adr/0013-configured-projects-and-workspace-revisions.md) for
alternatives, accepted costs and revisit conditions, and the
[retained-work deployment guide](../guides/retained-work-kubernetes.md) for setup.
