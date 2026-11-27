"""Keep controller state outside the environment passed to external programs."""
from contextlib import contextmanager
import hashlib
from itertools import chain, groupby
import json
import os
from pathlib import Path
import re
import runpy
import shlex
import stat
import subprocess
import sys
import tempfile
import time

_KEYS = ("CAGE_CLI_STATE_ROOT", "CAGE_CLI_ACCOUNT_ROOT", "CAGE_CLI_SHARED_ROOT",
         "CAGE_CLI_ACCOUNT_HELPER", "CAGE_CLI_BINARY", "CAGE_CLAUDE_SUBJECT", "CAGE_REMOTE_APP_CONTAINER")

_context = {**os.environ, **globals().get("_controller_context", {})}
_remote_windows = os.name == "nt" and _context.get("CAGE_REMOTE_APP_CONTAINER") == "1"
# Python 3.13's Windows 0700 ACL omits the package SID. Inherit the existing
# package-private ACL instead; this does not grant access to any other identity.
PRIVATE_MODE = 0o777 if _remote_windows else 0o700


@contextmanager
def launch_guard(base, *, login=False, track=True):
    """Coordinate a guest-shell launch with a managed login in the same profile."""
    if os.name == "nt":
        yield
        return
    import fcntl
    base.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (base / ".launch-lock").open("a") as guard:
        fcntl.flock(guard, (fcntl.LOCK_EX if login else fcntl.LOCK_SH) | fcntl.LOCK_NB)
        if login or not track:
            if not login:
                os.set_inheritable(guard.fileno(), True)
            yield
            return
        # The lease survives exec without a supervising process, so PTYs,
        # stdin and the child exit code remain the SSH client's.
        folder = base / ".plain-launches"
        folder.mkdir(mode=0o700, exist_ok=True)
        with tempfile.NamedTemporaryFile(dir=folder, prefix=".lease-", delete=False) as lease:
            pending = Path(lease.name)
            running = folder / f"{os.getpid()}-{pending.name[len('.lease-'):]}"
            try:
                fcntl.flock(lease, fcntl.LOCK_EX)
                pending.rename(running)
                os.set_inheritable(lease.fileno(), True)
                os.set_inheritable(guard.fileno(), True)
                yield
            finally:
                pending.unlink(missing_ok=True)
                running.unlink(missing_ok=True)


def plain_launch_count(base):
    if os.name == "nt":
        return 0
    import fcntl
    count = 0
    for marker in (base / ".plain-launches").glob("[0-9]*-*"):
        pid = marker.name.partition("-")[0]
        if not pid.isdigit():
            continue
        try:
            os.kill(int(pid), 0)
            fd = os.open(marker, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            try:
                fcntl.flock(fd, fcntl.LOCK_SH | fcntl.LOCK_NB)
            finally:
                os.close(fd)
        except BlockingIOError:
            count += 1
            continue
        except ProcessLookupError:
            pass
        except (OSError, OverflowError):
            continue
        try:
            marker.unlink(missing_ok=True)
        except OSError:
            pass
    return count


def _schedule_id(value):
    return value if isinstance(value, str) and 0 < len(value) <= 128 and value.isprintable() else ""


def selected_account(schedule):
    if "selected_account_id" in schedule:
        return _schedule_id(schedule["selected_account_id"])
    if schedule.get("mode") == "exhaust":
        for item in schedule.get("accounts", []):
            if isinstance(item, dict) and item.get("preferred") is True:
                return _schedule_id(item.get("id"))
    return ""


def native_schedule_managed(schedule):
    return (schedule.get("managed") is not False or bool(schedule["accounts"])
            or schedule.get("mode") == "manual"
            or schedule.get("mode") == "exhaust" and bool(selected_account(schedule)))


def exhaust_cursor(directory, schedule):
    if schedule.get("mode") != "exhaust":
        return {}
    try:
        cursor = json.loads((directory / ".auto-exhaust.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    if not isinstance(cursor, dict) or not _schedule_id(cursor.get("account_id")):
        return {}
    selected, revision = selected_account(schedule), schedule.get("selected_account_revision", "")
    if cursor.get("selected_account_id") != selected or cursor.get("selected_account_revision") != revision:
        return {}
    if cursor["account_id"] not in _account_order(schedule, []):
        return {}
    return {"account_id": cursor["account_id"], "selected_account_id": selected, "selected_account_revision": revision}


def _write_schedule_state(path, text):
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            stream.write(text)
            stream.flush()
            temporary.replace(path)
        finally:
            temporary.unlink(missing_ok=True)


def _project_pin(pins, cwd):
    if not isinstance(pins, dict) or cwd is None:
        return ""
    matched = ""
    longest = -1
    for workspace, target in pins.items():
        if not isinstance(workspace, str) or not workspace or not isinstance(target, str):
            continue
        kind, _, value = target.partition(":")
        if kind not in ("account", "group") or not _schedule_id(value):
            continue
        try:
            scope = Path(workspace).resolve()
        except (OSError, RuntimeError, ValueError):
            continue
        if len(scope.parts) > longest and (scope == cwd or scope in cwd.parents):
            matched, longest = target, len(scope.parts)
    return matched


def scheduling_headroom(quota, now, *, identity=None, windows=("five_hour", "seven_day")):
    """Return None for confirmed exhaustion and -1 for unknown headroom."""
    if not isinstance(quota, dict) or (identity is not None and quota.get("identity") != identity):
        return -1
    remaining = []
    for name in windows:
        window = quota.get(name)
        if not isinstance(window, dict):
            continue
        used, reset = window.get("used_percentage"), window.get("resets_at", 0)
        observed = window.get("observed_at") or quota.get("observed_at", 0)
        if type(used) not in (int, float) or not 0 <= used <= 100 or type(reset) is not int or reset < 0:
            continue
        if type(observed) is not int or not 0 < observed <= now or 0 < reset <= now:
            continue
        fresh = now - observed <= 15 * 60
        if used == 100 and (reset > now or fresh):
            return None
        if fresh:
            remaining.append(100 - used)
    return min(remaining, default=-1)


def merge_scheduling_quota(published, live):
    """Combine the newest observations from the controller and the local CLI."""
    merged = {}
    for quota in (published, live):
        if not isinstance(quota, dict):
            continue
        for name in ("five_hour", "seven_day"):
            window = quota.get(name)
            if not isinstance(window, dict):
                continue
            seen = window.get("observed_at") or quota.get("observed_at", 0)
            if type(seen) is int and seen > merged.get(name, {}).get("observed_at", 0):
                merged[name] = {**window, "observed_at": seen}
    return merged


def _nonnegative_count(value):
    return value if type(value) is int and value > 0 else 0


def launch_load(base, active, capacity):
    load = _nonnegative_count(active) + plain_launch_count(base)
    cap = _nonnegative_count(capacity)
    return None if cap and load >= cap else load


def native_launch_candidates(schedule, profile, read_quota, *, allow_default=False, windows=("five_hour", "seven_day"), now=None):
    now = int(time.time()) if now is None else now
    active = schedule.get("active")
    active = active if isinstance(active, dict) else {}
    candidates = []
    for item in schedule["accounts"]:
        if not isinstance(item, dict):
            continue
        account, expected = item.get("id"), item.get("subject")
        if not isinstance(account, str) or not (allow_default and account == "default" or re.fullmatch("[a-f0-9]{24}", account)):
            continue
        if not isinstance(expected, str) or not re.fullmatch("[a-f0-9]{64}", expected):
            continue
        base = profile(account)
        room = scheduling_headroom(read_quota(base, expected), now, identity=expected, windows=windows)
        if room is None or _nonnegative_count(item.get("cooldown_until")) > now:
            continue
        load = launch_load(base, active.get(account), item.get("max_concurrency"))
        if load is None:
            continue
        candidates.append({**item, "headroom": room, "load": load})
    return candidates


def _account_order(schedule, candidates):
    order = schedule.get("account_order")
    raw = schedule.get("accounts", schedule.get("candidates", []))
    ids = [_schedule_id(value) for value in order] if isinstance(order, list) else []
    if isinstance(raw, list):
        ids.extend(_schedule_id(item.get("id")) for item in raw if isinstance(item, dict))
    ids.extend(item["id"] for item in candidates)
    return list(dict.fromkeys(value for value in ids if value))


def launch_order(directory, schedule, candidates, mode, pin, *, legacy_rr=False):
    kind, _, value = pin.partition(":")
    if kind == "account":
        field = "id" if value in _account_order(schedule, candidates) else "guest_id"
        return [item for item in candidates if value == item.get(field)]
    if kind == "group":
        candidates = [item for item in candidates if item.get("handoff_group") == value]
    if mode == "manual" and not pin:
        selected = selected_account(schedule)
        return [item for item in candidates if item["id"] == selected]
    if mode in ("manual", "exhaust"):
        order = _account_order(schedule, candidates)
        cursor = exhaust_cursor(directory, schedule).get("account_id") if not pin else None
        seed = (cursor or selected_account(schedule)) if not pin else ""
        if seed in order:
            index = order.index(seed)
            order = order[index:] + order[:index]
        positions = {account: index for index, account in enumerate(order)}
        return sorted(candidates, key=lambda item: positions[item["id"]])

    if mode == "smart" and schedule.get("ranking_mode") in ("balanced", "quota_first", "drain", "sticky_project"):
        mode = schedule["ranking_mode"]
    legacy_balanced = mode == "balanced" and legacy_rr
    if mode not in ("smart", "balanced"):
        return sorted(candidates, key=lambda item: item.get("preferred") is not True)

    def rank(item):
        if legacy_balanced:
            return ()
        score = item.get("score")
        score = score if type(score) in (int, float) and -1e6 <= score <= 1e6 else 0
        return (item.get("preferred") is not True, item["load"], -item["headroom"], -score)

    ranked = sorted(candidates, key=rank)
    if (mode != "smart" and not legacy_balanced) or pin:
        return ranked
    try:
        previous = (directory / ".auto-rr").read_text(encoding="utf-8").strip()
    except (OSError, ValueError):
        previous = ""
    ordered = []
    for _, tier in groupby(ranked, key=lambda item: rank(item)[:3]):
        members = list(tier)
        # Host baseline scores can already include an ordering tiebreaker.
        # Rotate the similarly ranked accounts within each load/quota tier.
        close = len(members) if legacy_balanced else sum(rank(item)[3] - rank(members[0])[3] <= 5 for item in members)
        for index, item in enumerate(members[:close]):
            if previous == (item.get("guest_id") or item["id"]):
                members = members[index + 1:close] + members[:index + 1] + members[close:]
                break
        ordered.extend(members)
    return ordered


def admit_account_launch(directory, read_schedule, candidates, profile, start, *, default_mode="balanced", legacy_rr=False):
    """Hold one admission through selection, identity verification and exec."""
    import fcntl
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    # This descriptor is close-on-exec. Only the account's lease and guard
    # survive in the CLI, allowing other sessions to enter admission.
    fd = os.open(directory / ".auto-admission", os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        schedule = read_schedule()
        if not isinstance(schedule, dict):
            raise ValueError("Invalid account schedule")
        mode = schedule.get("mode") or default_mode
        try:
            cwd = Path.cwd().resolve()
        except (OSError, RuntimeError):
            cwd = None
        pin = _project_pin(schedule.get("project_pins"), cwd)
        pool = launch_order(directory, schedule, candidates(schedule), mode, pin, legacy_rr=legacy_rr)
        for item in pool:
            state_path, previous = None, None

            def before_exec():
                nonlocal state_path, previous
                if pin:
                    return
                if mode == "exhaust":
                    path = directory / ".auto-exhaust.json"
                    value = json.dumps({"account_id": item["id"], "selected_account_id": selected_account(schedule),
                                        "selected_account_revision": schedule.get("selected_account_revision", "")},
                                       separators=(",", ":"))
                elif mode == "smart" or mode == "balanced" and legacy_rr:
                    path = directory / ".auto-rr"
                    value = item.get("guest_id") or item["id"]
                else:
                    return
                try:
                    previous = path.read_text(encoding="utf-8")
                except (FileNotFoundError, UnicodeError):
                    previous = None
                if value == previous:
                    return
                _write_schedule_state(path, value)
                state_path = path

            try:
                with launch_guard(profile(item.get("guest_id") or item["id"])):
                    start(schedule, item, before_exec)
            except (OSError, ValueError, subprocess.SubprocessError, TypeError):
                pass
            finally:
                # A successful exec never returns here. Treat an exec error as
                # an unsuccessful admission and keep the old cursor.
                if state_path is not None:
                    if previous is None:
                        state_path.unlink(missing_ok=True)
                    else:
                        _write_schedule_state(state_path, previous)
    return mode, pin


_THREAD_ID = re.compile("[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", re.IGNORECASE)
_CODEX_ROLLOUT = re.compile(
    rf"rollout-.+-({_THREAD_ID.pattern})(?:_[^/]+)?\.jsonl(?:\.zst)?", re.IGNORECASE)
_CODEX_RESUME_VALUES = {
    "-a", "--ask-for-approval", "-c", "--config", "-C", "--cd", "-i", "--image", "-m", "--model",
    "-p", "--profile", "-s", "--sandbox", "--add-dir", "--disable", "--enable", "--local-provider",
    "--output-last-message", "-o", "--output-schema", "--remote", "--remote-auth-token-env", "--thread-source",
}
_CODEX_GLOBAL_SWITCHES = {
    "--approve-for-me", "--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust",
    "--no-alt-screen", "--no-daemon", "--oss", "--search", "--strict-config", "--worktree",
}


def codex_subcommand(argv):
    """Skip leading global config overrides such as Orca's -c setting."""
    args = iter(argv)
    for arg in args:
        if arg in _CODEX_RESUME_VALUES:
            next(args, None)
        elif arg.partition("=")[0] in _CODEX_RESUME_VALUES or arg in _CODEX_GLOBAL_SWITCHES:
            continue
        else:
            return (arg, *args)
    return ()


def codex_explicit_remote(argv):
    options = argv[:argv.index("--")] if "--" in argv else argv
    return any(arg == "--remote" or arg.startswith("--remote=") for arg in options)


def codex_history_request(argv):
    """Return None for another operation, empty for the picker, or the requested ID."""
    args = codex_subcommand(argv)
    if args[:1] == ("exec",):
        args = codex_subcommand(args[1:])
    if not args or args[0] not in ("resume", "unarchive"):
        return None
    args = args[1:]
    options = args[:args.index("--")] if "--" in args else args
    if any(arg in ("-h", "--help", "-V", "--version") for arg in options) or codex_explicit_remote(argv):
        return None
    if "--last" in options:
        return ""
    args = iter(args)
    for arg in args:
        if arg == "--":
            arg = next(args, "")
        elif arg in _CODEX_RESUME_VALUES:
            next(args, None)
            continue
        elif arg.startswith("-"):
            continue
        return arg.lower() if _THREAD_ID.fullmatch(arg) else ""
    return ""


def history_entries(folder, *, directories=False):
    """Enumerate only regular local entries. Vendor state and transcripts stay in place."""
    try:
        if folder.is_symlink():
            return
        with os.scandir(folder) as entries:
            for entry in entries:
                matches = entry.is_dir if directories else entry.is_file
                if matches(follow_symlinks=False):
                    yield Path(entry.path)
    except OSError:
        return


def claude_history(home, thread):
    explicit_path = Path(os.path.abspath(thread)) if thread.endswith(".jsonl") else None
    if explicit_path is not None:
        thread = explicit_path.stem
        if not _THREAD_ID.fullmatch(thread):
            return
    for project in history_entries(home / "projects", directories=True):
        if thread:
            path = project / (thread + ".jsonl")
            if (explicit_path is None or explicit_path == path) and not path.is_symlink() and path.is_file():
                yield path
        else:
            yield from (path for path in history_entries(project) if path.suffix == ".jsonl" and _THREAD_ID.fullmatch(path.stem))


def codex_history(home, thread):
    # Native pickers exclude archived sessions. An explicit ID may still be
    # resolved there by the installed CLI, without unarchiving it here.
    sessions = home / "sessions"
    folders = (sessions, home / "archived_sessions") if thread else (sessions,)
    for folder in folders:
        for root, dirs, files in os.walk(folder, followlinks=False):
            if Path(root).is_symlink():
                dirs[:] = []
                continue
            if folder == sessions:
                depth = len(Path(root).relative_to(folder).parts)
                pattern = r"\d{4}" if depth == 0 else r"\d{2}"
                dirs[:] = [name for name in dirs if depth < 3 and re.fullmatch(pattern, name)]
                if depth != 3:
                    continue
            for name in files:
                match = _CODEX_ROLLOUT.fullmatch(name)
                if match and (not thread or match[1].lower() == thread):
                    path = Path(root) / name
                    if not path.is_symlink() and path.is_file():
                        yield path


# A guest component sync can install this module before its account helpers.
def codex_resume_thread(argv):
    return codex_history_request(argv) or ""


def codex_has_thread(home, thread):
    return next(codex_history(home, thread), None) is not None


def history_account(roster, home_for_account, history, thread, *, additional_profiles=()):
    """Choose one published profile before launching the native resume command."""
    if not isinstance(roster, list):
        raise ValueError("无法读取会话账号配置。请同步环境组件后重试。")
    profiles = {}
    conflicts = set()
    for item in roster:
        if not isinstance(item, dict):
            continue
        account, expected = item.get("id"), item.get("subject")
        if not isinstance(account, str) or not re.fullmatch(r"default|[a-f0-9]{24}", account) or \
                not isinstance(expected, str) or not re.fullmatch(r"[a-f0-9]{64}", expected):
            continue
        if account in profiles and profiles[account][0] != expected:
            conflicts.add(account)
        label = item.get("label")
        label = "".join(char for char in label[:64] if char.isprintable()) if isinstance(label, str) else ""
        profiles[account] = (expected, label or account[:8])
    candidates = []
    published = ((account, expected, label, home_for_account(account))
                 for account, (expected, label) in profiles.items() if account not in conflicts)
    for account, expected, label, home in chain(published, additional_profiles):
        # A profile or one of its parents must not redirect history outside
        # the published account's directory.
        try:
            if home.resolve() != Path.home().resolve() / home.relative_to(Path.home()):
                continue
        except (OSError, ValueError, RuntimeError):
            continue
        if next(history(home, thread), None) is None:
            continue
        candidates.append((account, expected, label))
    if not candidates or (thread and len(candidates) != 1):
        raise ValueError("无法确定此会话的原账号。请检查原账号登录状态，或从原配置目录继续。")
    if len(candidates) == 1:
        return candidates[0][:2]
    if not sys.stdin.isatty() or not sys.stderr.isatty():
        raise ValueError("多个账号有历史会话。请在交互终端选择原账号，或指定会话 ID。")
    print("选择原账号以打开其会话列表：", file=sys.stderr)
    for index, (_, _, label) in enumerate(candidates, 1):
        print(f"  {index}. {label}", file=sys.stderr)
    while True:
        print("输入账号序号（回车取消）：", file=sys.stderr, end="", flush=True)
        answer = sys.stdin.readline().strip()
        if not answer:
            return None
        if answer.isascii() and answer.isdecimal() and len(answer) <= len(str(len(candidates))):
            index = int(answer) - 1
            if 0 <= index < len(candidates):
                return candidates[index][:2]


class RuntimePathConflict(ValueError):
    def __init__(self, alias, target):
        super().__init__("Runtime path is already in use")
        self.alias, self.target = alias, target


def root():
    owner = hashlib.sha256(str(Path.home()).encode()).hexdigest()[:16]
    temporary = Path.home() / "Temp" if _remote_windows else Path(tempfile.gettempdir())
    directory = temporary / ("runtime-" + owner)
    directory.mkdir(mode=PRIVATE_MODE, exist_ok=True)
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or os.name != "nt" and (info.st_uid != os.getuid() or info.st_mode & 0o077):
        raise ValueError("Private runtime directory unavailable")
    return directory


def path(value):
    """Alias an existing owned integration path without moving its contents."""
    target = Path(value)
    if _remote_windows:
        return str(target)
    if "cage" not in str(target).lower():
        return str(target)
    if not target.exists():
        return str(Path(path(target.parent)) / target.name) if target.parent != target else str(target)
    if os.name == "nt" and target.is_file() and "cage" not in target.name.lower():
        return str(Path(path(target.parent)) / target.name)
    directory = root() / "paths"
    directory.mkdir(mode=PRIVATE_MODE, exist_ok=True)
    name = hashlib.sha256(str(target.absolute()).encode()).hexdigest()[:32]
    if target.is_file() and target.suffix.lower() in (".exe", ".dll", ".json", ".py"):
        name += target.suffix.lower()
    alias = directory / name
    if not alias.exists():
        try:
            if os.name == "nt":
                platform = runpy.run_path(Path(__file__).with_name("windows_cli_platform.py"))
                platform["resource_link"](alias, target)
            else:
                alias.symlink_to(target.absolute(), target_is_directory=target.is_dir())
        except FileExistsError:
            pass
    if alias.resolve() != target.resolve() and not (os.name == "nt" and alias.is_file() and os.path.samefile(alias, target)):
        raise RuntimePathConflict(alias, target)
    return str(alias)


def python():
    # A directory junction preserves Python's adjacent DLLs and standard library.
    return str(Path(path(Path(sys.executable).parent)) / Path(sys.executable).name)


def launcher(script, *, executable=False, arguments=()):
    context = globals().get("_controller_context", os.environ)
    internal = {key: context[key] for key in _KEYS if key in context}
    target = str(Path(script).absolute())
    body = ("#!" + python() + " -I\nimport os,runpy,sys\n" +
            ("os.execv(" + repr(target) + ", [" + repr(target) + ", *" + repr(list(arguments)) + ", *sys.argv[1:]])\n" if executable else
             "sys.argv[0]=" + repr(target) + "\nrunpy.run_path(" + repr(target) +
             ",run_name='__main__',init_globals={'_controller_context':" + repr(internal) + "})\n"))
    destination = root() / (hashlib.sha256(body.encode()).hexdigest() + ".py")
    if not destination.exists():
        with tempfile.NamedTemporaryFile(dir=destination.parent, delete=False) as stream:
            stream.write(body.encode())
            temporary = stream.name
        os.chmod(temporary, 0o700)
        os.replace(temporary, destination)
    elif destination.is_symlink() or destination.read_text() != body:
        raise ValueError("Runtime launcher is already in use")
    return str(destination)


def command(script, args=()):
    return [python(), "-I", launcher(script), *args]


def command_text(script, args=()):
    argv = command(script, args)
    return subprocess.list2cmdline(argv) if os.name == "nt" else shlex.join(argv)


def environment(values):
    clean = {key: value for key, value in values.items() if not key.upper().startswith("CAGE_")}
    if "PATH" in clean:
        clean["PATH"] = os.pathsep.join(path(entry) if entry else entry for entry in clean["PATH"].split(os.pathsep))
    for key in ("CLAUDE_CONFIG_DIR", "CLAUDE_CODE_GIT_BASH_PATH", "XDG_RUNTIME_DIR", "XAUTHORITY"):
        if clean.get(key):
            clean[key] = path(clean[key])
    browser = clean.get("BROWSER", "")
    if "cage" in browser.lower() and Path(browser).is_file():
        clean["BROWSER"] = launcher(browser, executable=True)
    for key, prefix in (("PULSE_SERVER", "unix:"), ("IBUS_ADDRESS", "unix:path=")):
        value = clean.get(key, "")
        if value.startswith(prefix):
            clean[key] = prefix + path(value[len(prefix):])
    return clean


if __name__ == "__main__":
    os.execvpe(sys.argv[1], sys.argv[1:], environment(os.environ))
