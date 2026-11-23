"""Resident Claude session discovery for one workbench (Linux guests).

The workbench starts this once through the guest's login shell, like the
channel helper, and keeps it while clients poll. Framing follows
guest-session.py: "R <id> <length>\n" + JSON {"mode", "input"}, "C <id>\n"
ends a waiting request, and each reply is "D <id> <exit> <stdout length>
<stderr length>\n" + both. The channel helper's own discover() and view()
produce every answer; this process only remembers what it already read, and
adds the subagents and background tasks of live sessions (view_tasks).
It serves read paths only: handoff, purge and launch need a fresh registry
and stay with the channel helper. A list's Codex discovery runs the Codex
helper's own discover() here as well.
"""
import calendar
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sys
import threading
import time

# claude agents --json is reused while the CLI's session registry shows the
# same processes, and at most this long, in case a change bypasses it.
AGENTS_MAX_AGE = 30.0
WATCH_INTERVAL = 1.0
VIEW_INTERVAL = 0.25
WAIT_LIMIT = 25.0
# After its lifetime (seconds, from the workbench) the helper answers 4 without
# reading a request, and the workbench starts a successor through a new login
# shell, so changes to the login environment apply.
RETIRED = 4
# The Codex helper is not installed yet; the workbench uses its own bootstrap.
MISSING = 5
# The request itself failed (a transcript that vanished mid-read, a refused
# account, claude agents failing): the workbench answers this one request with
# the channel helper and keeps using this process. Malformed framing or an
# unknown operation still answers 1, which retires it.
REFUSED = 6
MODES = ("codex-discover", "discover", "watch", "view", "view-wait")
OUTPUT_LIMIT = 1 << 20
REQUEST_LIMIT = 1 << 20
SUMMARY_LIMIT = 20000

channel = None
codex = None
codex_name = ""
codex_lock = threading.Lock()
summaries = {}
# The subagent files each session's latest view read, so a held read wakes
# when a running child writes; sessions without running children add nothing.
children = {}
agents = {}
agent_locks = {}
states = {}
lock = threading.Lock()
local = threading.local()


# Subagents and background tasks, for display only, added here rather than in
# the channel helper, which must fit a Windows command line. Claude Code records
# a launch as an Agent or Bash tool call, its acceptance in the row's
# toolUseResult (agentId, backgroundTaskId), its end as a <task-notification>
# naming the launching tool call, and each subagent in
# <session>/subagents/agent-<id>.jsonl beside agent-<id>.meta.json (toolUseId,
# finished). The channel helper's answer is unchanged when a session has none.
AGENT_TOOLS = ("Agent", "Task")
STOP_TOOLS = ("TaskStop", "KillShell", "KillBash")
WAIT_TOOLS = ("Poll",)
TASK_STATES = {"completed": "completed", "failed": "failed", "killed": "stopped", "stopped": "stopped", "cancelled": "stopped"}
TASK_ID = re.compile(r"[A-Za-z0-9_.:-]{1,128}")
TASK_LIMIT = 256
CHILD_LIMIT = 16
CHILD_TAIL = 256 << 10
SCAN_TAIL = 8 << 20
DETAIL_FIELDS = ("description", "subject", "file_path", "pattern", "query", "skill", "subagent_type")
# Per transcript: where the previous read stopped and the task index so far;
# per subagent file: its tool calls so far. Both only grow by appended bytes.
task_scans = {}
scan_lock = threading.Lock()
child_scans = {}
child_lock = threading.Lock()


def tool_detail(args):
    for field in DETAIL_FIELDS:
        value = args.get(field) if isinstance(args, dict) else None
        if isinstance(value, str) and value.strip():
            return value.strip()[:160]
    return ""


def row_time(row):
    value = row.get("timestamp")
    if not isinstance(value, str) or len(value) < 19:
        return None
    try:
        seconds = calendar.timegm(time.strptime(value[:19], "%Y-%m-%dT%H:%M:%S"))
    except ValueError:
        return None
    fraction = re.match(r"\.(\d{1,6})", value[19:])
    return round(seconds + (float("0." + fraction.group(1)) if fraction else 0.0), 3)


def task_notice(text):
    if not text.lstrip().startswith("<task-notification>"):
        return None

    def field(name):
        match = re.search("<" + name + r">\s*([^<]{1,256}?)\s*</" + name + ">", text)
        return match.group(1) if match else ""
    return field("tool-use-id"), field("task-id"), field("status")


class Tasks:
    """The subagents and background tasks of one transcript, by launching tool call."""

    def __init__(self):
        self.entries, self.by_task, self.pending, self.stops, self.answered = {}, {}, {}, {}, set()

    def copy(self):
        # What one answer may change (an end found in a meta file, a now line).
        other = Tasks()
        other.entries = {key: dict(entry) for key, entry in self.entries.items()}
        return other

    def feed(self, row):
        if row.get("isSidechain"):
            return
        message = row.get("message") if isinstance(row.get("message"), dict) else {}
        content = message.get("content")
        when = row_time(row)
        if row.get("type") == "assistant" and isinstance(content, list):
            for block in content:
                if isinstance(block, dict) and block.get("type") == "tool_use":
                    self.launch(block, when)
        elif row.get("type") == "user":
            notice = task_notice(channel.text_content(message)) if isinstance(content, (str, list)) else None
            if notice:
                self.notice(*notice, when)
            elif isinstance(content, list):
                results = [block for block in content if isinstance(block, dict) and block.get("type") == "tool_result"]
                outcome = row.get("toolUseResult") if len(results) == 1 and isinstance(row.get("toolUseResult"), dict) else {}
                for block in results:
                    self.result(block, outcome, when)

    def add(self, key, entry, when):
        if key in self.entries:
            return
        if when:
            entry["started_at"] = when
        self.entries[key] = entry
        if len(self.entries) > TASK_LIMIT:
            ended = next((name for name, item in self.entries.items() if item["state"] != "running"), None)
            self.entries.pop(ended or next(iter(self.entries)))

    def launch(self, block, when):
        key, name = block.get("id"), block.get("name")
        args = block.get("input") if isinstance(block.get("input"), dict) else {}
        if not isinstance(key, str) or not 0 < len(key.encode()) <= 128:
            return
        detail = tool_detail(args)
        if name in AGENT_TOOLS:
            entry = {"name": name, "kind": "background_agent" if args.get("run_in_background") is True else "subagent", "state": "running"}
            if isinstance(args.get("subagent_type"), str) and args["subagent_type"].strip():
                entry["agent_type"] = args["subagent_type"].strip()[:64]
        elif name == "Bash" and args.get("run_in_background") is True:
            entry = {"name": name, "kind": "background_command", "state": "running"}
        elif name == "Bash":
            # A call can still move to the background when its result arrives.
            self.pending[key] = (when, detail)
            while len(self.pending) > 64:
                self.pending.pop(next(iter(self.pending)))
            return
        elif name in STOP_TOOLS:
            self.stops[key] = next((args[field] for field in ("task_id", "shell_id", "bash_id") if isinstance(args.get(field), str)), "")
            while len(self.stops) > 64:
                self.stops.pop(next(iter(self.stops)))
            return
        else:
            return
        if detail:
            entry["detail"] = detail
        self.add(key, entry, when)

    def track(self, key, task):
        if isinstance(task, str) and TASK_ID.fullmatch(task) and key in self.entries:
            self.entries[key]["_task"] = task
            self.by_task[task] = key

    def result(self, block, outcome, when):
        key, failed = block.get("tool_use_id"), bool(block.get("is_error"))
        if not isinstance(key, str):
            return
        if key in self.pending:
            started, detail = self.pending.pop(key)
            task = outcome.get("backgroundTaskId")
            if not failed and isinstance(task, str) and TASK_ID.fullmatch(task):
                self.add(key, {"name": "Bash", "kind": "background_command", "state": "running", **({"detail": detail} if detail else {})}, started)
                self.track(key, task)
            return
        if key in self.stops:
            target = outcome.get("task_id") if isinstance(outcome.get("task_id"), str) else self.stops[key]
            self.stops.pop(key)
            if not failed:
                self.end(self.by_task.get(target), "stopped", when)
            return
        entry = self.entries.get(key)
        if entry is None or key in self.answered:
            return
        self.answered.add(key)
        if entry["kind"] == "background_command":
            if failed:
                self.end(key, "failed", when)
                return
            task = outcome.get("backgroundTaskId")
            if not isinstance(task, str):
                text = block.get("content") if isinstance(block.get("content"), str) else channel.text_content({"content": block.get("content")})
                match = re.search(r"running in background with ID: ([A-Za-z0-9_.:-]{1,128})", text or "")
                task = match.group(1) if match else None
            self.track(key, task)
            return
        self.track(key, outcome.get("agentId"))
        if isinstance(outcome.get("agentId"), str) and TASK_ID.fullmatch(outcome["agentId"]):
            entry["_agent"] = outcome["agentId"]
        if (outcome.get("status") == "async_launched" or outcome.get("isAsync") is True) and not failed:
            return
        self.end(key, "failed" if failed else "completed", when)

    def notice(self, tool, task, status, when):
        key = tool if tool in self.entries else self.by_task.get(task)
        if key in self.entries and status in TASK_STATES:
            # An agent notifies each time it stops; the latest notice holds.
            self.end(key, TASK_STATES[status], when, True)

    def end(self, key, state, when, again=False):
        entry = self.entries.get(key)
        if entry is None or (entry["state"] != "running" and not again):
            return
        entry["state"] = state
        if when:
            entry["ended_at"] = when
        else:
            entry.pop("ended_at", None)

    def running(self):
        return [(key, entry) for key, entry in self.entries.items() if entry["state"] == "running"]


def public(entry):
    return {key: value for key, value in entry.items() if not key.startswith("_")}


def process_since(config, session_id, running):
    """Whether a CLI process of the session is live, and since when when known."""
    since, live_seen = None, False
    try:
        names = sorted(entry.name for entry in os.scandir(Path(config) / "sessions") if entry.name.endswith(".json"))[:64]
    except OSError:
        names = []
    for name in names:
        try:
            with (Path(config) / "sessions" / name).open("rb") as stream:
                value = json.loads(stream.read(64 << 10))
        except (OSError, ValueError):
            continue
        if not isinstance(value, dict) or value.get("sessionId") != session_id or not channel.live(value.get("pid")):
            continue
        live_seen = True
        started = value.get("startedAt")
        if isinstance(started, (int, float)) and started > 0:
            started = started / 1000 if started > 1e11 else float(started)
            since = started if since is None else max(since, started)
    return (True, since) if live_seen else (bool(running), None)


def settle_tasks(tasks, alive, since):
    # A child without an end lives no longer than the process that started it.
    for _, entry in tasks.running():
        started = entry.get("started_at")
        if not alive or (since is not None and started is not None and started < since - 2):
            entry["state"] = "stopped"


def child_activity(path):
    """The tool calls of a subagent file: count, whether counted from a tail only, and the latest two."""
    with child_lock:
        return read_activity(path)


def read_activity(path):
    info = os.stat(path)
    identity = (info.st_dev, info.st_ino)
    cached = child_scans.get(str(path))
    if cached is None or cached[0] != identity or cached[1] > info.st_size or info.st_size - cached[1] > CHILD_TAIL:
        start = max(0, info.st_size - CHILD_TAIL)
        cached = (identity, start, 0, start > 0, [], start == 0)
    _, offset, calls, partial, recent, aligned = cached
    if offset < info.st_size:
        with open(path, "rb") as stream:
            stream.seek(offset)
            data = stream.read(info.st_size - offset)
        if not aligned:
            cut = data.find(b"\n") + 1
            data, offset = data[cut:], offset + cut
        end = data.rfind(b"\n") + 1
        for line in data[:end].split(b"\n"):
            if b'"tool_use"' not in line or b'"assistant"' not in line or b'"api-request' in line:
                continue
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if row.get("type") != "assistant" or not isinstance(row.get("message"), dict) or not isinstance(row["message"].get("content"), list):
                continue
            for block in row["message"]["content"]:
                if isinstance(block, dict) and block.get("type") == "tool_use" and isinstance(block.get("name"), str) and re.fullmatch(r"[a-zA-Z0-9_.:-]{1,128}", block["name"]):
                    calls += 1
                    recent = (recent + [(block["name"], tool_detail(block.get("input")))])[-2:]
        offset += end
    if len(child_scans) > 256:
        child_scans.clear()
    child_scans[str(path)] = (identity, offset, calls, partial, recent, True)
    shown = recent[-2] if len(recent) == 2 and recent[-1][0] in WAIT_TOOLS else recent[-1] if recent else None
    activity = {"calls": calls}
    if partial:
        activity["calls_partial"] = True
    if shown:
        activity["now"] = {"name": shown[0], **({"detail": shown[1]} if shown[1] else {})}
    return activity, info.st_mtime


def read_children(tasks, folder, watch, activity=True):
    """Reads the files of running subagents, newest first and at most CHILD_LIMIT; returns their latest mtime."""
    agents = sorted((item for item in tasks.running() if item[1]["kind"] != "background_command"), key=lambda item: item[1].get("started_at") or 0, reverse=True)[:CHILD_LIMIT]
    if not agents or not folder.is_dir() or folder.is_symlink():
        return 0
    if any("_agent" not in entry for _, entry in agents):
        # A subagent that runs in the foreground names itself only in its meta file.
        known = {}
        for meta in sorted(folder.glob("agent-*.meta.json"))[:256]:
            try:
                with meta.open("rb") as stream:
                    value = json.loads(stream.read(8 << 10))
            except (OSError, ValueError):
                continue
            if isinstance(value, dict) and isinstance(value.get("toolUseId"), str):
                known[value["toolUseId"]] = meta.name[len("agent-"):-len(".meta.json")]
        for key, entry in agents:
            if "_agent" not in entry and TASK_ID.fullmatch(known.get(key, "")):
                entry["_agent"] = known[key]
    latest = 0
    for key, entry in agents:
        agent = entry.get("_agent")
        if not agent or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", agent):
            continue
        meta, history = folder / ("agent-" + agent + ".meta.json"), folder / ("agent-" + agent + ".jsonl")
        # Identities taken before the reads, so a write during them still wakes a held read.
        keys = [[str(item), file_key(item)] for item in (meta, history)]
        try:
            with meta.open("rb") as stream:
                value = json.loads(stream.read(8 << 10))
            if isinstance(value, dict):
                if "agent_type" not in entry and isinstance(value.get("agentType"), str) and value["agentType"].strip():
                    entry["agent_type"] = value["agentType"].strip()[:64]
                if value.get("finished") is True:
                    tasks.end(key, "completed", round(meta.stat().st_mtime, 3))
                    latest = max(latest, meta.stat().st_mtime)
                    continue
        except (OSError, ValueError):
            pass
        if watch is not None:
            watch.extend(keys)
        if not activity or history.is_symlink():
            continue
        try:
            found, modified = child_activity(history)
        except OSError:
            continue
        entry.update(found)
        latest = max(latest, modified)
    return latest


def scanned_tasks(path):
    """A copy of the task index of a transcript, read from where the previous
    call stopped (a bounded tail on the first call), so this process rereads nothing."""
    # A list and a view can read the same growing transcript at once: the size
    # is taken under the lock, so an older size never resets a newer index.
    with scan_lock:
        info = path.stat()
        identity = (info.st_dev, info.st_ino)
        cached = task_scans.get(str(path))
        if cached is None or cached[0] != identity or cached[1] > info.st_size:
            start = max(0, info.st_size - SCAN_TAIL)
            cached = [identity, start, Tasks(), start == 0]
        if cached[1] < info.st_size:
            with path.open("rb") as stream:
                stream.seek(cached[1])
                data = stream.read(info.st_size - cached[1])
            if not cached[3]:
                cut = data.find(b"\n") + 1
                if cut:
                    data, cached[1], cached[3] = data[cut:], cached[1] + cut, True
                else:
                    data = b""
            end = data.rfind(b"\n") + 1
            for line in data[:end].split(b"\n"):
                if b'"api-request' in line or (b"tool_use" not in line and b"task-notification" not in line):
                    continue
                try:
                    row = json.loads(line)
                except ValueError:
                    continue
                if isinstance(row, dict):
                    cached[2].feed(row)
            cached[1] += end
        if len(task_scans) >= SUMMARY_LIMIT:
            task_scans.clear()
        task_scans[str(path)] = cached
        return cached[2].copy()


def transcript_path(config, session_id):
    path = next((Path(config) / "projects").glob("*/" + session_id + ".jsonl"), None)
    return None if path is None or path.is_symlink() else path


def running_tasks(item):
    """How many subagents and background tasks of a listed live session still run."""
    try:
        path = transcript_path(item["config"], channel.identifier(item["cli_id"]))
        tasks = scanned_tasks(path) if path else None
    except (OSError, KeyError, TypeError, ValueError):
        return 0
    if not tasks or not tasks.running():
        return 0
    settle_tasks(tasks, *process_since(item["config"], item["cli_id"], True))
    read_children(tasks, path.parent / item["cli_id"] / "subagents", None, activity=False)
    return len(tasks.running())


def view_tasks(result, config, session_id, watch):
    """Adds the task fields to the channel helper's view: kind and state on each
    launching tool call, and the running ones (at most CHILD_LIMIT read)."""
    try:
        path = transcript_path(config, session_id)
        tasks = scanned_tasks(path) if path else None
    except OSError:
        return
    if not tasks or not tasks.entries:
        return
    tools = {tool.get("id"): tool for turn in result.get("turns", []) for tool in turn.get("tools", []) if isinstance(tool, dict)}
    latest = 0
    if tasks.running():
        settle_tasks(tasks, *process_since(config, session_id, result.get("running")))
        latest = read_children(tasks, path.parent / session_id / "subagents", watch)
    for key, entry in tasks.entries.items():
        if key in tools:
            tools[key].update({field: value for field, value in public(entry).items() if field not in ("name", "detail")})
    # The card and the strip show the same label: the tool call's own one when the view has it.
    still = sorted(({"id": key, **public(entry), **({field: tools[key][field] for field in ("name", "detail") if field in tools[key]} if key in tools else {})}
                    for key, entry in tasks.running()), key=lambda item: item.get("started_at") or 0, reverse=True)
    if latest > float(result.get("updated_at") or 0):
        result["updated_at"] = latest
    if still:
        result["tasks"] = {"count": len(still), "running": still[:CHILD_LIMIT]}


def file_key(path):
    try:
        info = os.stat(path)
    except OSError:
        return None
    return [info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns]


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False).encode()).hexdigest()


def read_state(path):
    key = file_key(path)
    cached = states.get(str(path))
    if cached is None or cached[0] != key or key is None:
        cached = (key, channel.read_json(path, {}))
        states[str(path)] = cached
    return copy.deepcopy(cached[1])


def registry_key(config):
    # The identity of each registered CLI process, without its status, so a
    # busy session does not rerun the CLI listing on every status change.
    folder = Path(config or os.environ.get("CLAUDE_CONFIG_DIR") or Path.home() / ".claude") / "sessions"
    try:
        entries = sorted(entry.name for entry in os.scandir(folder) if entry.name.endswith(".json"))
    except OSError:
        return []
    rows = []
    for name in entries:
        try:
            with open(folder / name, "rb") as stream:
                value = json.loads(stream.read(64 << 10))
            fields = [value.get(field) for field in ("pid", "sessionId", "startedAt", "procStart", "entrypoint", "kind")] if isinstance(value, dict) else [None]
        except (OSError, ValueError):
            fields = ["unreadable", file_key(folder / name)]
        rows.append([name, channel.live(name[:-5]), fields])
    return rows


def install(module):
    global channel
    channel = module
    summary, running, connection = module.history_summary, module.running_sessions, module.connection_for

    def history_summary(path):
        key = file_key(path)
        cached = summaries.get(str(path))
        if cached is not None and key is not None and cached[0] == key:
            return cached[1]
        result = summary(path)
        if len(summaries) >= SUMMARY_LIMIT:
            summaries.clear()
        summaries[str(path)] = (key, result)
        return result

    def running_sessions(config=""):
        key = registry_key(config)
        with lock:
            guard = agent_locks.setdefault(config, threading.Lock())
        with guard:
            cached = agents.get(config)
            if cached is None or cached[0] != key or time.monotonic() - cached[1] >= AGENTS_MAX_AGE:
                started = time.monotonic()
                cached = (key, started, running(config))
                agents[config] = cached
        return [row for row in cached[2] if module.live(row.get("pid"))]

    def connection_for(session_id, config):
        index = getattr(local, "states", None)
        if index is None:
            return connection(session_id, config)
        # The channel helper's choice, from one read of each state per discovery.
        return max(index.get((session_id, str(config)), []), key=lambda item: (module.live(item[1].get("pid")) and not item[1].get("closed"), module.live(item[1].get("channel_pid")), item[1].get("updated_at", 0)), default=("", {}))

    module.history_summary, module.running_sessions, module.connection_for = history_summary, running_sessions, connection_for


def discover(request):
    index = {}
    for path in channel.ROOT.glob("*/state.json"):
        state = read_state(path)
        index.setdefault((state.get("cli_id"), state.get("config")), []).append((path.parent.name, state))
    local.states = index
    try:
        sessions = channel.discover(request)
    finally:
        local.states = None
    for item in sessions:
        count = running_tasks(item) if item.get("running") else 0
        if count:
            item["running_tasks"] = count
    return sessions, digest(sessions)


def view_target(request):
    # The channel helper's own choice of account configuration for "view".
    config = Path(request["config"]) if request.get("config") else channel.config_directory(request.get("account") or "default")
    state = channel.read_json(channel.ROOT / channel.identifier(request.get("connection") or request["cli_id"]) / "state.json", {})
    if state.get("cli_id") == request["cli_id"] and state.get("config"):
        config = Path(state["config"])
    return config, channel.identifier(request["cli_id"])


def view_parts(config, session_id):
    # Everything view() reads: the transcript, the matching channel states,
    # an account switch's frozen history and the liveness of their processes.
    parts = [str(config)]
    for path in sorted((config / "projects").glob("*/" + session_id + ".jsonl")):
        parts.append([str(path), file_key(path)])
    for path in sorted(channel.ROOT.glob("*/state.json")):
        state = read_state(path)
        if state.get("cli_id") != session_id or state.get("config") != str(config):
            continue
        parts.append([path.parent.name, file_key(path), channel.live(state.get("pid")), channel.live(state.get("channel_pid"))])
        if state.get("resume_transcript"):
            parts.append([state["resume_transcript"], file_key(state["resume_transcript"])])
    return parts


def view_key(config, session_id):
    # view_parts() and the files of the subagents the latest view found running.
    with lock:
        paths = children.get((str(config), session_id), [])
    return digest(view_parts(config, session_id) + [[path, file_key(path)] for path in paths])


def view(request):
    config, session_id = view_target(request)
    # The children's keys are taken by view() as it reads them, so a write
    # after that read still wakes the next view-wait.
    watched = []
    parts = view_parts(config, session_id)
    result = channel.view(config, session_id)
    view_tasks(result, config, session_id, watched)
    with lock:
        if watched:
            if len(children) >= SUMMARY_LIMIT:
                children.clear()
            children[(str(config), session_id)] = [path for path, _ in watched]
        else:
            children.pop((str(config), session_id), None)
    result["watch_key"] = digest(parts + watched)
    return result


def waited(request, interval, check):
    deadline = time.monotonic() + max(0.0, min(float(request.get("timeout") or 0), WAIT_LIMIT))
    while True:
        result = check()
        if result["changed"]:
            return result
        remaining = deadline - time.monotonic()
        if remaining <= 0 or request["cancelled"].wait(min(interval, remaining)):
            return result


class Missing(Exception):
    pass


def load_module(name, path, digest):
    # Load only the exact file the workbench's own bootstrap installed.
    try:
        intact = re.fullmatch("[0-9a-f]{64}", digest or "") and hashlib.sha256(path.read_bytes()).hexdigest() == digest
    except OSError:
        intact = False
    if not intact:
        return None
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def codex_discover(request):
    global codex
    with codex_lock:
        if codex is None:
            codex = load_module("codex_sessions", Path.home() / ".local/share/cage/codex-session-code" / (codex_name + ".py"), codex_name)
            if codex is None:
                raise Missing()
    # The same answers as the Codex helper's main() gives for "discover".
    try:
        return codex.discover(request)
    except ValueError as error:
        return {"error": str(error)}
    except (OSError, KeyError, TypeError):
        return {"error": "电脑未能处理 Codex 会话请求，请稍后重试。"}


def handle(mode, request):
    if mode == "codex-discover":
        return codex_discover(request)
    if mode == "discover":
        sessions, hashed = discover(request)
        return {"hash": hashed, "sessions": sessions}
    if mode == "watch":
        def changed():
            hashed = discover(request)[1]
            return {"hash": hashed, "changed": hashed != request.get("known")}
        return waited(request, WATCH_INTERVAL, changed)
    if mode == "view":
        return view(request)
    if mode == "view-wait":
        def changed():
            key = view_key(*view_target(request))
            return {"key": key, "changed": key != request.get("key")}
        return waited(request, VIEW_INTERVAL, changed)
    raise ValueError("unsupported operation")


def serve(requests, replies, lifetime=600.0):
    writing = threading.Lock()
    pending = {}
    retire = time.monotonic() + lifetime

    def answer(ident, payload, cancelled):
        code, output, error = 0, b"", b""
        try:
            message = json.loads(payload)
            request = dict(message["input"], cancelled=cancelled)
            mode = message["mode"]
            if mode not in MODES:
                raise ValueError("unsupported operation")
        except Exception as failure:
            code, output, error = 1, b"", type(failure).__name__.encode()
        else:
            try:
                output = json.dumps(handle(mode, request), ensure_ascii=False).encode()
                if len(output) > OUTPUT_LIMIT:
                    code, output, error = 3, b"", b"output limit"
            except Missing:
                code, output, error = MISSING, b"", b""
            except Exception as failure:
                # The workbench answers this request with the channel helper.
                code, output, error = REFUSED, b"", type(failure).__name__.encode()
        with writing:
            pending.pop(ident, None)
            replies.write(b"D %d %d %d %d\n" % (ident, code, len(output), len(error)) + output + error)
            replies.flush()

    replies.write(b"CAGE-GUEST-SESSION 1\n")
    replies.flush()
    while True:
        fields = requests.readline(64).split()
        try:
            if len(fields) == 3 and fields[0] == b"R" and 0 < int(fields[2]) <= REQUEST_LIMIT:
                payload = requests.read(int(fields[2]))
                if len(payload) != int(fields[2]):
                    return
                ident, cancelled = int(fields[1]), threading.Event()
                with writing:
                    if time.monotonic() >= retire:
                        replies.write(b"D %d %d 0 0\n" % (ident, RETIRED))
                        replies.flush()
                        continue
                    pending[ident] = cancelled
                threading.Thread(target=answer, args=(ident, payload, cancelled), daemon=True).start()
            elif len(fields) == 2 and fields[0] == b"C":
                with writing:
                    cancelled = pending.get(int(fields[1]))
                if cancelled:
                    cancelled.set()
            else:
                return
        except ValueError:
            return


def main():
    global codex_name
    os.umask(0o077)
    arguments = sys.argv[1:5]
    name, expected, lifetime, codex_name = arguments + ["", "", "600", ""][len(arguments):]
    # The workbench's ordinary bootstrap installs the channel helper; without
    # that exact file every request is answered by the helper itself.
    module = None
    if re.fullmatch("[0-9a-f]{64}", name):
        module = load_module("cli_channel", Path.home() / ".local/share/cage/cli-channel-code" / (name + ".py"), expected)
    if module is None:
        return 3
    install(module)
    serve(sys.stdin.buffer, sys.stdout.buffer, float(lifetime))
    return 0


if __name__ == "__main__":
    sys.exit(main())
