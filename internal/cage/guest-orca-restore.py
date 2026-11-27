"""Use the official CLI against an already-running Orca on this Linux guest."""
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import socket
import stat
import struct
import subprocess
import time


SCOPE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
HANDLE = re.compile(r"term_[A-Za-z0-9_-]{4,128}")
THREAD = re.compile(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}")
LIMIT = 2 << 20
AGENT_FIELDS = ("orcaSessionId", "kind", "agent", "worktreeId", "hostId", "paneKey", "state")
TERMINAL_FIELDS = ("handle", "tabId", "leafId", "worktreeId", "worktreePath", "executionHostId",
                   "incarnationId", "connected", "writable", "orphaned")


def _text(value, size):
    return isinstance(value, str) and 0 < len(value) <= size and value.isprintable()


def _matches(regex, value):
    return isinstance(value, str) and regex.fullmatch(value) is not None


def _socket_identity(data, transport):
    endpoint, identity, _ = transport(data)
    if not _matches(SCOPE, identity):
        raise ValueError("unverified Orca runtime identity")
    info = endpoint.lstat()
    with socket.socket(socket.AF_UNIX) as client:
        client.settimeout(1)
        client.connect(str(endpoint))
        credentials = client.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i"))
        pid, uid, _ = struct.unpack("3i", credentials)
        if uid != os.getuid() or pid < 1:
            raise ValueError("local Orca belongs to another user")
    return identity, info.st_dev, info.st_ino, pid


def _read_cli(binary, args, env, home, wait):
    command = [binary, *args, "--environment", "local", "--json"]
    deadline = time.monotonic() + (32 if wait else 5)
    with subprocess.Popen(command, cwd=home, env=env, stdin=subprocess.DEVNULL,
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL) as process:
        try:
            data = bytearray()
            with selectors.DefaultSelector() as ready:
                ready.register(process.stdout, selectors.EVENT_READ)
                while time.monotonic() < deadline and ready.select(max(0, deadline - time.monotonic())):
                    chunk = os.read(process.stdout.fileno(), 64 << 10)
                    if not chunk:
                        if process.wait(timeout=max(0.01, deadline - time.monotonic())) != 0:
                            raise ValueError("Orca rejected the operation")
                        value = json.loads(data)
                        if not isinstance(value, dict) or value.get("ok") is False:
                            raise ValueError("Orca did not return a valid result")
                        result = value.get("result", value)
                        if not isinstance(result, dict):
                            raise ValueError("Orca result is not an object")
                        return result
                    data.extend(chunk)
                    if len(data) >= LIMIT:
                        raise ValueError("Orca result exceeded its limit")
            raise ValueError("Orca operation timed out")
        finally:
            if process.poll() is None:
                process.kill()


def _fields(value, names):
    if not isinstance(value, dict):
        raise ValueError("invalid Orca result")
    return {key: field for key in names if ((field := value.get(key)) is not None and
            (type(field) in (bool, int) or _text(field, 1200)))}


def _rows(result, key, fields, limit, host_key):
    rows = result.get(key)
    if not isinstance(rows, list) or len(rows) >= limit or result.get("truncated"):
        raise ValueError("Orca inventory is incomplete")
    if any(not isinstance(row, dict) for row in rows):
        raise ValueError("invalid Orca inventory")
    local_rows = [row for row in rows if row.get(host_key) in (None, "", "local")]
    if key == "terminals" and any(
            not _matches(HANDLE, row.get("handle")) or not _text(row.get("worktreeId"), 1200) or
            type(row.get("connected")) is not bool for row in local_rows):
        raise ValueError("Orca terminal inventory cannot be verified")
    return {key: [_fields(row, fields) for row in local_rows]}


def _verified_worktree(run, worktree, workspace):
    row = _fields(run(["worktree", "show", "--worktree", "id:" + worktree]).get("worktree"),
                  ("id", "path", "hostId", "executionHostId"))
    if row.get("id") != worktree or row.get("path") != workspace or (
            row.get("executionHostId") or row.get("hostId") or "local") != "local":
        raise ValueError("Orca worktree is not on this guest")
    return {"worktree": row}


def execute(request, *, home, managed_data, managed_binary, search_path, transport, read_cli=_read_cli):
    """Perform only operations needed by the shared Cage recovery resolver."""
    operations = {
        "probe": set(),
        "agents": set(),
        "terminals": set(),
        "worktree": {"worktree"},
        "wait": {"handle"},
        "resume": {"worktree", "subject", "account", "thread"},
    }
    try:
        operation = request.get("operation")
        if operation not in operations or set(request) - (
                {"operation", "runtime", "runtime_id", "workspace"} | operations[operation]):
            return {"runtime_id": ""}
        runtime, expected = request.get("runtime"), request.get("runtime_id", "")
        if runtime not in ("managed", "independent") or (not expected and operation != "probe"):
            return {"runtime_id": ""}
        workspace = request.get("workspace")
        if not _text(workspace, 1024) or not workspace.startswith("/") or workspace == "/" or (
                str(Path(workspace).resolve(strict=True)) != workspace or not Path(workspace).is_dir()):
            return {"runtime_id": ""}
        data = managed_data if runtime == "managed" else home / ".config/orca"
        identity = _socket_identity(data, transport)
        if expected and expected != identity[0]:
            return {"runtime_id": ""}
        if operation == "probe":
            return {"runtime_id": identity[0]}
        binary = managed_binary if runtime == "managed" else shutil.which("orca-ide", path=search_path)
        binary = Path(binary).resolve(strict=True)
        info = binary.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, os.getuid()) or info.st_mode & 0o022 or not os.access(binary, os.X_OK):
            return {"runtime_id": ""}
        env = {"HOME": str(home), "PATH": search_path, "SHELL": "/bin/sh", "ORCA_ENVIRONMENT": "local",
               "ORCA_USER_DATA": str(data), "ORCA_USER_DATA_PATH": str(data), "DO_NOT_TRACK": "1",
               "ORCA_TELEMETRY_DISABLED": "1", "ORCA_BACKGROUND_LAUNCH": "1"}
        xdg = Path("/run/user") / str(os.getuid())
        if xdg.is_dir() and xdg.stat().st_uid == os.getuid():
            env["XDG_RUNTIME_DIR"] = str(xdg)

        def run(args):
            if _socket_identity(data, transport) != identity:
                raise ValueError("Orca runtime changed")
            result = read_cli(str(binary), args, env, home, operation == "wait")
            if _socket_identity(data, transport) != identity:
                raise ValueError("Orca runtime changed")
            return result

        worktree = request.get("worktree")
        selector = ["--worktree", "path:" + workspace]
        if "worktree" in operations[operation] and not _text(worktree, 1200):
            return {"runtime_id": ""}
        if operation == "agents":
            result = _rows(run(["agent", "list", *selector]), "agents", AGENT_FIELDS, 512, "hostId")
        elif operation == "terminals":
            result = _rows(run(["terminal", "list", *selector, "--limit", "200"]),
                           "terminals", TERMINAL_FIELDS, 200, "executionHostId")
        elif operation == "worktree":
            result = _verified_worktree(run, worktree, workspace)
        elif operation == "wait":
            handle = request.get("handle")
            if not _matches(HANDLE, handle):
                return {"runtime_id": ""}
            status = run(["terminal", "wait", "--terminal", handle, "--for", "exit", "--timeout-ms", "30000"])
            result = {"wait": _fields(status.get("wait"), ("handle", "condition", "status", "satisfied", "exitCode"))}
        else:
            subject, account, thread = (request.get(key) for key in ("subject", "account", "thread"))
            if not _matches(re.compile("[0-9a-f]{64}"), subject) or not _matches(
                    re.compile("[0-9a-f]{24}"), account) or not _matches(THREAD, thread):
                return {"runtime_id": ""}
            _verified_worktree(run, worktree, workspace)
            command = f"exec env CAGE_CODEX_SUBJECT={subject} /usr/local/bin/cage-codex-account run {account} resume {thread}"
            status = run(["terminal", "create", "--worktree", "id:" + worktree, "--command", command, "--keep-open"])
            result = {"terminal": _fields(status.get("terminal"), TERMINAL_FIELDS)}
        return {"runtime_id": identity[0], "result": result}
    except (AttributeError, OSError, RuntimeError, TypeError, ValueError, subprocess.SubprocessError):
        return {"runtime_id": ""}
