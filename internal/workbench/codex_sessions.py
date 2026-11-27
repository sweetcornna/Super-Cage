"""Guest-side Codex CLI sessions: discovery, bounded transcripts, detached phone exec
turns, and the lease a desktop terminal's Codex TUI holds while it owns a thread."""
import calendar
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path.home() / ".local/share/cage/codex-sessions"
ACCOUNTS = Path.home() / ".local/share/cage/codex-accounts"
ACCOUNT_HELPER = Path("/usr/local/bin/cage-codex-account")
PROC = Path("/proc")
PROMPT_LIMIT = 32 << 10
REPLY_LIMIT = 128 << 10
HISTORY_LIMIT = 448 << 10
# Rollouts reach several MiB. Metadata and titles come from a bounded head and
# transcripts from a bounded tail; no operation reads a whole rollout.
HEAD_LIMIT = 256 << 10
TAIL_LIMIT = 4 << 20
LINE_LIMIT = 1 << 20
LISTED = 300
TOOL_LIMIT = 64
ACCOUNT_REFUSED = 78
# A desktop terminal's launch refused to start Codex; the message is on stderr.
LAUNCH_REFUSED = 75
PROBE_TIMEOUT = 15
# Purge runs inside the host's 15 s cap on a helper call: its probe and the
# CLI's delete share this budget, and the rollout goes when they cannot.
PURGE_BUDGET = 12.0
# Bumped when probe.json gains fields, so a cached older probe is redone.
PROBE_SCHEMA = 2
UNPROBED = {"installed": False, "complete": False, "version": "", "resume": False, "no_daemon": False, "resume_no_daemon": False,
            "cd": False, "add_dir": False, "resume_cd": False, "resume_add_dir": False, "delete": False}
UUID = r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
# Newer CLIs name a rollout <thread>_<rollout id> when the two differ, and can
# compress cold rollouts (.jsonl.zst), which this helper cannot read.
ROLLOUT = re.compile(r"rollout-.+-(" + UUID + r")(?:_[0-9A-Za-z-]{1,64})?\.jsonl")
COMPRESSED = re.compile(r"rollout-.+-(" + UUID + r")(?:_[0-9A-Za-z-]{1,64})?\.jsonl\.zst")
STAMP = re.compile(r"(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})")
# Newer CLIs hold a per-thread writer lock open while a session is open.
WRITER_LOCK = re.compile(r"(" + UUID + r")\.lock")
# Context some Codex versions record as user input. It is never a prompt.
INJECTED = re.compile(r"\s*(?:<(?:environment_context|user_instructions|turn_aborted|user_shell_command|subagent_notification|skill|INSTRUCTIONS)\b|# AGENTS\.md instructions for )")
TUI_OPEN = "这段会话正在电脑上的 Codex 终端中打开。请先退出该终端中的 Codex，再从手机继续。"
DESKTOP_OPEN = "这段会话正在电脑的 Codex 终端中打开。可在手机的终端页打开该终端继续，或等电脑上退出 Codex 后再发送。"
DESKTOP_NOTICE = "这段会话正在电脑的 Codex 终端中打开，手机上可以查看和停止本轮。"
STATE_UNREADABLE = "电脑上这段 Codex 会话的状态无法读取，请在电脑上重新打开这段会话后再试。"
STATE_UNREADABLE_BUSY = "这段 Codex 会话的状态无法读取，且这段会话可能仍有 Codex 在运行。请先结束电脑上运行它的 Codex，或重启环境后再打开。"
PHONE_RUNNING = "手机发起的一轮仍在运行，完成后再在终端中继续。"
OTHER_TERMINAL = "这段会话已在另一个电脑终端中打开。"
OTHER_PROCESS = "这段会话正在其他 Codex 进程中打开，请先退出该进程后再继续。"
COMPRESSED_NOTICE = "这段会话的历史由较新的 Codex 压缩保存，暂时无法在这里显示，可在电脑终端中继续。"
_ORIGINAL_UMASK = None


def identifier(value):
    return str(uuid.UUID(value))


def read_json(path, default=None):
    try:
        return json.loads(path.read_text())
    except (FileNotFoundError, ValueError):
        return default


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as stream:
        json.dump(value, stream, ensure_ascii=False)
        temporary = stream.name
    os.replace(temporary, path)


def load_state(path):
    """A state file's content and whether it could be read; a missing file is empty."""
    try:
        text = path.read_text()
    except FileNotFoundError:
        return {}, True
    try:
        value = json.loads(text)
    except ValueError:
        return {}, False
    return (value, True) if isinstance(value, dict) else ({}, False)


@contextlib.contextmanager
def state_file(connection, strict=False, recover=None):
    """The state of one connection under its lock. strict refuses an unreadable
    file, which could have recorded a desktop lease or a phone turn, instead of
    starting afresh; recover(path), called under the lock, may set such a file
    aside (and say so) when nothing it can record still runs."""
    folder = ROOT / identifier(connection)
    folder.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (folder / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        path = folder / "state.json"
        state, readable = load_state(path)
        if not readable and recover is not None and recover(path):
            state, readable = {}, True
        if strict and not readable:
            raise ValueError(STATE_UNREADABLE)
        before = json.dumps(state, sort_keys=True)
        yield state
        if json.dumps(state, sort_keys=True) != before:
            save(path, state)


@contextlib.contextmanager
def admission():
    """Admitting a phone turn or a desktop TUI, across connections: each checks
    the other connections' runs and leases and records its own under this
    lock, so neither is admitted beside the other on one thread while its
    Codex has not started. Taken before any connection's lock. The process
    scan stays: an older helper copy does not take it."""
    ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (ROOT / "admission.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def codex_home(account):
    if account == "default":
        configured = os.environ.get("CODEX_HOME")
        return Path(configured) if configured else Path.home() / ".codex"
    if not isinstance(account, str) or not re.fullmatch(r"[0-9a-f]{24}", account):
        raise ValueError("Codex 账号 ID 无效")
    return ACCOUNTS / account / "home"


def clip(text, limit):
    return text.encode("utf-8")[:limit].decode("utf-8", errors="ignore")


def within(path, root):
    return isinstance(path, str) and (path == root or path.startswith(root.rstrip("/") + "/"))


def process_start(pid):
    """Start time of a live process, so a reused PID never matches a recorded one:
    "" when there is no such process, None when it cannot be told."""
    if not isinstance(pid, int) or pid <= 0:
        return ""
    stat = PROC / str(pid) / "stat"
    if (PROC / "self").exists() or stat.exists():
        try:
            fields = stat.read_text().rsplit(")", 1)[1].split()
        except FileNotFoundError:
            return ""
        except (OSError, IndexError):
            return None
        if len(fields) < 20:
            return None
        return "" if fields[0] == "Z" else fields[19]
    try:
        listing = subprocess.run(["ps", "-o", "stat=,lstart=", "-p", str(pid)], capture_output=True, text=True, timeout=2).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return None
    return "" if not listing or listing.startswith("Z") else listing.split(None, 1)[-1]


def fingerprint(pid):
    return process_start(pid) or ""


def run_state(run, connection=None):
    """"running", "ended" or "unknown" for a phone turn's supervisor of
    connection: ended when there is no process or it recorded its exit,
    unknown when the process exists but cannot be told from another one with
    a reused PID, or when an admitted turn has no PID recorded (the send that
    launched it was cut off) and its supervisor cannot be ruled out."""
    if not isinstance(run, dict) or run.get("exit_code") is not None:
        return "ended"
    if not run.get("pid"):
        if not run.get("argv"):
            return "ended"
        return "ended" if connection and supervising(connection) is False else "unknown"
    current = process_start(run.get("pid"))
    if current is None or current and not run.get("start"):
        return "unknown"
    return "running" if current and current == run["start"] else "ended"


def alive(run, connection=None):
    # An unknown state counts as running: nothing writes beside a turn that may run.
    return run_state(run, connection) != "ended"


def phone_turns(thread, states):
    """Connections whose phone turn runs (or may) on thread."""
    return [connection for connection, state in states.items() if alive(state.get("run"), connection) and claimed_thread(connection, state) == thread]


def boot_time():
    """When the system booted, in seconds since the epoch, or None."""
    try:
        for line in (PROC / "stat").read_text().splitlines():
            if line.startswith("btime "):
                return float(line.split()[1])
    except (OSError, ValueError, IndexError):
        pass
    return None


def supervising(connection):
    """Whether a phone turn's supervisor of connection runs; None when the
    process table cannot be read."""
    def supervises(words):
        return any(word == "supervise" and following == connection for word, following in zip(words, words[1:]))
    try:
        entries = [entry for entry in PROC.iterdir() if entry.name.isdigit()]
    except FileNotFoundError:
        # No /proc (a macOS guest): the command lines from ps.
        try:
            listing = subprocess.run(["ps", "-axww", "-o", "command="], capture_output=True, text=True, timeout=2)
        except (OSError, subprocess.SubprocessError):
            return None
        if listing.returncode != 0:
            return None
        return any(supervises(line.split()) for line in listing.stdout.splitlines())
    except OSError:
        return None
    for entry in entries:
        try:
            words = [os.fsdecode(word) for word in (entry / "cmdline").read_bytes().split(b"\0") if word]
        except OSError:
            continue
        if supervises(words):
            return True
    return False


def unreadable_recovery(connection, thread, home, users):
    """recover for state_file in a desktop open: an unreadable state is set
    aside (renamed, never overwritten) only when nothing it may record can
    still run. That takes a process table showing no phone turn's supervisor
    of this connection, and either the open naming a thread no process holds,
    or a file written before this boot (it may have recorded a new session's
    turn whose thread is unknown). The boot time alone is not enough: it moves
    when the guest's clock is stepped. Otherwise the open is refused."""
    def recover(path):
        try:
            written = path.stat().st_mtime
        except OSError:
            return False
        if supervising(connection) is not False:
            raise ValueError(STATE_UNREADABLE_BUSY)
        if thread:
            held = find_rollout(home, thread) or compressed_path(home, thread)
            if held is not None and foreign(held, thread, users, set()):
                raise ValueError(STATE_UNREADABLE_BUSY)
        else:
            booted = boot_time()
            if booted is None or written >= booted:
                raise ValueError(STATE_UNREADABLE_BUSY)
        os.replace(path, path.with_name("state.json.unreadable-%d" % int(time.time())))
        return True
    return recover


def lease_state(lease):
    """"held", "ended" or "unknown" for the lease of a desktop terminal's Codex
    process. The lease belongs to that process, so a crash or exit ends it."""
    if not isinstance(lease, dict) or not lease.get("start"):
        return "ended"
    current = process_start(lease.get("pid"))
    if current is None:
        return "unknown"
    return "held" if current == lease["start"] else "ended"


def leased(lease):
    # An unknown state counts as held: the phone never writes when it is unclear.
    return lease_state(lease) != "ended"


def group_of(pid):
    try:
        return os.getpgid(pid)
    except OSError:
        return None


def lease_member(lease, pid):
    """Whether pid is the leased process or runs in its process group (a CLI
    launcher can start the real binary as a child)."""
    return pid == lease.get("pid") or bool(lease.get("pgid")) and group_of(pid) == lease["pgid"]


def threads_held(lease, users):
    """Threads whose writer lock or rollout a lease's processes hold open."""
    files, threads = users
    held = {thread for thread, pids in threads.items() if any(lease_member(lease, pid) for pid in pids)}
    for path, pids in files.items():
        match = ROLLOUT.fullmatch(Path(path).name)
        if match and any(lease_member(lease, pid) for pid in pids):
            held.add(match.group(1))
    return held


def stamp(value):
    """Seconds since the epoch of an ISO time written by Codex (UTC), or None."""
    match = STAMP.match(value) if isinstance(value, str) else None
    if not match:
        return None
    try:
        return float(calendar.timegm(tuple(int(part) for part in match.groups()) + (0, 0, 0)))
    except (ValueError, OverflowError):
        return None


def real(path):
    try:
        return os.path.realpath(path) if isinstance(path, str) and path else ""
    except (OSError, ValueError):
        return ""


def candidate(meta, created, lease):
    """Whether a rollout may be the thread a new desktop session's Codex created:
    the lease has no thread yet, the cwd is the lease's and it is not older."""
    if not isinstance(lease, dict) or lease.get("thread") or not meta:
        return False
    root = real(lease.get("cwd"))
    return bool(root) and within(real(meta.get("cwd")), root) and created >= float(lease.get("acquired_at") or 0) - 5


def all_states(exclude=""):
    """Every readable connection state, keyed by connection."""
    states = {}
    for path in ROOT.glob("*/state.json"):
        try:
            connection = identifier(path.parent.name)
        except ValueError:
            continue
        if connection != exclude:
            states[connection] = read_json(path, {}) or {}
    return states


def claimed_thread(connection, state):
    if state.get("thread_id"):
        return state["thread_id"]
    return started_thread(ROOT / connection) if state.get("run") else ""


def desktop_leases(thread, account, path, states):
    """Held leases of other connections that own thread, or that may own it
    because their new session has not learnt its thread yet."""
    found = []
    meta, created = None, 0.0
    for connection, state in states.items():
        lease = state.get("lease")
        if not isinstance(lease, dict) or (lease.get("account") or "default") != account or not leased(lease):
            continue
        if lease.get("thread") == thread:
            found.append(lease)
        elif not lease.get("thread") and path is not None:
            if meta is None:
                try:
                    meta = head(path) or {}
                    created = meta.get("created") or path.stat().st_mtime
                except OSError:
                    meta, created = {}, 0.0
            others = {claimed_thread(key, value) for key, value in states.items()}
            if thread not in others and candidate(meta, created, lease):
                found.append(lease)
    return found


def settle_lease(connection, state, users):
    """Drop a lease whose Codex has ended. A new desktop session that never
    learnt its thread takes the one thread its Codex can have made: in the
    lease's directory, started after the lease was taken, claimed by no other
    session and held open by no process. Caller holds the connection's lock."""
    lease = state.get("lease")
    if not isinstance(lease, dict) or lease_state(lease) != "ended":
        return ""
    state.pop("lease")
    if lease.get("thread") or state.get("thread_id"):
        return ""
    try:
        home = codex_home(lease.get("account") or "default")
    except ValueError:
        return ""
    since = float(lease.get("acquired_at") or 0) - 5
    claimed = {claimed_thread(key, value) for key, value in all_states(connection).items()} - {""}
    files, threads = users
    found = set()
    for mtime, path, _ in rollouts(home)[:LISTED]:
        if mtime < since:
            continue
        try:
            meta = head(path)
        except OSError:
            continue
        if (meta and not meta["subagent"] and meta["id"] not in claimed and candidate(meta, meta.get("created") or mtime, lease)
                and not files.get(os.path.realpath(path)) and not threads.get(meta["id"])):
            found.add(meta["id"])
    if len(found) != 1:
        return ""
    state["thread_id"] = found.pop()
    return state["thread_id"]


def bind_lease(connection, state, users):
    """Bind a new desktop session to the one thread its Codex process holds open.
    Caller holds the connection's lock. Binding happens once; a thread the TUI
    opens later never re-binds the session."""
    lease = state.get("lease")
    if not isinstance(lease, dict) or lease.get("thread") or state.get("thread_id") or lease_state(lease) != "held":
        return ""
    held = threads_held(lease, users)
    if len(held) != 1:
        return ""
    thread = held.pop()
    if any(claimed_thread(key, value) == thread for key, value in all_states(connection).items()):
        return ""
    if find_rollout(codex_home(lease.get("account") or "default"), thread) is None:
        return ""
    state["thread_id"] = lease["thread"] = thread
    return thread


def rollout_users():
    """Processes holding each rollout open, and processes holding or naming a thread."""
    files, threads = {}, {}
    try:
        entries = [entry for entry in PROC.iterdir() if entry.name.isdigit()]
    except OSError:
        return files, threads
    for entry in entries:
        pid = int(entry.name)
        try:
            words = [os.fsdecode(word) for word in (entry / "cmdline").read_bytes().split(b"\0") if word]
        except OSError:
            continue
        if any("codex" in Path(word).name for word in words[:3]):
            for word in words:
                if re.fullmatch(UUID, word):
                    threads.setdefault(word, set()).add(pid)
        try:
            descriptors = list((entry / "fd").iterdir())
        except OSError:
            continue
        for descriptor in descriptors:
            try:
                target = os.readlink(descriptor)
            except OSError:
                continue
            name = Path(target).name
            lock = WRITER_LOCK.fullmatch(name) if Path(target).parent.name == "thread-writer-locks" else None
            if lock:
                threads.setdefault(lock.group(1), set()).add(pid)
            elif target.endswith(".jsonl") and ROLLOUT.fullmatch(name):
                files.setdefault(target, set()).add(pid)
    return files, threads


def foreign(path, thread, users, groups):
    """Users of a rollout other than this helper and the exec runs it started."""
    files, threads = users
    found = set(files.get(os.path.realpath(path), ())) | set(threads.get(thread, ()))
    result = set()
    for pid in found:
        try:
            group = os.getpgid(pid)
        except OSError:
            group = None
        if pid != os.getpid() and group not in groups:
            result.add(pid)
    return result


def rollouts(home):
    """Rollout files of one Codex home, newest first. Symlinks are never followed."""
    def children(folder, pattern):
        try:
            with os.scandir(folder) as entries:
                return [Path(entry.path) for entry in entries if entry.is_dir(follow_symlinks=False) and re.fullmatch(pattern, entry.name)]
        except OSError:
            return []
    found = []
    for year in children(home / "sessions", r"\d{4}"):
        for month in children(year, r"\d{2}"):
            for day in children(month, r"\d{2}"):
                try:
                    with os.scandir(day) as entries:
                        for entry in entries:
                            match = ROLLOUT.fullmatch(entry.name)
                            if match and entry.is_file(follow_symlinks=False):
                                found.append((entry.stat(follow_symlinks=False).st_mtime, str(entry.path), match.group(1)))
                except OSError:
                    continue
    found.sort(reverse=True)
    return [(mtime, Path(path), thread) for mtime, path, thread in found]


def find_rollout(home, thread):
    folder = home / "sessions"
    paths = [path for pattern in ("rollout-*-" + thread + ".jsonl", "rollout-*-" + thread + "_*.jsonl")
             for path in folder.glob("[0-9]*/[0-9]*/[0-9]*/" + pattern) if path.is_file() and not path.is_symlink() and ROLLOUT.fullmatch(path.name)]
    return max(paths, key=lambda path: path.stat().st_mtime, default=None)


def compressed_path(home, thread):
    """The newest compressed rollout of a thread, which this helper cannot read
    but the Codex TUI can resume."""
    folder = home / "sessions"
    paths = [path for pattern in ("rollout-*-" + thread + ".jsonl.zst", "rollout-*-" + thread + "_*.jsonl.zst")
             for path in folder.glob("[0-9]*/[0-9]*/[0-9]*/" + pattern) if COMPRESSED.fullmatch(path.name) and not path.is_symlink()]
    return max(paths, key=lambda path: path.stat().st_mtime, default=None)


def compressed_rollout(home, thread):
    return compressed_path(home, thread) is not None


def texts(content, kinds):
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        return ""
    return "\n".join(item["text"] for item in content if isinstance(item, dict) and item.get("type") in kinds and isinstance(item.get("text"), str))


def user_prompt(row):
    """(source, text) of a real user input record, or None."""
    kind, payload = row.get("type"), row.get("payload")
    if not isinstance(payload, dict):
        return None
    text, images = None, False
    if kind == "event_msg" and payload.get("type") == "user_message" and payload.get("kind") in (None, "plain"):
        text = payload.get("message") if isinstance(payload.get("message"), str) else ""
        images = bool(payload.get("images") or payload.get("local_images"))
        source = "event"
    elif kind == "event_msg" and payload.get("type") == "item_completed" and isinstance(payload.get("item"), dict) and payload["item"].get("type") == "UserMessage":
        content = payload["item"].get("content")
        text = texts(content, ("text", "input_text", "Text"))
        images = isinstance(content, list) and any(isinstance(item, dict) and "image" in str(item.get("type", "")).lower() for item in content)
        source = "item"
    elif kind == "response_item" and payload.get("type") == "message" and payload.get("role") == "user":
        text = texts(payload.get("content"), ("input_text", "text"))
        source = "response"
    else:
        return None
    if text and INJECTED.match(text):
        return None
    if not (text or "").strip():
        return (source, "[图片]") if images else None
    return source, text


def preview(value):
    if isinstance(value, list):
        value = " ".join(str(item) for item in value if isinstance(item, (str, int, float)))
    if not isinstance(value, str):
        return ""
    return next((line.strip() for line in value.splitlines() if line.strip()), "")[:160]


def argument_preview(arguments):
    try:
        value = json.loads(arguments) if isinstance(arguments, str) else arguments
    except ValueError:
        return preview(arguments)
    if isinstance(value, dict):
        for field in ("cmd", "command", "path", "file_path", "query", "pattern", "url", "message", "description", "prompt"):
            text = preview(value.get(field))
            if text:
                return text
        return ""
    return preview(value)


def input_preview(text):
    if not isinstance(text, str):
        return ""
    changed = re.search(r"^\*\*\* (?:Add|Update|Delete) File: (.+)$", text, re.M)
    return changed.group(1).strip()[:160] if changed else preview(text)


def tool_call(payload, position):
    """Display summary of a model tool call: name, short input preview and status."""
    kind, status = payload.get("type"), "called"
    if kind == "function_call":
        name, namespace = payload.get("name"), payload.get("namespace")
        if isinstance(name, str) and isinstance(namespace, str) and namespace:
            name = namespace + "." + name
        detail = argument_preview(payload.get("arguments"))
    elif kind == "custom_tool_call":
        name, detail = payload.get("name"), input_preview(payload.get("input"))
    elif kind == "local_shell_call":
        action = payload.get("action") if isinstance(payload.get("action"), dict) else {}
        name, detail = "shell", preview(action.get("command"))
    elif kind == "web_search_call":
        action = payload.get("action") if isinstance(payload.get("action"), dict) else {}
        name, detail, status = "web_search", preview(action.get("query") or action.get("url")), "completed"
    else:
        return None
    key = payload.get("call_id") or payload.get("id") or "call-" + str(position)
    if not isinstance(key, str) or not 0 < len(key.encode()) <= 128:
        return None
    if not isinstance(name, str) or not re.fullmatch(r"[a-zA-Z0-9_.:-]{1,128}", name):
        return None
    tool = {"id": key, "name": name, "status": status}
    if detail:
        tool["detail"] = detail
    return tool


def tool_output(payload):
    if payload.get("type") not in ("function_call_output", "custom_tool_call_output", "local_shell_call_output"):
        return None
    output = payload.get("output")
    failed = isinstance(output, dict) and output.get("success") is False
    return payload.get("call_id"), "failed" if failed else "completed"


def turn_id(thread, position):
    # Stable across reads of the append-only rollout, and shaped like the
    # phone's request IDs so stop requests reach the guest for a clear answer.
    return str(uuid.UUID(bytes=hashlib.sha256((thread + ":" + str(position)).encode()).digest()[:16], version=4))


def prompt_key(text):
    return hashlib.sha256(text.strip().encode()).hexdigest()


def session_meta(line):
    try:
        row = json.loads(line)
    except ValueError:
        return None
    payload = row.get("payload") if isinstance(row, dict) and row.get("type") == "session_meta" else None
    if not isinstance(payload, dict) or not isinstance(payload.get("cwd"), str):
        return None
    try:
        thread = identifier(payload.get("id"))
    except (TypeError, ValueError, AttributeError):
        return None
    source = payload.get("source")
    # Sub-agent threads belong to their parent session and are never listed.
    subagent = payload.get("thread_source") == "subagent" or isinstance(source, dict) and "subagent" in source
    return {"id": thread, "cwd": payload["cwd"], "subagent": bool(subagent), "title": "",
            "created": stamp(row.get("timestamp")) or stamp(payload.get("timestamp"))}


def head(path):
    """Metadata and the first prompt from a bounded head of a rollout."""
    with path.open("rb") as stream:
        first = stream.readline(LINE_LIMIT)
        meta = session_meta(first)
        if meta is None:
            return None
        budget = HEAD_LIMIT
        while budget > 0:
            line = stream.readline(min(LINE_LIMIT, budget))
            if not line:
                break
            budget -= len(line)
            try:
                row = json.loads(line)
            except ValueError:
                continue
            prompt = user_prompt(row) if isinstance(row, dict) else None
            if prompt:
                meta["title"] = " ".join(prompt[1].split())[:40]
                break
    return meta


def transcript(path, thread):
    """Turns from a bounded tail of a rollout, with the byte offset of each prompt."""
    turns, cwd, pending, truncated = [], "", False, False
    if path is None:
        return turns, cwd, pending, truncated
    with path.open("rb") as stream:
        size = stream.seek(0, os.SEEK_END)
        start = max(0, size - TAIL_LIMIT)
        stream.seek(start)
        data = stream.read(size - start)
    if start:
        # The window starts inside a line; earlier turns are not shown.
        cut = data.find(b"\n") + 1
        data, start, truncated = (data[cut:] if cut else b""), start + cut, True
    offset = start
    for raw in data.split(b"\n"):
        position = offset
        offset += len(raw) + 1
        if not raw.strip() or len(raw) > LINE_LIMIT:
            continue
        try:
            row = json.loads(raw)
        except ValueError:
            continue
        payload = row.get("payload") if isinstance(row, dict) else None
        if not isinstance(payload, dict):
            continue
        kind = row.get("type")
        if kind in ("turn_context", "session_meta") and isinstance(payload.get("cwd"), str):
            cwd = payload["cwd"]
        if kind == "event_msg":
            event = payload.get("type")
            if event == "task_started":
                pending = True
            elif event in ("task_complete", "turn_aborted"):
                pending = False
                if turns:
                    turn = turns[-1]
                    turn["_done"] = True
                    if event == "turn_aborted":
                        turn["status"] = "stopped"
                    else:
                        turn["status"] = "completed"
                        turn.pop("error", None)
                        message = payload.get("last_agent_message")
                        if not turn["text"] and isinstance(message, str):
                            turn["text"], turn["truncated"] = clip(message, REPLY_LIMIT), len(message.encode()) > REPLY_LIMIT
            elif event == "error" and turns and isinstance(payload.get("message"), str):
                turns[-1]["status"], turns[-1]["error"] = "failed", clip(payload["message"], 1000)
        prompt = user_prompt(row)
        if prompt:
            source, text = prompt
            last = turns[-1] if turns else None
            # Codex records one input as a response item and as an event.
            if last and source not in last["_sources"] and last["_key"] == prompt_key(text) and not last["text"] and not last.get("tools"):
                last["_sources"].add(source)
                continue
            turns.append({"id": turn_id(thread, position), "prompt": clip(text, PROMPT_LIMIT), "text": "", "status": "completed",
                          "_offset": position, "_sources": {source}, "_key": prompt_key(text)})
            if len(text.encode()) > PROMPT_LIMIT:
                turns[-1]["prompt_truncated"] = True
            del turns[:-256]
            continue
        if not turns or kind != "response_item":
            continue
        turn = turns[-1]
        if payload.get("type") == "message" and payload.get("role") == "assistant":
            text = texts(payload.get("content"), ("output_text", "text"))
            if text:
                value = turn["text"] + ("\n\n" if turn["text"] else "") + text
                turn["text"], turn["truncated"] = clip(value, REPLY_LIMIT), len(value.encode()) > REPLY_LIMIT
            continue
        tools = turn.setdefault("tools", [])
        output = tool_output(payload)
        if output:
            for tool in tools:
                if tool["id"] == output[0]:
                    tool["status"] = output[1]
        else:
            tool = tool_call(payload, position)
            if tool and len(tools) < TOOL_LIMIT and all(item["id"] != tool["id"] for item in tools):
                tools.append(tool)
        if not tools:
            turn.pop("tools")
    return turns, cwd, pending, truncated


def tail_pending(path):
    """Whether the newest task in a rollout is still unfinished."""
    with path.open("rb") as stream:
        size = stream.seek(0, os.SEEK_END)
        stream.seek(max(0, size - (256 << 10)))
        lines = stream.read().splitlines()
    for line in reversed(lines):
        if b'"event_msg"' not in line:
            continue
        try:
            payload = json.loads(line).get("payload")
        except (ValueError, AttributeError):
            continue
        if isinstance(payload, dict) and payload.get("type") in ("task_started", "task_complete", "turn_aborted"):
            return payload["type"] == "task_started"
    return False


def started_thread(folder):
    """The thread a new exec run announced in its JSON output."""
    try:
        with (folder / "exec.log").open("rb") as stream:
            lines = stream.read(64 << 10).splitlines()
    except OSError:
        return ""
    for line in lines:
        try:
            event = json.loads(line)
            if event.get("type") == "thread.started":
                return identifier(event["thread_id"])
            message = event.get("msg") or {}
            if message.get("type") == "session_configured":
                return identifier(message["session_id"])
        except (ValueError, TypeError, KeyError, AttributeError):
            continue
    return ""


def run_failure(folder, run):
    """A short reason for an exec run that ended without completing its turn."""
    code = run.get("exit_code")
    if code == ACCOUNT_REFUSED:
        return "所选 Codex 账号未登录或已变化。请在 Codex 账号管理中检查登录状态后重试。"
    message = ""
    try:
        with (folder / "exec.log").open("rb") as stream:
            size = stream.seek(0, os.SEEK_END)
            stream.seek(max(0, size - (64 << 10)))
            lines = stream.read().splitlines()
    except OSError:
        lines = []
    for line in lines:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        error = event.get("error") if event.get("type") == "turn.failed" else event if event.get("type") == "error" else None
        if isinstance(error, dict) and isinstance(error.get("message"), str):
            message = error["message"]
    if message:
        return clip(message.strip(), 500)
    return "Codex 未能完成本轮（退出码 " + str(code) + "）。请检查电脑上的 Codex 登录和网络后重试。"


def state_view(connection):
    if not connection or not (ROOT / connection).is_dir():
        return contextlib.nullcontext({})
    return state_file(connection)


def view(request):
    account = request.get("account") or "default"
    home = codex_home(account)
    thread = identifier(request["thread"]) if request.get("thread") else ""
    connection = identifier(request["connection"]) if request.get("connection") else ""
    readable = not connection or load_state(ROOT / connection / "state.json")[1]
    users = rollout_users()
    with state_view(connection) as state:
        settle_lease(connection, state, users)
        lease = state.get("lease")
        if lease:
            bind_lease(connection, state, users)
        if state and not state.get("thread_id"):
            state["thread_id"] = started_thread(ROOT / connection)
        if state.get("thread_id"):
            if thread and thread != state["thread_id"]:
                raise ValueError("会话身份已变化，请重新读取会话列表")
            thread = state["thread_id"]
        if state and state.get("account", account) != account:
            raise ValueError("会话账号已变化，请重新读取会话列表")
        path = find_rollout(home, thread) if thread else None
        meta = head(path) if path else None
        turns, cwd, pending, truncated = transcript(path, thread)
        run = state.get("run") or {}
        ours = alive(run, connection)
        leases = ([lease] if lease else []) + (desktop_leases(thread, account, path, all_states(connection)) if thread else [])
        desktop = bool(leases)
        groups = ({run["pid"]} if ours and run.get("pid") else set()) | {item["pgid"] for item in leases if item.get("pgid")}
        held = path or (compressed_path(home, thread) if thread else None)
        tui = foreign(held, thread, users, groups) if held else set()
        tui = {pid for pid in tui if not any(lease_member(item, pid) for item in leases)}
        claimed = set()
        for sent in state.get("sent", []):
            match = next((turn for turn in turns if id(turn) not in claimed and turn["_offset"] >= sent["offset"] and turn["_key"] == sent["key"]), None)
            if match:
                claimed.add(id(match))
                match["id"] = sent["id"]
                if sent.pop("text", None) is not None:
                    sent["delivered"] = True
            elif not sent.get("delivered"):
                status = "running" if ours and run.get("request_id") == sent["id"] else "interrupted"
                turns.append({"id": sent["id"], "prompt": sent.get("text", ""), "text": "", "status": status, "_offset": sent["offset"]})
        if turns:
            last = turns[-1]
            mine = last["id"] == run.get("request_id")
            if ours and (pending or mine and not last.get("_done")) or (tui or desktop) and pending:
                last["status"] = "running"
            elif pending and last["status"] == "completed" and not last.get("_done"):
                last["status"] = "interrupted"
            if not ours and mine and not last.get("_done"):
                if state.get("stopped") == last["id"]:
                    last["status"] = "stopped"
                elif run.get("exit_code") not in (None, 0):
                    last["status"], last["error"] = "failed", run_failure(ROOT / connection, run)
        notice = ""
        compressed = bool(thread and path is None and compressed_rollout(home, thread))
        if not readable:
            notice = STATE_UNREADABLE
        elif desktop:
            notice = DESKTOP_NOTICE
        elif tui:
            notice = TUI_OPEN
        elif compressed:
            notice = COMPRESSED_NOTICE
        elif thread and path is None:
            notice = "未找到这段 Codex 会话的历史，可能已在电脑上删除。"
        owner = "phone" if ours else "desktop" if desktop else "external" if tui else ""
        for turn in turns:
            for key in ("_offset", "_sources", "_key", "_done"):
                turn.pop(key, None)
        shortened = truncated or any(turn.pop("prompt_truncated", False) for turn in turns)
        while len(turns) > 1 and len(json.dumps(turns, ensure_ascii=False).encode()) > HISTORY_LIMIT:
            turns.pop(0)
            shortened = True
        updated = max(path.stat().st_mtime if path else 0, float(run.get("finished_at") or run.get("started_at") or 0))
        result = {"thread_id": thread, "account": account, "connection": connection,
                  "workspace": cwd or (meta or {}).get("cwd") or state.get("cwd", "") or (lease or {}).get("cwd", ""),
                  "title": (meta or {}).get("title") or (turns[0]["prompt"].strip()[:40] if turns else ""),
                  "updated_at": updated, "turns": turns, "running": owner != "", "active": bool(ours or (tui or desktop) and pending),
                  "can_send": readable and owner == "" and not (thread and path is None), "history_truncated": shortened,
                  "connection_notice": notice, "account_refused": not ours and run.get("exit_code") == ACCOUNT_REFUSED,
                  "owner": owner, "lease": (lease or {}).get("id", ""), "history_compressed": compressed,
                  "history_found": readable and bool(compressed or meta and meta["id"] == thread)}
    if request.get("probe"):
        result["cli"] = probe({})
    return result


def cached_head(path, index, fresh):
    key, size = str(path), path.stat().st_size
    entry = index.get(key)
    # A rollout only grows. Re-read the head only while it lacks a title.
    if not isinstance(entry, dict) or (entry.get("skip") or not entry.get("title")) and entry.get("size") != size:
        meta = head(path)
        entry = {"size": size, **(meta or {"skip": True})}
    fresh[key] = entry
    return None if entry.get("skip") else entry


def discover(request):
    roots = [root for root in request.get("workspaces", []) if isinstance(root, str) and root.startswith("/")]
    accounts = [account for account in request.get("accounts", []) if isinstance(account, str)]
    states = all_states()
    users = rollout_users()
    # A new desktop session learns its thread from the process holding it.
    for connection, state in states.items():
        lease = state.get("lease")
        if not isinstance(lease, dict) or lease.get("thread") or state.get("thread_id"):
            continue
        held = lease_state(lease)
        if held == "held" and threads_held(lease, users) or held == "ended":
            with state_file(connection) as current:
                if bind_lease(connection, current, users) or settle_lease(connection, current, users) or "lease" not in current:
                    states[connection] = json.loads(json.dumps(current))
    leases = {key: state["lease"] for key, state in states.items() if isinstance(state.get("lease"), dict) and leased(state["lease"])}
    runs = {state["run"]["pid"] for key, state in states.items() if alive(state.get("run"), key) and state["run"].get("pid")}
    claimed = {claimed_thread(key, state) for key, state in states.items()} - {""}
    unbound = [lease for lease in leases.values() if not lease.get("thread")]
    index = read_json(ROOT / "index.json", {}) or {}
    fresh, sessions, listed = {}, [], set()
    for account in accounts:
        try:
            home = codex_home(account)
        except ValueError:
            continue
        for mtime, path, _ in rollouts(home)[:LISTED]:
            try:
                meta = cached_head(path, index, fresh)
            except OSError:
                continue
            if not meta or meta["subagent"] or not any(within(meta["cwd"], root) for root in roots):
                continue
            thread = meta["id"]
            # Until a new desktop session learns its thread, threads it may have
            # created are withheld: listing one would make a second session
            # for it that the phone could write to.
            if thread not in claimed and any((lease.get("account") or "default") == account and candidate(meta, meta.get("created") or mtime, lease) for lease in unbound):
                continue
            connection, state = next(((key, value) for key, value in states.items()
                                      if value.get("thread_id") == thread and value.get("account", "default") == account), ("", {}))
            run = state.get("run") or {}
            ours = connection and alive(run, connection)
            holders = [lease for lease in leases.values() if lease.get("thread") == thread and (lease.get("account") or "default") == account]
            # A desktop TUI that moved to another thread (/new, /resume) writes
            # it as any other process would: only the thread's own lease and
            # an unbound one (not yet learnt) are not someone else.
            mine = holders + [lease for lease in unbound if (lease.get("account") or "default") == account]
            groups = runs | {lease["pgid"] for lease in mine if lease.get("pgid")}
            tui = {pid for pid in foreign(path, thread, users, groups) if not any(lease_member(lease, pid) for lease in mine)}
            owner = "phone" if ours else "desktop" if holders else "external" if tui else ""
            lease = (state.get("lease") if connection in leases else None) or (holders[0] if holders else {})
            sessions.append({"thread_id": thread, "account": account, "connection": connection, "workspace": meta["cwd"],
                             "title": meta.get("title") or "", "updated_at": max(mtime, float(run.get("started_at") or 0)),
                             "running": owner != "", "active": bool(ours or owner and tail_pending(path)),
                             "owner": owner, "lease": lease.get("id", "")})
            listed.add(connection)
    # A new session's first turn, or a new desktop session's TUI, can run
    # before its rollout is listed.
    for connection, state in states.items():
        run = state.get("run") or {}
        ours, desktop = alive(run, connection), connection in leases
        if connection in listed or state.get("account", "default") not in accounts or not (ours or desktop):
            continue
        cwd = state.get("cwd", "") or (leases.get(connection) or {}).get("cwd", "")
        if not any(within(cwd, root) for root in roots):
            continue
        sessions.append({"thread_id": state.get("thread_id") or started_thread(ROOT / connection), "account": state.get("account", "default"),
                         "connection": connection, "workspace": cwd, "title": "",
                         "updated_at": float(run.get("started_at") or (leases.get(connection) or {}).get("acquired_at") or 0),
                         "running": True, "active": ours, "owner": "phone" if ours else "desktop",
                         "lease": (leases.get(connection) or {}).get("id", "")})
    if fresh != index:
        save(ROOT / "index.json", fresh)
    return sessions


def launch(folder, connection, text, cwd, env):
    with tempfile.TemporaryFile(dir=folder) as prompt, (folder / "exec.log").open("wb") as log:
        prompt.write(text.encode())
        prompt.flush()
        prompt.seek(0)
        # The supervisor leads its own session and process group, outlives this
        # SSH request and records Codex's exit status.
        process = subprocess.Popen([sys.executable, "-I", str(Path(__file__).resolve()), "supervise", connection],
                                   stdin=prompt, stdout=log, stderr=subprocess.STDOUT, cwd=cwd, env=env,
                                   start_new_session=True, umask=_ORIGINAL_UMASK if _ORIGINAL_UMASK is not None else -1)
    return process.pid


def send(request):
    connection = identifier(request["connection"])
    request_id = identifier(request["request_id"])
    text = request["text"]
    if not isinstance(text, str) or not text.strip() or len(text.encode()) > PROMPT_LIMIT:
        raise ValueError("消息为空或过长")
    account = request.get("account") or "default"
    home = codex_home(account)
    thread = identifier(request["thread"]) if request.get("thread") else ""
    cwd = Path(request["cwd"])
    folder = ROOT / connection
    with admission(), state_file(connection, strict=True) as state:
        settle_lease(connection, state, rollout_users())
        known = state.get("thread_id") or started_thread(folder)
        if known and thread and known != thread:
            raise ValueError("会话身份已变化，请重新读取会话列表")
        thread = thread or known
        if state.get("account", account) != account:
            raise ValueError("会话账号已变化，请重新读取会话列表")
        digest = hashlib.sha256(text.encode()).hexdigest()
        requests = state.setdefault("requests", {})
        if request_id in requests:
            if requests[request_id] != digest:
                raise ValueError("同一发送编号已用于另一条消息")
            return {"ok": True}
        previous = state.get("run") or {}
        if alive(previous, connection):
            raise ValueError("这段会话已有消息正在处理，请等待完成后再发送")
        # A desktop terminal's Codex owns the thread while its lease holds.
        lease = state.get("lease")
        if isinstance(lease, dict):
            if leased(lease):
                raise ValueError(DESKTOP_OPEN)
            state.pop("lease")
        path = None
        if thread:
            path = find_rollout(home, thread)
            if path is None:
                raise ValueError("未找到这段 Codex 会话的历史，请重新读取会话列表")
            others = all_states(connection)
            if desktop_leases(thread, account, path, others):
                raise ValueError(DESKTOP_OPEN)
            if phone_turns(thread, others):
                raise ValueError("这段会话已有消息正在处理，请等待完成后再发送")
            if foreign(path, thread, rollout_users(), set()):
                raise ValueError(TUI_OPEN)
        if not cwd.is_absolute() or not cwd.is_dir():
            raise ValueError("会话目录不可用，请确认项目文件夹已连接")
        args = ["exec", "--json", "--skip-git-repo-check", "-C", str(cwd)]
        for directory in request.get("additional_directories") or []:
            if not isinstance(directory, str) or not Path(directory).is_absolute() or not Path(directory).is_dir():
                raise ValueError("项目文件夹未连接，请重新打开项目")
            args += ["--add-dir", directory]
        args += ["resume", thread, "-"] if thread else ["-"]
        env = {key: value for key, value in os.environ.items() if not key.startswith("CAGE_")}
        env["PATH"] = os.pathsep.join([str(Path.home() / ".local/bin"), env.get("PATH", os.defpath)])
        if account == "default":
            binary = shutil.which("codex", path=env["PATH"])
            if not binary:
                raise ValueError("未找到 Codex CLI。请先在环境中安装 Codex 后重试。")
            argv = [binary, *args]
        else:
            # The account helper verifies the signed-in identity and runs the
            # official CLI with the account's own CODEX_HOME.
            subject = request.get("subject") or ""
            if not re.fullmatch(r"[0-9a-f]{64}", subject):
                raise ValueError("Codex 账号状态已变化，请重试")
            if not ACCOUNT_HELPER.is_file():
                raise ValueError("Super Cage 环境组件需要更新，请在电脑更新环境组件后重试")
            argv = [str(ACCOUNT_HELPER), "run", account, *args]
            env["CAGE_CODEX_SUBJECT"] = subject
        backup = json.loads(json.dumps(state))
        state.update(thread_id=thread, account=account, cwd=str(cwd))
        requests[request_id] = digest
        for stale in list(requests)[:-256]:
            requests.pop(stale)
        sent = state.setdefault("sent", [])
        sent.append({"id": request_id, "text": text, "key": prompt_key(text), "offset": path.stat().st_size if path else 0})
        del sent[:-64]
        state["run"] = {"request_id": request_id, "argv": argv, "started_at": time.time()}
        state.pop("stopped", None)
        # Admission is durable before Codex can read the prompt, so a lost
        # acknowledgement never starts the same request twice.
        save(folder / "state.json", state)
        try:
            pid = launch(folder, connection, text, cwd, env)
        except OSError:
            state.clear()
            state.update(backup)
            save(folder / "state.json", state)
            raise ValueError("无法启动 Codex，请检查环境后重试")
        state["run"].update(pid=pid, start=fingerprint(pid))
    return {"ok": True}


def supervise(connection):
    for name in ("SIGINT", "SIGTERM", "SIGHUP"):
        # Stop signals the whole group. Outlive Codex to record its exit.
        signal.signal(getattr(signal, name), lambda *_: None)
    with state_file(connection) as state:
        run = dict(state.get("run") or {})
    if not run.get("argv"):
        return 1
    try:
        code = subprocess.call(run["argv"])
    except OSError:
        code = 127
    thread = started_thread(ROOT / identifier(connection))
    with state_file(connection) as state:
        current = state.get("run") or {}
        if current.get("request_id") == run.get("request_id"):
            current.update(exit_code=code, finished_at=time.time())
        if thread and not state.get("thread_id"):
            state["thread_id"] = thread
    return 0


def stop(request):
    connection = identifier(request["connection"])
    # Archiving stops whatever phone turn runs, without knowing its request.
    archive = request.get("reason") == "archive" and not request.get("request_id")
    request_id = "" if archive else identifier(request["request_id"])
    if not (ROOT / connection).is_dir():
        if archive:
            return {"ok": True}
        raise ValueError("这次运行不是从手机发起的，请在电脑终端停止")
    with state_file(connection) as state:
        run = state.get("run") or {}
        current = run_state(run, connection)
        if archive:
            if current == "ended":
                return {"ok": True}
            request_id = run.get("request_id") or ""
        if run.get("request_id") != request_id or current == "ended":
            raise ValueError("这次运行已结束或不是从手机发起的，请重新读取会话")
        if current != "running":
            # Only a process it can identify is signalled: a reused PID could
            # belong to anyone.
            raise ValueError("无法确认本轮在电脑上的进程，请稍后重试")
        state["stopped"] = request_id
    # Signal outside the lock: the supervisor needs it to record the exit.
    for number, wait in ((signal.SIGINT, 5), (signal.SIGTERM, 2), (signal.SIGKILL, 1)):
        try:
            os.killpg(run["pid"], number)
        except ProcessLookupError:
            break
        deadline = time.monotonic() + wait
        while time.monotonic() < deadline and run_state(run, connection) == "running":
            time.sleep(0.05)
        if run_state(run, connection) != "running":
            break
    return {"ok": True}


def purge(request):
    deadline = time.monotonic() + PURGE_BUDGET
    account = request.get("account") or "default"
    home = codex_home(account)
    thread = identifier(request["thread"]) if request.get("thread") else ""
    connection = identifier(request["connection"]) if request.get("connection") else ""
    with admission():
        return purge_admitted(request, account, home, thread, connection, deadline)


def purge_admitted(request, account, home, thread, connection, deadline):
    """purge under the admission lock: no phone turn or desktop TUI is admitted
    on the thread while it checks and deletes."""
    state, readable = load_state(ROOT / connection / "state.json") if connection else ({}, True)
    if not readable:
        raise ValueError(STATE_UNREADABLE)
    if alive(state.get("run"), connection or None):
        raise ValueError("Codex 仍在处理这段会话，请先停止本轮再删除")
    if leased(state.get("lease")):
        raise ValueError("这段会话正在电脑的 Codex 终端中打开，请先关闭该终端再删除")
    thread = thread or state.get("thread_id", "")
    path = find_rollout(home, thread) if thread else None
    if path is not None:
        meta = head(path)
        if not meta or meta["id"] != thread or not within(meta["cwd"], request["workspace"]):
            raise ValueError("会话历史与已授权项目不一致，请检查电脑上的 Codex 配置")
        others = all_states(connection)
        if desktop_leases(thread, account, path, others):
            raise ValueError("这段会话正在电脑的 Codex 终端中打开，请先关闭该终端再删除")
        if phone_turns(thread, others):
            raise ValueError("Codex 仍在处理这段会话，请先停止本轮再删除")
        if foreign(path, thread, rollout_users(), set()):
            raise ValueError("这段会话正在电脑上的 Codex 终端中打开，请先退出后再删除")
        # Codex's own delete also removes compressed history, metadata and
        # spawned sub-agent threads; the file is removed here if it remains.
        codex_delete(account, home, thread, deadline)
        if path.exists():
            path.unlink()
    if connection:
        shutil.rmtree(ROOT / connection, ignore_errors=True)
    return {"ok": True}


def cli_environment():
    env = {key: value for key, value in os.environ.items() if not key.startswith("CAGE_")}
    env["PATH"] = os.pathsep.join([str(Path.home() / ".local/bin"), env.get("PATH", os.defpath)])
    return env


def help_commands(text):
    """Subcommand names listed under "Commands:" in a --help text."""
    names, inside = set(), False
    for line in text.splitlines():
        if line.strip() == "Commands:":
            inside = True
            continue
        if inside:
            if not line.strip():
                break
            match = re.match(r"  ([a-z][a-z0-9-]*)(?:\s|$)", line)
            if match:
                names.add(match.group(1))
    return names


def has_option(text, name):
    """Whether a --help text lists the option name as a whole token: "--cd" is
    not "--cdp-port"."""
    return re.search(r"(?:^|[\s,\[(])" + re.escape(name) + r"(?=$|[\s,=<\[\])])", text, re.M) is not None


def probe_binary(binary, env, deadline=None):
    """What this Codex CLI supports, from its own help. Cached per binary, and
    only when every help it read answered in time: a timeout is not taken for
    an option the CLI lacks. deadline (monotonic) bounds the whole probe."""
    target = os.path.realpath(binary)
    info = os.stat(target)
    key = [target, info.st_size, info.st_mtime_ns, PROBE_SCHEMA]
    cached = read_json(ROOT / "probe.json", {}) or {}
    if cached.get("key") == key and isinstance(cached.get("result"), dict):
        return dict(cached["result"])

    def output(*args):
        timeout = PROBE_TIMEOUT if deadline is None else min(PROBE_TIMEOUT, deadline - time.monotonic())
        if timeout <= 0:
            return None
        try:
            done = subprocess.run([binary, *args], stdin=subprocess.DEVNULL, capture_output=True, text=True, errors="replace",
                                  timeout=timeout, env=env, cwd=str(Path.home()))
        except (OSError, subprocess.SubprocessError):
            return None
        return done.stdout + done.stderr if done.returncode == 0 else ""

    version, top = output("--version"), output("--help")
    if version is None or top is None:
        # Not cached: a slow or broken start is retried next time.
        return dict(UNPROBED, installed=True)
    found = re.search(r"(\d+\.\d+\.\d+[0-9A-Za-z.+-]*)", version or "")
    commands = help_commands(top)
    result = dict(UNPROBED, installed=True, complete=True, version=found.group(1) if found else "", resume="resume" in commands,
                  no_daemon=has_option(top, "--no-daemon"), cd=has_option(top, "--cd"), add_dir=has_option(top, "--add-dir"))
    answered = True
    if result["resume"]:
        options = output("resume", "--help")
        answered = answered and options is not None
        options = options or ""
        result.update(resume_no_daemon=has_option(options, "--no-daemon"), resume_cd=has_option(options, "--cd"), resume_add_dir=has_option(options, "--add-dir"))
    if "delete" in commands:
        options = output("delete", "--help")
        answered = answered and options is not None
        result["delete"] = has_option(options or "", "--force")
    if not answered:
        return dict(result, complete=False)
    ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    save(ROOT / "probe.json", {"key": key, "result": result})
    return result


def probe(request):
    env = cli_environment()
    binary = shutil.which("codex", path=env["PATH"])
    if not binary:
        return dict(UNPROBED, complete=True)
    result = probe_binary(binary, env)
    result["writer_locks"] = (codex_home("default") / "thread-writer-locks").is_dir()
    return result


def codex_delete(account, home, thread, deadline=None):
    """Delete a thread through the CLI when it supports deleting by UUID, by
    deadline (monotonic) when one is given."""
    env = cli_environment()
    binary = shutil.which("codex", path=env["PATH"])
    if not binary or not probe_binary(binary, env, deadline).get("delete"):
        return False
    env.pop("CODEX_HOME", None)
    if account != "default" or os.environ.get("CODEX_HOME"):
        env["CODEX_HOME"] = str(home)
    timeout = 20 if deadline is None else deadline - time.monotonic()
    if timeout <= 0:
        return False
    try:
        return subprocess.run([binary, "delete", "--force", thread], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              timeout=timeout, env=env, cwd=str(Path.home())).returncode == 0
    except (OSError, subprocess.SubprocessError):
        return False


def tui_args(caps, thread, cwd, directories):
    """The Codex TUI's arguments. Each optional flag needs the probe to have
    seen it in this CLI's help: without -C the TUI runs from cwd, which the
    launcher changes to, and without --add-dir it gets the project only."""
    resume = bool(thread)
    options = []
    if caps.get("resume_cd" if resume else "cd"):
        options += ["-C", str(cwd)]
    if caps.get("resume_add_dir" if resume else "add_dir"):
        for directory in directories:
            options += ["--add-dir", directory]
    # A shared background server could keep the thread loaded after the TUI
    # exits; a TUI of its own ends its writes when it ends.
    if caps.get("resume_no_daemon" if resume else "no_daemon"):
        options.append("--no-daemon")
    return ["resume", *options, thread] if resume else options


def tui_launch(request):
    """Start a desktop terminal's Codex TUI under a lease on its connection. The
    lease records this process, which becomes Codex through exec, so it lasts
    exactly as long as that Codex runs. Never returns when Codex starts."""
    connection = identifier(request["connection"])
    lease_id = identifier(request["lease"])
    account = request.get("account") or "default"
    home = codex_home(account)
    thread = identifier(request["thread"]) if request.get("thread") else ""
    cwd = Path(request.get("cwd") or "")
    if not cwd.is_absolute() or not cwd.is_dir():
        raise ValueError("会话目录不可用，请确认项目文件夹已连接")
    directories = request.get("additional_directories") or []
    for directory in directories:
        if not isinstance(directory, str) or not Path(directory).is_absolute() or not Path(directory).is_dir():
            raise ValueError("项目文件夹未连接，请重新打开项目")
    env = {key: value for key, value in os.environ.items() if key not in ("CAGE_CODEX_SUBJECT", "CAGE_CODEX_ACCOUNT_ID")}
    env["PATH"] = os.pathsep.join([str(Path.home() / ".local/bin"), env.get("PATH", os.defpath)])
    binary = shutil.which("codex", path=env["PATH"])
    if not binary:
        raise ValueError("未找到 Codex CLI。请先在环境中安装 Codex 后重试。")
    caps = probe_binary(binary, cli_environment())
    subject = request.get("subject") or ""
    if account != "default":
        if not re.fullmatch(r"[0-9a-f]{64}", subject):
            raise ValueError("Codex 账号状态已变化，请重试")
        if not ACCOUNT_HELPER.is_file():
            raise ValueError("Super Cage 环境组件需要更新，请在电脑更新环境组件后重试")
    start = fingerprint(os.getpid())
    if not start:
        raise ValueError("无法确认终端进程，请关闭此终端后重试")
    with admission():
        users = rollout_users()
        with state_file(connection, strict=True, recover=unreadable_recovery(connection, thread, home, users)) as state:
            thread = admit_tui(connection, state, users, lease_id, account, home, thread, cwd, start, caps)
    args = tui_args(caps, thread, cwd, directories)
    if account == "default":
        argv = [binary, *args]
    else:
        argv = [str(ACCOUNT_HELPER), "run", account, *args]
        env["CAGE_CODEX_SUBJECT"] = subject
    os.umask(_ORIGINAL_UMASK if _ORIGINAL_UMASK is not None else 0o022)
    os.chdir(cwd)
    try:
        os.execve(argv[0], argv, env)
    except OSError:
        raise ValueError("无法启动 Codex，请检查环境后重试")


def admit_tui(connection, state, users, lease_id, account, home, thread, cwd, start, caps):
    """tui_launch's checks and lease, under the admission and connection locks.
    Returns the thread the TUI resumes ("" for a new one)."""
    settle_lease(connection, state, users)
    if state.get("account", account) != account:
        raise ValueError("会话账号已变化，请重新读取会话列表")
    known = state.get("thread_id") or started_thread(ROOT / connection)
    if known and thread and known != thread:
        raise ValueError("会话身份已变化，请重新读取会话列表")
    thread = thread or known
    if alive(state.get("run"), connection):
        raise ValueError(PHONE_RUNNING)
    if leased(state.get("lease")):
        raise ValueError(OTHER_TERMINAL)
    others = all_states(connection)
    if thread:
        path = find_rollout(home, thread)
        packed = None if path else compressed_path(home, thread)
        if path is None and packed is None:
            raise ValueError("未找到这段 Codex 会话的历史，可能已在电脑上删除。")
        if phone_turns(thread, others):
            raise ValueError(PHONE_RUNNING)
        if desktop_leases(thread, account, path, others):
            raise ValueError(OTHER_TERMINAL)
        if foreign(path or packed, thread, users, set()):
            raise ValueError(OTHER_PROCESS)
        if not caps.get("complete"):
            # Resuming on a CLI without resume would start a new thread
            # with the thread ID as its prompt.
            raise ValueError("暂时无法确认电脑上的 Codex 版本，请稍后重试。")
        if not caps.get("resume"):
            raise ValueError("电脑上的 Codex 版本不支持继续会话，请更新 Codex 后重试。")
    state.update(account=account, cwd=str(cwd))
    if thread:
        state["thread_id"] = thread
    state["lease"] = {"id": lease_id, "pid": os.getpid(), "start": start, "pgid": os.getpgid(0), "thread": thread,
                      "account": account, "cwd": str(cwd), "acquired_at": time.time()}
    return thread


def main():
    global _ORIGINAL_UMASK
    _ORIGINAL_UMASK = os.umask(0o077)
    mode, argument = sys.argv[1:3]
    if mode == "supervise":
        os.umask(_ORIGINAL_UMASK)
        return supervise(argument)
    request = json.load(sys.stdin) if argument == "-" else json.loads(argument)
    if mode == "tui-launch":
        # Runs in a desktop terminal: refusals are shown there and end it.
        try:
            tui_launch(request)
        except ValueError as error:
            message = str(error)
        except (OSError, KeyError, TypeError, subprocess.SubprocessError):
            message = "电脑未能启动 Codex，请稍后重试。"
        sys.stderr.buffer.write(("\r\n" + message + "\r\n").encode())
        sys.stderr.flush()
        return LAUNCH_REFUSED
    operations = {"discover": discover, "view": view, "send": send, "stop": stop, "purge": purge, "probe": probe}
    if mode not in operations:
        raise ValueError("unsupported operation")
    try:
        result = operations[mode](request)
    except ValueError as error:
        result = {"error": str(error)}
    except (OSError, KeyError, TypeError, subprocess.SubprocessError):
        result = {"error": "电脑未能处理 Codex 会话请求，请稍后重试。"}
    print(json.dumps(result, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
