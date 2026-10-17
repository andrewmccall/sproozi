# 0006: Native CLI and terminal access before GUI mobility

Status: accepted
Date: 2026-10-10
Reconciled: 2026-10-12
Implementation: native terminal transport implemented; GUI/IDE mobility deferred

## Problem and constraints

The same native conversation must survive the local-to-Kubernetes-to-local round
trip. Conversation continuity, terminal attachment and an official GUI connecting
to remote native state are different capabilities. Treating all client interfaces
as one requirement would couple the first mobility release to unproved vendor
client behavior.

At the initial decision Sproozi launched bounded stock CLI workers without
interactive attachment. Native GUI mobility remains a separate unimplemented
integration.

## Decision

The first complete mobility experience uses the native CLI locally and terminal
attachment to the real native CLI remotely. Support detach/reconnect as part of
that experience. Retain the native harness loop and conversation; a new Sproozi
chat UI is not required.

The public executable is `sproozi`, selected in the 12 October naming review.
Keep the existing flat verbs, including `serve`, `start`, `attach` and `move`.
Session remains the domain term for conversation continuity and internal state;
the executable identifies the product. The separate `session-profile` command is
an administrator service rather than the developer entry point.

Defer native GUI/IDE integration to a separate compatibility proof. A CLI resume
command, app-server protocol or terminal connection does not justify claiming
that an official GUI can attach to an arbitrary Sproozi execution.

CLI-first delivery still requires the complete conversation/workspace round trip.
Terminal reconnect alone cannot satisfy it. The implemented path uses the native
supervisor's actual PTY, a bounded output replay buffer and one input attachment.
The private home socket and fixed remote HTTPS profile authenticate and bind
attachments to the exact active placement incarnation. Disconnect leaves bounded
execution running; explicit cancellation revokes and stops it. ADRs
[0010](0010-managed-session-authority-and-admission.md) and
[0011](0011-native-session-compatibility-and-completion.md) record those mechanisms
and their independent runtime fence.

## Alternatives and tradeoffs

Requiring native GUI/IDE access in the first release could preserve a more familiar
user experience. It would also add client discovery, authentication, endpoint
selection and vendor compatibility to the checkpoint/move proof. Defer it while
the native CLI establishes continuity.

A custom Sproozi conversation UI could avoid some native GUI connection constraints,
but introduces another interface and conversation representation to maintain.
The accepted experience keeps the existing harness interaction.

Batch-only resume would reduce interactive transport work, but would not supply
the selected ability to interact with the session while it runs remotely.

Keeping the generic executable name `session` describes one domain object but
does not identify Sproozi. A compatibility alias would preserve an unreleased
name and create another entry point to document. Rename the entry point and its
callers together. A `sproozi session ...` hierarchy adds an extra level without
a current conflicting workflow; retain the flat verbs for this release.

## Accepted costs and consequences

The first experience is terminal-oriented and can involve explicit attachment
commands. Developers who require GUI-only interaction must wait for that specific
integration. Each supported client path needs its own versioned evidence. Native
conversation continuity must not depend on reproducing a GUI's private state.

## Evidence and reconciliation

The user accepted native CLI locally and terminal attachment remotely during the
10 October 2026 grilling. The [session CLI](../../cmd/sproozi/README.md),
[native supervisor](../../cmd/native-session/README.md) and
[fixed remote profile](../../cmd/session-profile/README.md) now expose actual
terminal attachment. The deterministic Codex core and extended proofs used the
production terminal through both placements, including real tools, gated input
and forced cancellation during a held move.

Official [Codex app-server documentation](https://learn.chatgpt.com/docs/app-server)
describes stored-thread resume and client/server methods. That is a candidate
native interface considered during design. The selected implementation uses a
real terminal rather than a prompt wrapper or app-server connection. Its CLI/TUI
proofs do not establish native GUI or IDE attachment. The production Claude Code
2.1.27 round trip separately used its real TUI in both placements, retained the
same UUID and conversation, and executed foreground Bash. Its selected completion
and shutdown behavior differs from Codex, as recorded in ADR 0011; neither proof
establishes an official desktop-client connection.

Related design: [client compatibility levels](../explanations/future-architecture/harness-portability.md#client-compatibility-has-separate-levels)
and [session mobility](../explanations/future-architecture/session-mobility.md).

## Revisit conditions

Add a native GUI/IDE path when a supported client can select the remote
session, preserve continuity and respect attachment authorization/revocation.
Revisit its priority if the terminal experience prevents intended users from
adopting mobility. Revisit a custom UI only if native interfaces cannot meet a
concrete requirement and its additional ownership cost is accepted explicitly.
Revisit command grouping when another shipped workflow creates ambiguous root
verbs. Once an interface is released, weigh compatibility before renaming it.
