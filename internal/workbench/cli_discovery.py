"""Resident Claude session discovery for one workbench (Linux guests).

The workbench starts this once through the guest's login shell, like the
channel helper, and keeps it while clients poll. Framing follows
guest-session.py: "R <id> <length>\n" + JSON {"mode", "input"}, "C <id>\n"
ends a waiting request, and each reply is "D <id> <exit> <stdout length>
<stderr length>\n" + both. The channel helper's own discover() and view()
produce every answer; this process only remembers what it already read.
It serves read paths only: handoff, purge and launch need a fresh registry
and stay with the channel helper. A list's Codex discovery runs the Codex
helper's own discover() here as well.
"""
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
agents = {}
agent_locks = {}
states = {}
lock = threading.Lock()
local = threading.local()


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
    return sessions, digest(sessions)


def view_target(request):
    # The channel helper's own choice of account configuration for "view".
    config = Path(request["config"]) if request.get("config") else channel.config_directory(request.get("account") or "default")
    state = channel.read_json(channel.ROOT / channel.identifier(request.get("connection") or request["cli_id"]) / "state.json", {})
    if state.get("cli_id") == request["cli_id"] and state.get("config"):
        config = Path(state["config"])
    return config, channel.identifier(request["cli_id"])


def view_key(config, session_id):
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
    return digest(parts)


def view(request):
    config, session_id = view_target(request)
    key = view_key(config, session_id)
    result = channel.view(config, session_id)
    result["watch_key"] = key
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
