#!/usr/bin/env python3
"""Keep the native dsh TUI on its original SSH device, workspace, and profile."""
import contextlib
import ctypes
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import uuid


SESSION_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,159}")
LOG = re.compile(r"session(?:\.v([1-9][0-9]*))?\.jsonl(?:\.zstd)?")
MAX_HEADER = 65536
PROFILE = "tui"
BUNDLE = "@sagmans/dsh-tui"


def digest(value):
    return hashlib.sha256(value).hexdigest()


def ssh_owner():
    managed = os.environ.get("CAGE_DSH_SSH_DEVICE", "")
    if re.fullmatch(r"[a-zA-Z0-9_-]{16,80}", managed):
        return "cage:" + managed
    if not os.environ.get("SSH_CONNECTION") or not os.environ.get("SSH_USER_AUTH"):
        return None
    # OpenSSH supplies the authenticated public key, independent of client IP,
    # forwarding, and the client-side port. Keep only its fingerprint.
    try:
        with open(os.environ["SSH_USER_AUTH"], "rb") as auth:
            keys = {
                b" ".join(parts[1:3])
                for line in auth.read(32768).splitlines()
                if (parts := line.split()) and parts[0] == b"publickey" and len(parts) >= 3
            }
    except OSError:
        return None
    if len(keys) == 1:
        return "openssh:" + digest(keys.pop())
    return None


def native_tui(args):
    if args[:1] == [PROFILE]:
        args = args[1:]
    elif args[:2] == ["--profile", PROFILE]:
        args = args[2:]
    elif args[:1] == ["--profile=" + PROFILE]:
        args = args[1:]
    else:
        return None
    if args[:1] == ["resume"]:
        args = ["--resume", *args[1:]]
    if not args or args in (["--resume"], ["--new"]):
        return args
    if len(args) == 2 and args[0] == "--resume" and SESSION_ID.fullmatch(args[1]):
        return args
    if len(args) == 1 and args[0].startswith("--resume=") and SESSION_ID.fullmatch(args[0][9:]):
        return ["--resume", args[0][9:]]
    return None


def supported_profile(home):
    try:
        with (home / "profiles" / PROFILE / "package.json").open("rb") as handle:
            profile = json.load(handle)["dsh"]["profile"]
        return isinstance(profile["bundles"], list) and BUNDLE in profile["bundles"]
    except (OSError, ValueError, KeyError, TypeError):
        return False


def sync_dir(path):
    fd = os.open(path, os.O_DIRECTORY | os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def private_dir(path, parents=False):
    if parents:
        missing = []
        parent = path.parent
        while not parent.is_dir():
            missing.append(parent)
            parent = parent.parent
        for parent in reversed(missing):
            parent.mkdir(mode=0o700, exist_ok=True)
            sync_dir(parent.parent)
    try:
        path.mkdir(mode=0o700)
        sync_dir(path.parent)
    except FileExistsError:
        pass
    status = path.lstat()
    if not stat.S_ISDIR(status.st_mode) or status.st_uid != os.getuid() or status.st_mode & 0o077:
        raise OSError("dsh SSH state directory must be owned by this user and private")
    return path


@contextlib.contextmanager
def file_lock(path, nonblocking=False):
    fd = os.open(path, os.O_NOFOLLOW | os.O_CREAT | os.O_RDWR, 0o600)
    with os.fdopen(fd, "rb") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX | (fcntl.LOCK_NB if nonblocking else 0))
        yield


def read_state(directory):
    try:
        with (directory / "state.json").open("rb") as handle:
            record = json.load(handle)
    except FileNotFoundError:
        return None
    if not isinstance(record, dict) or record.get("version") != 1:
        raise ValueError("unrecognized dsh SSH state")
    return record


def write_state(directory, record):
    fd, filename = tempfile.mkstemp(dir=directory, prefix=".state-")
    try:
        with os.fdopen(fd, "w") as handle:
            json.dump(record, handle, separators=(",", ":"))
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(filename, directory / "state.json")
        sync_dir(directory)
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(filename)


def project_key(workspace):
    # Native JSONL backend escapes UTF-16 code units and bounds the project slug.
    workspace = re.sub(r"[/\\:]+", "/", workspace)
    data = workspace.encode("utf-16-le")
    readable = ""
    for (unit,) in struct.iter_unpack("<H", data):
        char = chr(unit)
        if char == "/":
            readable += "-"
        elif char.isascii() and (char.isalnum() or char in "._-"):
            readable += char
        else:
            readable += f"~{unit:04X}"
    return "--" + (readable.lstrip("-") or "root")[:251] + "--"


def zstd_header(handle):
    # The native log stores the header in its own bounded Zstandard frame.
    # Read only that frame, even when the same file contains conversation data.
    frame = bytearray(handle.read(5))
    if len(frame) != 5 or frame[:4] != b"\x28\xb5\x2f\xfd":
        raise ValueError("invalid dsh Zstandard header")
    flags = frame[4]
    single_segment = bool(flags & 0x20)
    size = (1 if single_segment else 0, 2, 4, 8)[flags >> 6]
    dictionary = (0, 1, 2, 4)[flags & 3]
    frame.extend(handle.read(size + dictionary + (not single_segment)))
    while True:
        block = handle.read(3)
        if len(block) != 3:
            raise ValueError("incomplete dsh Zstandard frame")
        frame.extend(block)
        info = int.from_bytes(block, "little")
        block_type = (info >> 1) & 3
        count = 1 if block_type == 1 else info >> 3
        if block_type == 3 or len(frame) + count > MAX_HEADER:
            raise ValueError("invalid dsh Zstandard block")
        frame.extend(handle.read(count))
        if info & 1:
            break
    if flags & 4:
        frame.extend(handle.read(4))
    zstd = ctypes.CDLL("libzstd.so.1")
    zstd.ZSTD_decompress.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_void_p, ctypes.c_size_t]
    zstd.ZSTD_decompress.restype = ctypes.c_size_t
    output = ctypes.create_string_buffer(MAX_HEADER)
    length = zstd.ZSTD_decompress(output, len(output), bytes(frame), len(frame))
    if length > MAX_HEADER:
        raise ValueError("unreadable dsh Zstandard header")
    return output.raw[:length]


def confirmed_session(binding, session):
    if not isinstance(session, str) or not SESSION_ID.fullmatch(session):
        return False
    root = Path(binding["home"]) / "sessions"
    workspace = binding["workspace"]
    folder = root / project_key(workspace) / session
    try:
        if folder.resolve(strict=True) != folder:
            return False
        logs = []
        for entry in folder.iterdir():
            match = LOG.fullmatch(entry.name)
            if match:
                logs.append((int(match[1] or 0), entry))
        if not logs:
            return False
        highest = max(version for version, _ in logs)
        candidates = [entry for version, entry in logs if version == highest]
        if len(candidates) != 1:
            return False
        path = candidates[0]
        fd = os.open(path, os.O_NOFOLLOW | os.O_NONBLOCK | os.O_RDONLY)
        with os.fdopen(fd, "rb", buffering=0) as handle:
            if not stat.S_ISREG(os.fstat(handle.fileno()).st_mode):
                return False
            line = zstd_header(handle) if path.suffix == ".zstd" else handle.readline(MAX_HEADER)
        if not line.endswith(b"\n") or line.count(b"\n") != 1:
            return False
        header = json.loads(line)
        return (
            isinstance(header, dict) and header.get("type") == "session"
            and header.get("id") == session and header.get("cwd") == workspace
            and header.get("version") == highest and header.get("origin") != "subagent"
        )
    except (OSError, RuntimeError, ValueError):
        return False


def report(directory, generation, request):
    if not isinstance(request, dict) or not isinstance(request.get("params"), dict):
        return {}
    params = request["params"]
    if params.get("pane_id") != generation:
        return {}
    method = request.get("method")
    if method == "pane.get":
        return {"pane": {"agent": "dsh"}}
    if method != "pane.report_agent_session" or params.get("source") != "custom:dsh-tui" or params.get("agent") != "dsh":
        return {}
    session, sequence = params.get("agent_session_id"), params.get("seq")
    if not isinstance(session, str) or not SESSION_ID.fullmatch(session) or type(sequence) is not int:
        return {}
    with file_lock(directory / "state.lock"):
        record = read_state(directory)
        if record and record.get("generation") == generation and not record["closed"] and sequence > record["sequence"]:
            record.update(session_id=session, sequence=sequence)
            write_state(directory, record)
    return {}


def serve_report(listener, directory, generation):
    conn, _ = listener.accept()
    with conn:
        conn.settimeout(0.15)
        try:
            with conn.makefile("rb") as handle:
                request = json.loads(handle.readline(MAX_HEADER))
            result = report(directory, generation, request)
            conn.sendall(json.dumps({"id": request["id"], "result": result}).encode() + b"\n")
        except (OSError, ValueError, KeyError, TypeError):
            return


def run(directory, generation, native, args):
    with file_lock(directory / "running.lock", nonblocking=True):
        with file_lock(directory / "state.lock"):
            record = read_state(directory)
            if not record or record.get("generation") != generation or record["closed"]:
                return 1
        binding = record["binding"]
        os.chdir(binding["workspace"])
        env = dict(os.environ, DSH_HOME=binding["home"], HERDR_ENV="1", HERDR_PANE_ID=generation)
        # Short private socket paths work even when the Cage home is deeply nested.
        with tempfile.TemporaryDirectory(prefix="cage-dsh-", dir="/tmp") as temporary:
            env["HERDR_SOCKET_PATH"] = temporary + "/status"
            # The Sagmans reporter also attempts a redundant Herdr CLI release.
            env["HERDR_BIN_PATH"] = "/bin/true"
            with socket.socket(socket.AF_UNIX) as listener:
                listener.bind(env["HERDR_SOCKET_PATH"])
                listener.listen()
                listener.settimeout(0.15)
                with subprocess.Popen([native, PROFILE, *args], env=env) as child:
                    while child.poll() is None:
                        try:
                            serve_report(listener, directory, generation)
                        except TimeoutError:
                            pass
                    status = child.returncode
        if status == 0:
            with file_lock(directory / "state.lock"):
                record = read_state(directory)
                if record and record.get("generation") == generation:
                    record["closed"] = True
                    write_state(directory, record)
        return status if status >= 0 else 128 - status


def state_root():
    return Path.home() / ".local/state/cage/dsh-ssh"


def automatic_binding(owner):
    root = state_root()
    if not root.exists():
        return None
    requested_home = os.environ.get("DSH_HOME")
    if requested_home:
        requested_home = str(Path(requested_home).resolve())
    active = []
    for directory in private_dir(root).iterdir():
        if not re.fullmatch(r"[0-9a-f]{64}", directory.name):
            continue
        record = read_state(private_dir(directory))
        if not record or record.get("closed"):
            continue
        binding = record.get("binding", {})
        if binding.get("owner") == owner and binding.get("profile") == PROFILE:
            active.append(binding)
    # The reconnecting device identifies the owner, but cannot identify one
    # window if it previously left more than one workspace or profile active.
    if len(active) != 1:
        return None
    binding = active[0]
    if requested_home and binding.get("home") != requested_home:
        return None
    if not os.path.isdir(binding.get("workspace", "")):
        return None
    return binding


def launch(native, argv, reconnect=False):
    args = native_tui(argv)
    owner = ssh_owner()
    tmux = shutil.which("tmux")
    if (
        sys.platform != "linux" or args is None or not owner or not tmux
        or not all(os.isatty(fd) for fd in (0, 1, 2))
        or os.environ.get("CAGE_DSH_RESTORE") == "off" or os.environ.get("HERDR_ENV") == "1"
    ):
        if reconnect:
            return 0
        os.execv(native, [native, *argv])
    if reconnect:
        binding = automatic_binding(owner)
        if binding is None:
            return 0
        dsh_home = Path(binding["home"])
    else:
        dsh_home = Path(os.environ.get("DSH_HOME") or (Path.home() / ".dsh")).resolve()
        binding = {"owner": owner, "workspace": os.getcwd(), "home": str(dsh_home), "profile": PROFILE}
    if not supported_profile(dsh_home):
        if reconnect:
            return 0
        os.execv(native, [native, *argv])
    root = private_dir(state_root(), parents=True)
    key = digest(json.dumps(binding, sort_keys=True).encode())
    directory = private_dir(root / key)
    # A separate server keeps shell death, user tmux config and other Cage
    # clients from tearing down this pane. It keeps no disk scrollback.
    env = dict(os.environ, TMUX_TMPDIR="/tmp")
    env.pop("TMUX", None)
    command = [tmux, "-L", "cage-dsh-" + key, "-f", "/dev/null"]
    with file_lock(directory / "state.lock"):
        previous = read_state(directory)
        if reconnect and (not previous or previous.get("closed")):
            return 0
        if previous and previous.get("binding") != binding:
            raise ValueError("dsh SSH session binding has changed")
        live = subprocess.run([*command, "has-session", "-t", "=dsh"], env=env, capture_output=True).returncode == 0
        if live:
            if args and (not previous or args != ["--resume", previous.get("session_id")]):
                raise ValueError("dsh is already open for this SSH device, workspace and profile; use dsh tui to reattach")
        else:
            with file_lock(directory / "running.lock", nonblocking=True):
                pass
            if not args and previous and not previous["closed"]:
                session = previous.get("session_id")
                if not confirmed_session(binding, session):
                    raise ValueError("dsh has no verified saved session here; use dsh tui --resume to select one")
                args = ["--resume", session]
            generation = uuid.uuid4().hex
            session = args[1] if len(args) == 2 and confirmed_session(binding, args[1]) else None
            record = {
                "version": 1, "binding": binding, "generation": generation,
                "session_id": session, "sequence": 0, "closed": False,
            }
            write_state(directory, record)
            size = os.get_terminal_size()
            start = [
                *command, "new-session", "-d", "-x", str(size.columns or 80),
                "-y", str(size.lines or 24), "-s", "dsh", "--",
                sys.executable, os.path.abspath(__file__), "run", str(directory), generation, native, *args,
            ]
            if subprocess.run(start, env=env).returncode:
                if previous is not None:
                    write_state(directory, previous)
                else:
                    write_state(directory, dict(record, closed=True))
                return 1
    os.execve(tmux, [*command, "attach-session", "-t", "=dsh"], env)


if __name__ == "__main__":
    try:
        if sys.argv[1] == "run":
            sys.exit(run(Path(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5:]))
        if sys.argv[1] == "reconnect":
            sys.exit(launch(sys.argv[2], [PROFILE], reconnect=True))
        sys.exit(launch(sys.argv[1], sys.argv[2:]))
    except BlockingIOError:
        sys.exit("dsh is still running for this SSH device, workspace and profile; reconnect when its terminal is ready")
    except (OSError, ValueError) as error:
        sys.exit(str(error))
