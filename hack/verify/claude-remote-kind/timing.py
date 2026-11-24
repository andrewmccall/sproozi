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


def run_timing(agent_runs, uid):
    """The run's admission and completion times and the whole seconds between
    them, from the AgentRun list's JSON, for the run with this UID. Empty if
    there is no such run or it records neither time."""
    for run in agent_runs.get("items", []):
        if run.get("metadata", {}).get("uid") != uid:
            continue
        status = run.get("status", {})
        if not status.get("startedAt") or not status.get("completedAt"):
            return {}
        return {"runAdmittedAt": status["startedAt"], "runCompletedAt": status["completedAt"],
                "runSeconds": round(parse_time(status["completedAt"]) - parse_time(status["startedAt"]))}
    return {}
