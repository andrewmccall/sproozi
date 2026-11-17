"""A task's timing from timestamps the system recorded, so that no polling
interval is in it. Pure functions: verify.py gathers the inputs and records
the result beside its own polled figure, `seconds`.

Two recorded sources, neither needing product code:

* The fixture provider stamps each request with the cluster's clock to the
  millisecond (provider.py), and the executor sets a task's deadline when it
  accepts the submission, as that moment plus the profile's maxDuration, to the
  nanosecond. Their difference is the time from submission to the first model
  request. The deadline is a derived start, not a recorded one, so the figure
  is kept only if it is plausible (see server_timing).
* The AgentRun's status.startedAt and status.completedAt, which the controller
  records when it admits the run and when the run reaches a terminal phase.
  They are whole seconds, so runSeconds is good to about a second.
"""
from datetime import datetime, timezone
import re
import time

DURATION = re.compile(r"(\d+)([hms])")
UNITS = {"h": 3600, "m": 60, "s": 1}


def parse_time(timestamp):
    """Seconds since the epoch of an RFC 3339 timestamp, with any number of
    fractional digits."""
    return datetime.fromisoformat(timestamp.replace("Z", "+00:00")).timestamp()


def iso(epoch):
    return datetime.fromtimestamp(epoch, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def parse_duration(text):
    """Seconds in a duration such as 30m, 1h30m or 90s."""
    if not text or "".join(m.group(0) for m in DURATION.finditer(text)) != text:
        raise ValueError("Unsupported duration " + repr(text))
    return sum(int(n) * UNITS[unit] for n, unit in DURATION.findall(text))


def server_timing(deadline, max_duration, first_request_at, waited):
    """The seconds from the executor accepting a task to the provider's first
    request for it, from the deadline the executor set (accepted + max_duration
    seconds) and the provider's stamp (epoch seconds). Both come from clocks in
    the same cluster. waited is the monotonic time from just before the CLI
    started the task to the poll that saw it retained: the first request cannot
    come before the submission or after the task was seen retained, and a
    figure outside that means the deadline was not set from max_duration, so it
    is not reported."""
    submitted = parse_time(deadline) - max_duration
    seconds = first_request_at - submitted
    if not 0 <= seconds <= waited + 1:
        return {"serverTimingNote": "No submission-to-first-request figure: it came to %.1fs against a %.1fs wait, so the "
                                    "deadline was not the submission time plus the profile's maxDuration" % (seconds, waited)}
    return {"submittedAt": iso(submitted), "firstModelRequestAt": iso(first_request_at),
            "secondsToFirstModelRequest": round(seconds, 3)}


def server_timing_from(started, request, max_duration, waited):
    """server_timing from the task's run output, the provider's first request
    for it and the profile cap (seconds, or a string saying why it could not be
    read). A figure that cannot be had comes back as serverTimingNote saying
    which input was missing."""
    note = "No submission-to-first-request figure: "
    if isinstance(max_duration, str):
        return {"serverTimingNote": note + max_duration}
    if not started.get("deadline"):
        return {"serverTimingNote": note + "the run output has no deadline"}
    if not request or "at" not in request:
        return {"serverTimingNote": note + "the provider recorded no first request time (at) for the task"}
    return server_timing(started["deadline"], max_duration, request["at"], waited)


# How long to wait for the controller to record completedAt. It sees the
# sandbox end after the executor has already reported the task retained: about
# 2 s later in the run that showed it (controller log 21:32:49, retained seen
# about 21:32:47).
RUN_COMPLETION_WAIT_SECONDS = 30


def run_timing(agent_runs, uid):
    """The run's admission and completion times and the whole seconds between
    them, from the AgentRun list's JSON, for the run with this UID. Returns
    (fields, complete): fields hold the figure, or runTimingNote saying what
    was missing; complete is False only when the run exists without
    status.completedAt, which may yet be set."""
    items = agent_runs.get("items", [])
    for run in items:
        if run.get("metadata", {}).get("uid") != uid:
            continue
        status = run.get("status", {})
        if not status.get("startedAt"):
            return {"runTimingNote": "The AgentRun was found without status.startedAt"}, True
        if not status.get("completedAt"):
            return {"runTimingNote": "The AgentRun was found without status.completedAt"}, False
        return {"runAdmittedAt": status["startedAt"], "runCompletedAt": status["completedAt"],
                "runSeconds": round(parse_time(status["completedAt"]) - parse_time(status["startedAt"]))}, True
    return {"runTimingNote": "No AgentRun has UID %s among the %d listed" % (uid, len(items))}, True


def await_run_timing(read, uid, limit=RUN_COMPLETION_WAIT_SECONDS, clock=None, sleep=None):
    """run_timing, read again until status.completedAt is set or limit seconds
    pass. read returns the AgentRun list's JSON. A failed read, or any other
    absence, is final and noted."""
    clock, sleep = clock or time.monotonic, sleep or time.sleep
    end = clock() + limit
    while True:
        try:
            fields, complete = run_timing(read(), uid)
        except Exception as error:  # the list call or its output failed
            return {"runTimingNote": "The AgentRun list call failed: %s: %s" % (type(error).__name__, error)}
        if complete or clock() >= end:
            if not complete:
                fields["runTimingNote"] += " within %ds" % limit
            return fields
        sleep(1)
