# 0066: A contradicted retained record refuses work and never blocks an end

Status: accepted
Date: 2026-11-17

## Problem and constraints

The remote executor keeps one private file per Kubernetes visit, the
[retained record](../../GLOSSARY.md#retained-record), beside the
[session registration](../../GLOSSARY.md#session-registration) in the
cluster. The record holds the launch, the registration's reference and what a
stop or cancellation has established so far: a quiet source, an exit, a
turnless first visit, a fenced writer, the output the stop keeps. It can also
hold an aborted preparation's tombstone. The executor writes these facts in a
fixed order, and records the registration's next phase only after the facts
it depends on are saved.

[ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md) rule 2
asks that a record be validated on load and save. It says nothing about what
a record that fails validation does to the operations that read it. The
record had no validation at all before this decision, and every fact in it
was trusted.

That choice matters in both directions:

- **Trusting a contradicted fact can do harm.** A record that claims a fenced
  writer while its registration is still active would let a cancellation
  take a vanished Pod as proof that the writer stopped, and seal its usage.
  A record that claims output it never captured would let a stop keep it as
  the visit's checkpoint.
- **Refusing a contradicted record can strand a session.** A session that
  cannot be stopped, cancelled or removed is worse than one with a loose
  record. An earlier slice hit exactly this in the home catalogue: an
  unreadable journal blocked every `rm`.

The file is on a user's machine, so there is no migration step. Unknown
fields stay refused, and every file the current code writes must still load.

## Decision

1. **One rule says which records are valid.** `retained.check` in
   `internal/runtime/kubernetes/placement/storage.go` lists each
   contradiction of the write order, between the record's own facts or with
   the registration's phase, conversation and receipt (`retainedRules`), and
   how it is corrected. `load` applies it to the record alone. Every read
   beside the registration applies it again with that registration, and
   every `save` refuses a contradicted record, checked beside the
   registration the caller holds.
2. **The registration is the authority for what its phase proves.** The
   executor records a phase only after the facts before it are retained, so
   the phase proves those facts:
   - a stopped registration's receipt proves the source quiet, the writer
     fenced and the output it names. A record that lacks them takes them from
     the receipt;
   - a cancelled registration with a receipt proves the writer fenced: it was
     sealed only once the fence was seen. A record without that fence is not
     a contradiction, since an abort of a preparation this executor never saw
     writes one, so the check adds the fence;
   - a stopping registration proves only that the gateway's authority was
     revoked, and the other phases prove no fact of the record.

   A record that lost its reference, because the executor lost the record
   and a replayed `Prepare` wrote it again, finds its registration by
   replaying `Create`, which returns the registration as it now is. The
   record takes what that registration proves before it is saved, so the
   replay never leaves a record that every later save refuses.
3. **Operations that start or extend work refuse a contradicted record.**
   `Activate` and `Attach` refuse it before any side effect, and so does
   `Prepare` for a contradicted record it loads. A `Prepare` that finds no
   record first does what every replay of a lost preparation does: it
   claims the session's epoch or finds it already claimed for the same
   launch, writes the input and a record holding the launch alone, and
   replays the registration's `Create`. When that registration has moved on
   it contradicts the new record: `Prepare` saves the record corrected from
   the registration, as in decision 2, and then refuses. In process the
   refusal is `session.ErrRecordContradicted`. Over the profile protocol it
   is `conflict` (HTTP 409), the existing code for a request that conflicts
   with retained state, which a retry does not cure. The remote executor's
   driver records a problem that states the contradiction alone, without
   the errors around it, which its log keeps, and says to cancel a task or
   to stop or cancel an interactive session.
4. **Operations that observe or end a visit continue from the facts that
   hold.** Any fact a contradiction makes untrustworthy, and that the
   registration does not prove, is forgotten:
   - `Stop`, `Abort` and `AbortPreparation` save the corrected record before
     they go on;
   - `Inspect` and `Exited` do not save the record to correct it, so it
     keeps its contradiction until an operation that ends the visit reads
     it. A save `Inspect` makes for its own reason, such as recording a
     fence it observes, writes the corrected record.
   Each operation observes a forgotten fact again, from the Pod, by
   capturing again or by asking the supervisor.
5. **A forgotten fence is never trusted.** When the Pod is gone, nothing can
   show the writer stopped, so the operation reports the fence unknown
   rather than stopped.
6. **An end fails only when what proves it is lost.** A contradiction never
   keeps a visit from being stopped, cancelled and removed while the
   evidence its end needs exists. A stopped visit's end returns the
   checkpoint its receipt names, read from the executor's output file and
   checked against the receipt's digest. If that file is lost, or holds
   other bytes, the stop's replay, `Inspect` and `Abort` each fail, and
   none reports the visit stopped. Cancelling cannot end it either, since an
   abort of a stopped visit reports the same stop, so the session cannot be
   removed until the executor's storage is restored.
7. **A contradiction is logged once per process.** The exit sweep reads an
   active visit every few seconds, so the placement logs each distinct
   contradiction once. A restarted executor logs it again.

## Alternatives

- **Refuse a contradicted record everywhere.** This is the simplest rule, and
  nothing is ever done from a record known to be wrong. But stop and cancel
  would refuse too, so the session could never be ended or removed, the
  failure the home catalogue had. Only an administrator editing the file
  would free it.
- **Trust the file.** This was the behaviour before this decision: no
  operation fails because of the record. It is what makes a contradicted
  fence a false proof of a stopped writer, and contradicted output a kept
  checkpoint nobody captured. Both break ADR 0010's rule that an unknown stop
  or usage stays unknown.
- **Trust the registration everywhere.** The registration is in the cluster
  and compare-and-swapped, so it is the stronger record. But it holds none
  of the executor's observations about an active or stopping visit: a quiet
  source, an exit, a turnless visit, a fence before the stop completes. It
  can be the authority only for what its phase proves, which is what rule 2
  takes from it.
- **Repair on load.** Writing the corrected record whenever it is read keeps
  the file clean after one read. But it makes `Inspect` and the exit sweep
  write on every read of a contradicted record. Saving only where the
  operation writes the record anyway, or ends the visit, keeps the reads
  that only observe read-only.

## Consequences

- No record the current code writes is refused. The placement's test suite
  checks every save against the rule, and
  `TestRetainedRecordsMatchTheRecordedTable` records what load, a refusing
  read and save do with every combination of facts beside every registration
  phase.
- `TestEndingOperationsSettleAContradictedRecord` runs `Inspect`, `Exited`,
  `Stop`, `Abort` and `AbortPreparation` on visits in every phase they reach
  whose record has one fact contradicted. Each either settles or fails in a
  way an abort then ends; none reports the contradiction; and on a stopped
  or cancelled registration each replays as the registration says.
- `TestAStoppedVisitWhoseRecordWasLostSettlesFromItsReceipt` loses a
  stopped visit's record and replays its `Prepare`: the visit then settles
  from its receipt, and with the output file lost too it fails until the
  file is restored. `TestStoppedVisitRefusesAnotherCheckpointInPlaceOfItsOutput`
  puts another valid checkpoint of the same thread in place of the output
  file, which every end refuses.
- Not settled by this decision: a record that lost its reference finds its
  registration only by replaying `Create`, which first checks that the
  launch is still admitted. Past the launch's deadline, or once its
  template or policy no longer admit it, `Inspect`, `Stop` and `Abort` of
  such a visit fail, as they did before. Finding the registration by its
  run name instead, as `AbortPreparation` does, would end that.
- A contradicted record keeps a session from starting work until it is
  stopped or cancelled. A home-coordinated session sees only `conflict`;
  the contradiction itself is in the executor's log.
- One reachable behaviour changed outside the contradictions. An abort of the
  preparation of a visit that a cancellation already fenced and sealed, on
  an executor that lost the visit's record, now trusts the receipt's fence.
  It used to report the fence unknown for good once the Pod was gone.
- The stored format and unknown-field handling are unchanged.

## Revisit conditions

- The registration comes to hold the executor's observations of a visit, so
  the file is no longer needed for them.
- A migration step for executor storage, which would let a contradicted
  record be rewritten once instead of corrected on every read.
- Contradictions appear in practice. Each one is a bug, an older binary or a
  hand edit, and its cause may call for a narrower rule.
- The remote executor moves into the cluster, and its record with it.

## Links

- Implementation: `internal/runtime/kubernetes/placement/storage.go`,
  `internal/runtime/kubernetes/placement/lifecycle.go`,
  `internal/runtime/kubernetes/placement/placement.go`,
  `internal/runtime/kubernetes/registry/registry.go` (`CompleteCancel`),
  `internal/runtime/kubernetes/placement/abort_preparation.go`,
  `internal/session/types.go` (`ErrRecordContradicted`),
  `internal/work/driver.go`
- Tests: `internal/runtime/kubernetes/placement/retained_record_test.go`,
  `internal/runtime/kubernetes/placement/ending_test.go`,
  `internal/runtime/kubernetes/placement/contradicted_test.go`,
  `internal/runtime/kubernetes/placement/lost_record_test.go`,
  `internal/runtime/kubernetes/placement/stopped_output_test.go`,
  `internal/work/outcomes_test.go`
- Related: [ADR 0010](0010-managed-session-authority-and-admission.md),
  [ADR 0057](0057-the-remote-executor-owns-its-sessions-through-the-coordinator.md),
  [ADR 0058](0058-lifecycle-records-declare-states-and-transitions.md),
  [ADR 0063](0063-a-session-releases-executions-it-never-recorded.md)
