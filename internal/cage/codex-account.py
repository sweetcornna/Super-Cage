#!/usr/bin/python3
"""Run the official Codex CLI with a private CODEX_HOME and export only its subscription status."""
import base64
from datetime import date, datetime, time as clock
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import runpy
import select
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time

try:
    import tomllib
except ImportError:  # Python before 3.11 cannot read config.toml; Codex still runs.
    tomllib = None

_controller_context = {**os.environ, **globals().get("_controller_context", {})}
runtime_source = Path(__file__).resolve()
runtime_path = runtime_source.with_name("client_runtime.py")
if not runtime_path.exists():
    runtime_path = runtime_source.parent.parent / "client_runtime.py"
_runtime = runpy.run_path(runtime_path, init_globals={"_controller_context": _controller_context})
PRIVATE_MODE = _runtime.get("PRIVATE_MODE", 0o700)

SHIM = "/usr/local/lib/cage-codex-bin"
DEFAULT = "default"
# Subcommands that manage the CLI itself or a login run exactly as typed.
UNSCHEDULED = {"login", "logout", "auth", "mcp", "features", "completion", "debug", "cloud",
               "help", "-V", "--version", "-h", "--help"}
# Provider, credential and state-location variables select another login or
# another account's data. A managed account always uses its own CODEX_HOME.
PRIVATE_ENVIRONMENT = ("OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_BASE", "OPENAI_ORG_ID",
                       "OPENAI_ORGANIZATION", "OPENAI_PROJECT_ID", "OPENAI_PROJECT", "CODEX_ACCESS_TOKEN",
                       "CODEX_SQLITE_HOME", "CODEX_HOME")


def _is_cage_shim_dir(entry):
    if not entry:
        return True
    if entry in (SHIM, "/usr/local/lib/cage-claude-bin"):
        return True
    auto_shims = str(Path.home() / ".local/share/cage/auto-shims")
    agent_shims = str(Path.home() / ".local/share/cage/agent-shims")
    orca_shims = str(Path.home() / ".local/share/cage/orca/shims")
    t3_shims = str(Path.home() / ".local/share/cage/t3code/shims")
    if entry in (auto_shims, orca_shims, t3_shims) or entry == agent_shims or entry.startswith(agent_shims + os.sep):
        return True
    return False
# Local instructions and helpers are shared. Credentials, conversations,
# history, logs, state databases, plugins and agent roles stay per account.
SHARED_RESOURCES = ("AGENTS.md", "AGENTS.override.md", "prompts", "rules")
# Top-level config.toml keys that only change local behavior. Anything else,
# including model_provider(s), profiles, base URLs, login and credential-store
# settings, stays in the account's private config.toml.
SHARED_CONFIG_KEYS = {"model", "model_reasoning_effort", "model_reasoning_summary", "model_verbosity",
                      "model_supports_reasoning_summaries", "review_model", "approval_policy", "sandbox_mode",
                      "sandbox_workspace_write", "shell_environment_policy", "notify", "hide_agent_reasoning",
                      "show_raw_agent_reasoning", "file_opener", "features", "tools", "web_search",
                      "project_doc_max_bytes", "project_doc_fallback_filenames", "personality", "mcp_servers",
                      "notice", "check_for_update_on_startup", "disable_paste_burst", "projects",
                      "developer_instructions", "compact_prompt", "history", "tui", "skills"}
# Words that name or carry a credential, in a setting name, an environment
# variable or a command argument (after splitting snake, kebab and camelCase).
SECRET_WORDS = {"key", "keys", "apikey", "token", "tokens", "secret", "secrets", "password", "passwords", "passwd",
                "pass", "pwd", "passphrase", "pat", "auth", "oauth", "authorization", "bearer", "credential",
                "credentials", "cred", "creds", "cookie", "cookies", "session", "sessions", "private", "signature"}
PRIVATE_CONFIG_WORDS = SECRET_WORDS | {"proxy", "url", "uri", "host", "endpoint", "header", "headers", "provider",
                                       "providers", "account", "workspace", "login", "cert", "certs", "ca"}
PRIVATE_ENV_WORDS = {word.upper() for word in SECRET_WORDS} | {
    "PROXY", "URL", "HOST", "ENDPOINT", "REGION", "PROJECT", "PROFILE", "ACCOUNT", "ORG", "HELPER", "CERT", "CERTS",
    "CA", "BASE"}
# Fragments that mark a credential even inside a longer word, as in PGPASSWORD.
SECRET_FRAGMENTS = ("password", "passwd", "secret", "apikey", "credential", "bearer")
# An MCP server with its own environment or request headers stays private.
PRIVATE_MCP_KEYS = {"env", "env_vars"}
# Shared settings that run a command: dropped when any element looks secret-bearing.
COMMAND_KEYS = {"notify"}
# Well-known token formats, recognized even without a flag naming them.
KNOWN_SECRET = re.compile(r"(?<![A-Za-z0-9])(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]{8,}"
                          r"|glpat-[A-Za-z0-9_-]{8,}|xox[abeprs]-[A-Za-z0-9-]{8,}|AKIA[0-9A-Z]{12,})")
# The terminal ends instead of opening a shell, so the host re-checks the account.
ACCOUNT_REFUSED = 78
PRIVATE_ENV_PREFIXES = ("OPENAI_", "CODEX_", "AZURE_", "ANTHROPIC_", "AWS_", "GOOGLE_", "GEMINI_")
WINDOWS = ("primary", "secondary")
ORCA_ACCOUNT_ID = re.compile(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}")


class StatusError(ValueError):
    pass


class StatusTimeout(StatusError):
    pass


def profile(account_id):
    if account_id != DEFAULT and not re.fullmatch("[0-9a-f]{24}", account_id):
        raise ValueError("invalid account ID")
    return Path.home() / ".local/share/cage/codex-accounts" / account_id


def codex_home(base):
    if base.name == DEFAULT:
        configured = os.environ.get("CODEX_HOME")
        return Path(configured) if configured else Path.home() / ".codex"
    return base / "home"


def cli_environment(base):
    env = os.environ.copy()
    # Executing the real binary must never recurse into any Cage codex shim.
    path = [p for p in env.get("PATH", os.defpath).split(os.pathsep) if not _is_cage_shim_dir(p)]
    env["PATH"] = os.pathsep.join([str(Path.home() / ".local/bin"), *path])
    if base.name != DEFAULT:
        for name in PRIVATE_ENVIRONMENT:
            env.pop(name, None)
        env["CODEX_HOME"] = str(codex_home(base))
    return _runtime["environment"](env)


def login_mode(binary, env, cwd, timeout=10):
    """Return chatgpt, api_key, other or None from the CLI's own status report."""
    try:
        result = subprocess.run([binary, "login", "status"], cwd=cwd, env=env, timeout=timeout,
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except subprocess.TimeoutExpired as error:
        raise StatusTimeout("login status timed out") from error
    lines = [line.strip() for line in (result.stderr + result.stdout).decode(errors="replace").splitlines()]
    if result.returncode == 1 and "Not logged in" in lines:
        return None
    if result.returncode == 0:
        if "Logged in using ChatGPT" in lines:
            return "chatgpt"
        if any(line.startswith("API key configured") for line in lines):
            return "api_key"
        if any(line.startswith("Logged in using") for line in lines):
            return "other"
    # Any other failure must not sign a known user out.
    raise StatusError("unexpected login status")


def token_claims(home):
    """Read only identity claims from the ID token; tokens never leave this process."""
    try:
        data = json.loads((home / "auth.json").read_text())
    except FileNotFoundError:
        return None
    tokens = data.get("tokens") if isinstance(data, dict) else None
    token = tokens.get("id_token") if isinstance(tokens, dict) else None
    if not isinstance(token, str) or token.count(".") != 2:
        return None
    segment = token.split(".")[1]
    claims = json.loads(base64.urlsafe_b64decode(segment + "=" * (-len(segment) % 4)))
    if not isinstance(claims, dict):
        return None
    auth = claims.get("https://api.openai.com/auth")
    auth = auth if isinstance(auth, dict) else {}
    profile_claims = claims.get("https://api.openai.com/profile")
    profile_claims = profile_claims if isinstance(profile_claims, dict) else {}
    email = claims.get("email") or profile_claims.get("email")
    organization = auth.get("chatgpt_account_id") or tokens.get("account_id") or ""
    plan = auth.get("chatgpt_plan_type")
    return {"email": email if isinstance(email, str) else None,
            "organization": organization if isinstance(organization, str) else "",
            "plan": plan if isinstance(plan, str) else ""}


def identity(binary, env, cwd, timeout=10):
    mode = login_mode(binary, env, cwd, timeout)
    if mode != "chatgpt":
        return {"logged_in": False, "auth_mode": mode or ""}
    claims = token_claims(Path(env.get("CODEX_HOME") or Path.home() / ".codex"))
    if claims is None:
        # A keyring-backed login has no auth.json. Ask the CLI for the account.
        with tempfile.TemporaryDirectory(prefix="codex-status-") as directory:
            with AppServer(binary, env, directory, time.monotonic() + timeout) as server:
                account = server.account()
        claims = {"email": account.get("email"), "organization": "", "plan": account.get("planType") or ""}
    if not claims.get("email"):
        return {"logged_in": False, "auth_mode": mode}
    return {"logged_in": True, "auth_mode": mode, "email": claims["email"], "organization": claims["organization"],
            "plan": claims["plan"]}


def subject(summary):
    if not summary.get("logged_in") or not summary.get("email"):
        return None
    email = summary["email"].strip().lower()
    organization = (summary.get("organization") or "").strip()
    return hashlib.sha256(f"{email}\n{organization}".encode()).hexdigest()


class AppServer:
    """Minimal client of the CLI's stdio app server for account status reads."""

    def __init__(self, binary, env, cwd, deadline):
        self.deadline, self.sequence, self.buffer, self.received = deadline, 0, b"", 0
        self.home = Path(env.get("CODEX_HOME") or Path.home() / ".codex")
        self.process = subprocess.Popen([binary, "app-server"], cwd=cwd, env=env, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)

    def __enter__(self):
        try:
            info = self.request("initialize", {"clientInfo": {"name": "workbench", "title": "Workbench", "version": "1"}})
            # Never attribute another directory's account to this profile.
            if not isinstance(info.get("codexHome"), str) or Path(info["codexHome"]).resolve() != self.home.resolve():
                raise StatusError("unexpected Codex home")
            self.send({"method": "initialized"})
        except BaseException:
            self.__exit__()
            raise
        return self

    def __exit__(self, *_):
        try:
            self.process.stdin.close()
        except OSError:
            pass
        try:
            self.process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()
        self.process.stdout.close()

    def send(self, message):
        self.process.stdin.write((json.dumps(message) + "\n").encode())
        self.process.stdin.flush()

    def request(self, method, params=None):
        self.sequence += 1
        message = {"method": method, "id": self.sequence}
        if params is not None:
            message["params"] = params
        self.send(message)
        while True:
            for line in self.lines():
                reply = json.loads(line)
                if isinstance(reply, dict) and reply.get("id") == self.sequence and "method" not in reply:
                    if "error" in reply or not isinstance(reply.get("result"), dict):
                        raise StatusError(method + " failed")
                    return reply["result"]

    def lines(self):
        while b"\n" not in self.buffer:
            left = self.deadline - time.monotonic()
            if left <= 0:
                raise StatusTimeout("app server timed out")
            ready, _, _ = select.select([self.process.stdout], [], [], left)
            if not ready:
                continue
            chunk = os.read(self.process.stdout.fileno(), 65536)
            self.received += len(chunk)
            if not chunk or self.received > (4 << 20):
                raise StatusError("app server closed")
            self.buffer += chunk
        lines, self.buffer = self.buffer.rsplit(b"\n", 1)
        return [line for line in lines.split(b"\n") if line.strip()]

    def account(self):
        account = self.request("account/read", {"refreshToken": False}).get("account")
        if not isinstance(account, dict) or account.get("type") != "chatgpt":
            return {}
        return account


def json_file(path):
    try:
        return json.loads(path.read_text())
    except FileNotFoundError:
        return {}


def write_json(path, data):
    path.parent.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as stream:
        json.dump(data, stream, separators=(",", ":"))
    os.replace(stream.name, path)


def valid_window(data, now):
    if not isinstance(data, dict):
        return None
    used, reset, minutes = data.get("used_percentage"), data.get("resets_at"), data.get("window_minutes", 0)
    if type(used) not in (int, float) or not math.isfinite(used) or used < 0:
        return None
    if type(reset) != int or (reset != 0 and reset <= now):
        return None
    if type(minutes) != int or minutes < 0:
        minutes = 0
    # Codex reports usage above 100 once a window is exceeded.
    return {"used_percentage": min(used, 100), "resets_at": reset, "window_minutes": minutes}


def reported_window(used, reset, minutes, seen, now):
    window = valid_window({"used_percentage": used, "resets_at": 0 if reset is None else reset,
                           "window_minutes": 0 if minutes is None else minutes}, now)
    return {**window, "observed_at": seen} if window else None


def read_quota(base):
    try:
        stored = json_file(base / "quota.json")
    except (ValueError, OSError):
        return None
    if not isinstance(stored, dict):
        return None
    digest, seen, now = stored.get("identity", ""), stored.get("observed_at"), int(time.time())
    if not isinstance(digest, str) or not re.fullmatch("[a-f0-9]{64}", digest) or type(seen) != int or not 0 < seen <= now:
        return None
    result = {}
    for key in WINDOWS:
        window = valid_window(stored.get(key), now)
        window_seen = stored[key].get("observed_at", seen) if window else None
        if window and type(window_seen) == int and 0 < window_seen <= now:
            result[key] = {**window, "observed_at": window_seen}
    return {**result, "identity": digest, "observed_at": seen} if result else None


def read_identity(base):
    try:
        record = json_file(base / "identity.json")
    except (ValueError, OSError):
        return None
    if (isinstance(record, dict) and isinstance(record.get("identity"), str) and re.fullmatch("[a-f0-9]{64}", record["identity"])
            and type(record.get("since")) == int and record["since"] >= 0):
        return record
    return None


def note_identity(base, home, digest):
    """Bind later local rate-limit events to the identity that produced them."""
    record = read_identity(base)
    if record and record["identity"] == digest:
        return record
    try:
        # Events after the last credential write cannot belong to an earlier login.
        since = int((home / "auth.json").stat().st_mtime) if not record else int(time.time())
    except OSError:
        since = int(time.time())
    record = {"identity": digest, "since": since}
    write_json(base / "identity.json", record)
    if (read_quota(base) or {}).get("identity") not in (None, digest):
        (base / "quota.json").unlink(missing_ok=True)
    return record


def event_time(stamp):
    try:
        moment = datetime.fromisoformat(stamp.replace("Z", "+00:00"))
    except (AttributeError, ValueError):
        return None
    return int(moment.timestamp()) if moment.tzinfo else None


def rollout_snapshot(line, now):
    try:
        item = json.loads(line)
    except ValueError:
        return None
    payload = item.get("payload") if isinstance(item, dict) and item.get("type") == "event_msg" else None
    if not isinstance(payload, dict) or payload.get("type") != "token_count":
        return None
    limits = payload.get("rate_limits")
    seen = event_time(item.get("timestamp"))
    # A model-specific bucket never substitutes for the account's Codex limit.
    if not isinstance(limits, dict) or limits.get("limit_id") not in (None, "codex") or not seen or seen > now:
        return None
    result = {}
    for key in WINDOWS:
        window = limits.get(key)
        if isinstance(window, dict):
            reading = reported_window(window.get("used_percent"), window.get("resets_at"), window.get("window_minutes"), seen, now)
            if reading:
                result[key] = reading
    return {**result, "observed_at": seen} if result else None


def rollout_reading(home, since, now, files=8):
    """Read the newest rate-limit event recorded by recent local sessions."""
    def children(folder, pattern):
        try:
            return sorted((p for p in folder.iterdir() if p.is_dir() and not p.is_symlink() and re.fullmatch(pattern, p.name)), reverse=True)
        except OSError:
            return []
    candidates = []
    for year in children(home / "sessions", r"\d{4}"):
        for month in children(year, r"\d{2}"):
            for day in children(month, r"\d{2}"):
                candidates += [p for p in day.glob("rollout-*.jsonl") if p.is_file() and not p.is_symlink()]
                if len(candidates) >= files:
                    break
            if len(candidates) >= files:
                break
        if len(candidates) >= files:
            break
    def modified(path):
        try:
            return path.stat().st_mtime
        except OSError:
            return 0
    best = None
    for path in sorted(candidates, key=modified, reverse=True)[:files]:
        if modified(path) < since:
            continue
        try:
            with path.open("rb") as stream:
                stream.seek(max(0, stream.seek(0, os.SEEK_END) - (1 << 20)))
                tail = stream.read().decode(errors="replace").splitlines()
        except OSError:
            continue
        for line in reversed(tail):
            reading = rollout_snapshot(line, now)
            if reading:
                if reading["observed_at"] >= since and (best is None or reading["observed_at"] > best["observed_at"]):
                    best = reading
                break
    return best


def merged_quota(base, home, expected=None):
    record = read_identity(base)
    digest = expected or (record or {}).get("identity")
    if not digest:
        return None
    now = int(time.time())
    windows = {}
    stored = read_quota(base)
    if stored and stored["identity"] == digest:
        windows = {key: stored[key] for key in WINDOWS if key in stored}
    if record and record["identity"] == digest:
        live = rollout_reading(home, record["since"], now)
        for key in WINDOWS:
            if live and key in live and (key not in windows or live[key]["observed_at"] > windows[key]["observed_at"]):
                windows[key] = live[key]
    if not windows:
        return None
    return {**windows, "identity": digest, "observed_at": max(window["observed_at"] for window in windows.values())}


def app_windows(result, now):
    by_limit = result.get("rateLimitsByLimitId")
    snapshot = by_limit.get("codex") if isinstance(by_limit, dict) else None
    if not isinstance(snapshot, dict):
        snapshot = result.get("rateLimits")
        if not isinstance(snapshot, dict) or snapshot.get("limitId") not in (None, "codex"):
            return {}
    windows = {}
    for key in WINDOWS:
        window = snapshot.get(key)
        if isinstance(window, dict):
            reading = reported_window(window.get("usedPercent"), window.get("resetsAt"), window.get("windowDurationMins"), now, now)
            if reading:
                windows[key] = reading
    return windows


def changed_identity():
    return {"logged_in": False, "usage_status": "identity_changed", "usage_checked_at": int(time.time())}


def query_usage(binary, env, base, home, expected):
    if not expected or not re.fullmatch("[a-f0-9]{64}", expected):
        raise ValueError("missing expected account")
    deadline = time.monotonic() + 15
    summary = identity(binary, env, Path.home(), timeout=5)
    if subject(summary) != expected:
        return changed_identity()
    note_identity(base, home, expected)
    status, windows = "error", None
    with tempfile.TemporaryDirectory(prefix="codex-usage-") as directory:
        try:
            with AppServer(binary, env, directory, deadline) as server:
                account = server.account()
                if not account:
                    status = "not_applicable"
                elif (account.get("email") or "").strip().lower() != summary["email"].strip().lower():
                    return changed_identity()
                else:
                    windows = app_windows(server.request("account/rateLimits/read"), int(time.time()))
                    if account.get("planType"):
                        summary["plan"] = account["planType"]
        except (OSError, ValueError, TypeError, AttributeError, subprocess.SubprocessError):
            # The CLI can print account data on failure. Report a category only.
            windows = None
    after = identity(binary, env, Path.home(), timeout=max(1, min(5, deadline - time.monotonic())))
    if subject(after) != expected:
        return changed_identity()
    if windows:
        stored = read_quota(base) or {}
        record = {key: windows[key] for key in WINDOWS if key in windows}
        for key in WINDOWS:
            if key not in record and stored.get("identity") == expected and key in stored:
                record[key] = stored[key]
        write_json(base / "quota.json", {**record, "identity": expected, "observed_at": int(time.time())})
    if windows is not None and status == "error":
        status = {0: "unavailable", 1: "partial", 2: "reported"}[len(windows)]
    quota = merged_quota(base, home, expected)
    if quota:
        summary["quota"] = quota
        if status == "unavailable":
            status = "cached"
    summary.update(usage_status=status, usage_checked_at=int(time.time()))
    return summary


def name_words(text):
    """Lowercase words of a snake_case, kebab-case, dotted or camelCase name."""
    text = re.sub(r"([a-z0-9])([A-Z])", r"\1 \2", str(text))
    text = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1 \2", text)
    text = re.sub(r"([A-Za-z])([0-9])", r"\1 \2", text)
    return {word for word in re.split(r"[^a-z0-9]+", text.lower()) if word}


def names_secret(words):
    return bool(SECRET_WORDS & words) or any(fragment in word for word in words for fragment in SECRET_FRAGMENTS)


def private_config_name(name):
    words = name_words(name)
    return bool(PRIVATE_CONFIG_WORDS & words) or names_secret(words)


def shared_env_variable(name):
    """A shared variable may change tool behavior but never routing or login."""
    if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", name):
        return False
    words = name_words(name)
    return (not name.upper().startswith(PRIVATE_ENV_PREFIXES) and not PRIVATE_ENV_WORDS & {word.upper() for word in words}
            and not names_secret(words))


def secret_argument(value):
    """A command element that names, assigns or carries a credential."""
    if not isinstance(value, str):
        return False
    assignment = re.match(r"\s*([A-Za-z_][A-Za-z0-9_]*)=", value)
    return bool(names_secret(name_words(value)) or KNOWN_SECRET.search(value)
                or assignment and not shared_env_variable(assignment.group(1)))


def shared_command(value):
    command = value if isinstance(value, list) else [value]
    return not any(secret_argument(item) for item in command)


def shared_mcp_server(server):
    """A server is shared only without credentials in its settings, command or arguments."""
    if not isinstance(server, dict) or any(key in PRIVATE_MCP_KEYS or private_config_name(key) for key in server):
        return False
    return shared_command(server.get("command", "")) and shared_command(server.get("args", [])) and shared_value(server) == server


def shared_value(value, name=""):
    if isinstance(value, dict):
        result = {}
        for key, item in value.items():
            if name in ("env", "set"):
                if shared_env_variable(key) and not isinstance(item, (dict, list)) and not KNOWN_SECRET.search(str(item)):
                    result[key] = item
            elif name == "projects":
                # Project paths are local folders; only their settings are filtered.
                result[key] = shared_value(item)
            elif name == "mcp_servers":
                # A server with a credential, endpoint or header stays private as a whole.
                if shared_mcp_server(item):
                    result[key] = item
            elif not private_config_name(key):
                result[key] = shared_value(item, key)
        return result
    if isinstance(value, list):
        return [shared_value(item) for item in value]
    return value


def subtract(shared, layer, atomic=False):
    """Remove settings another config layer already defines."""
    result = {}
    for key, value in shared.items():
        if key not in layer:
            result[key] = value
        elif not atomic and isinstance(value, dict) and isinstance(layer[key], dict):
            rest = subtract(value, layer[key], key == "mcp_servers")
            if rest:
                result[key] = rest
    return result


def toml_value(value):
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        if math.isnan(value):
            return "nan"
        return ("inf" if value > 0 else "-inf") if math.isinf(value) else repr(value)
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False).replace("\x7f", "\\u007f")
    if isinstance(value, (datetime, date, clock)):
        return value.isoformat()
    if isinstance(value, list):
        return "[" + ", ".join(toml_value(item) for item in value) + "]"
    if isinstance(value, dict):
        return "{" + ", ".join(toml_value(str(key)) + " = " + toml_value(item) for key, item in value.items()) + "}"
    raise TypeError("unsupported TOML value")


def shared_overrides(home, cwd):
    """Apply shared local settings as CLI flags without changing layer precedence."""
    common = Path.home() / ".codex/config.toml"
    if tomllib is None or home.resolve() == common.parent.resolve():
        return []
    try:
        loaded = tomllib.loads(common.read_text())
    except FileNotFoundError:
        return []
    except (OSError, ValueError):
        print("~/.codex/config.toml 无法读取，本次未共用其中的设置。", file=sys.stderr)
        return []
    shared = {key: shared_value(value, key) for key, value in loaded.items()
              if key in SHARED_CONFIG_KEYS and (key not in COMMAND_KEYS or shared_command(value))}
    layers = [home / "config.toml", *[folder / ".codex/config.toml" for folder in (cwd, *cwd.parents)]]
    for path in layers:
        if path.resolve() == common.resolve():
            continue
        try:
            shared = subtract(shared, tomllib.loads(path.read_text()))
        except FileNotFoundError:
            continue
        except (OSError, ValueError):
            # The CLI reports an unreadable layer itself; never override it blindly.
            return []
    return [argument for key, value in shared.items() for argument in ("-c", key + "=" + toml_value(value))]


def same_file(local, shared):
    try:
        return local.is_file() and shared.is_file() and local.read_bytes() == shared.read_bytes()
    except OSError:
        return False


def share_resources(base, home):
    """Link local customizations from ~/.codex; never move or overwrite account files."""
    common = Path.home() / ".codex"
    if home.resolve() == common.resolve():
        return []
    pairs = [(home / name, common / name) for name in SHARED_RESOURCES]
    skills = common / "skills"
    if skills.is_dir() and not (home / "skills").is_symlink():
        (home / "skills").mkdir(mode=PRIVATE_MODE, exist_ok=True)
        # Codex installs its bundled .system skills itself in every home.
        pairs += [(home / "skills" / entry.name, entry) for entry in sorted(skills.iterdir()) if entry.name != ".system"]
    kept = []
    with (base / "resources.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        for local, shared in pairs:
            if not shared.exists():
                continue
            if local.is_symlink():
                if local.resolve() != shared.resolve():
                    kept.append(local.name)
                continue
            if local.exists():
                if not same_file(local, shared):
                    kept.append(local.name)
                    continue
                local.unlink()
            local.symlink_to(shared, target_is_directory=shared.is_dir())
    return kept


def print_json(value):
    print(json.dumps(value, separators=(",", ":")))


def main(args, *, quiet=False, before_exec=None):
    mode, account_id, *rest = args
    if mode == "quotas":
        result = {}
        for candidate in [account_id, *rest]:
            base = profile(candidate)
            quota = merged_quota(base, codex_home(base))
            count = _runtime["plain_launch_count"](base)
            if count:
                quota = {**(quota or {}), "plain_launches": count}
            if quota:
                result[candidate] = quota
        print_json(result)
        return 0
    base = profile(account_id)
    home = codex_home(base)
    if mode in ("snapshot", "usage") and not home.is_dir():
        print_json(changed_identity() if mode == "usage" else {"logged_in": False})
        return 0
    original_umask = os.umask(0o077)
    base.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    if account_id != DEFAULT:
        home.mkdir(mode=PRIVATE_MODE, exist_ok=True)
    env = cli_environment(base)
    binary = shutil.which("codex", path=env["PATH"])
    if not binary:
        raise FileNotFoundError("Codex is not installed")
    if mode == "snapshot":
        summary = identity(binary, env, Path.home())
        digest = subject(summary)
        if digest:
            note_identity(base, home, digest)
            quota = merged_quota(base, home, digest)
            if quota:
                summary["quota"] = quota
        print_json(summary)
        return 0
    if mode == "usage":
        if len(rest) != 1:
            raise ValueError("usage requires an expected account")
        print_json(query_usage(binary, env, base, home, rest[0]))
        return 0
    if mode == "login":
        if account_id == DEFAULT:
            raise ValueError("new logins require a private profile")
        def interrupted(_signum, _frame):
            raise KeyboardInterrupt()
        signal.signal(signal.SIGHUP, interrupted)
        signal.signal(signal.SIGTERM, interrupted)
        with _runtime["launch_guard"](base, login=True):
            for name in ("quota.json", "identity.json"):
                (base / name).unlink(missing_ok=True)
            print("请在任意浏览器打开下方链接，登录 ChatGPT 后输入一次性验证码。完成后此终端会自动结束。", flush=True)
            return subprocess.call([binary, "login", "--device-auth"], cwd=Path.home(), env=env)
    if mode == "run":
        kept = share_resources(base, home) if account_id != DEFAULT else []
        if kept:
            print("Codex 账号目录中的 " + "、".join(sorted(set(kept))) + " 与 ~/.codex 不同，已保留账号自己的版本。", file=sys.stderr)
        expected = _controller_context.get("CAGE_CODEX_SUBJECT")
        reading = identity(binary, env, Path.cwd())
        digest = subject(reading)
        if not expected or digest != expected:
            if not quiet:
                print("此终端未登录所选 Codex 账号。请在 Codex 账号管理中检查登录状态或重新登录。", file=sys.stderr)
            return ACCOUNT_REFUSED
        note_identity(base, home, digest)
        overrides = shared_overrides(home, Path.cwd()) if account_id != DEFAULT else []
        # Codex and its tools create files in the shared project with the user's umask.
        os.umask(original_umask)
        if before_exec is not None:
            before_exec()
        os.execve(binary, [binary, *overrides, *rest], env)
    raise ValueError("unknown operation")


def launch_candidates(schedule, now=None):
    return _runtime["native_launch_candidates"](
        schedule, profile, lambda base, expected: merged_quota(base, codex_home(base), expected),
        windows=WINDOWS, now=now)


def orca_login_status(directory, schedule, args):
    # A status probe consults the same policy without allocating a lease or
    # advancing the scheduler. A quota limit does not end a login.
    mode = schedule.get("mode") or "balanced"
    pin = _runtime["_project_pin"](schedule.get("project_pins"), Path.cwd())
    candidates = [item for item in schedule["accounts"] if isinstance(item, dict)
                  and isinstance(item.get("id"), str) and re.fullmatch("[0-9a-f]{24}", item["id"])
                  and isinstance(item.get("subject"), str) and re.fullmatch("[0-9a-f]{64}", item["subject"])]
    ranked = launch_candidates(schedule)
    if mode not in ("manual", "exhaust") and not pin and ranked:
        candidates = ranked
    else:
        candidates = [{"load": 0, "headroom": -1, **item} for item in candidates]
    for item in _runtime["launch_order"](directory, schedule, candidates, mode, pin):
        try:
            env = cli_environment(profile(item["id"]))
            binary = shutil.which("codex", path=env["PATH"])
            if binary and subject(identity(binary, env, Path.cwd(), timeout=3)) == item["subject"]:
                os.execve(binary, [binary, *args], env)
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            continue
    print("Not logged in", file=sys.stderr)
    return 1


def orca_data_path():
    profile = os.environ.get("ORCA_USER_DATA_PATH")
    if not profile:
        return None
    home = Path.home()
    standard = home / ("Library/Application Support/orca" if sys.platform == "darwin" else ".config/orca")
    data = Path(profile)
    return data if data in (standard, home / ".local/share/cage/orca/data") else None


def from_orca():
    if not os.environ.get("ORCA_USER_DATA_PATH"):
        return os.environ.get("TERM_PROGRAM") == "Orca"
    return orca_data_path() is not None


def orca_profile_owned(data, home):
    try:
        folders, ident, kind = home.relative_to(data).parts
        if folders != "codex-accounts" or kind != "home" or not ORCA_ACCOUNT_ID.fullmatch(ident):
            return False
        if home.resolve() != home:
            return False
        for directory in (data, data / folders, home.parent, home):
            info = directory.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
                return False
            # Orca's intermediate directories inherit its umask inside the
            # private root. The data root and credential home remain private.
            if directory in (data, home) and info.st_mode & 0o077:
                return False
        marker = home / ".orca-managed-home"
        info = marker.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_size > 40:
            return False
        return marker.read_text(encoding="ascii").strip() == ident
    except (OSError, ValueError, RuntimeError):
        return False


def orca_roster_subjects(schedule):
    profiles = {}
    history = schedule.get("history_accounts", [])
    for item in [*schedule["accounts"], *(history if isinstance(history, list) else [])]:
        if not isinstance(item, dict):
            continue
        ident, digest = item.get("id"), item.get("subject")
        if isinstance(ident, str) and re.fullmatch("[0-9a-f]{24}", ident) and \
                isinstance(digest, str) and re.fullmatch("[0-9a-f]{64}", digest):
            profiles.setdefault(ident, set()).add(digest)
    digests = {}
    for ident, subjects in profiles.items():
        for digest in subjects:
            digests.setdefault(digest, set()).add(ident)
    return {digest for digest, ids in digests.items() if len(ids) == 1 and len(profiles[next(iter(ids))]) == 1}


def orca_profile_subject(data, home, subjects):
    if not orca_profile_owned(data, home):
        return None
    try:
        info = (home / "auth.json").lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            return None
        claims = token_claims(home)
        if not claims or not claims.get("organization", "").strip():
            return None
        digest = subject({**claims, "logged_in": True})
        if digest in subjects:
            env = cli_environment(profile(DEFAULT))
            env["CODEX_HOME"] = str(home)
            binary = shutil.which("codex", path=env["PATH"])
            if binary and login_mode(binary, env, Path.cwd(), timeout=3) == "chatgpt":
                return digest
    except (OSError, ValueError, TypeError, subprocess.SubprocessError):
        pass
    return None


def orca_history_profiles(data, selected_home, selected_subject, subjects, thread):
    for entry in _runtime["history_entries"](data / "codex-accounts", directories=True):
        home = entry / "home"
        if not orca_profile_owned(data, home) or not _runtime["codex_has_thread"](home, thread):
            continue
        digest = selected_subject if home == selected_home else orca_profile_subject(data, home, subjects)
        if digest:
            yield home, digest, f"Orca {entry.name[:8]}", home


def codex_passthrough(args, home=None):
    path = os.pathsep.join([str(Path.home() / ".local/bin"), *(
        p for p in os.environ.get("PATH", os.defpath).split(os.pathsep) if not _is_cage_shim_dir(p))])
    binary = shutil.which("codex", path=path)
    if not binary:
        print("未找到 codex 命令。", file=sys.stderr)
        return 127
    if home is not None:
        os.execve(binary, [binary, *args], {**os.environ, "CODEX_HOME": str(home)})
    os.execv(binary, [binary, *args])


def auto(args, *, orca=False):
    """Start a usable managed account, or preserve an explicit/default invocation."""
    required = os.environ.pop("CAGE_REQUIRE_CODEX_SCHEDULE", "") == "1"
    magpie_takeover = (Path.home() / ".local/share/cage/magpie/takeover").is_file()
    disabled = magpie_takeover or os.environ.get("CAGE_ACCOUNT_SCHEDULING", "").strip().lower() in ("0", "off", "false", "no")
    pinned = os.environ.get("CAGE_CODEX_ACCOUNT_ID", "")
    if not disabled and pinned not in ("", "auto", DEFAULT):
        return entrypoint(["run", pinned, *args])
    explicit = disabled or pinned == DEFAULT or _runtime["codex_explicit_remote"](args) or any(
        os.environ.get(name) for name in PRIVATE_ENVIRONMENT if name != "CODEX_HOME")
    command = _runtime["codex_subcommand"](args)
    configured_home = os.environ.get("CODEX_HOME")
    # Orca marks native PTYs and supplies its data root. Both are needed before
    # treating a vendor-managed home as a hint for a scheduled launch.
    data = orca_data_path() if os.environ.get("TERM_PROGRAM") == "Orca" else None
    injected_home = Path(configured_home) if configured_home and data else None
    orca_probe = not configured_home and command == ("login", "status") and (orca or from_orca())
    if configured_home and os.environ.get("CAGE_CLI_ACCOUNT_CODEX", "") not in ("", "auto"):
        explicit = True
    if not explicit and (not configured_home or injected_home is not None) and \
            (orca_probe or not (command and command[0] in UNSCHEDULED)):
        launch = profile(DEFAULT).parent / "launch.json"
        injected_subject = None

        def read_schedule():
            try:
                schedule = json.loads(launch.read_text())
            except FileNotFoundError:
                return None if required else {"managed": False, "accounts": []}
            except (OSError, ValueError):
                return None
            if not isinstance(schedule, dict) or not isinstance(schedule.get("accounts"), list):
                return None
            if injected_subject and injected_subject not in orca_roster_subjects(schedule):
                return None
            return schedule

        schedule = read_schedule()
        if injected_home is not None:
            subjects = orca_roster_subjects(schedule) if schedule is not None else set()
            injected_subject = orca_profile_subject(data, injected_home, subjects) if subjects else None
            if not injected_subject:
                return codex_passthrough(args)
        if schedule is None:
            print("无法读取 Super Cage 的 Codex 调度配置。请同步环境组件后重试。", file=sys.stderr)
            return ACCOUNT_REFUSED
        thread = _runtime["codex_history_request"](args)
        if _runtime["native_schedule_managed"](schedule) or (injected_home is not None and thread is not None):
            if orca_probe:
                return orca_login_status(launch.parent, schedule, args)
            mask = os.umask(0)
            os.umask(mask)
            if thread is not None:
                try:
                    roster = schedule.get("history_accounts", schedule["accounts"])
                    additional_profiles = ()
                    if injected_home is not None:
                        if thread and _runtime["codex_has_thread"](injected_home, thread):
                            return codex_passthrough(args)
                        additional_profiles = orca_history_profiles(data, injected_home, injected_subject, subjects, thread)
                    extras = {"additional_profiles": additional_profiles} if injected_home is not None else {}
                    selected = _runtime["history_account"](roster, lambda account: codex_home(profile(account)),
                                                           _runtime["codex_history"], thread, **extras)
                except KeyboardInterrupt:
                    return 130
                except ValueError as error:
                    print(error, file=sys.stderr)
                    return ACCOUNT_REFUSED
                if selected is None:
                    return 0
                account, expected = selected
                if isinstance(account, Path):
                    return codex_passthrough(args, home=account)
                _controller_context["CAGE_CODEX_SUBJECT"] = expected
                try:
                    with _runtime["launch_guard"](profile(account)):
                        return main(["run", account, *args])
                except KeyboardInterrupt:
                    return 130
                except (OSError, ValueError, subprocess.SubprocessError, TypeError):
                    print("无法打开原账号的 Codex 会话。请检查原账号的登录状态或稍后重试。", file=sys.stderr)
                    return ACCOUNT_REFUSED
                finally:
                    os.umask(mask)
                    _controller_context.pop("CAGE_CODEX_SUBJECT", None)

            def start(_schedule, item, before_exec):
                _controller_context["CAGE_CODEX_SUBJECT"] = item["subject"]
                try:
                    main(["run", item["id"], *args], quiet=True, before_exec=before_exec)
                finally:
                    os.umask(mask)
                    _controller_context.pop("CAGE_CODEX_SUBJECT", None)

            try:
                mode, pin = _runtime["admit_account_launch"](launch.parent, read_schedule, launch_candidates, profile, start)
            except KeyboardInterrupt:
                return 130
            except (OSError, ValueError):
                print("无法读取 Super Cage 的 Codex 调度配置。请同步环境组件后重试。", file=sys.stderr)
                return ACCOUNT_REFUSED
            if pin:
                print("此项目绑定的 Codex 账号不可用。请检查项目绑定、登录状态和额度后重试。", file=sys.stderr)
            elif mode == "manual":
                print("手动选择的 Codex 账号不可用。请检查登录状态和额度，或在账号管理中重新选择。", file=sys.stderr)
            else:
                print("暂无可启动的 Super Cage Codex 账号。请检查登录状态、参与状态和额度后重试。", file=sys.stderr)
            return ACCOUNT_REFUSED
    return codex_passthrough(args)


def entrypoint(args):
    if args[:1] in (["auto"], ["orca-auto"]):
        return auto(args[1:], orca=args[0] == "orca-auto")
    try:
        if args[:1] == ["run"] and len(args) >= 2:
            try:
                with _runtime["launch_guard"](profile(args[1]), track=False):
                    return main(args)
            except BlockingIOError:
                print("所选 Codex 账号正在登录，请稍后重试。", file=sys.stderr)
                return ACCOUNT_REFUSED
        return main(args)
    except KeyboardInterrupt:
        return 130
    except StatusTimeout:
        print("读取 Codex 登录状态超时，请稍后重试；现有登录信息已保留。", file=sys.stderr)
    except StatusError:
        print("无法读取 Codex 登录状态，请确认 Codex CLI 可正常运行后重试；现有登录信息已保留。", file=sys.stderr)
    except _runtime["RuntimePathConflict"]:
        print("终端运行路径发生冲突，原数据已保留，请同步环境组件后重试。", file=sys.stderr)
    except FileNotFoundError:
        print("未找到 Codex CLI。请先在环境中安装 Codex（例如 ~/.local/bin/codex）后重试。", file=sys.stderr)
    except (OSError, ValueError, subprocess.SubprocessError, TypeError):
        # No raw CLI output, paths, credentials or device codes go to logs.
        print("准备 Codex 账号或启动 Codex CLI 失败，请检查目录权限并同步环境组件后重试；现有登录信息已保留。", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(entrypoint(sys.argv[1:]))
