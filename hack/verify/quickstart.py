#!/usr/bin/env python3
"""Drive the documented Kind paths in an owned checkout and retain their proof."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import threading
import time
from datetime import datetime, timezone
from uuid import uuid4


ROOT = Path(__file__).resolve().parents[2]
EVIDENCE_ROOT = ROOT / ".local/verification/quickstart"
REQUIRED_CRDS = {
    "agentruns.sproozi.com", "agentruntimes.sproozi.com",
    "agentpolicies.sproozi.com", "agenttemplates.sproozi.com",
}
CNI_POLICY_CRDS = {"adminnetworkpolicies.policy.networking.k8s.io",
                   "baselineadminnetworkpolicies.policy.networking.k8s.io"}
TOOLS = ["docker", "kind", "kubectl", "git", "go", "make", "openssl", "curl", "shasum"]
LOCK = EVIDENCE_ROOT / "driver.lock"
# hack/verify/kind.sh names the `make test-e2e` cluster; keep these in step.
DEPENDENCY_CLUSTER_PREFIX = "sproozi-verify-"
DEMO_CLUSTER_PREFIX = "sproozi-quickstart-"
LABELS = {"dependencies": "Dependencies check", "demo": "Demo run"}
HEARTBEAT = 120


class Interrupted(KeyboardInterrupt):
    """A handled termination signal, recorded so result.json says why the run stopped."""

    def __init__(self, name):
        super().__init__(name)
        self.signal_name = name

    def __str__(self):
        return "Interrupted by " + self.signal_name


def say(message):
    # A closed terminal (SIGHUP) must not stop cleanup or the result record.
    try:
        print(message, flush=True)
    except OSError:
        pass


def elapsed(started):
    seconds = int(time.monotonic() - started)
    return f"{seconds // 60}m{seconds % 60:02d}s" if seconds >= 60 else f"{seconds}s"


class phase:
    """Print one line as a step starts and one as it finishes, with its duration."""

    def __init__(self, name):
        self.name = name

    def __enter__(self):
        self.started = time.monotonic()
        say("==> " + self.name)

    def __exit__(self, kind, *_):
        outcome = "done in" if kind is None else \
            "interrupted after" if issubclass(kind, KeyboardInterrupt) else "failed after"
        say(f"    {outcome} {elapsed(self.started)}")


def shown(path):
    """A path relative to the current directory when it is inside it."""
    relative = os.path.relpath(Path(path).resolve())
    return str(Path(path).resolve()) if relative.startswith("..") else relative


def script_command(*arguments):
    """The command a person can paste from their current directory."""
    return " ".join(["python3", shown(__file__), *map(str, arguments)])


def capture(command, cwd=ROOT, env=None):
    """Run a short command; failures become one line naming it and its last error line."""
    name = " ".join(command[:2]) if command[0] != "bash" else "bash"
    try:
        return subprocess.run(command, cwd=cwd, env=env, capture_output=True, text=True,
                              timeout=30, check=True).stdout.strip()
    except subprocess.CalledProcessError as error:
        lines = (error.stderr or "").strip().splitlines()
        raise RuntimeError(f"{name} exited {error.returncode}" + (": " + lines[-1].strip() if lines else "")) \
            from None
    except subprocess.TimeoutExpired:
        raise RuntimeError(f"{name} did not finish within 30s") from None


def read_demo_env(env_file, output, checkout=ROOT, env=None):
    """Print output (a bash printf) after loading env_file through checkout's hack/demo/env.sh.

    demo_env_load checks the file before bash sources it, as every demo script
    does: lines that would run commands, and names such as BASH_ENV or PATH, are
    refused, never run.
    """
    load = 'set -e; source "$1"; demo_env_load "$2"; '
    return capture(["bash", "-c", load + output, "demo-env", str(checkout / "hack/demo/env.sh"), str(env_file)],
                   cwd=checkout, env=env)


def check_demo_settings(env_file):
    """Give the verdict `make demo-check` gives, using hack/demo/check.sh --credentials.

    check.sh rejects lines that would run commands before it sources the file,
    and it never prints values, so its messages are safe to repeat.
    """
    if env_file is None:
        raise RuntimeError("Demo needs --env-file pointing to a configured local environment")
    if not env_file.exists():
        raise RuntimeError(f"{env_file} does not exist")
    if not env_file.is_file() or not os.access(env_file, os.R_OK):
        raise RuntimeError(f"cannot read {env_file}")
    env = dict(os.environ, SPROOZI_DEMO_ENV_FILE=str(env_file))
    try:
        checked = subprocess.run(["bash", str(ROOT / "hack/demo/check.sh"), "--credentials"], cwd=ROOT,
                                 env=env, capture_output=True, text=True, timeout=600)
    except subprocess.TimeoutExpired:
        raise RuntimeError("hack/demo/check.sh --credentials did not finish within 10 minutes") from None
    if checked.returncode:
        raise RuntimeError(checked.stderr.strip() or f"hack/demo/check.sh --credentials exited {checked.returncode}")
    return read_demo_env(env_file, 'printf %s "${MODEL_AUTH_MODE:-api_key}"', env=env)


def doctor(demo=False, env_file=None, passed=lambda line: None):
    """Read-only host checks; do not run setup or provider inference here.

    Every check runs, and passed(line) is called as each one passes. The
    failures are raised together, in check order, as one RuntimeError whose
    message joins them with "; ". A check whose tool is missing is skipped.
    """
    tools = TOOLS + (["gh"] if demo else [])
    missing = [tool for tool in tools if not shutil.which(tool)]
    failures, versions = [], {}

    def check(tool, run):
        if tool in missing:
            return
        try:
            passed(run())
        except (RuntimeError, OSError, subprocess.SubprocessError, ValueError, KeyError) as error:
            failures.append(str(error))

    if missing:
        failures.append("Missing tools: " + ", ".join(missing))
    else:
        passed("tools on PATH: " + ", ".join(tools))

    def docker():
        versions["dockerServer"] = capture(["docker", "info", "--format", "{{.ServerVersion}}"])
        return "Docker server " + versions["dockerServer"]

    def kind():
        versions["kind"] = capture(["kind", "version"])
        return versions["kind"]

    def kubectl():
        versions["kubectl"] = json.loads(capture(["kubectl", "version", "--client", "-o", "json"]))
        # The node image in the documented demo is Kubernetes 1.33.1.
        client = versions["kubectl"]["clientVersion"]
        minor = int(client["minor"].rstrip("+"))
        found = (client.get("gitVersion") or f"{client['major']}.{client['minor']}").lstrip("v")
        if client["major"] != "1" or not 32 <= minor <= 34:
            raise RuntimeError("Use kubectl 1.32, 1.33 or 1.34 for the pinned Kubernetes 1.33.1 demo"
                               f" (found {found}); see \"Prepare your computer\" in"
                               " docs/guides/getting-started.md to install a matching client into .local/bin")
        return "kubectl " + found

    def go():
        versions["go"] = capture(["go", "version"])
        return f"{versions['go']} (go.mod requires {check_go(versions['go'])})"

    def demo_settings():
        versions["modelAuthMode"] = check_demo_settings(env_file)
        return f"demo settings in {shown(env_file)} (model auth {versions['modelAuthMode']}), gh sign-in"

    check("docker", docker)
    check("kind", kind)
    check("kubectl", kubectl)
    check("go", go)
    if demo:
        check("gh", demo_settings)
    if failures:
        raise RuntimeError("\n  ".join(failures))
    return versions


def check_go(output):
    """Require the Go version that go.mod declares and return it; `go version` prints e.g. go1.26.2."""
    declared = re.search(r"^go (\d+)\.(\d+)(?:\.(\d+))?\s*$", (ROOT / "go.mod").read_text(), re.M)
    found = re.search(r"\bgo(\d+)\.(\d+)(?:\.(\d+))?", output)
    if declared is None:
        raise RuntimeError("go.mod does not declare a go version")
    minimum = tuple(int(part or 0) for part in declared.groups())
    if found is None or tuple(int(part or 0) for part in found.groups()) < minimum:
        raise RuntimeError(f"Use Go {'.'.join(map(str, minimum))} or later, as go.mod requires"
                           f" (found {found.group(0) if found else output})")
    return ".".join(map(str, minimum))


def snapshot(destination):
    """Copy the current public sources, including unstaged edits and new guides."""
    names = capture(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"])
    manifest = {}
    for name in sorted(set(names.split("\0")) - {""}):
        source = ROOT / name
        if not source.is_file():
            continue
        if source.is_symlink():
            raise RuntimeError("Verification source snapshots require regular files: " + name)
        target = destination / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)
        manifest[name] = hashlib.sha256(source.read_bytes()).hexdigest()
    capture(["git", "init", "--quiet", str(destination)])
    return manifest


def run_command(command, work, env, log, started_group=lambda process: None):
    """Run one gate in its own process group; started_group(process) records that group.

    The log is a plain file, so the gate is asked not to colour its output."""
    env = dict(env, GINKGO_NO_COLOR="TRUE", NO_COLOR="1")
    with log.open("a") as output:
        output.write("\n$ " + " ".join(command) + "\n")
        output.flush()
        process = subprocess.Popen(command, cwd=work, env=env, stdout=output,
                                   stderr=subprocess.STDOUT, start_new_session=True)
        started = time.monotonic()
        try:
            started_group(process)
            while True:
                try:
                    status = process.wait(timeout=HEARTBEAT)
                    break
                except subprocess.TimeoutExpired:
                    if time.monotonic() - started >= 3600:
                        raise
                    say(f"    still running {' '.join(command[:2])} ({elapsed(started)})")
        except BaseException:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            raise
        if status:
            raise RuntimeError(f"{' '.join(command[:2])} exited {status}")


def observe_crds(kubeconfig, stop, observed):
    """Read discovery while the existing gate installs and removes the CRDs."""
    while not stop.is_set():
        if kubeconfig.is_file():
            try:
                output = subprocess.run(
                    ["kubectl", "--kubeconfig", str(kubeconfig), "get", "crds", "-o", "json",
                     "--request-timeout=3s"], capture_output=True, text=True, timeout=5)
                if output.returncode == 0:
                    for crd in json.loads(output.stdout)["items"]:
                        observed[crd["metadata"]["name"]] = {
                            "group": crd["spec"]["group"], "kind": crd["spec"]["names"]["kind"],
                        }
            except (subprocess.TimeoutExpired, json.JSONDecodeError):
                pass  # The API is not ready yet, or the owned cluster is stopping.
        stop.wait(1)


def check_crds(observed):
    if not REQUIRED_CRDS.issubset(observed):
        raise RuntimeError("Did not observe all four Sproozi CRDs in the owned cluster")
    if not any(crd["group"] == "crd.projectcalico.org" for crd in observed.values()):
        raise RuntimeError("Did not observe the default Calico CRDs")
    unexpected = {name for name, crd in observed.items()
                  if name not in REQUIRED_CRDS | CNI_POLICY_CRDS
                  and crd["group"] != "crd.projectcalico.org"}
    if unexpected:
        raise RuntimeError("Unexpected external CRDs: " + ", ".join(sorted(unexpected)))


def owned_state(directory, action="Cleanup"):
    directory = directory.resolve()
    if directory.parent != EVIDENCE_ROOT.resolve():
        raise RuntimeError(f"{action} requires one quickstart evidence directory under {shown(EVIDENCE_ROOT)}")
    if not directory.exists():
        raise RuntimeError(f"{shown(directory)} does not exist")
    if not (directory / "owner.json").is_file():
        raise RuntimeError(f"{shown(directory)} has no owner.json; it is not a quickstart run")
    try:
        state = json.loads((directory / "owner.json").read_text())
        if not isinstance(state, dict) or not {"path", "cluster"} <= state.keys():
            raise ValueError
    except (OSError, ValueError):
        raise RuntimeError(f"{shown(directory / 'owner.json')} is not a readable quickstart owner record") \
            from None
    run_id = directory.name
    if not re.fullmatch(r"[0-9]{8}t[0-9]{6}z-[a-f0-9]{8}", run_id):
        raise RuntimeError("Invalid quickstart run identity")
    if state["path"] not in {"dependencies", "demo"}:
        raise RuntimeError("Unknown recorded verification path")
    expected = (DEPENDENCY_CLUSTER_PREFIX if state["path"] == "dependencies" else DEMO_CLUSTER_PREFIX) + run_id
    if state["cluster"] != expected:
        raise RuntimeError("Cluster does not match the recorded verification owner")
    return state


def process_start(pid):
    """The process's start time as ps reports it, "" when it has gone, None when unknown."""
    try:
        listed = subprocess.run(["ps", "-o", "lstart=", "-p", str(pid)], capture_output=True,
                                text=True, timeout=10, env=dict(os.environ, LC_ALL="C"))
    except (OSError, subprocess.SubprocessError):
        return None
    return listed.stdout.strip() if listed.returncode in (0, 1) else None


def valid_pid(value):
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


def owner_process(state):
    """Whether the run that wrote owner.json still runs.

    "running", "stopped", "unknown" (no PID recorded) or "unconfirmed" (a
    process with that PID runs, but its start time is not recorded or cannot
    be read). The recorded start time stops a later process that reuses the
    PID from being mistaken for the owner.
    """
    pid = state.get("pid")
    if not valid_pid(pid):
        return "unknown"
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return "stopped"
    except PermissionError:
        pass  # Another user's process with that PID exists; ps decides.
    started = process_start(pid)
    if started == "":
        return "stopped"
    if started is None or not state.get("started"):
        return "unconfirmed"
    return "running" if started == state["started"] else "stopped"


def describe_owner(state):
    return f"PID {state['pid']}" + (f", started {state['started']}" if state.get("started") else "")


def unconfirmed_reason(state):
    why = "its owner.json records no start time" if not state.get("started") else "ps could not read its start time"
    return f"a process with PID {state['pid']} is running, but {why}, so this cannot tell whether it is that run"


def group_processes(pgid):
    """{PID: start time} for each live process in process group pgid, or None when ps cannot list them.

    Zombies are left out: they have already exited, and macOS refuses to signal them.
    """
    try:
        listed = subprocess.run(["ps", "-A", "-o", "pid=,pgid=,stat=,lstart="], capture_output=True, text=True,
                                timeout=10, env=dict(os.environ, LC_ALL="C"))
    except (OSError, subprocess.SubprocessError):
        return None
    if listed.returncode:
        return None
    members = {}
    for line in listed.stdout.splitlines():
        fields = line.split(None, 3)
        if len(fields) == 4 and fields[1] == str(pgid) and fields[0].isdigit() and not fields[2].startswith("Z"):
            members[int(fields[0])] = fields[3].strip()
    return members


def wait_for_group(pgid, seconds):
    """The group's remaining processes after waiting up to seconds for them to exit."""
    deadline = time.monotonic() + seconds
    while True:
        members = group_processes(pgid)
        if members == {} or time.monotonic() >= deadline:
            return members
        time.sleep(0.2)


def stop_run_group(state, force):
    """Stop the gate process group that a crashed run left behind, before its cluster is deleted.

    owner.json records the group's leader PID (which is the group ID) and its
    start time. Only a group whose leader is alive with that start time is
    signalled, so a later group that reuses the ID is never touched.
    """
    group = state.get("group")
    if not isinstance(group, dict) or not valid_pid(group.get("pgid")):
        return
    pgid, recorded = group["pgid"], group.get("started")
    members = group_processes(pgid)
    if members == {}:
        return
    leader = (members or {}).get(pgid)
    if leader is not None and recorded and leader != recorded:
        return  # The ID now belongs to an unrelated group.
    if members is None or leader is None or not recorded:
        why = ("ps could not list its processes" if members is None else
               f"its leader has exited and it cannot confirm that PIDs "
               f"{', '.join(map(str, sorted(members)))} belong to that run" if leader is None else
               "owner.json records no start time for it")
        if force:
            say(f"Did not stop process group {pgid} from the stopped run: {why}")
            return
        raise RuntimeError(f"The stopped run's process group {pgid} may still be running, but {why}. Stop it "
                           f"if it is that run's (kill -TERM -{pgid}), or add --force to clean up without "
                           f"stopping it")
    say(f"Stopping process group {pgid} left by the stopped run (PIDs {', '.join(map(str, sorted(members)))})")
    for number, wait in ((signal.SIGTERM, 10), (signal.SIGKILL, 5)):
        try:
            os.killpg(pgid, number)
        except ProcessLookupError:
            break
        except PermissionError:
            if wait_for_group(pgid, 0) == {}:
                break  # Only zombies were left, which macOS refuses to signal.
            raise RuntimeError(f"Not permitted to stop process group {pgid} from the stopped run") from None
        members = wait_for_group(pgid, wait)
        if members == {}:
            break
        leader = (members or {}).get(pgid)
        if leader is not None and leader != recorded:
            break  # Gone, and the ID reused; never signal the newcomer.
    else:
        raise RuntimeError(f"Process group {pgid} from the stopped run is still running after SIGKILL")
    say(f"Stopped process group {pgid} ({signal.Signals(number).name})")


def lock_owner():
    """The run ID recorded in driver.lock, or None when it records none."""
    try:
        return (LOCK / "owner").read_text().strip() or None
    except OSError:
        return None


def cleanup(directory, force=False, from_run=False):
    """Remove one run's cluster and scratch checkout; from_run is the owning run cleaning up after itself."""
    state = owned_state(directory)
    directory = directory.resolve()
    if not from_run and not force:
        status = owner_process(state)
        if status == "running":
            raise RuntimeError(f"Refusing to clean up {shown(directory)}: its quickstart run is still running "
                               f"({describe_owner(state)}). Stop that run (it cleans up as it stops), or "
                               f"add --force to delete its cluster anyway")
        if status == "unknown":
            raise RuntimeError(f"Refusing to clean up {shown(directory)}: its owner.json records no process, so "
                               f"it cannot tell whether that run is still going. If it has stopped, add --force")
        if status == "unconfirmed":
            raise RuntimeError(f"Refusing to clean up {shown(directory)}: {unconfirmed_reason(state)}. If that "
                               f"run has stopped, add --force")
    if not from_run and owner_process(state) == "stopped":
        stop_run_group(state, force)
    cluster = state["cluster"]
    if cluster in capture(["kind", "get", "clusters"]).splitlines():
        try:
            subprocess.run(["kind", "delete", "cluster", "--name", cluster], check=True, timeout=120,
                           capture_output=True)
        except subprocess.CalledProcessError as error:
            raise RuntimeError(f"kind delete cluster {cluster} exited {error.returncode}") from None
        except subprocess.TimeoutExpired:
            raise RuntimeError(f"kind delete cluster {cluster} did not finish within 120s") from None
        say("Deleted Kind cluster " + cluster)
    elif from_run:
        # The gate deletes its own cluster, so this is the normal end of a finished run.
        say("Kind cluster " + cluster + " removed")
    else:
        say("Kind cluster " + cluster + " was already removed")
    if cluster in capture(["kind", "get", "clusters"]).splitlines():
        raise RuntimeError("Owned cluster remains after cleanup")
    work, problem = directory / "checkout", None
    if work.is_symlink():
        # Following the link would delete files outside the evidence directory.
        link = os.path.join(shown(directory), work.name)  # shown() would follow the link.
        problem = (f"{link} is a symbolic link, not a scratch checkout, so cleanup left it and its "
                   f"target alone; remove the link with: rm {link}")
    elif work.is_dir():
        try:
            shutil.rmtree(work)
        except OSError as error:
            raise RuntimeError(f"Could not remove scratch checkout {shown(work)}: "
                               f"{error.strerror or error}") from None
        say("Removed scratch checkout " + shown(work))
    if lock_owner() == directory.name:
        (LOCK / "owner").unlink()
        LOCK.rmdir()
        say("Released " + shown(LOCK))
    result_file = directory / "result.json"
    if result_file.is_symlink():
        problem = problem or f"{shown(result_file)} is a symbolic link, so cleanup did not update it"
    elif result_file.is_file():
        try:
            result = json.loads(result_file.read_text())
        except ValueError:
            result = None
        if not isinstance(result, dict):
            raise RuntimeError(f"{shown(result_file)} is not a quickstart result (a JSON object); the cluster "
                               f"and checkout are removed")
        result["clusterRemoved"] = True
        result["scratchRemoved"] = not work.exists()
        result_file.write_text(json.dumps(result, indent=2) + "\n")
    if problem:
        raise RuntimeError(problem)
    say("Kept evidence in " + shown(directory))


def acquire_lock(run_id):
    """Take driver.lock, or name the run that holds it and whether it is safe to release."""
    EVIDENCE_ROOT.mkdir(parents=True, exist_ok=True)
    try:
        LOCK.mkdir()
    except FileExistsError:
        recorded = lock_owner()
        owner = EVIDENCE_ROOT / recorded if recorded else None
        try:
            state = owned_state(owner) if owner else None
        except RuntimeError:
            state = None
        if state is None:
            raise RuntimeError(f"Another quickstart owns {shown(LOCK)} but no run is recorded; if no "
                               f"quickstart is running, remove it with: rm -r {shown(LOCK)}") from None
        status = owner_process(state)
        if status == "running":
            raise RuntimeError(f"Another quickstart run is still running ({describe_owner(state)}) and owns "
                               f"{shown(LOCK)}: {shown(owner)}\nWait for it to finish, or stop it; it removes "
                               f"its cluster and releases the lock as it stops") from None
        if status == "unconfirmed":
            raise RuntimeError(f"An earlier quickstart run owns {shown(LOCK)}: {shown(owner)}\nIts PID is in "
                               f"use: {unconfirmed_reason(state)}. If that run has stopped, release it with: "
                               f"{script_command('cleanup', '--force', shown(owner))}") from None
        if status == "stopped":
            raise RuntimeError(f"An earlier quickstart run owns {shown(LOCK)} but its process has exited "
                               f"({describe_owner(state)}): {shown(owner)}\nRelease it with: "
                               f"{script_command('cleanup', shown(owner))}") from None
        raise RuntimeError(f"An earlier quickstart run owns {shown(LOCK)}: {shown(owner)}\nIts owner.json "
                           f"records no process, so this cannot tell whether it is still running. If it is "
                           f"not, release it with: {script_command('cleanup', '--force', shown(owner))}") from None
    (LOCK / "owner").write_text(run_id + "\n")


def write_owner(directory, state):
    """Replace owner.json in one step, so a crash never leaves half a record."""
    partial = directory / "owner.json.partial"
    partial.write_text(json.dumps(state) + "\n")
    os.replace(partial, directory / "owner.json")


def release_lock(run_id):
    if lock_owner() == run_id:
        (LOCK / "owner").unlink()
        LOCK.rmdir()


def verify(args):
    label, started = LABELS[args.path], time.monotonic()
    try:
        with phase("Checking host tools"):
            versions = doctor(args.path == "demo", args.env_file)
        os.umask(0o077)
        run_id = datetime.now(timezone.utc).strftime("%Y%m%dt%H%M%Sz") + "-" + uuid4().hex[:8]
        directory = EVIDENCE_ROOT / run_id
        cluster = (DEPENDENCY_CLUSTER_PREFIX if args.path == "dependencies" else DEMO_CLUSTER_PREFIX) + run_id
        source_commit = capture(["git", "rev-parse", "HEAD"])
        guide = hashlib.sha256((ROOT / "docs/guides/getting-started.md").read_bytes()).hexdigest()
        if cluster in capture(["kind", "get", "clusters"]).splitlines():
            raise RuntimeError("Refusing to reuse an existing cluster")
        acquire_lock(run_id)
        try:
            directory.mkdir()
            # The PID and its start time let another run tell whether this one is still going.
            state = {"cluster": cluster, "path": args.path, "pid": os.getpid(),
                     "started": process_start(os.getpid())}
            write_owner(directory, state)
        except BaseException:
            release_lock(run_id)
            raise
    except (RuntimeError, OSError, subprocess.SubprocessError) as error:
        raise RuntimeError(f"{label} failed: {error}") from error
    work, log = directory / "checkout", directory / "commands.log"
    result = {"path": args.path, "outcome": "failed", "toolVersions": versions,
              "cluster": cluster, "sourceCommit": source_commit, "guideSHA256": guide,
              "features": {"dependencies": "not_run", "demo": "not_run", "exploration": "not_run"},
              "fullDemo": "not_run", "clusterRemoved": False, "scratchRemoved": False}
    stop, observed = threading.Event(), {}
    watcher = None
    def record_group(process):
        # cleanup stops this group if the run dies without stopping it (kill -9).
        state["group"] = {"pgid": process.pid, "started": process_start(process.pid)}
        write_owner(directory, state)
    try:
        say(f"Quickstart {args.path}: {shown(directory)}")
        say(f"Command output goes to {shown(log)}; follow it with: tail -f {shown(log)}")
        with phase("Copying the current checkout"):
            manifest = snapshot(work)
        (directory / "source-files.json").write_text(json.dumps(manifest, indent=2) + "\n")
        env = os.environ.copy()
        # Use only this run's context and evidence. Never inherit a developer cluster.
        env.update(SPROOZI_PRESERVE_ON_FAILURE="0", SPROOZI_VERIFY_ID=run_id,
                   SPROOZI_E2E_METRICS="0",
                   SPROOZI_LIVE_REPORT=str(directory / "live.json"))
        local = work / ".local"
        local.mkdir()
        env_file = local / "demo.env"
        if args.path == "demo":
            shutil.copyfile(args.env_file, env_file)
        else:
            for key in ("SPROOZI_CNI_MANIFEST", "SPROOZI_CNI_SELECTOR"):
                env.pop(key, None)
        env["SPROOZI_DEMO_ENV_FILE"] = str(env_file)
        if args.path == "dependencies":
            env["SPROOZI_E2E_ORCHESTRATION"] = "0"
            with phase("Configuring Kind networking (hack/demo/setup.sh --network-only)"):
                run_command(["bash", "hack/demo/setup.sh", "--network-only"], work, env, log, record_group)
            config = read_demo_env(env_file, 'printf "%s\\n%s" "$SPROOZI_CNI_MANIFEST" "$SPROOZI_CNI_SELECTOR"',
                                   work, env).splitlines()
            env.update(SPROOZI_CNI_MANIFEST=config[0], SPROOZI_CNI_SELECTOR=config[1])
            kubeconfig = directory / "cluster.kubeconfig"
            env.update(SPROOZI_KUBECONFIG=str(kubeconfig), KUBECONFIG=str(kubeconfig))
            command = ["make", "test-e2e"]
        else:
            kubeconfig = local / "demo" / (cluster + ".kubeconfig")
            command = ["make", "demo", "IMG=controller:sproozi-quickstart-" + run_id,
                       "KIND_CLUSTER=" + cluster]
        watcher = threading.Thread(target=observe_crds, args=(kubeconfig, stop, observed))
        watcher.start()
        with phase(f"Running {' '.join(command[:2])} in Kind cluster {cluster}; this takes several minutes"):
            run_command(command, work, env, log, record_group)
        stop.set()
        watcher.join()
        check_crds(observed)
        if args.path == "demo":
            live = json.loads((directory / "live.json").read_text())
            if live["outcome"] != "passed" or not all(live[key] for key in
                    ("observedSandbox", "denialProbesVerified", "cleanupVerified", "prURL", "prCommit")):
                raise RuntimeError("The live report did not prove the documented demo")
            result.update(fullDemo="passed", prURL=live["prURL"])
            result["features"]["demo"] = "passed"
            shutil.copyfile(kubeconfig, directory / "cluster.kubeconfig")
        result.update(outcome="passed", requiredCRDsObserved=True, externalSandboxCRDsRequired=False)
        result["features"]["dependencies"] = "passed"
    except (RuntimeError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as error:
        result["error"] = str(error) or "Interrupted"
        if isinstance(error, KeyboardInterrupt):
            result["interrupted"] = getattr(error, "signal_name", "SIGINT")
    finally:
        hold_signals()
        stop.set()
        if watcher is not None:
            watcher.join()
        (directory / "crds-observed.json").write_text(json.dumps(observed, indent=2) + "\n")
        (directory / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        if args.keep_cluster and result["outcome"] == "passed":
            say("Owned cluster retained for exploration; when finished run: "
                + script_command("cleanup", shown(directory)))
        else:
            try:
                with phase("Removing the owned cluster and scratch checkout"):
                    cleanup(directory, from_run=True)
            except (RuntimeError, OSError, subprocess.SubprocessError) as error:
                result.update(outcome="failed", cleanupError=str(error))
                (directory / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    say("Result: " + shown(directory / "result.json"))
    if result["outcome"] == "passed":
        say(f"{label} passed ({elapsed(started)})")
        return 0
    error = result.get("error") or "cleanup failed: " + result.get("cleanupError", "unknown error")
    say(f"{label} failed after {elapsed(started)}: {error}")
    reasons = log_errors(log)
    if reasons:
        say(f"Last errors in {shown(log)}:")
        for line in reasons:
            say("    " + line)
    elif log.is_file():
        say("Full output: " + shown(log))
    return 1


ERROR_LINE = re.compile(r"error|fail|fatal|panic|denied|refused|cannot|unable|not found|timed out|"
                        r"no such|invalid|missing|forbidden|exited", re.I)
NOISE_LINE = re.compile(r"^(\$ |make(\[\d+\])?: \*\*\*|---|ok\s|PASS|=== RUN)")
ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")


def log_errors(log, count=5):
    """The last few lines of commands.log that look like the reason a gate failed."""
    try:
        lines = log.read_text(errors="replace").splitlines()[-400:]
    except OSError:
        return []
    lines = [ANSI.sub("", line).rstrip() for line in lines]
    lines = [line for line in lines if line.strip() and not NOISE_LINE.match(line.strip())]
    errors = [line for line in lines if ERROR_LINE.search(line)]
    return [line[:300] for line in (errors or lines)[-count:]]


def check_evidence(directory):
    """Check that a retained run's kubeconfig still reaches its own cluster."""
    state = owned_state(directory, "Doctor --evidence")
    config = directory / "cluster.kubeconfig"
    if not config.is_file():
        raise RuntimeError(f"{shown(directory)} has no cluster.kubeconfig; only a retained cluster has one")
    context = capture(["kubectl", "--kubeconfig", str(config), "config", "current-context"])
    if context != "kind-" + state["cluster"]:
        raise RuntimeError("Kubeconfig context does not match the owned cluster")
    try:
        capture(["kubectl", "--kubeconfig", str(config), "get", "nodes", "--request-timeout=5s"])
    except RuntimeError as error:
        raise RuntimeError(f"Kind cluster {state['cluster']} is not reachable: {error}") from None
    return {"cluster": state["cluster"], "context": context, "apiReachable": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="path", required=True)
    check = subparsers.add_parser("doctor", help="Read-only host and optional demo credential checks")
    check.add_argument("--env-file", type=lambda value: Path(value).resolve())
    check.add_argument("--evidence", type=lambda value: Path(value).resolve())
    check.add_argument("--json", action="store_true", help="Print the checked versions as JSON")
    subparsers.add_parser("dependencies", help="Prove the provider-free Kind dependency path")
    demo = subparsers.add_parser("demo", help="Run the documented real AgentRun-to-PR demo")
    demo.add_argument("--env-file", required=True, type=lambda value: Path(value).resolve())
    demo.add_argument("--keep-cluster", action="store_true", help="Retain a successful owned cluster for exploration")
    remove = subparsers.add_parser("cleanup", help="Remove only the recorded cluster and scratch checkout")
    remove.add_argument("evidence", type=lambda value: Path(value).resolve())
    remove.add_argument("--force", action="store_true",
                        help="Clean up even when the run that owns it still appears to be running")
    args = parser.parse_args()
    if args.path == "doctor":
        passed = []
        def report(line):
            passed.append(line)
            if not args.json:
                print("  ok  " + line, flush=True)
        try:
            health = doctor(args.env_file is not None, args.env_file, report)
            if args.evidence:
                health.update(check_evidence(args.evidence))
                report(f"Kind cluster {health['cluster']} reachable through context {health['context']}")
        except (RuntimeError, OSError, subprocess.SubprocessError, ValueError, KeyError) as error:
            if not args.json:
                # One failure stays on the header line; several go indented below it.
                separator = "\n  " if "\n" in str(error) else " "
                raise RuntimeError("Doctor check failed:" + separator + str(error)) from error
            print(json.dumps({"ok": False, "error": str(error), "passed": passed}, indent=2))
            return 1
        if args.json:
            print(json.dumps({"ok": True, **health, "passed": passed}, indent=2))
            return 0
        checked = "host tools and versions" + (", demo credentials" if args.env_file else "") + \
            (", owned cluster " + health["cluster"] if args.evidence else "")
        print("Doctor checks passed: " + checked)
        return 0
    if args.path == "cleanup":
        cleanup(args.evidence, force=args.force)
        return 0
    if args.path == "dependencies":
        args.env_file, args.keep_cluster = None, False
    return verify(args)


def hold_signals():
    """Let cleanup and the result record finish once a run starts to stop."""
    for handled in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(handled, signal.SIG_IGN)


def interrupted(number, _):
    hold_signals()
    raise Interrupted(signal.Signals(number).name)


if __name__ == "__main__":
    for handled in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(handled, interrupted)
    try:
        raise SystemExit(main())
    except (RuntimeError, OSError, subprocess.SubprocessError) as error:
        raise SystemExit(str(error)) from error
    except KeyboardInterrupt as error:
        raise SystemExit(str(error) or "Interrupted") from error
