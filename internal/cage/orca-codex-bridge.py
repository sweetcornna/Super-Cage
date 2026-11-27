"""Reconcile native Orca selection with verified Cage Codex identities."""
import hashlib
import json
import os
from pathlib import Path
import re
import runpy
import shutil
import socket
import stat
import struct
import subprocess
import sys
import time
import uuid


ORCA_ID = re.compile(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}")
CAGE_ID = re.compile(r"[0-9a-f]{24}")
SUBJECT = re.compile(r"[0-9a-f]{64}")
IMPORT_CAPABILITY = "accounts.import-host-credentials.v1"


class OrcaRPCError(ValueError):
    def __init__(self, code):
        super().__init__("Orca rejected its local Codex account request")
        self.code = code


def own_directory(path, *, private=False, enclosed=False):
    try:
        info = path.lstat()
    except OSError:
        return False
    mask = 0o077 if private else (0 if enclosed else 0o022)
    return stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid() and not info.st_mode & mask


def own_home(root, directories):
    # Orca's account index and UUID directories inherit its umask. The
    # verified 0700 data root shields those intermediates from other users.
    enclosed = own_directory(root, private=True)
    for index, name in enumerate(directories):
        root = root / name
        if not own_directory(root, private=index == len(directories) - 1, enclosed=enclosed):
            return None
        enclosed = enclosed or own_directory(root, private=True)
    return root


def runtime_transport(data):
    if not own_directory(data, private=True) or data.resolve() != data:
        raise ValueError("Orca runtime data is not private")
    fd = os.open(data / "orca-runtime.json", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ValueError("Orca runtime metadata is not private")
        metadata = json.loads(stream.read(64 * 1024))
    transports = metadata.get("transports", [metadata.get("transport")])
    endpoint = next((item.get("endpoint") for item in transports
                     if isinstance(item, dict) and item.get("kind") == "unix"), None)
    runtime_id, token = metadata.get("runtimeId"), metadata.get("authToken")
    if not isinstance(endpoint, str) or not isinstance(runtime_id, str) or not isinstance(token, str) or not token:
        raise ValueError("Orca local account RPC is unavailable")
    endpoint = Path(endpoint)
    if not endpoint.is_absolute() or not own_directory(endpoint.parent, private=True):
        raise ValueError("Orca runtime socket is not private")
    info = endpoint.lstat()
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
        raise ValueError("Orca runtime socket is not owned by this user")
    pid = metadata.get("pid")
    if sys.platform == "linux" and type(pid) is int and pid > 0:
        if Path(f"/proc/{pid}").stat().st_uid != os.getuid():
            raise ValueError("Orca runtime belongs to another user")
    return endpoint, runtime_id, token


def local_rpc(data, method, params, expected_runtime_id=None):
    # The installed Orca CLI uses newline-delimited authenticated JSON over
    # the runtime's private Unix socket. Registration, like the CLI's own
    # account add, is an execution-host operation.
    endpoint, runtime_id, token = runtime_transport(data)
    if expected_runtime_id is not None and runtime_id != expected_runtime_id:
        raise ValueError("Orca runtime changed")
    request_id = str(uuid.uuid4())
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(12)
        conn.connect(str(endpoint))
        if hasattr(socket, "SO_PEERCRED"):
            credentials = conn.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i"))
            if struct.unpack("3i", credentials)[1] != os.getuid():
                raise ValueError("Orca runtime belongs to another user")
        request = {"id": request_id, "authToken": token, "method": method, "params": params}
        conn.sendall((json.dumps(request, separators=(",", ":")) + "\n").encode())
        with conn.makefile("rb") as stream:
            while True:
                line = stream.readline(1024 * 1024)
                if not line or not line.endswith(b"\n"):
                    raise ValueError("Orca account RPC ended without a response")
                reply = json.loads(line)
                if "_keepalive" in reply:
                    continue
                if reply.get("id") != request_id or reply.get("_meta", {}).get("runtimeId") != runtime_id:
                    raise ValueError("Orca runtime changed")
                if reply.get("ok") is not True or not isinstance(reply.get("result"), dict):
                    error = reply.get("error")
                    raise OrcaRPCError(error.get("code") if isinstance(error, dict) else None)
                return reply["result"]


def native_subject(account):
    email = account.get("email")
    organization = account.get("providerAccountId")
    if not isinstance(email, str) or not isinstance(organization, str):
        return ""
    email, organization = email.strip().lower(), organization.strip()
    if not email or not organization or any(c in email + organization for c in "\r\n\0"):
        return ""
    return hashlib.sha256(f"{email}\n{organization}".encode()).hexdigest()


def native_accounts(snapshot, data, home):
    accounts = {}
    active = snapshot["activeAccountId"]
    if active is not None and not (isinstance(active, str) and ORCA_ID.fullmatch(active)):
        raise ValueError("unsupported Orca Codex account selection")
    default = snapshot.get("systemDefault")
    if isinstance(default, dict) and default.get("hasAuth") is True and default.get("authKind") == "oauth":
        accounts["system"] = (native_subject(default), own_home(home, (".codex",)))
    for account in snapshot["accounts"]:
        if not isinstance(account, dict):
            continue
        ident = account.get("id")
        if account.get("managedHomeRuntime") == "host" and isinstance(ident, str) and ORCA_ID.fullmatch(ident):
            accounts[ident] = (native_subject(account), own_home(data, ("codex-accounts", ident, "home")))
    return active or "system", accounts


def observation_fingerprint(data, home, roster):
    # Deployed Orca may use the default profile JSON or its SQLite authority.
    # Inspect only metadata. A missed or unsupported layout still gets the
    # periodic full probe.
    state_files = ("orca-data.json", "profile-state.db", "profile-state.db-wal")
    files = [data / "orca-runtime.json", data / "orca-profile-index.json", *(data / name for name in state_files)]
    for path in (data / "profiles", data / "codex-accounts"):
        if own_directory(path, enclosed=True):
            files.append(path)
            for entry in sorted(path.iterdir()):
                if own_directory(entry, enclosed=True):
                    if path.name == "profiles":
                        files.extend(entry / name for name in state_files)
                    elif ORCA_ID.fullmatch(entry.name) and own_directory(entry / "home", private=True):
                        files.append(entry / "home/auth.json")
    default = own_home(home, (".codex",))
    if default:
        files.append(default / "auth.json")
    for ident in sorted(roster):
        cage = own_home(home / ".local/share/cage", ("codex-accounts", ident, "home"))
        if cage and cage.resolve() == cage:
            files.append(cage / "auth.json")
    stamp = hashlib.sha256(json.dumps(roster, sort_keys=True).encode())
    for file in files:
        stamp.update(str(file).encode())
        try:
            info = file.lstat()
            if info.st_uid == os.getuid() and not stat.S_ISLNK(info.st_mode):
                stamp.update(repr((info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)).encode())
        except FileNotFoundError:
            pass
    return stamp.hexdigest()


def bridge(payload, *, data, home, path, helper, registration_record=None, write_record=None):
    try:
        _, runtime_id, _ = runtime_transport(data)
    except (OSError, ValueError):
        return {"available": False}
    roster = payload.get("accounts")
    if not isinstance(roster, dict) or len(roster) > 256:
        raise ValueError("invalid Cage Codex account roster")
    for account, subject in roster.items():
        if not CAGE_ID.fullmatch(account) or not isinstance(subject, str) or not SUBJECT.fullmatch(subject):
            raise ValueError("invalid Cage Codex identity")
    if payload.get("register"):
        if not registration_supported(data, runtime_id):
            raise ValueError("This Orca runtime does not support local Codex registration")
        return register_account(payload["register"], roster, data, home, path, helper,
                                registration_record, write_record, runtime_id)
    target = payload.get("target", "")
    fingerprint = observation_fingerprint(data, home, roster)
    if not target and fingerprint == payload.get("fingerprint"):
        return {"available": True, "unchanged": True, "fingerprint": fingerprint}
    active, accounts = read_native_accounts(data, home, runtime_id)
    response = {"available": True, "orca_account_id": active, "fingerprint": fingerprint, "runtime_id": runtime_id,
                "registration_supported": registration_supported(data, runtime_id)}
    if target and (active != payload.get("expected_orca_account_id") or runtime_id != payload.get("expected_runtime_id")):
        return response

    account, subject = (target, roster.get(target, "")) if target else ("", accounts.get(active, ("", None))[0])
    if not subject:
        return response
    cage_ids = [ident for ident, expected in roster.items() if expected == subject]
    orca_ids = [ident for ident, (reading, _) in accounts.items() if reading == subject]
    if len(cage_ids) != 1 or len(orca_ids) != 1 or (target and cage_ids[0] != account):
        return response
    cage_id, orca_id = cage_ids[0], orca_ids[0]
    cage_home = own_home(home / ".local/share/cage", ("codex-accounts", cage_id, "home"))
    orca_home = accounts[orca_id][1]
    if not cage_home or not orca_home or cage_home.resolve() != cage_home or orca_home.resolve() != orca_home:
        return response
    if not verify_profiles(home, path, helper, (cage_home, orca_home), subject):
        return response
    if target and orca_id != active:
        changed = local_rpc(data, "accounts.selectCodex", {"accountId": None if orca_id == "system" else orca_id}, runtime_id)
        active, current = native_accounts(changed, data, home)
        response["orca_account_id"] = active
        if active != orca_id or current.get(active, (None, None))[0] != subject:
            return response
    response["cage_account_id"] = cage_id
    response["subject"] = subject
    return response


def registration_supported(data, runtime_id):
    status = local_rpc(data, "status.get", {}, runtime_id)
    capabilities = status.get("capabilities")
    if isinstance(capabilities, list) and any(isinstance(item, str) and item.startswith("accounts.") for item in capabilities):
        return IMPORT_CAPABILITY in capabilities
    # Legacy status replies can omit feature discovery. Let the authenticated
    # endpoint decide whether it supports the native operation.
    return True


def read_native_accounts(data, home, runtime_id):
    result = local_rpc(data, "accounts.list", {"refreshUsage": False}, runtime_id)
    return native_accounts(result["codex"], data, home)


def register_account(account, roster, data, home, path, helper, record, write_record, runtime_id):
    subject = roster.get(account)
    cage_home = own_home(home / ".local/share/cage", ("codex-accounts", account, "home")) if subject else None
    if not cage_home or cage_home.resolve() != cage_home or not verify_profiles(home, path, helper, (cage_home,), subject):
        raise ValueError("Cage Codex account is no longer verified")
    previous, native = read_native_accounts(data, home, runtime_id)
    pending = record if isinstance(record, dict) and record.get("account_id") else None
    if pending:
        if pending.get("account_id") != account or pending.get("subject") != subject:
            raise ValueError("Retry the pending Codex account registration first")
        previous = pending["previous"]
        if previous != "system" and not ORCA_ID.fullmatch(previous):
            raise ValueError("Invalid pending Orca Codex selection")
    existing = [ident for ident, (reading, _) in native.items() if reading == subject]
    created = False
    if not existing:
        write_record({"account_id": account, "subject": subject, "previous": previous})
        # Orca owns the registered home and copies credentials locally through
        # its normal import path. No authentication data enters the bridge RPC.
        try:
            local_rpc(data, "accounts.addCodexFromHome", {"sourceHome": str(cage_home), "runtime": "host"}, runtime_id)
        except OrcaRPCError as error:
            if error.code in ("unknown_method", "method_not_found", "not_found"):
                write_record({})
                raise ValueError("This Orca runtime does not support local Codex registration") from None
        except (OSError, ValueError):
            # A transport failure may follow a committed registration. Read
            # back the native state before deciding whether to retry.
            pass
        pending, created = True, True
        _, native = read_native_accounts(data, home, runtime_id)
        existing = [ident for ident, (reading, _) in native.items() if reading == subject]
    if len(existing) != 1:
        if not existing:
            write_record({})
        raise ValueError("Orca did not report one registered Codex account with the verified identity")
    ident = existing[0]
    active, _ = read_native_accounts(data, home, runtime_id)
    # Registration's native add selects the account. Restore the old selection
    # if the runtime still has that automatic selection.
    if pending and active == ident and active != previous:
        restored = local_rpc(data, "accounts.selectCodex", {"accountId": None if previous == "system" else previous}, runtime_id)
        active, _ = native_accounts(restored, data, home)
        if active != previous:
            raise ValueError("Could not restore the previous Orca Codex selection")
    if pending:
        write_record({})
    # Selecting back to the previous login can update Orca's managed home.
    # Allow a short read-only settle window before reporting verification.
    deadline = time.monotonic() + 2.0
    while True:
        active, current = read_native_accounts(data, home, runtime_id)
        reading, account_home = current.get(ident, ("", None))
        if (reading == subject and account_home and account_home.resolve() == account_home
                and verify_profiles(home, path, helper, (account_home,), subject)):
            return {"available": True, "registered": True, "created": created, "selected": active == ident,
                    "cage_account_id": account, "subject": subject}
        if time.monotonic() >= deadline:
            raise ValueError("Orca account was found, but its Codex login did not verify")
        time.sleep(0.15)


def verify_profiles(home, path, helper, account_homes, expected):
    codex = runpy.run_path(helper)
    search = os.pathsep.join(p for p in path.split(os.pathsep) if not codex["_is_cage_shim_dir"](p))
    env = {"HOME": str(home), "PATH": search}
    binary = shutil.which("codex", path=search)
    if not binary:
        return False
    # Do not import, copy, or report any credentials. The actual CLI checks each
    # profile in place, and Cage's existing identity reader yields only a digest.
    # Orca's tenant label alone may be stale after a different login.
    try:
        for account_home in account_homes:
            env["CODEX_HOME"] = str(account_home)
            info = (account_home / "auth.json").lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                return False
            if codex["login_mode"](binary, env, home, timeout=3) != "chatgpt":
                return False
            claims = codex["token_claims"](account_home)
            if not claims or codex["subject"]({**claims, "logged_in": True}) != expected:
                return False
    except (OSError, ValueError, TypeError, subprocess.SubprocessError):
        return False
    return True
