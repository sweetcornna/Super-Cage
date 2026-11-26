#!/usr/bin/python3
"""Universal guest runtime for mainstream and custom Agent CLIs.

Design principles:
- Zero third-party dependencies (Python 3 standard library only).
- Blazing-fast batched probing: single process, package.json version inspection
  (0.2 ms vs 600 ms Node startup), fingerprint-keyed cache for CLIs and
  prerequisites, thread-safe parallel fallback.
- Ultra-low-latency pure-shell shims (<0.5 ms overhead) sourcing 0600 POSIX
  profile.env files with zero Python startup on CLI invocation.
- Strict account isolation: per-agent, per-account config/XDG/home roots,
  0700 directories, 0600 credentials, never passing API keys on CLI argv,
  and never exporting raw OAuth tokens or API keys back to the host UI.
- Conflict-safe shared resources (AGENTS.md, GEMINI.md, skills, prompts, etc.)
  with backups before any symlink consolidation.
- Managed T3 Code guest lifecycle with one-time pairing URL bootstrap.
"""
import base64
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unicodedata
from urllib.parse import urlparse
import uuid

HOME = Path.home()
ACCOUNTS_ROOT = HOME / ".local/share/cage/agent-accounts"
SHIMS_ROOT = HOME / ".local/share/cage/agent-shims"
AUTO_SHIMS_DIR = HOME / ".local/share/cage/auto-shims"
SHARED_ROOT = HOME / ".local/share/cage/agent-shared"
AGENT_LAUNCH_FILE = ACCOUNTS_ROOT / "launch.json"
CLAUDE_LAUNCH_FILE = HOME / ".local/share/cage/claude-accounts/launch.json"
CODEX_LAUNCH_FILE = HOME / ".local/share/cage/codex-accounts/launch.json"
T3CODE_DIR = HOME / ".local/share/cage/t3code"
T3CODE_ROOT = T3CODE_DIR
T3CODE_STATE_DIR = T3CODE_ROOT / "state"
T3CODE_STATE_FILE = T3CODE_ROOT / "runtime.json"
T3CODE_LOG_FILE = T3CODE_ROOT / "server.log"
T3CODE_LOCK_FILE = T3CODE_ROOT / "lifecycle.lock"
T3CODE_PAIR_FILE = T3CODE_ROOT / "pair.once"
T3CODE_LOG_MAX_BYTES = 65536
MAGPIE_ROOT = HOME / ".local/share/cage/magpie"
MAGPIE_STATE_FILE = MAGPIE_ROOT / "runtime.json"
MAGPIE_LOG_FILE = MAGPIE_ROOT / "server.log"
MAGPIE_LOCK_FILE = MAGPIE_ROOT / "lifecycle.lock"
MAGPIE_KEY_FILE = MAGPIE_ROOT / "web.key"
MAGPIE_TAKEOVER_FILE = MAGPIE_ROOT / "takeover"
MAGPIE_DEFAULT_PORT = 3430
MAGPIE_DEFAULT_GATEWAY_PORT = 3425
MAGPIE_LOG_MAX_BYTES = 65536
MAGPIE_KEY_RE = re.compile(r"^[A-Za-z0-9._~-]{16,}$")
HERDR_ROOT = HOME / ".local/share/cage/herdr"
HERDR_LOCK_FILE = HERDR_ROOT / "lifecycle.lock"
ORCA_ROOT = HOME / ".local/share/cage/orca"
ORCA_RELEASES_DIR = ORCA_ROOT / "releases"
ORCA_CURRENT_LINK = ORCA_ROOT / "current"
ORCA_PREVIOUS_LINK = ORCA_ROOT / "previous"
ORCA_DATA_DIR = ORCA_ROOT / "data"
ORCA_SHIMS_DIR = ORCA_ROOT / "shims"
ORCA_STATE_FILE = ORCA_ROOT / "runtime.json"
ORCA_LOG_FILE = ORCA_ROOT / "server.log"
ORCA_LOCK_FILE = ORCA_ROOT / "lifecycle.lock"
ORCA_INSTANCE_ID_FILE = ORCA_ROOT / "instance-id"
ORCA_BROWSER_WRAPPER = ORCA_ROOT / "open-browser.sh"
ORCA_DEFAULT_PORT = 16768
ORCA_LOG_MAX_BYTES = 65536
ORCA_VERSION_RE = re.compile(r"^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$")
ORCA_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
ORCA_MEM_LIMIT_RE = re.compile(r"^\d+[KMGT]$")
ORCA_RELEASES_API = "https://api.github.com/repos/stablyai/orca/releases/latest"
ORCA_RELEASE_DL_BASE = "https://github.com/stablyai/orca/releases/download"
AUTO_UNSCHEDULED_ARGS = frozenset({
    "auth", "login", "logout", "setup-token", "update", "upgrade", "install",
    "uninstall", "migrate-installer", "doctor", "mcp", "plugin", "plugins",
    "config", "completion", "features", "debug", "cloud", "version", "help",
    "-v", "-V", "--version", "-h", "--help",
})
SHELL_SHIMS_MARKER_START = "# >>> super-cage-auto-shims >>>"
SHELL_SHIMS_MARKER_END = "# <<< super-cage-auto-shims <<<"
CACHE_DIR = HOME / ".cache/cage"
PROBE_CACHE_FILE = CACHE_DIR / "agent-probe-cache.json"
OPEN_BIN = "/usr/local/libexec/cage-cli/bin/cage-open" if sys.platform == "darwin" else "/usr/local/bin/cage-open"

CLI_ID_RE = re.compile(r"^[a-z][a-z0-9-]{1,31}$")
ACCOUNT_ID_RE = re.compile(r"^[0-9a-f]{24}$")
PREP_HASH_RE = re.compile(r"^[A-Za-z0-9_-]{8,64}$")
ENV_NAME_RE = re.compile(r"^[A-Z][A-Z0-9_]{0,63}$")
FLAG_RE = re.compile(r"^--?[a-zA-Z0-9][a-zA-Z0-9._=-]{0,63}$")
VERSION_RE = re.compile(r"v?(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)")
NPM_PKG_RE = re.compile(r"^(@[a-z0-9-~][a-z0-9-._~]*/)?[a-z0-9-~][a-z0-9-._~]*$")
PY_PKG_RE = re.compile(r"^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,126}[a-zA-Z0-9])?$")
ANSI_ESCAPE_RE = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")
T3_QR_GLYPHS = frozenset("█▀▄▌▐░▒▓")
T3_SENSITIVE_LINE_RE = re.compile(
    r"(?i)(?:token="
    r"|['\"]?(?:access_?token|refresh_?token|id_?token|auth_?token|session_?token"
    r"|device_?token|invite_?token|relay_?token|public_?key_?b64|pairing_?url"
    r"|token|api[_-]?key|secret|client_?secret|authorization|cookie|set-cookie)['\"]?\s*[:=]\s*\S+"
    r"|pair#|/pair[/?#]|t3://|t3code://|orca://pair[^\s\"'<>]*|bearer\s+\S+"
    r"|[?&]k=[A-Za-z0-9._~-]{8,}|MAGPIE_WEB_KEY\s*[:=]\s*\S+|magpie_web_[A-Za-z0-9_]+\s*=\s*\S+)"
    r"|sk-|eyJ|AIza[0-9A-Za-z_-]{20,}|gh[pousr]_[0-9A-Za-z_]{20,}"
    r"|github_pat_[0-9A-Za-z_]{20,}|gsk_[0-9A-Za-z]{20,}|xai-[0-9A-Za-z]{20,}"
    r"|ak_[0-9A-Za-z]{20,}|r8_[0-9A-Za-z]{20,}|hf_[0-9A-Za-z]{20,}"
)


def _is_qr_line(clean_line):
    count = 0
    for ch in clean_line:
        if ch in T3_QR_GLYPHS:
            count += 1
            if count >= 2:
                return True
    return False

FORBIDDEN_ENV = {
    "PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT",
    "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "DYLD_FRAMEWORK_PATH",
    "BASH_ENV", "ENV", "PROMPT_COMMAND", "IFS", "HOME", "USER", "LOGNAME",
    "SHELL", "SSH_AUTH_SOCK", "SSH_AGENT_PID", "PYTHONPATH", "PYTHONSTARTUP",
    "NODE_OPTIONS", "NODE_PATH", "RUBYOPT", "PERL5OPT", "GIT_SSH_COMMAND",
    "GIT_EXEC_PATH", "BROWSER", "TERM",
}
FORBIDDEN_SHARED = {
    ".credentials.json", "credentials.json", "auth.json", "oauth_creds.json",
    "secrets.json", ".env", "token.json", "tokens.json", "hosts.json",
    "apps.json", "google_accounts.json",
}
AUTH_FIELD_KEYWORDS = (
    "token", "key", "secret", "auth", "session",
    "credential", "cookie", "oauth", "refresh", "email", "user",
)
SAFE_USER_BIN_DIRS = (
    HOME / ".local/bin",
    HOME / ".cursor/bin",
    HOME / ".opencode/bin",
    HOME / ".amp/bin",
    HOME / ".kimi/bin",
    HOME / ".factory/bin",
)


def is_safe_env_name(name):
    return (
        isinstance(name, str)
        and bool(ENV_NAME_RE.match(name))
        and name not in FORBIDDEN_ENV
        and not name.startswith("CAGE_")
    )


def enriched_path(exclude_dir=None):
    """Build a reliable search PATH covering user-local, mise, uv, cargo and brew bins."""
    candidates = [
        str(HOME / ".local/bin"),
        str(HOME / ".local/share/mise/shims"),
        str(HOME / ".cargo/bin"),
        str(HOME / ".cursor/bin"),
        str(HOME / ".opencode/bin"),
        str(HOME / ".amp/bin"),
        str(HOME / ".kimi/bin"),
        str(HOME / ".factory/bin"),
        str(HOME / ".npm-global/bin"),
    ]
    if sys.platform == "darwin":
        candidates.extend([
            "/usr/local/libexec/cage-cli/bin",
            "/opt/homebrew/bin",
            "/usr/local/bin",
        ])
    existing = os.environ.get("PATH", os.defpath).split(os.pathsep)
    seen = set()
    out = []
    exclude_resolved = str(Path(exclude_dir).resolve()) if exclude_dir else None
    for item in [*candidates, *existing]:
        if not item:
            continue
        if exclude_resolved:
            try:
                if str(Path(item).resolve()) == exclude_resolved:
                    continue
            except OSError:
                if item == str(exclude_dir):
                    continue
        # Never pick our own agent/auto/tool shims when resolving real binaries.
        if (
            item.startswith(str(SHIMS_ROOT))
            or item == str(AUTO_SHIMS_DIR)
            or item == str(ORCA_SHIMS_DIR)
            or item == str(T3CODE_ROOT / "shims")
            or item in ("/usr/local/lib/cage-claude-bin", "/usr/local/lib/cage-codex-bin")
        ):
            continue
        if item not in seen:
            seen.add(item)
            out.append(item)
    return os.pathsep.join(out)


def find_binary(spec, path_str=None):
    path_str = path_str or enriched_path()
    names = [spec.get("binary", "")] + list(spec.get("fallback_binaries") or [])
    for name in names:
        if not name:
            continue
        found = shutil.which(name, path=path_str)
        if found:
            return found
    return ""


def read_json(path, default=None):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return default


def atomic_write_text(path, text, mode=0o600):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, delete=False) as tmp:
        tmp.write(text)
        tmp_name = tmp.name
    os.chmod(tmp_name, mode)
    os.replace(tmp_name, path)


def atomic_write_json(path, value, mode=0o600):
    atomic_write_text(path, json.dumps(value, ensure_ascii=False, separators=(",", ":")), mode=mode)


def fast_npm_package_version(binary_path):
    """Read version from nearest package.json in 0.2 ms without spawning Node.js."""
    try:
        resolved = Path(binary_path).resolve()
    except OSError:
        return ""
    if "node_modules" not in resolved.parts:
        return ""
    cur = resolved.parent
    for _ in range(5):
        pkg = cur / "package.json"
        if pkg.is_file():
            data = read_json(pkg)
            if isinstance(data, dict) and isinstance(data.get("version"), str):
                ver = data["version"].strip()
                if ver and len(ver) <= 64:
                    return ver
        if cur.name == "node_modules" or cur == cur.parent:
            break
        cur = cur.parent
    return ""


def binary_fingerprint(binary_path):
    try:
        p = Path(binary_path)
        resolved = p.resolve()
        st = resolved.stat()
        return f"{resolved}:{st.st_mtime_ns}:{st.st_size}"
    except OSError:
        return ""


def _cache_get(cache, key, lock=None):
    if lock is not None:
        with lock:
            return cache.get(key)
    return cache.get(key)


def _cache_set(cache, key, value, lock=None):
    if lock is not None:
        with lock:
            cache[key] = value
    else:
        cache[key] = value


def read_cli_version(binary_path, version_args, path_str, cache, cache_lock=None):
    fp = binary_fingerprint(binary_path)
    if fp:
        cached = _cache_get(cache, fp, cache_lock)
        if isinstance(cached, str) and cached:
            return cached, False
    ver = fast_npm_package_version(binary_path)
    if not ver:
        env = os.environ.copy()
        env["PATH"] = path_str
        env["DISABLE_AUTOUPDATER"] = "1"
        env["NO_COLOR"] = "1"
        args = [binary_path, *(version_args or ["--version"])]
        try:
            res = subprocess.run(args, env=env, stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 text=True, timeout=4)
            output = (res.stdout or "").strip()
            for line in output.splitlines()[:5]:
                m = VERSION_RE.search(line)
                if m:
                    ver = m.group(1)
                    break
            if not ver and res.returncode == 0 and output:
                ver = output.splitlines()[0].strip()[:64]
        except (OSError, subprocess.SubprocessError):
            ver = ""
    if fp and ver:
        _cache_set(cache, fp, ver, cache_lock)
        return ver, True
    return ver, False


def check_prerequisites(path_str, cache=None, cache_lock=None):
    out = {}
    dirty = False
    env = os.environ.copy()
    env["PATH"] = path_str
    tools = {
        "node": ["node", "--version"],
        "npm": ["npm", "--version"],
        "python3": ["python3", "--version"],
        "uv": ["uv", "--version"],
        "git": ["git", "--version"],
        "curl": ["curl", "--version"],
    }

    def check_one(item):
        name, cmd = item
        binary = shutil.which(cmd[0], path=path_str)
        if not binary:
            return name, {"available": False, "version": ""}, False
        fp = binary_fingerprint(binary)
        cache_key = f"prereq:{name}:{fp}" if fp else ""
        if cache is not None and cache_key:
            cached = _cache_get(cache, cache_key, cache_lock)
            if isinstance(cached, dict) and cached.get("available") is True:
                return name, {"available": True, "version": str(cached.get("version") or "")}, False
        try:
            res = subprocess.run([binary, *cmd[1:]], env=env, stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 text=True, timeout=3)
            line = (res.stdout or "").strip().splitlines()[0] if res.stdout else ""
            m = VERSION_RE.search(line)
            info = {"available": res.returncode == 0, "version": m.group(1) if m else line[:40]}
            if info["available"] and cache is not None and cache_key:
                _cache_set(cache, cache_key, info, cache_lock)
                return name, info, True
            return name, info, False
        except (OSError, subprocess.SubprocessError):
            return name, {"available": False, "version": ""}, False

    with ThreadPoolExecutor(max_workers=6) as pool:
        for name, info, updated in pool.map(check_one, tools.items()):
            out[name] = info
            dirty = dirty or updated
    return out, dirty


def profile_root(agent_id, account_id):
    if not CLI_ID_RE.match(agent_id):
        raise ValueError("invalid agent_id")
    if account_id != "default" and not ACCOUNT_ID_RE.match(account_id):
        raise ValueError("invalid account_id")
    return ACCOUNTS_ROOT / agent_id / account_id


def resolve_dirs(spec, base):
    iso = spec.get("isolation") or {}
    mode = iso.get("mode", "xdg")
    if base.name == "default":
        defaults = iso.get("default_config_paths") or []
        cfg = Path(os.path.expanduser(defaults[0])) if defaults else (HOME / f".{spec['id']}")
        data = Path(os.path.expanduser(defaults[1])) if len(defaults) > 1 else cfg
        return {"profile": base, "config": cfg, "data": data, "state": data}
    if mode == "env_home":
        subdir = iso.get("config_subdir") or f".{spec['id']}"
        cfg = base / subdir
        return {"profile": base, "config": cfg, "data": base / "data", "state": base / "state"}
    return {
        "profile": base,
        "config": base / "config",
        "data": base / "data",
        "state": base / "state",
    }


def decode_jwt_claims(token):
    """Decode ONLY public payload claims of a JWT without returning or validating the secret."""
    if not isinstance(token, str) or token.count(".") < 2:
        return {}
    try:
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        raw = base64.urlsafe_b64decode(payload.encode("ascii"))
        data = json.loads(raw.decode("utf-8"))
        return data if isinstance(data, dict) else {}
    except Exception:
        return {}


def ensure_codex_file_credentials_store(cfg_dir):
    """Idempotently ensure top-level `cli_auth_credentials_store = \"file\"` in Codex config.toml."""
    cfg_dir = Path(cfg_dir)
    cfg_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    cfg_path = cfg_dir / "config.toml"
    setting_line = 'cli_auth_credentials_store = "file"\n'
    if not cfg_path.exists():
        atomic_write_text(cfg_path, setting_line, mode=0o600)
        return
    try:
        existing_mode = cfg_path.stat().st_mode & 0o777
        text = cfg_path.read_text(encoding="utf-8", errors="ignore")
    except OSError:
        return
    lines = text.splitlines(keepends=True)
    first_table_idx = None
    for idx, line in enumerate(lines):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("["):
            first_table_idx = idx
            break
        if re.match(r"^cli_auth_credentials_store\s*=", stripped):
            return
    if first_table_idx is not None:
        prefix = "".join(lines[:first_table_idx])
        suffix = "".join(lines[first_table_idx:])
        if prefix and not prefix.endswith("\n"):
            prefix += "\n"
        updated = prefix + setting_line + ("\n" if not prefix else "") + suffix
    else:
        body = text
        if body and not body.endswith("\n"):
            body += "\n"
        updated = body + setting_line
    atomic_write_text(cfg_path, updated, mode=existing_mode or 0o600)


def _parse_timestamp_seconds(val):
    if val is None:
        return 0
    if isinstance(val, bool):
        return None
    if isinstance(val, (int, float)):
        if val < 0:
            return None
        if val > 1e12:
            return int(val / 1000)
        return int(val)
    if isinstance(val, str):
        s = val.strip()
        if not s:
            return 0
        if s.isdigit():
            n = int(s)
            return int(n / 1000) if n > 1e12 else n
        try:
            dt = datetime.fromisoformat(s.replace("Z", "+00:00"))
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=timezone.utc)
            return int(dt.timestamp())
        except (ValueError, OverflowError):
            return None
    return None


def _parse_codex_window(window, observed_at, now):
    if not isinstance(window, dict):
        return None
    used = window.get("used_percent")
    if used is None:
        used = window.get("used_percentage")
    if isinstance(used, bool) or not isinstance(used, (int, float)) or not (0 <= used <= 100):
        return None
    resets_at = _parse_timestamp_seconds(window.get("resets_at"))
    if resets_at is None:
        return None
    if resets_at > 0 and resets_at <= now:
        return None
    return {
        "used_percentage": round(float(used), 2),
        "resets_at": int(resets_at),
        "observed_at": int(observed_at),
    }


def _find_newest_codex_rollouts(sessions_dir, limit=3):
    sessions_dir = Path(sessions_dir)
    if not sessions_dir.is_dir():
        return []
    found = []

    def collect(directory, depth):
        if len(found) >= limit or depth > 4:
            return
        try:
            entries = sorted(directory.iterdir(), key=lambda p: p.name, reverse=True)
        except OSError:
            return
        for entry in entries:
            if entry.is_file() and entry.name.startswith("rollout-") and entry.name.endswith(".jsonl"):
                found.append(entry)
                if len(found) >= limit:
                    return
        for entry in entries:
            if entry.is_dir() and not entry.is_symlink():
                collect(entry, depth + 1)
                if len(found) >= limit:
                    return

    collect(sessions_dir, 0)
    return found


def read_codex_quota(cfg_dir):
    """Fast O(1) bounded reader for Codex 5h/7d rate_limits from newest rollout-*.jsonl files."""
    rollouts = _find_newest_codex_rollouts(Path(cfg_dir) / "sessions", limit=3)
    if not rollouts:
        return None
    now = int(time.time())
    five_hour = None
    seven_day = None
    for path in rollouts:
        try:
            st = path.stat()
            size = st.st_size
            if size <= 0:
                continue
            offset = max(0, size - 65536)
            with path.open("rb") as fp:
                if offset > 0:
                    fp.seek(offset)
                raw = fp.read()
        except OSError:
            continue
        if offset > 0:
            nl = raw.find(b"\n")
            if nl >= 0:
                raw = raw[nl + 1:]
        file_mtime = int(st.st_mtime) if st.st_mtime > 0 else now
        for raw_line in reversed(raw.splitlines()):
            if b"rate_limits" not in raw_line:
                continue
            try:
                obj = json.loads(raw_line.decode("utf-8", errors="ignore"))
            except ValueError:
                continue
            if not isinstance(obj, dict):
                continue
            rl = obj.get("rate_limits")
            if not isinstance(rl, dict) and isinstance(obj.get("payload"), dict):
                rl = obj["payload"].get("rate_limits")
            if not isinstance(rl, dict) and isinstance(obj.get("msg"), dict):
                rl = obj["msg"].get("rate_limits")
            if not isinstance(rl, dict):
                continue
            obs = _parse_timestamp_seconds(obj.get("timestamp")) or _parse_timestamp_seconds(rl.get("observed_at")) or file_mtime
            if not obs or obs > now + 300:
                obs = min(file_mtime, now)
            if five_hour is None:
                five_hour = _parse_codex_window(rl.get("primary") or rl.get("five_hour"), obs, now)
            if seven_day is None:
                seven_day = _parse_codex_window(rl.get("secondary") or rl.get("seven_day"), obs, now)
            if five_hour is not None and seven_day is not None:
                break
        if five_hour is not None and seven_day is not None:
            break
    result = {}
    if five_hour is not None:
        result["five_hour"] = five_hour
    if seven_day is not None:
        result["seven_day"] = seven_day
    if not result:
        return None
    result["observed_at"] = max(w["observed_at"] for w in result.values())
    return result


def _json_has_nonempty_scalar(obj, depth=0):
    if depth > 6:
        return False
    if isinstance(obj, str):
        return bool(obj.strip())
    if isinstance(obj, dict):
        return any(_json_has_nonempty_scalar(v, depth + 1) for v in obj.values())
    if isinstance(obj, list):
        return any(_json_has_nonempty_scalar(v, depth + 1) for v in obj[:32])
    return False


def _json_has_auth_field(obj, depth=0):
    if depth > 6:
        return False
    if isinstance(obj, dict):
        for k, v in obj.items():
            kl = str(k).lower()
            if any(kw in kl for kw in AUTH_FIELD_KEYWORDS):
                if isinstance(v, str) and v.strip():
                    return True
                if isinstance(v, dict) and v and _json_has_nonempty_scalar(v, depth + 1):
                    return True
                if isinstance(v, list) and any(
                    (isinstance(x, str) and x.strip()) or (isinstance(x, dict) and _json_has_nonempty_scalar(x, depth + 1))
                    for x in v[:32]
                ):
                    return True
            if isinstance(v, (dict, list)) and _json_has_auth_field(v, depth + 1):
                return True
    elif isinstance(obj, list):
        for item in obj[:32]:
            if isinstance(item, (dict, list)) and _json_has_auth_field(item, depth + 1):
                return True
    return False


def _env_file_has_value(path):
    try:
        text = Path(path).read_text(encoding="utf-8", errors="ignore")
    except OSError:
        return False
    for line in text.splitlines():
        s = line.strip()
        if not s or s.startswith("#"):
            continue
        if s.startswith("export "):
            s = s[7:].strip()
        if "=" not in s:
            continue
        key, val = s.split("=", 1)
        key = key.strip()
        val = val.strip()
        if len(val) >= 2 and val[0] == val[-1] and val[0] in ("'", '"'):
            val = val[1:-1].strip()
        if key and val and ENV_NAME_RE.match(key):
            return True
    return False


def _candidate_oauth_roots(spec, dirs):
    """Return deduplicated candidate directories for OAuth credential inspection."""
    roots = []
    seen = set()

    def add(p):
        if p is None:
            return
        key = str(p)
        if key not in seen:
            seen.add(key)
            roots.append(p)

    cfg = dirs.get("config")
    data_dir = dirs.get("data")
    state_dir = dirs.get("state")
    add(cfg)
    add(data_dir)
    add(state_dir)

    iso = spec.get("isolation") or {}
    if iso.get("mode", "xdg") == "xdg":
        subdirs = []
        if iso.get("config_subdir"):
            subdirs.append(iso["config_subdir"])
        for raw in iso.get("default_config_paths") or []:
            name = Path(os.path.expanduser(raw)).name
            if name:
                subdirs.append(name)
        agent_id = spec.get("id", "")
        binary = spec.get("binary", "")
        if agent_id:
            subdirs.append(agent_id)
        if binary:
            subdirs.append(binary)
        if agent_id == "antigravity":
            subdirs.extend([
                "antigravity-cli",
                "antigravity",
                "gemini/antigravity-cli",
                "gemini",
            ])
        for sub in subdirs:
            if not isinstance(sub, str) or not sub or sub.startswith("/") or ".." in Path(sub).parts:
                continue
            for base_dir in (cfg, data_dir, state_dir):
                if base_dir is not None:
                    add(base_dir / sub)
    return roots


def inspect_oauth_files(spec, dirs):
    """Extract ONLY non-secret public identity from an agent's config/data directory."""
    agent_id = spec.get("id", "")
    cfg = dirs["config"]
    data_dir = dirs["data"]
    auth_spec = spec.get("auth") or {}

    if agent_id == "codex":
        auth = read_json(cfg / "auth.json")
        if isinstance(auth, dict):
            mode = auth.get("auth_mode") or ""
            tokens = auth.get("tokens") if isinstance(auth.get("tokens"), dict) else auth
            id_claims = decode_jwt_claims(tokens.get("id_token") or "")
            acc_claims = decode_jwt_claims(tokens.get("access_token") or "")
            auth_ns = acc_claims.get("https://api.openai.com/auth") or id_claims.get("https://api.openai.com/auth") or {}
            profile_ns = acc_claims.get("https://api.openai.com/profile") or {}
            email = id_claims.get("email") or profile_ns.get("email") or acc_claims.get("email") or ""
            plan = auth_ns.get("chatgpt_plan_type") or ""
            org = auth_ns.get("chatgpt_account_id") or auth_ns.get("organization_id") or ""
            exp = acc_claims.get("exp") or id_claims.get("exp") or 0
            if tokens.get("access_token") or tokens.get("refresh_token") or auth.get("OPENAI_API_KEY"):
                is_key = mode == "apiKey" or (bool(auth.get("OPENAI_API_KEY")) and not tokens.get("access_token"))
                out = {
                    "logged_in": True,
                    "auth_mode": "api_key" if is_key else "oauth",
                    "email": str(email),
                    "organization": str(org),
                    "plan": str(plan),
                    "expires_at": int(exp) if isinstance(exp, (int, float)) else 0,
                }
                quota = read_codex_quota(cfg)
                if quota:
                    out["quota"] = quota
                return out
        return {"logged_in": False}

    if agent_id in ("gemini", "antigravity"):
        roots = _candidate_oauth_roots(spec, dirs)
        email = ""
        for root in roots:
            accounts = read_json(root / "google_accounts.json")
            if isinstance(accounts, dict):
                active = accounts.get("active")
                if isinstance(active, str) and active.strip():
                    email = active.strip()
                    break
                if isinstance(active, dict) and isinstance(active.get("email"), str) and active["email"].strip():
                    email = active["email"].strip()
                    break
        for root in roots:
            oauth = read_json(root / "oauth_creds.json")
            has_token = isinstance(oauth, dict) and any(
                isinstance(oauth.get(k), str) and oauth.get(k).strip()
                for k in ("access_token", "refresh_token", "id_token")
            )
            if has_token:
                claims = decode_jwt_claims(oauth.get("id_token") or "")
                found_email = email or claims.get("email") or ""
                exp_ms = oauth.get("expiry_date") or 0
                exp = int(exp_ms / 1000) if isinstance(exp_ms, (int, float)) and exp_ms > 1e12 else int(exp_ms or 0)
                return {
                    "logged_in": True,
                    "auth_mode": "oauth",
                    "email": str(found_email),
                    "plan": "Google OAuth",
                    "expires_at": exp,
                }
        if agent_id == "antigravity":
            for root in roots:
                pbtxt = root / "jetski_state.pbtxt"
                if pbtxt.is_file():
                    try:
                        if pbtxt.stat().st_size > 2:
                            text = pbtxt.read_text(encoding="utf-8", errors="ignore").strip()
                            if text:
                                found_email = email
                                if not found_email:
                                    m = re.search(r'(?i)\b(?:user_)?email\s*:\s*"([^"\s@]+@[^"\s@]+)"', text)
                                    if m:
                                        found_email = m.group(1)
                                return {
                                    "logged_in": True,
                                    "auth_mode": "oauth",
                                    "email": str(found_email),
                                    "plan": "Google OAuth",
                                }
                    except OSError:
                        pass
                creds = read_json(root / "credentials.json")
                if isinstance(creds, dict) and _json_has_auth_field(creds):
                    claims = decode_jwt_claims(creds.get("id_token") or "")
                    found_email = email or claims.get("email") or creds.get("email") or ""
                    return {
                        "logged_in": True,
                        "auth_mode": "oauth",
                        "email": str(found_email),
                        "plan": "Google OAuth",
                    }
        return {"logged_in": False}

    if agent_id == "opencode":
        for candidate in (data_dir / "opencode/auth.json", data_dir / "auth.json", cfg / "opencode/auth.json", cfg / "auth.json"):
            auth = read_json(candidate)
            if isinstance(auth, dict) and len(auth) > 0:
                providers = sorted(k for k, v in auth.items() if isinstance(k, str) and v)
                if providers:
                    return {
                        "logged_in": True,
                        "auth_mode": "oauth",
                        "provider": ", ".join(providers[:4]),
                        "plan": f"{len(providers)} 个供应商",
                    }
        return {"logged_in": False}

    if agent_id == "copilot":
        for candidate in (
            cfg / "github-copilot/hosts.json",
            cfg / "github-copilot/apps.json",
            cfg / "hosts.json",
            cfg / "apps.json",
            cfg / "config.json",
        ):
            data = read_json(candidate)
            if isinstance(data, dict):
                user = data.get("user") or data.get("login") or data.get("logged_in_users")
                if isinstance(user, list) and user:
                    first = user[0]
                    user = first.get("login") if isinstance(first, dict) else str(first)
                nested_token = False
                for val in data.values():
                    if isinstance(val, dict):
                        if not user and isinstance(val.get("user"), str) and val["user"].strip():
                            user = val["user"].strip()
                        if (isinstance(val.get("oauth_token"), str) and val["oauth_token"].strip()) or (
                            isinstance(val.get("token"), str) and val["token"].strip()
                        ):
                            nested_token = True
                top_token = (
                    (isinstance(data.get("oauth_token"), str) and bool(data["oauth_token"].strip()))
                    or (isinstance(data.get("token"), str) and bool(data["token"].strip()))
                )
                if user or top_token or nested_token:
                    return {"logged_in": True, "auth_mode": "oauth", "username": str(user or ""), "plan": "GitHub Copilot"}
        return {"logged_in": False}

    if agent_id == "qwen":
        oauth = read_json(cfg / "oauth_creds.json")
        if isinstance(oauth, dict) and (oauth.get("access_token") or oauth.get("refresh_token")):
            claims = decode_jwt_claims(oauth.get("id_token") or oauth.get("access_token") or "")
            return {
                "logged_in": True,
                "auth_mode": "oauth",
                "email": str(claims.get("email") or ""),
                "username": str(claims.get("name") or claims.get("sub") or ""),
                "plan": "Qwen OAuth",
            }
        return {"logged_in": False}

    # Generic fallback for amp, cline, cursor, vibe, kimi, droid, pi, custom agents.
    candidate_roots = _candidate_oauth_roots(spec, dirs)
    for rel in auth_spec.get("credential_files") or []:
        for root in candidate_roots:
            target = root / rel
            if not target.is_file():
                continue
            try:
                if target.stat().st_size <= 2:
                    continue
            except OSError:
                continue
            if target.suffix == ".json":
                data = read_json(target)
                if not _json_has_auth_field(data):
                    continue
                info = {"logged_in": True, "auth_mode": "oauth"}
                if isinstance(data, dict):
                    for k in ("email", "user_email", "account_email"):
                        if isinstance(data.get(k), str) and "@" in data[k]:
                            info["email"] = data[k]
                            break
                    for k in ("username", "user", "login"):
                        if isinstance(data.get(k), str) and data[k].strip():
                            info["username"] = data[k].strip()
                            break
                return info
            if target.name == ".env" or target.suffix == ".env":
                if not _env_file_has_value(target):
                    continue
                return {"logged_in": True, "auth_mode": "oauth"}
            try:
                content = target.read_text(encoding="utf-8", errors="ignore").strip()
                if content:
                    return {"logged_in": True, "auth_mode": "oauth"}
            except OSError:
                continue
    return {"logged_in": False}


def read_api_key_meta(base):
    cred = read_json(base / "credentials.json")
    if not isinstance(cred, dict) or not cred.get("api_key"):
        return None
    return {
        "logged_in": True,
        "auth_mode": "api_key",
        "provider": str(cred.get("provider") or ""),
        "base_url": str(cred.get("base_url") or ""),
        "model": str(cred.get("model") or ""),
        "key_hint": str(cred.get("key_hint") or ""),
        "key_fingerprint": str(cred.get("key_fingerprint") or ""),
        "env_var": str(cred.get("env_var") or ""),
    }


def snapshot_account(spec, account_id):
    base = profile_root(spec["id"], account_id)
    if account_id != "default":
        api_meta = read_api_key_meta(base)
        if api_meta:
            return api_meta
    dirs = resolve_dirs(spec, base)
    return inspect_oauth_files(spec, dirs)


def probe_all(payload):
    specs = payload.get("specs") or []
    force = bool(payload.get("force"))
    path_str = enriched_path()
    cache = {} if force else (read_json(PROBE_CACHE_FILE, {}) or {})
    if not isinstance(cache, dict):
        cache = {}
    cache_lock = threading.Lock()
    prereqs, prereq_dirty = check_prerequisites(path_str, cache=cache, cache_lock=cache_lock)
    cache_dirty = prereq_dirty

    def probe_one(spec):
        agent_id = spec.get("id", "")
        binary = find_binary(spec, path_str)
        if not binary:
            return agent_id, {"installed": False, "binary_path": "", "version": "", "default_logged_in": False}, False
        version, updated = read_cli_version(binary, spec.get("version_args"), path_str, cache, cache_lock=cache_lock)
        default_logged = False
        if spec.get("isolation", {}).get("mode") != "claude_native":
            try:
                snap = snapshot_account(spec, "default")
                default_logged = bool(snap.get("logged_in"))
            except Exception:
                default_logged = False
        return agent_id, {
            "installed": True,
            "binary_path": binary,
            "version": version,
            "default_logged_in": default_logged,
        }, updated

    agents = {}
    with ThreadPoolExecutor(max_workers=8) as pool:
        for agent_id, state, updated in pool.map(probe_one, specs):
            if agent_id:
                agents[agent_id] = state
                cache_dirty = cache_dirty or updated

    if cache_dirty:
        with cache_lock:
            if len(cache) > 256:
                snapshot_cache = dict(list(cache.items())[-128:])
            else:
                snapshot_cache = dict(cache)
        try:
            atomic_write_json(PROBE_CACHE_FILE, snapshot_cache, mode=0o600)
        except OSError:
            pass
    return {"prerequisites": prereqs, "agents": agents}


def ensure_npm_user_prefix(env):
    prefix = str(HOME / ".local")
    (HOME / ".local/bin").mkdir(parents=True, exist_ok=True)
    try:
        cur = subprocess.run(["npm", "prefix", "--global"], env=env, stdin=subprocess.DEVNULL,
                             capture_output=True, text=True, timeout=10)
        if cur.returncode == 0 and cur.stdout.strip() != prefix:
            subprocess.run(["npm", "config", "set", "--location=user", f"prefix={prefix}"],
                           env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, timeout=10)
    except (OSError, subprocess.SubprocessError):
        pass


def install_agent(payload):
    spec = payload["spec"]
    channel = payload["channel"]
    kind = channel.get("kind")
    path_str = enriched_path()
    env = os.environ.copy()
    env["PATH"] = path_str
    env["npm_config_fetch_retries"] = "3"
    env["npm_config_fetch_timeout"] = "120000"
    env["npm_config_audit"] = "false"
    env["npm_config_fund"] = "false"

    if kind == "existing":
        binary = find_binary(spec, path_str)
        if not binary:
            raise ValueError(f"未在环境 PATH 中找到 {spec['binary']}，请先确认已安装")
    elif kind == "npm":
        pkg = channel.get("package", "")
        if not NPM_PKG_RE.match(pkg):
            raise ValueError("invalid npm package")
        npm = shutil.which("npm", path=path_str)
        if not npm:
            raise ValueError("环境中未找到 npm，请先安装 Node.js 或同步环境工具")
        ensure_npm_user_prefix(env)
        print(f"Installing {pkg}@latest via npm official registry...", flush=True)
        res = subprocess.run(
            [npm, "install", "--registry=https://registry.npmjs.org", "--prefix", str(HOME / ".local"), "--global", f"{pkg}@latest"],
            env=env, stdin=subprocess.DEVNULL,
        )
        if res.returncode != 0:
            raise ValueError(f"npm 安装 {pkg} 失败（退出码 {res.returncode}）")
    elif kind == "uv":
        pkg = channel.get("package", "")
        if not PY_PKG_RE.match(pkg):
            raise ValueError("invalid python package")
        uv = shutil.which("uv", path=path_str)
        if uv:
            print(f"Installing {pkg} via uv tool...", flush=True)
            res = subprocess.run([uv, "tool", "install", "--upgrade", pkg], env=env, stdin=subprocess.DEVNULL)
        else:
            pipx = shutil.which("pipx", path=path_str)
            if pipx:
                print(f"Installing {pkg} via pipx...", flush=True)
                res = subprocess.run([pipx, "install", "--force", pkg], env=env, stdin=subprocess.DEVNULL)
            else:
                py = shutil.which("python3", path=path_str) or sys.executable
                print(f"Installing {pkg} via python3 -m pip --user...", flush=True)
                res = subprocess.run([py, "-m", "pip", "install", "--user", "--upgrade", pkg], env=env, stdin=subprocess.DEVNULL)
        if res.returncode != 0:
            raise ValueError(f"安装 Python 工具 {pkg} 失败（退出码 {res.returncode}）")
    elif kind == "pip":
        pkg = channel.get("package", "")
        if not PY_PKG_RE.match(pkg):
            raise ValueError("invalid python package")
        py = shutil.which("python3", path=path_str) or sys.executable
        print(f"Installing {pkg} via pip...", flush=True)
        res = subprocess.run([py, "-m", "pip", "install", "--user", "--upgrade", pkg], env=env, stdin=subprocess.DEVNULL)
        if res.returncode != 0:
            raise ValueError(f"pip 安装 {pkg} 失败（退出码 {res.returncode}）")
    elif kind == "script":
        script_url = channel.get("script_url", "")
        parsed = urlparse(script_url)
        if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password:
            raise ValueError("invalid installer URL")
        curl = shutil.which("curl", path=path_str)
        if not curl:
            raise ValueError("环境中未找到 curl")
        print(f"Downloading official installer from {parsed.hostname}...", flush=True)
        with tempfile.NamedTemporaryFile(prefix="cage-cli-install-", suffix=".sh", delete=False) as tmp:
            script_path = tmp.name
        try:
            dl = subprocess.run(
                [curl, "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "-fsSL", "--connect-timeout", "20", "-o", script_path, script_url],
                env=env, stdin=subprocess.DEVNULL,
            )
            if dl.returncode != 0:
                raise ValueError(f"下载官方安装脚本失败（curl 退出码 {dl.returncode}）")
            print(f"Running official installer for {spec['name']}...", flush=True)
            res = subprocess.run(["/bin/bash", script_path], env=env, stdin=subprocess.DEVNULL)
            if res.returncode != 0:
                raise ValueError(f"官方安装脚本执行失败（退出码 {res.returncode}）")
        finally:
            try:
                os.unlink(script_path)
            except OSError:
                pass
    else:
        raise ValueError(f"unsupported install channel kind: {kind}")

    # Invalidate probe cache and verify installation immediately.
    try:
        PROBE_CACHE_FILE.unlink(missing_ok=True)
    except OSError:
        pass
    refreshed_path = enriched_path()
    binary = find_binary(spec, refreshed_path)
    if not binary:
        raise ValueError(f"安装完成但未在 PATH 中找到可执行文件 {spec['binary']}，请检查安装日志")
    version, _ = read_cli_version(binary, spec.get("version_args"), refreshed_path, {})
    print(f"Installed {spec['name']} ({version or 'ready'}) at {binary}", flush=True)
    return {"installed": True, "binary_path": binary, "version": version}


def _is_in_safe_user_bin_dir(path):
    p = Path(path)
    try:
        parent = p.parent.resolve()
    except OSError:
        return False
    for allowed in SAFE_USER_BIN_DIRS:
        try:
            allowed_resolved = allowed.resolve()
        except OSError:
            continue
        if parent == allowed_resolved:
            try:
                st = p.lstat()
                if st.st_uid == os.getuid():
                    return True
            except OSError:
                return False
    return False


def uninstall_agent(payload):
    spec = payload["spec"]
    channel = payload.get("channel") or {}
    kind = channel.get("kind") or ""
    path_str = enriched_path()
    env = os.environ.copy()
    env["PATH"] = path_str

    if kind == "npm":
        pkg = channel.get("package", "")
        if not NPM_PKG_RE.match(pkg):
            raise ValueError("invalid npm package")
        npm = shutil.which("npm", path=path_str)
        if not npm:
            raise ValueError("环境中未找到 npm，无法卸载")
        res = subprocess.run(
            [npm, "uninstall", "--prefix", str(HOME / ".local"), "--global", pkg],
            env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True,
        )
        if res.returncode != 0:
            raise ValueError(f"npm 卸载 {pkg} 失败（退出码 {res.returncode}）")
    elif kind == "uv":
        pkg = channel.get("package", "")
        if not PY_PKG_RE.match(pkg):
            raise ValueError("invalid python package")
        succeeded = False
        errors = []
        uv = shutil.which("uv", path=path_str)
        if uv:
            res = subprocess.run([uv, "tool", "uninstall", pkg], env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True)
            if res.returncode == 0:
                succeeded = True
            else:
                errors.append(f"uv({res.returncode})")
        if not succeeded:
            pipx = shutil.which("pipx", path=path_str)
            if pipx:
                res = subprocess.run([pipx, "uninstall", pkg], env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True)
                if res.returncode == 0:
                    succeeded = True
                else:
                    errors.append(f"pipx({res.returncode})")
        if not succeeded:
            py = shutil.which("python3", path=path_str) or sys.executable
            res = subprocess.run([py, "-m", "pip", "uninstall", "-y", pkg], env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True)
            if res.returncode == 0:
                succeeded = True
            else:
                errors.append(f"pip({res.returncode})")
        if not succeeded and find_binary(spec, enriched_path()):
            detail = ", ".join(errors) if errors else "未找到可用的 Python 卸载工具"
            raise ValueError(f"卸载 Python 工具 {pkg} 失败：{detail}")
    elif kind == "pip":
        pkg = channel.get("package", "")
        if not PY_PKG_RE.match(pkg):
            raise ValueError("invalid python package")
        py = shutil.which("python3", path=path_str) or sys.executable
        res = subprocess.run(
            [py, "-m", "pip", "uninstall", "-y", pkg],
            env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True,
        )
        if res.returncode != 0:
            raise ValueError(f"pip 卸载 {pkg} 失败（退出码 {res.returncode}）")
    elif kind == "script":
        names = [n for n in [spec.get("binary", ""), *(spec.get("fallback_binaries") or [])] if n]
        removed = []
        for bin_dir in SAFE_USER_BIN_DIRS:
            for name in names:
                candidate = bin_dir / name
                if (candidate.exists() or candidate.is_symlink()) and _is_in_safe_user_bin_dir(candidate):
                    try:
                        candidate.unlink()
                        removed.append(str(candidate))
                    except OSError as exc:
                        raise ValueError(f"移除 {candidate} 失败：{exc}") from exc
        remaining = find_binary(spec, enriched_path())
        if remaining and not _is_in_safe_user_bin_dir(remaining):
            raise ValueError(f"检测到系统目录中的可执行文件 {remaining}，为保护系统安全不会自动删除，请手动卸载")
    elif kind not in ("", "existing"):
        raise ValueError(f"unsupported uninstall channel kind: {kind}")

    # Remove only the Cage-managed PATH shim; never delete user accounts or credentials.
    shim_dir = SHIMS_ROOT / spec["id"]
    if shim_dir.exists():
        shutil.rmtree(shim_dir, ignore_errors=True)
    try:
        PROBE_CACHE_FILE.unlink(missing_ok=True)
    except OSError:
        pass
    return {"ok": True}


def _validate_base_url(base_url):
    base_url = (base_url or "").strip()
    if base_url:
        parsed = urlparse(base_url)
        if parsed.scheme not in ("https", "http") or not parsed.hostname or parsed.username or parsed.password:
            raise ValueError("自定义网关地址无效")
        if parsed.scheme == "http" and parsed.hostname not in ("127.0.0.1", "localhost", "::1"):
            raise ValueError("非本机网关必须使用 HTTPS 地址")
    return base_url


def _validate_model(model):
    model = (model or "").strip()
    if len(model) > 128 or any(ch in model for ch in ("\x00", "\r", "\n")):
        raise ValueError("模型名称无效")
    return model


@contextmanager
def _profile_lock(base):
    """Serialize profile mutations (prepare/credentials/profile.env) per account."""
    os.umask(0o077)
    base = Path(base)
    base.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock_path = base / ".profile.lock"
    fd = os.open(str(lock_path), os.O_RDWR | os.O_CREAT, 0o600)
    os.chmod(lock_path, 0o600)
    with os.fdopen(fd, "a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def _quoted_literal(val):
    sv = str(val)
    if any(ch in sv for ch in ("\x00", "\r", "\n")):
        return None
    return shlex.quote(sv)


def _default_prep_hash(spec, account_id):
    canonical = json.dumps(
        {
            "spec": spec,
            "runtime": str(Path(__file__).resolve()),
            "account_id": account_id,
        },
        sort_keys=True,
        ensure_ascii=False,
        separators=(",", ":"),
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def set_api_key(payload):
    spec = payload["spec"]
    account_id = payload["account_id"]
    if account_id == "default":
        raise ValueError("API key must use an isolated profile")
    base = profile_root(spec["id"], account_id)
    api_key = (payload.get("api_key") or "").strip()
    if not api_key or len(api_key) > 4096 or any(ch in api_key for ch in ("\x00", "\r", "\n")):
        raise ValueError("API Key 格式无效")
    env_var = (payload.get("env_var") or "").strip()
    if not env_var:
        candidates = (spec.get("auth") or {}).get("api_key_env_vars") or []
        env_var = candidates[0] if candidates else ""
    if not is_safe_env_name(env_var):
        raise ValueError("API Key 环境变量名无效")
    base_url = _validate_base_url(payload.get("base_url"))
    model = _validate_model(payload.get("model"))
    provider = (payload.get("provider") or "").strip()[:64]
    prefix = api_key[:4] if len(api_key) >= 8 else ""
    suffix = api_key[-4:] if len(api_key) >= 4 else "****"
    hint = f"{prefix}...{suffix}" if prefix else f"***{suffix}"
    fingerprint = hashlib.sha256(f"{spec['id']}\n{env_var}\n{base_url}\n{api_key}".encode("utf-8")).hexdigest()[:24]

    with _profile_lock(base):
        record = {
            "api_key": api_key,
            "env_var": env_var,
            "base_url": base_url,
            "model": model,
            "provider": provider,
            "key_hint": hint,
            "key_fingerprint": fingerprint,
            "updated_at": int(time.time()),
        }
        atomic_write_json(base / "credentials.json", record, mode=0o600)
        write_profile_env(spec, account_id)
    return {
        "logged_in": True,
        "auth_mode": "api_key",
        "provider": provider,
        "base_url": base_url,
        "model": model,
        "key_hint": hint,
        "key_fingerprint": fingerprint,
        "env_var": env_var,
    }


def update_api_key_meta(payload):
    spec = payload["spec"]
    account_id = payload["account_id"]
    if account_id == "default":
        raise ValueError("API key must use an isolated profile")
    base = profile_root(spec["id"], account_id)
    with _profile_lock(base):
        cred_path = base / "credentials.json"
        cred = read_json(cred_path)
        if not isinstance(cred, dict) or not cred.get("api_key"):
            raise ValueError("未找到已保存的 API Key 凭据")

        if "env_var" in payload and payload["env_var"] is not None:
            env_var = str(payload["env_var"]).strip()
            if not env_var:
                candidates = (spec.get("auth") or {}).get("api_key_env_vars") or []
                env_var = cred.get("env_var") or (candidates[0] if candidates else "")
            if not is_safe_env_name(env_var):
                raise ValueError("API Key 环境变量名无效")
        else:
            env_var = str(cred.get("env_var") or "")

        base_url = _validate_base_url(payload["base_url"]) if ("base_url" in payload and payload["base_url"] is not None) else str(cred.get("base_url") or "")
        model = _validate_model(payload["model"]) if ("model" in payload and payload["model"] is not None) else str(cred.get("model") or "")
        provider = str(payload["provider"]).strip()[:64] if ("provider" in payload and payload["provider"] is not None) else str(cred.get("provider") or "")

        api_key = str(cred["api_key"])
        fingerprint = hashlib.sha256(f"{spec['id']}\n{env_var}\n{base_url}\n{api_key}".encode("utf-8")).hexdigest()[:24]

        cred.update({
            "env_var": env_var,
            "base_url": base_url,
            "model": model,
            "provider": provider,
            "key_fingerprint": fingerprint,
            "updated_at": int(time.time()),
        })
        atomic_write_json(cred_path, cred, mode=0o600)
        write_profile_env(spec, account_id)
    return {
        "logged_in": True,
        "auth_mode": "api_key",
        "provider": provider,
        "base_url": base_url,
        "model": model,
        "key_hint": str(cred.get("key_hint") or ""),
        "key_fingerprint": fingerprint,
        "env_var": env_var,
    }


def clear_credentials(payload):
    spec = payload["spec"]
    account_id = payload["account_id"]
    if account_id == "default":
        raise ValueError("refusing to delete default directory")
    base = profile_root(spec["id"], account_id)
    with _profile_lock(base):
        cred = base / "credentials.json"
        cred.unlink(missing_ok=True)
        write_profile_env(spec, account_id)
    return {"ok": True}


def sync_shared_resources(spec, base, dirs):
    """Safely share non-secret instructions/skills across an agent's accounts."""
    iso = spec.get("isolation") or {}
    resources = iso.get("shared_resources") or []
    if not resources or base.name == "default":
        return
    defaults = iso.get("default_config_paths") or []
    if not defaults:
        return
    common = Path(os.path.expanduser(defaults[0]))
    if iso.get("mode", "xdg") == "xdg":
        app_name = iso.get("config_subdir") or common.name
        if app_name and not app_name.startswith("/") and ".." not in Path(app_name).parts:
            config = dirs["config"] / app_name
        else:
            config = dirs["config"]
    else:
        config = dirs["config"]
    if config.resolve() == common.resolve():
        return
    common.mkdir(parents=True, exist_ok=True, mode=0o700)
    config.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock_dir = SHARED_ROOT / spec["id"]
    lock_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (lock_dir / "resources.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        for rel in resources:
            if rel.startswith("/") or ".." in Path(rel).parts or Path(rel).name.lower() in FORBIDDEN_SHARED:
                continue
            shared = common / rel
            local = config / rel
            if not shared.exists():
                continue
            if local.is_symlink():
                if local.resolve() == shared.resolve():
                    continue
                # Preserve existing custom symlink rather than overwriting.
                continue
            if local.exists():
                # Never overwrite a conflicting user-authored instruction/skill.
                if local.is_file() and shared.is_file() and local.read_bytes() == shared.read_bytes():
                    backup = lock_dir / "backups" / base.name / rel
                    backup.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                    if not backup.exists():
                        shutil.copy2(local, backup)
                    local.unlink()
                else:
                    continue
            local.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            local.symlink_to(shared, target_is_directory=shared.is_dir())


def isolation_env_exports(spec, account_id, dirs):
    """Return strictly non-secret isolation directory env vars for an account."""
    if account_id == "default":
        return {}
    iso = spec.get("isolation") or {}
    mode = iso.get("mode", "xdg")
    exports = {}
    if mode == "env_dir" and is_safe_env_name(iso.get("env_var", "")):
        exports[iso["env_var"]] = str(dirs["config"])
    elif mode == "env_home" and is_safe_env_name(iso.get("env_var", "")):
        exports[iso["env_var"]] = str(dirs["profile"])
    elif mode == "xdg":
        exports["XDG_CONFIG_HOME"] = str(dirs["config"])
        exports["XDG_DATA_HOME"] = str(dirs["data"])
        exports["XDG_STATE_HOME"] = str(dirs["state"])
    for k, template in (iso.get("extra_env") or {}).items():
        if is_safe_env_name(k) and isinstance(template, str) and not any(ch in template for ch in ("\x00", "\r", "\n")):
            exports[k] = (
                template.replace("{profile}", str(dirs["profile"]))
                .replace("{config}", str(dirs["config"]))
                .replace("{data}", str(dirs["data"]))
                .replace("{state}", str(dirs["state"]))
            )
    return exports


def write_profile_env(spec, account_id):
    """Write a 0600 POSIX shell profile.env for ultra-low-latency shim sourcing."""
    base = profile_root(spec["id"], account_id)
    dirs = resolve_dirs(spec, base)
    os.umask(0o077)
    base.mkdir(parents=True, exist_ok=True, mode=0o700)
    if account_id != "default":
        for d in (dirs["config"], dirs["data"], dirs["state"]):
            d.mkdir(parents=True, exist_ok=True, mode=0o700)
        if spec.get("id") == "codex":
            ensure_codex_file_credentials_store(dirs["config"])

    lines = ["# Generated by Cage agent-runtime (mode 0600)"]
    for k, v in isolation_env_exports(spec, account_id, dirs).items():
        q = _quoted_literal(v)
        if is_safe_env_name(k) and q is not None:
            lines.append(f"export {k}={q}")

    for k, v in ((spec.get("terminal") or {}).get("env_defaults") or {}).items():
        q = _quoted_literal(v)
        if is_safe_env_name(k) and q is not None:
            lines.append(f'if [ -z "${{{k}+x}}" ]; then export {k}={q}; fi')

    auth = spec.get("auth") or {}
    cred = read_json(base / "credentials.json") if account_id != "default" else None
    if isinstance(cred, dict) and cred.get("api_key"):
        env_var = cred.get("env_var", "")
        q_key = _quoted_literal(cred["api_key"])
        if is_safe_env_name(env_var) and q_key is not None:
            lines.append(f"export {env_var}={q_key}")
        base_url_var = auth.get("base_url_env_var") or ""
        if cred.get("base_url") and is_safe_env_name(base_url_var):
            q_url = _quoted_literal(cred["base_url"])
            if q_url is not None:
                lines.append(f"export {base_url_var}={q_url}")
        model_var = auth.get("model_env_var") or ""
        if cred.get("model") and is_safe_env_name(model_var):
            q_model = _quoted_literal(cred["model"])
            if q_model is not None:
                lines.append(f"export {model_var}={q_model}")
    elif account_id != "default":
        for var in auth.get("strip_env_on_oauth") or []:
            if is_safe_env_name(var):
                lines.append(f"unset {var}")

    env_path = base / "profile.env"
    atomic_write_text(env_path, "\n".join(lines) + "\n", mode=0o600)
    return env_path


def build_agent_env(spec, account_id, for_login=False):
    base = profile_root(spec["id"], account_id)
    dirs = resolve_dirs(spec, base)
    if account_id != "default":
        for d in (dirs["profile"], dirs["config"], dirs["data"], dirs["state"]):
            d.mkdir(parents=True, exist_ok=True, mode=0o700)
        if spec.get("id") == "codex":
            ensure_codex_file_credentials_store(dirs["config"])

    env = os.environ.copy()
    env["PATH"] = enriched_path()
    auth = spec.get("auth") or {}

    for k, v in isolation_env_exports(spec, account_id, dirs).items():
        env[k] = v

    for k, v in ((spec.get("terminal") or {}).get("env_defaults") or {}).items():
        if is_safe_env_name(k):
            env.setdefault(k, str(v))

    cred = read_json(base / "credentials.json") if account_id != "default" else None
    if not for_login and isinstance(cred, dict) and cred.get("api_key"):
        env_var = cred.get("env_var", "")
        if is_safe_env_name(env_var):
            env[env_var] = cred["api_key"]
        base_url_var = auth.get("base_url_env_var") or ""
        if cred.get("base_url") and is_safe_env_name(base_url_var):
            env[base_url_var] = cred["base_url"]
        model_var = auth.get("model_env_var") or ""
        if cred.get("model") and is_safe_env_name(model_var):
            env[model_var] = cred["model"]
    elif account_id != "default":
        for var in auth.get("strip_env_on_oauth") or []:
            env.pop(var, None)

    return base, dirs, env


def _agent_account_env_var(agent_id):
    return "CAGE_CLI_ACCOUNT_" + re.sub(r"[^A-Z0-9]", "_", agent_id.upper())


def ensure_shim(spec):
    """Create an ultra-low-latency pure-POSIX-shell PATH shim (<0.5ms overhead)."""
    agent_id = spec["id"]
    if not CLI_ID_RE.match(agent_id):
        raise ValueError("invalid agent_id")
    shim_dir = SHIMS_ROOT / agent_id
    shim_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    spec_file = shim_dir / "spec.json"
    atomic_write_json(spec_file, spec, mode=0o600)
    runtime_path = str(Path(__file__).resolve())
    accounts_agent_dir = str(ACCOUNTS_ROOT / agent_id)
    shims_root_str = str(SHIMS_ROOT)
    per_agent_var = _agent_account_env_var(agent_id)

    iso = spec.get("isolation") or {}
    config_flag = iso.get("config_flag") or ""
    use_flag_dir = iso.get("mode") == "flag_dir" and bool(FLAG_RE.match(config_flag))

    search_prefixes = [
        "$HOME/.local/bin",
        "$HOME/.local/share/mise/shims",
        "$HOME/.cargo/bin",
        "$HOME/.cursor/bin",
        "$HOME/.opencode/bin",
        "$HOME/.amp/bin",
        "$HOME/.kimi/bin",
        "$HOME/.factory/bin",
        "$HOME/.npm-global/bin",
    ]
    if sys.platform == "darwin":
        search_prefixes.extend([
            "/usr/local/libexec/cage-cli/bin",
            "/opt/homebrew/bin",
            "/usr/local/bin",
        ])
    search_expr = ":".join(search_prefixes) + ":${PATH:-/usr/bin:/bin}"

    for name in [spec["binary"], *(spec.get("fallback_binaries") or [])]:
        if not name or "/" in name or any(ch in name for ch in ("\x00", "\r", "\n")):
            continue
        shim = shim_dir / name
        q_py = shlex.quote(sys.executable)
        q_rt = shlex.quote(runtime_path)
        q_spec = shlex.quote(str(spec_file))
        q_accts = shlex.quote(accounts_agent_dir)
        q_shims = shlex.quote(shims_root_str)
        q_auto = shlex.quote(str(AUTO_SHIMS_DIR))
        q_orca_shims = shlex.quote(str(ORCA_SHIMS_DIR))
        q_takeover = shlex.quote(str(MAGPIE_TAKEOVER_FILE))
        q_name = shlex.quote(name)

        exec_lines = []
        if use_flag_dir:
            q_flag = shlex.quote(config_flag)
            exec_lines.append(
                'if [ "$_acct" != "default" ]; then\n'
                f'  exec "$_bin" {q_flag} "$_profile/config" "$@"\n'
                'fi\n'
            )
        exec_lines.append('exec "$_bin" "$@"\n')

        content = (
            "#!/bin/sh\n"
            f'if [ -f {q_takeover} ]; then\n'
            + _posix_strip_and_exec(name)
            + 'fi\n'
            f'_acct="${{{per_agent_var}:-${{CAGE_CLI_ACCOUNT_ID:-default}}}}"\n'
            'case "$_acct" in\n'
            '  default) ;;\n'
            '  *)\n'
            '    case "$_acct" in\n'
            '      *[!0-9a-f]*) _acct="" ;;\n'
            '      *) [ "${#_acct}" = 24 ] || _acct="" ;;\n'
            '    esac\n'
            '    ;;\n'
            'esac\n'
            '_env_file=""\n'
            'if [ -n "$_acct" ]; then\n'
            f'  _profile={q_accts}/"$_acct"\n'
            '  if [ -r "$_profile/profile.env" ]; then\n'
            '    _env_file="$_profile/profile.env"\n'
            '  fi\n'
            'fi\n'
            'if [ -z "$_env_file" ]; then\n'
            f'  exec {q_py} -I {q_rt} exec-shim {q_spec} "${{{per_agent_var}:-${{CAGE_CLI_ACCOUNT_ID:-default}}}}" "$@"\n'
            'fi\n'
            '_bin=""\n'
            f'_search="{search_expr}"\n'
            '_old_ifs="$IFS"\n'
            'IFS=:\n'
            'set -f\n'
            'for _d in $_search; do\n'
            '  [ -n "$_d" ] || continue\n'
            '  case "$_d" in\n'
            f'    {q_shims}|{q_shims}/*|{q_auto}|{q_orca_shims}|/usr/local/lib/cage-claude-bin|/usr/local/lib/cage-codex-bin) continue ;;\n'
            '  esac\n'
            f'  if [ -f "$_d"/{q_name} ] && [ -x "$_d"/{q_name} ]; then\n'
            f'    _bin="$_d"/{q_name}\n'
            '    break\n'
            '  fi\n'
            'done\n'
            'set +f\n'
            'IFS="$_old_ifs"\n'
            'if [ -z "$_bin" ]; then\n'
            f'  exec {q_py} -I {q_rt} exec-shim {q_spec} "$_acct" "$@"\n'
            'fi\n'
            '. "$_env_file"\n'
            + "".join(exec_lines)
        )
        if not shim.exists() or shim.read_text(encoding="utf-8", errors="ignore") != content:
            atomic_write_text(shim, content, mode=0o755)
    return shim_dir


def exec_agent(spec, account_id, extra_args):
    original_umask = os.umask(0o077)
    base = profile_root(spec["id"], account_id)
    with _profile_lock(base):
        base, dirs, env = build_agent_env(spec, account_id, for_login=False)
        sync_shared_resources(spec, base, dirs)
        write_profile_env(spec, account_id)
    binary = find_binary(spec, env["PATH"])
    if not binary:
        print(f"未找到 {spec['name']} ({spec['binary']})，请先在「设置 → Agent CLI」中安装。", file=sys.stderr)
        return 1
    args = [binary]
    iso = spec.get("isolation") or {}
    if account_id != "default" and iso.get("mode") == "flag_dir" and iso.get("config_flag"):
        args.extend([iso["config_flag"], str(dirs["config"])])
    args.extend(extra_args)
    os.umask(original_umask)
    os.execve(binary, args, env)


def run_login(payload):
    spec = payload["spec"]
    account_id = payload["account_id"]
    login_mode = payload.get("login_mode") or "oauth"
    if account_id == "default":
        raise ValueError("new logins require an isolated profile")
    original_umask = os.umask(0o077)
    base, _, env = build_agent_env(spec, account_id, for_login=True)
    binary = find_binary(spec, env["PATH"])
    if not binary:
        print(f"未找到 {spec['name']} ({spec['binary']})，请先在「设置 → Agent CLI」中安装。", file=sys.stderr)
        return 1
    auth = spec.get("auth") or {}
    allowed_hosts = ",".join(
        str(h).strip().lower()
        for h in (auth.get("oauth_domains") or [])
        if isinstance(h, str) and h.strip()
    )
    env["CAGE_CLAUDE_LOGIN"] = account_id
    env["CAGE_LOGIN_ALLOWED_HOSTS"] = allowed_hosts
    if os.path.isfile(OPEN_BIN):
        wrapper = base / "browser-open.sh"
        wrapper_content = (
            "#!/bin/sh\n"
            f"export CAGE_CLAUDE_LOGIN={shlex.quote(account_id)}\n"
            f"export CAGE_LOGIN_ALLOWED_HOSTS={shlex.quote(allowed_hosts)}\n"
            f'exec {shlex.quote(OPEN_BIN)} --login-id {shlex.quote(account_id)} "$@"\n'
        )
        atomic_write_text(wrapper, wrapper_content, mode=0o700)
        env["BROWSER"] = str(wrapper)
    login_args = auth.get("device_login_args") if (login_mode == "device" and auth.get("device_login_args")) else (auth.get("login_args") or [])
    iso = spec.get("isolation") or {}
    cmd = [binary]
    if iso.get("mode") == "flag_dir" and iso.get("config_flag"):
        cmd.extend([iso["config_flag"], str(base / "config")])
    cmd.extend(login_args)

    def interrupted(_sig, _frame):
        raise KeyboardInterrupt()

    if hasattr(signal, "SIGHUP"):
        signal.signal(signal.SIGHUP, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    os.umask(original_umask)
    try:
        return subprocess.call(cmd, cwd=HOME, env=env)
    finally:
        if os.path.isfile(OPEN_BIN):
            subprocess.run([OPEN_BIN, "--login-id", account_id, "--finish-login"],
                           env=os.environ, timeout=8, check=False,
                           stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def prepare_session(payload):
    """Prepare isolated dirs, shared resources, profile.env, hashed stamp and pure-shell shim."""
    spec = payload["spec"]
    account_id = payload.get("account_id") or "default"
    raw_hash = payload.get("prep_hash")
    if raw_hash is not None and str(raw_hash).strip():
        prep_hash = str(raw_hash).strip()
        if not PREP_HASH_RE.match(prep_hash):
            raise ValueError("invalid prep_hash")
    else:
        prep_hash = _default_prep_hash(spec, account_id)
    base = profile_root(spec["id"], account_id)
    with _profile_lock(base):
        base, dirs, _ = build_agent_env(spec, account_id, for_login=False)
        sync_shared_resources(spec, base, dirs)
        write_profile_env(spec, account_id)
        shim_dir = ensure_shim(spec)
        stamp_path = base / f".prepared-{prep_hash}"
        atomic_write_text(stamp_path, f"{int(time.time())}\n", mode=0o600)
        for old in base.iterdir():
            if old.name == stamp_path.name:
                continue
            if (old.name == ".prepared" or old.name.startswith(".prepared-")) and (old.is_file() or old.is_symlink()):
                try:
                    old.unlink()
                except OSError:
                    pass
    return {
        "shim_dir": str(shim_dir),
        "account_id": account_id,
        "config_dir": str(dirs["config"]),
        "env_exports": isolation_env_exports(spec, account_id, dirs),
        "prepared_stamp": str(stamp_path),
        "prep_hash": prep_hash,
    }


# ---------------------------------------------------------------------------
# Universal CLI Auto-Scheduling Shims (shells, Herdr, SSH, T3 Code, Orca)
# ---------------------------------------------------------------------------

def _claude_account_helper():
    if sys.platform == "darwin" and Path("/usr/local/libexec/cage-cli/bin/cage-claude-account").is_file():
        return "/usr/local/libexec/cage-cli/bin/cage-claude-account"
    return "/usr/local/bin/cage-claude-account"


def _codex_account_helper():
    return "/usr/local/bin/cage-codex-account"


def _ensure_shell_shims_hook():
    """Idempotently add a zero-fork POSIX PATH guard to user shell rc files so
    universal auto-shims and Orca explicit override shims survive login-shell
    PATH resets (including Orca's `$SHELL -ilc 'env -0'`)."""
    block = (
        f"{SHELL_SHIMS_MARKER_START}\n"
        'if [ -d "$HOME/.local/share/cage/auto-shims" ]; then\n'
        '  case ":$PATH:" in\n'
        '    *":$HOME/.local/share/cage/auto-shims:"*) ;;\n'
        '    *) export PATH="$HOME/.local/share/cage/auto-shims:$PATH" ;;\n'
        '  esac\n'
        'fi\n'
        'if [ -n "${CAGE_ORCA_SHIM_DIR:-}" ] && [ -d "$CAGE_ORCA_SHIM_DIR" ]; then\n'
        '  export PATH="$CAGE_ORCA_SHIM_DIR:$PATH"\n'
        'fi\n'
        f"{SHELL_SHIMS_MARKER_END}\n"
    )
    targets = [HOME / ".bashrc", HOME / ".profile", HOME / ".zshenv", HOME / ".zshrc"]
    has_zsh = bool(shutil.which("zsh"))
    for rc in targets:
        try:
            exists = rc.exists()
            if rc.is_symlink():
                continue
            if not exists and rc.name in (".zshenv", ".zshrc") and not has_zsh:
                continue
            existing = rc.read_text(encoding="utf-8", errors="ignore") if exists else ""
            if SHELL_SHIMS_MARKER_START in existing and SHELL_SHIMS_MARKER_END in existing:
                start_idx = existing.index(SHELL_SHIMS_MARKER_START)
                end_idx = existing.index(SHELL_SHIMS_MARKER_END) + len(SHELL_SHIMS_MARKER_END)
                if end_idx < len(existing) and existing[end_idx] == "\n":
                    end_idx += 1
                updated = existing[:start_idx] + block + existing[end_idx:]
                if updated == existing:
                    continue
            else:
                sep = "" if (not existing or existing.endswith("\n")) else "\n"
                updated = existing + sep + block
            mode = (rc.stat().st_mode & 0o777) if exists else 0o644
            atomic_write_text(rc, updated, mode=mode)
        except OSError:
            continue


def _posix_strip_and_exec(binary_name):
    """Return a POSIX sh snippet that strips all Cage shim dirs from PATH and execs real binary."""
    q_shims = shlex.quote(str(SHIMS_ROOT))
    q_auto = shlex.quote(str(AUTO_SHIMS_DIR))
    q_orca = shlex.quote(str(ORCA_SHIMS_DIR))
    q_t3 = shlex.quote(str(T3CODE_ROOT / "shims"))
    q_name = shlex.quote(binary_name)
    search_prefixes = [
        "$HOME/.local/bin",
        "$HOME/.local/share/mise/shims",
        "$HOME/.cargo/bin",
        "$HOME/.cursor/bin",
        "$HOME/.opencode/bin",
        "$HOME/.amp/bin",
        "$HOME/.kimi/bin",
        "$HOME/.factory/bin",
        "$HOME/.npm-global/bin",
    ]
    if sys.platform == "darwin":
        search_prefixes.extend([
            "/usr/local/libexec/cage-cli/bin",
            "/opt/homebrew/bin",
            "/usr/local/bin",
        ])
    search_expr = ":".join(search_prefixes) + ":${PATH:-/usr/bin:/bin}"
    return (
        '_clean_path=""\n'
        '_old_ifs="$IFS"\n'
        'IFS=:\n'
        'set -f\n'
        'for _d in ${PATH:-/usr/bin:/bin}; do\n'
        '  [ -n "$_d" ] || continue\n'
        '  case "$_d" in\n'
        f'    {q_shims}|{q_shims}/*|{q_auto}|{q_orca}|{q_t3}|/usr/local/lib/cage-claude-bin|/usr/local/lib/cage-codex-bin) continue ;;\n'
        '  esac\n'
        '  _clean_path="${_clean_path:+$_clean_path:}$_d"\n'
        'done\n'
        'export PATH="${_clean_path:-/usr/bin:/bin}"\n'
        '_bin=""\n'
        f'_search="{search_expr}"\n'
        'for _d in $_search; do\n'
        '  [ -n "$_d" ] || continue\n'
        '  case "$_d" in\n'
        f'    {q_shims}|{q_shims}/*|{q_auto}|{q_orca}|{q_t3}|/usr/local/lib/cage-claude-bin|/usr/local/lib/cage-codex-bin) continue ;;\n'
        '  esac\n'
        f'  if [ -f "$_d"/{q_name} ] && [ -x "$_d"/{q_name} ]; then\n'
        f'    _bin="$_d"/{q_name}\n'
        '    break\n'
        '  fi\n'
        'done\n'
        'set +f\n'
        'IFS="$_old_ifs"\n'
        'if [ -n "$_bin" ]; then\n'
        '  exec "$_bin" "$@"\n'
        'fi\n'
        f'echo "未找到 {binary_name} 命令。" >&2\n'
        'exit 127\n'
    )


def _write_claude_auto_shim():
    helper = _claude_account_helper()
    q_helper = shlex.quote(helper)
    q_launch = shlex.quote(str(CLAUDE_LAUNCH_FILE))
    q_takeover = shlex.quote(str(MAGPIE_TAKEOVER_FILE))
    content = (
        "#!/bin/sh\n"
        f'if [ -f {q_takeover} ] || [ "${{CAGE_CLAUDE_ACCOUNT_ID:-}}" = "default" ] || [ "${{CAGE_ACCOUNT_SCHEDULING:-}}" = "off" ] || [ "${{CAGE_ACCOUNT_SCHEDULING:-}}" = "0" ]; then\n'
        + _posix_strip_and_exec("claude")
        + 'fi\n'
        f'if [ -n "${{CAGE_CLAUDE_ACCOUNT_ID:-}}" ] && [ -x {q_helper} ]; then\n'
        f'  exec {q_helper} run "$CAGE_CLAUDE_ACCOUNT_ID" "$@"\n'
        'fi\n'
        f'if [ -z "${{CLAUDE_CONFIG_DIR:-}}" ] && [ -r {q_launch} ] && [ -x {q_helper} ]; then\n'
        f'  exec {q_helper} auto "$@"\n'
        'fi\n'
        + _posix_strip_and_exec("claude")
    )
    shim = AUTO_SHIMS_DIR / "claude"
    if not shim.exists() or shim.read_text(encoding="utf-8", errors="ignore") != content:
        atomic_write_text(shim, content, mode=0o755)


def _write_codex_auto_shim(has_native_codex, has_agent_codex):
    helper = _codex_account_helper()
    q_helper = shlex.quote(helper)
    q_native_launch = shlex.quote(str(CODEX_LAUNCH_FILE))
    q_agent_launch = shlex.quote(str(AGENT_LAUNCH_FILE))
    q_takeover = shlex.quote(str(MAGPIE_TAKEOVER_FILE))
    q_py = shlex.quote(sys.executable)
    q_rt = shlex.quote(str(Path(__file__).resolve()))
    q_accts = shlex.quote(str(ACCOUNTS_ROOT / "codex"))
    parts = [
        "#!/bin/sh\n",
        f'if [ -f {q_takeover} ] || [ "${{CAGE_CODEX_ACCOUNT_ID:-}}" = "default" ] || [ "${{CAGE_CLI_ACCOUNT_CODEX:-}}" = "default" ] || [ "${{CAGE_ACCOUNT_SCHEDULING:-}}" = "off" ] || [ "${{CAGE_ACCOUNT_SCHEDULING:-}}" = "0" ]; then\n',
        _posix_strip_and_exec("codex"),
        'fi\n',
        f'if [ -n "${{CAGE_CODEX_ACCOUNT_ID:-}}" ] && [ -x {q_helper} ]; then\n',
        f'  exec {q_helper} run "$CAGE_CODEX_ACCOUNT_ID" "$@"\n',
        'fi\n',
        'if [ -n "${CAGE_CLI_ACCOUNT_CODEX:-}" ] && [ "${CAGE_CLI_ACCOUNT_CODEX}" != "auto" ]; then\n',
        f'  _env={q_accts}/"$CAGE_CLI_ACCOUNT_CODEX"/profile.env\n',
        '  if [ -r "$_env" ]; then\n',
        '    . "$_env"\n',
        _posix_strip_and_exec("codex"),
        '  fi\n',
        'fi\n',
    ]
    if has_native_codex:
        parts.extend([
            f'if [ -z "${{CODEX_HOME:-}}" ] && [ -r {q_native_launch} ] && [ -x {q_helper} ]; then\n',
            f'  exec {q_helper} auto "$@"\n',
            'fi\n',
        ])
    if has_agent_codex:
        parts.extend([
            f'if [ -z "${{CODEX_HOME:-}}" ] && [ -r {q_agent_launch} ]; then\n',
            f'  exec {q_py} -I {q_rt} auto-exec codex codex "$@"\n',
            'fi\n',
        ])
    parts.append(_posix_strip_and_exec("codex"))
    content = "".join(parts)
    shim = AUTO_SHIMS_DIR / "codex"
    if not shim.exists() or shim.read_text(encoding="utf-8", errors="ignore") != content:
        atomic_write_text(shim, content, mode=0o755)


def _write_generic_auto_shim(agent_id, binary_name, spec):
    per_agent_var = _agent_account_env_var(agent_id)
    q_py = shlex.quote(sys.executable)
    q_rt = shlex.quote(str(Path(__file__).resolve()))
    q_accts = shlex.quote(str(ACCOUNTS_ROOT / agent_id))
    q_launch = shlex.quote(str(AGENT_LAUNCH_FILE))
    q_takeover = shlex.quote(str(MAGPIE_TAKEOVER_FILE))
    q_agent = shlex.quote(agent_id)
    q_bin_name = shlex.quote(binary_name)

    iso = spec.get("isolation") or {}
    config_flag = iso.get("config_flag") or ""
    use_flag_dir = iso.get("mode") == "flag_dir" and bool(FLAG_RE.match(config_flag))

    pinned_exec = ""
    if use_flag_dir:
        q_flag = shlex.quote(config_flag)
        pinned_exec = (
            '    _clean_bin=""\n'
            + _posix_strip_and_exec(binary_name).replace('exec "$_bin" "$@"', f'exec "$_bin" {q_flag} "$_profile/config" "$@"')
        )
    else:
        pinned_exec = _posix_strip_and_exec(binary_name)

    content = (
        "#!/bin/sh\n"
        f'_pin="${{{per_agent_var}:-${{CAGE_CLI_ACCOUNT_ID:-}}}}"\n'
        f'if [ -f {q_takeover} ] || [ "$_pin" = "default" ] || [ "${{CAGE_ACCOUNT_SCHEDULING:-}}" = "off" ] || [ "${{CAGE_ACCOUNT_SCHEDULING:-}}" = "0" ]; then\n'
        + _posix_strip_and_exec(binary_name)
        + 'fi\n'
        'if [ -n "$_pin" ] && [ "$_pin" != "auto" ]; then\n'
        '  case "$_pin" in\n'
        '    *[!0-9a-f]*) _pin="" ;;\n'
        '    *) [ "${#_pin}" = 24 ] || _pin="" ;;\n'
        '  esac\n'
        '  if [ -n "$_pin" ]; then\n'
        f'    _profile={q_accts}/"$_pin"\n'
        '    if [ -r "$_profile/profile.env" ]; then\n'
        '      . "$_profile/profile.env"\n'
        + pinned_exec
        + '    fi\n'
        '  fi\n'
        'fi\n'
        f'if [ -r {q_launch} ]; then\n'
        f'  exec {q_py} -I {q_rt} auto-exec {q_agent} {q_bin_name} "$@"\n'
        'fi\n'
        + _posix_strip_and_exec(binary_name)
    )
    shim = AUTO_SHIMS_DIR / binary_name
    if not shim.exists() or shim.read_text(encoding="utf-8", errors="ignore") != content:
        atomic_write_text(shim, content, mode=0o755)


def _launch_has_accounts(path):
    data = read_json(path)
    if not isinstance(data, dict):
        return False
    accts = data.get("accounts")
    return isinstance(accts, list) and len(accts) > 0


def _clear_all_scheduling_artifacts():
    """Remove all Cage launch manifests and executable shims so every harness
    runs in its native default config directory (e.g. when Magpie takes over)."""
    for launch_file in (CLAUDE_LAUNCH_FILE, CODEX_LAUNCH_FILE, AGENT_LAUNCH_FILE):
        try:
            launch_file.unlink(missing_ok=True)
        except OSError:
            pass
    for shim_dir in (AUTO_SHIMS_DIR, ORCA_SHIMS_DIR, T3CODE_ROOT / "shims"):
        if not shim_dir.is_dir():
            continue
        try:
            for child in list(shim_dir.iterdir()):
                if child.is_file() or child.is_symlink():
                    try:
                        child.unlink()
                    except OSError:
                        pass
        except OSError:
            pass
    if SHIMS_ROOT.is_dir():
        try:
            for agent_dir in list(SHIMS_ROOT.iterdir()):
                if not agent_dir.is_dir():
                    continue
                for child in list(agent_dir.iterdir()):
                    if child.name == "spec.json":
                        continue
                    if child.is_file() or child.is_symlink():
                        try:
                            child.unlink()
                        except OSError:
                            pass
        except OSError:
            pass


def sync_auto_shims(payload=None):
    """Publish optional agent launch schedule and synchronize universal auto-shims
    in ~/.local/share/cage/auto-shims for Claude, Codex, and all Agent CLIs."""
    payload = payload if isinstance(payload, dict) else {}
    os.umask(0o077)
    ACCOUNTS_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)

    disable_all = bool(payload.get("disable_all"))
    magpie_takeover = payload.get("magpie_takeover")
    if magpie_takeover is True or disable_all:
        MAGPIE_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
        atomic_write_text(MAGPIE_TAKEOVER_FILE, "1\n", mode=0o600)
    elif magpie_takeover is False:
        try:
            MAGPIE_TAKEOVER_FILE.unlink(missing_ok=True)
        except OSError:
            pass

    if disable_all or MAGPIE_TAKEOVER_FILE.is_file():
        _clear_all_scheduling_artifacts()
        return {"ok": True, "shims": [], "dir": str(AUTO_SHIMS_DIR), "magpie_takeover": True}

    if "schedule" in payload and isinstance(payload["schedule"], dict):
        atomic_write_json(AGENT_LAUNCH_FILE, payload["schedule"], mode=0o600)

    schedule = read_json(AGENT_LAUNCH_FILE)
    agents_map = schedule.get("agents") if isinstance(schedule, dict) and isinstance(schedule.get("agents"), dict) else {}

    has_claude = _launch_has_accounts(CLAUDE_LAUNCH_FILE)
    has_native_codex = _launch_has_accounts(CODEX_LAUNCH_FILE)
    codex_entry = agents_map.get("codex") if isinstance(agents_map.get("codex"), dict) else None
    has_agent_codex = bool(codex_entry and isinstance(codex_entry.get("candidates"), list) and len(codex_entry["candidates"]) > 0)

    AUTO_SHIMS_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
    desired = set()

    if has_claude:
        _write_claude_auto_shim()
        desired.add("claude")

    if has_native_codex or has_agent_codex:
        if has_agent_codex and isinstance(codex_entry.get("spec"), dict):
            spec = codex_entry["spec"]
            for cand in codex_entry.get("candidates") or []:
                gid = str((cand or {}).get("guest_id") or "")
                if gid and gid != "default" and ACCOUNT_ID_RE.match(gid):
                    base = profile_root("codex", gid)
                    if not (base / "profile.env").is_file():
                        with _profile_lock(base):
                            base, dirs, _ = build_agent_env(spec, gid, for_login=False)
                            sync_shared_resources(spec, base, dirs)
                            write_profile_env(spec, gid)
        _write_codex_auto_shim(has_native_codex, has_agent_codex)
        desired.add("codex")

    for agent_id, entry in agents_map.items():
        if agent_id in ("claude", "codex") or not CLI_ID_RE.match(str(agent_id)):
            continue
        if not isinstance(entry, dict):
            continue
        candidates = entry.get("candidates")
        spec = entry.get("spec")
        if not isinstance(candidates, list) or not candidates or not isinstance(spec, dict):
            continue
        for cand in candidates:
            gid = str((cand or {}).get("guest_id") or "")
            if gid and gid != "default" and ACCOUNT_ID_RE.match(gid):
                base = profile_root(agent_id, gid)
                if not (base / "profile.env").is_file():
                    with _profile_lock(base):
                        base, dirs, _ = build_agent_env(spec, gid, for_login=False)
                        sync_shared_resources(spec, base, dirs)
                        write_profile_env(spec, gid)
        names = [spec.get("binary", ""), *(spec.get("fallback_binaries") or [])]
        for bin_name in names:
            if not bin_name or "/" in bin_name or bin_name in ("claude", "codex") or any(ch in bin_name for ch in ("\x00", "\r", "\n")):
                continue
            _write_generic_auto_shim(agent_id, bin_name, spec)
            desired.add(bin_name)

    # Remove stale shims for agents whose pools are now empty.
    try:
        for child in list(AUTO_SHIMS_DIR.iterdir()):
            if child.name not in desired and (child.is_file() or child.is_symlink()):
                try:
                    child.unlink()
                except OSError:
                    pass
    except OSError:
        pass

    if desired or payload.get("ensure_shell_hook"):
        _ensure_shell_shims_hook()

    return {"ok": True, "shims": sorted(desired), "dir": str(AUTO_SHIMS_DIR), "magpie_takeover": False}


def _match_project_pin(pins, cwd_str):
    if not isinstance(pins, dict) or not pins or not cwd_str:
        return ""
    best_len = -1
    matched = ""
    cwd_norm = cwd_str.rstrip("/") or "/"
    for ws, target in pins.items():
        if not isinstance(ws, str) or not isinstance(target, str) or not target.strip():
            continue
        forms = [ws]
        try:
            resolved_ws = str(Path(ws).resolve())
            if resolved_ws != ws:
                forms.append(resolved_ws)
        except OSError:
            pass
        for cand_ws in forms:
            ws_norm = cand_ws.rstrip("/") or "/"
            if cwd_norm == ws_norm or cwd_norm.startswith(ws_norm + "/"):
                if len(ws_norm) > best_len:
                    best_len = len(ws_norm)
                    matched = target.strip()
    return matched


def _codex_candidate_exhausted(agent_id, guest_id, now):
    if agent_id != "codex" or guest_id == "default":
        return False
    try:
        cfg_dir = profile_root("codex", guest_id) / "config"
        quota = read_codex_quota(cfg_dir)
    except Exception:
        return False
    if not isinstance(quota, dict):
        return False
    for key in ("five_hour", "seven_day"):
        w = quota.get(key)
        if not isinstance(w, dict):
            continue
        used = w.get("used_percentage", 0)
        resets_at = w.get("resets_at", 0)
        obs = w.get("observed_at", 0)
        fresh = obs > 0 and (now - obs) <= 15 * 60
        if used >= 100 and (resets_at > now or fresh):
            return True
    return False


def _pick_auto_candidate(agent_id, entry, cwd_str, now=None):
    now = int(time.time()) if now is None else int(now)
    candidates = entry.get("candidates")
    if not isinstance(candidates, list) or not candidates:
        return None
    mode = str(entry.get("mode") or "smart").strip().lower()
    pin = _match_project_pin(entry.get("project_pins"), cwd_str)

    eligible = []
    for cand in candidates[:64]:
        if not isinstance(cand, dict):
            continue
        gid = str(cand.get("guest_id") or "").strip()
        cid = str(cand.get("id") or "").strip()
        if not gid or (gid != "default" and not ACCOUNT_ID_RE.match(gid)):
            continue
        cooldown = cand.get("cooldown_until") or 0
        if isinstance(cooldown, (int, float)) and cooldown > now:
            continue
        if pin.startswith("account:"):
            want = pin[len("account:"):]
            if cid != want and gid != want:
                continue
        elif pin.startswith("group:"):
            want_group = pin[len("group:"):]
            if str(cand.get("handoff_group") or "") != want_group:
                continue
        if _codex_candidate_exhausted(agent_id, gid, now):
            continue
        eligible.append(cand)

    if not eligible:
        return None
    if len(eligible) == 1 or mode in ("sequential", "quota_first", "drain", "sticky_project"):
        return eligible[0]

    # For balanced (all eligible) or smart (top-tier within 5 score points), rotate via a tiny 0600 cursor
    # so consecutive invocations across Orca / Herdr / scripts distribute load smoothly.
    pool = eligible
    if mode == "smart":
        top_score = eligible[0].get("score")
        if isinstance(top_score, (int, float)):
            near = [c for c in eligible if isinstance(c.get("score"), (int, float)) and (top_score - c["score"]) <= 5.0]
            if near:
                pool = near
    if len(pool) == 1:
        return pool[0]

    rr_dir = ACCOUNTS_ROOT / agent_id
    rr_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    rr_file = rr_dir / ".auto-rr"
    idx = 0
    try:
        fd = os.open(str(rr_file), os.O_RDWR | os.O_CREAT, 0o600)
        os.chmod(rr_file, 0o600)
        with os.fdopen(fd, "r+") as fp:
            fcntl.flock(fp, fcntl.LOCK_EX)
            try:
                raw = fp.read().strip()
                last_id = raw
                ids = [str(c.get("guest_id") or "") for c in pool]
                if last_id in ids:
                    idx = (ids.index(last_id) + 1) % len(pool)
                chosen = pool[idx]
                fp.seek(0)
                fp.truncate()
                fp.write(str(chosen.get("guest_id") or ""))
                fp.flush()
                return chosen
            finally:
                fcntl.flock(fp, fcntl.LOCK_UN)
    except OSError:
        return pool[0]


def _exec_real_binary(binary_name, extra_args):
    path_str = enriched_path()
    binary = shutil.which(binary_name, path=path_str)
    if not binary:
        print(f"未找到 {binary_name} 命令。", file=sys.stderr)
        return 127
    env = os.environ.copy()
    env["PATH"] = path_str
    os.execve(binary, [binary, *extra_args], env)


def auto_exec_agent(agent_id, binary_name, extra_args):
    """Dynamically schedule a non-Claude Agent CLI invocation from launch.json.
    Anything explicit or administrative bypasses scheduling and runs the real CLI."""
    if not CLI_ID_RE.match(agent_id):
        return _exec_real_binary(binary_name, extra_args)

    disabled = (
        MAGPIE_TAKEOVER_FILE.is_file()
        or os.environ.get("CAGE_ACCOUNT_SCHEDULING", "").strip().lower() in ("0", "off", "false", "no")
    )
    if disabled or (extra_args and extra_args[0] in AUTO_UNSCHEDULED_ARGS):
        return _exec_real_binary(binary_name, extra_args)

    schedule = read_json(AGENT_LAUNCH_FILE)
    agents_map = schedule.get("agents") if isinstance(schedule, dict) and isinstance(schedule.get("agents"), dict) else {}
    entry = agents_map.get(agent_id)
    if not isinstance(entry, dict) or not isinstance(entry.get("spec"), dict):
        return _exec_real_binary(binary_name, extra_args)

    spec = entry["spec"]
    iso = spec.get("isolation") or {}
    auth = spec.get("auth") or {}
    # Respect caller's explicit isolation directory or API key overrides.
    explicit_vars = set()
    iso_var = iso.get("env_var") or ""
    if is_safe_env_name(iso_var):
        explicit_vars.add(iso_var)
    for v in (auth.get("api_key_env_vars") or []):
        if is_safe_env_name(v):
            explicit_vars.add(v)
    for v in (auth.get("strip_env_on_oauth") or []):
        if is_safe_env_name(v):
            explicit_vars.add(v)
    if any(os.environ.get(v) for v in explicit_vars):
        return _exec_real_binary(binary_name, extra_args)

    try:
        cwd_str = str(Path.cwd().resolve())
    except OSError:
        cwd_str = ""
    chosen = _pick_auto_candidate(agent_id, entry, cwd_str)
    if not chosen:
        return _exec_real_binary(binary_name, extra_args)

    guest_id = str(chosen.get("guest_id") or "default").strip() or "default"
    if guest_id == "default":
        return _exec_real_binary(binary_name, extra_args)

    # Pin child processes within this session to the chosen account (matching Claude's CLAUDE_CONFIG_DIR stickiness).
    os.environ[_agent_account_env_var(agent_id)] = guest_id
    return exec_agent(spec, guest_id, extra_args)


# ---------------------------------------------------------------------------
# Managed T3 Code Guest Lifecycle
# ---------------------------------------------------------------------------

@contextmanager
def _t3code_lifecycle_lock():
    """Serialize all T3 Code lifecycle and pairing mint operations."""
    os.umask(0o077)
    T3CODE_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(str(T3CODE_LOCK_FILE), os.O_RDWR | os.O_CREAT, 0o600)
    os.chmod(T3CODE_LOCK_FILE, 0o600)
    with os.fdopen(fd, "a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def _validate_port(port):
    if isinstance(port, bool) or not isinstance(port, int) or not (1024 <= port <= 65535):
        raise ValueError("T3 Code 端口须在 1024-65535 之间")
    return port


def _requested_port(payload, default=13773):
    raw = payload.get("port") if isinstance(payload, dict) else None
    if raw is None or raw == "" or raw == 0:
        return _validate_port(default)
    if isinstance(raw, bool):
        raise ValueError("T3 Code 端口须在 1024-65535 之间")
    try:
        port = int(raw)
    except (TypeError, ValueError) as exc:
        raise ValueError("T3 Code 端口须在 1024-65535 之间") from exc
    return _validate_port(port)


def _pid_alive(pid):
    if isinstance(pid, bool) or not isinstance(pid, int) or pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return False
    # Guard against zombie processes on Linux.
    proc_stat = Path(f"/proc/{pid}/stat")
    if proc_stat.is_file():
        try:
            text = proc_stat.read_text(encoding="utf-8", errors="ignore")
            rparen = text.rfind(")")
            if rparen >= 0:
                fields = text[rparen + 2:].split()
                if fields and fields[0] == "Z":
                    return False
        except OSError:
            pass
    return True


def _http_responsive(host, port, timeout=0.45):
    """Verify an HTTP server actually replies to GET / on host:port within <500ms."""
    try:
        with socket.create_connection((host, port), timeout=timeout) as sock:
            sock.settimeout(timeout)
            sock.sendall(
                f"GET / HTTP/1.0\r\nHost: {host}:{port}\r\nConnection: close\r\n\r\n".encode("ascii")
            )
            head = sock.recv(64)
            return head.startswith(b"HTTP/")
    except OSError:
        return False


def _t3_pair_url_regex(port):
    return re.compile(
        rf"http://(?:127\.0\.0\.1|localhost):{int(port)}/(?:[^\s\"'<>]*token=[^\s\"'<>]+|(?:[^\s\"'<>]*/)?pair[#/?][^\s\"'<>]+)",
        re.IGNORECASE,
    )


def _extract_t3_pair_url(text, port):
    if not text:
        return ""
    clean = ANSI_ESCAPE_RE.sub("", text)
    matches = _t3_pair_url_regex(port).findall(clean)
    if not matches:
        return ""
    return matches[-1].rstrip(".,;)]}")


def _read_pair_once(port):
    try:
        text = T3CODE_PAIR_FILE.read_text(encoding="utf-8", errors="ignore").strip()
    except OSError:
        return ""
    return _extract_t3_pair_url(text, port)


def _pid_identity(pid):
    """Return a stable process identity token (start time) to prevent signaling a reused PID."""
    if isinstance(pid, bool) or not isinstance(pid, int) or pid <= 0:
        return ""
    proc_stat = Path(f"/proc/{pid}/stat")
    if proc_stat.is_file():
        try:
            text = proc_stat.read_text(encoding="utf-8", errors="ignore")
            rparen = text.rfind(")")
            if rparen >= 0:
                fields = text[rparen + 2:].split()
                # Field 20 (0-indexed 19 after comm) is starttime in clock ticks since boot.
                if len(fields) > 19:
                    return f"linux:{fields[19]}"
        except OSError:
            return ""
    if sys.platform == "darwin":
        try:
            res = subprocess.run(
                ["/bin/ps", "-o", "lstart=", "-p", str(pid)],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                timeout=1.0,
            )
            val = (res.stdout or "").strip()
            if res.returncode == 0 and val:
                return f"darwin:{val}"
        except (OSError, subprocess.SubprocessError):
            return ""
    return ""


def _pid_state(pid, expected_identity="", require_identity=True):
    """Classify a PID relative to its persisted identity: gone | matched | mismatch | unknown."""
    if not _pid_alive(pid):
        return "gone"
    if expected_identity:
        current = _pid_identity(pid)
        if not current:
            return "unknown"
        return "matched" if current == expected_identity else "mismatch"
    if require_identity:
        # Persisted records missing process identity must NEVER match on PID liveness alone.
        return "mismatch"
    return "matched"


def _pid_matches(pid, expected_identity="", require_identity=True):
    return _pid_state(pid, expected_identity, require_identity=require_identity) == "matched"


def _redact_t3_log_tail(log_path, max_lines=6, max_chars=400):
    try:
        text = Path(log_path).read_text(encoding="utf-8", errors="ignore")
    except OSError:
        return ""
    kept = []
    for raw_line in text.splitlines():
        clean = ANSI_ESCAPE_RE.sub("", raw_line).strip()
        if not clean:
            continue
        if _is_qr_line(clean):
            continue
        if T3_SENSITIVE_LINE_RE.search(clean):
            kept.append("[REDACTED]")
            continue
        kept.append(clean)
    joined = " | ".join(kept[-max_lines:])
    return joined[:max_chars]


class _BoundedRedactedLog:
    """Append-only 0600 log writer that redacts secrets/QR lines and caps disk size at 64 KiB."""

    def __init__(self, path, max_bytes=T3CODE_LOG_MAX_BYTES):
        self.path = Path(path)
        self.max_bytes = max_bytes
        self.buf = bytearray()
        self.in_qr = False
        self.path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        self._fd = os.open(str(self.path), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        os.chmod(self.path, 0o600)

    def write_raw_line(self, raw_line):
        clean = ANSI_ESCAPE_RE.sub("", raw_line).rstrip("\r\n")
        if _is_qr_line(clean):
            if self.in_qr:
                return
            self.in_qr = True
            out = b"[REDACTED QR]\n"
        else:
            self.in_qr = False
            if T3_SENSITIVE_LINE_RE.search(clean):
                out = b"[REDACTED]\n"
            elif clean:
                out = (clean + "\n").encode("utf-8", errors="replace")
            else:
                return
        self._append_bytes(out)

    def _append_bytes(self, data):
        if len(data) > self.max_bytes:
            data = data[-self.max_bytes:]
        if len(self.buf) + len(data) <= self.max_bytes:
            self.buf.extend(data)
            try:
                os.write(self._fd, data)
            except OSError:
                pass
            return
        keep_target = max(0, (self.max_bytes * 3) // 4 - len(data))
        if keep_target == 0:
            new_buf = bytearray(data[-self.max_bytes:])
        else:
            tail = bytes(self.buf[-keep_target:])
            nl = tail.find(b"\n")
            if 0 <= nl < len(tail) - 1:
                tail = tail[nl + 1:]
            new_buf = bytearray(tail)
            new_buf.extend(data)
        self.buf = new_buf
        try:
            os.close(self._fd)
        except OSError:
            pass
        self._rewrite_atomic()

    def _rewrite_atomic(self):
        with tempfile.NamedTemporaryFile(mode="wb", dir=self.path.parent, delete=False) as tmp:
            tmp.write(self.buf)
            tmp_name = tmp.name
        os.chmod(tmp_name, 0o600)
        os.replace(tmp_name, self.path)
        self._fd = os.open(str(self.path), os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        os.chmod(self.path, 0o600)

    def close(self):
        try:
            os.close(self._fd)
        except OSError:
            pass


def t3code_supervise(cfg):
    """Detached supervisor for `t3 serve` with live redaction, 64 KiB log cap, and pair.once capture."""
    os.umask(0o077)
    cmd = cfg["cmd"]
    port = _validate_port(int(cfg["port"]))
    cwd = cfg.get("cwd") or str(HOME)
    log_file = Path(cfg.get("log_file") or T3CODE_LOG_FILE)
    pair_file = Path(cfg.get("pair_file") or T3CODE_PAIR_FILE)
    handshake_file = Path(cfg["handshake_file"])

    if hasattr(signal, "SIGHUP"):
        signal.signal(signal.SIGHUP, signal.SIG_IGN)

    logger = _BoundedRedactedLog(log_file, max_bytes=T3CODE_LOG_MAX_BYTES)
    child = None
    term_state = {"requested": False, "sent": False}

    def forward_term(_signum, _frame):
        term_state["requested"] = True
        if child is not None and not term_state["sent"]:
            term_state["sent"] = True
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except OSError:
                try:
                    child.terminate()
                except OSError:
                    pass

    signal.signal(signal.SIGTERM, forward_term)
    signal.signal(signal.SIGINT, forward_term)

    try:
        child = subprocess.Popen(
            cmd,
            cwd=cwd,
            env=os.environ.copy(),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
    except OSError as exc:
        logger.write_raw_line(f"Failed to start t3: {exc}")
        logger.close()
        try:
            atomic_write_json(handshake_file, {"error": str(exc), "supervisor_pid": os.getpid()}, mode=0o600)
        except OSError:
            pass
        return 1

    if term_state["requested"] and not term_state["sent"]:
        forward_term(signal.SIGTERM, None)

    try:
        atomic_write_json(
            handshake_file,
            {"child_pid": child.pid, "supervisor_pid": os.getpid()},
            mode=0o600,
        )
    except OSError:
        pass

    pair_captured = False
    try:
        assert child.stdout is not None
        while True:
            raw = child.stdout.readline(65536)
            if not raw:
                break
            line = raw.decode("utf-8", errors="replace")
            if not pair_captured:
                pair_url = _extract_t3_pair_url(line, port)
                if pair_url:
                    try:
                        atomic_write_text(pair_file, pair_url + "\n", mode=0o600)
                        pair_captured = True
                    except OSError:
                        pass
            logger.write_raw_line(line)
    finally:
        if child.stdout is not None:
            try:
                child.stdout.close()
            except OSError:
                pass
        rc = child.wait()
        logger.close()
    return rc if isinstance(rc, int) else 0


def _read_t3_version(binary, path_str):
    if not binary:
        return ""
    cache = read_json(PROBE_CACHE_FILE, {}) or {}
    if not isinstance(cache, dict):
        cache = {}
    ver, dirty = read_cli_version(binary, ["--version"], path_str, cache)
    if dirty:
        try:
            atomic_write_json(PROBE_CACHE_FILE, cache, mode=0o600)
        except OSError:
            pass
    return ver


def t3code_status(payload):
    requested_port = _requested_port(payload, default=13773)
    path_str = enriched_path()
    binary = shutil.which("t3", path=path_str) or ""
    version = _read_t3_version(binary, path_str) if binary else ""
    state = read_json(T3CODE_STATE_FILE)
    running = False
    pid = 0
    has_pair_url = False
    if isinstance(state, dict):
        cand_pid = state.get("pid")
        cand_port = state.get("port")
        cand_ident = str(state.get("pid_identity") or "")
        if (
            not isinstance(cand_pid, bool)
            and isinstance(cand_pid, int)
            and cand_pid > 0
            and not isinstance(cand_port, bool)
            and isinstance(cand_port, int)
            and cand_port == requested_port
            and _pid_matches(cand_pid, cand_ident)
            and _http_responsive("127.0.0.1", requested_port, timeout=0.45)
        ):
            running = True
            pid = cand_pid
            has_pair_url = bool(_read_pair_once(requested_port))
    return {
        "installed": bool(binary),
        "binary_path": binary,
        "version": version,
        "running": running,
        "pid": pid if running else 0,
        "port": requested_port,
        "has_pair_url": has_pair_url,
    }


def t3code_install(payload):
    channel = payload.get("channel") or "npm"
    if isinstance(channel, dict):
        channel = channel.get("kind") or channel.get("id") or "npm"
    channel = str(channel).strip().lower()
    path_str = enriched_path()
    env = os.environ.copy()
    env["PATH"] = path_str
    env["npm_config_fetch_retries"] = "3"
    env["npm_config_fetch_timeout"] = "120000"
    env["npm_config_audit"] = "false"
    env["npm_config_fund"] = "false"

    if channel == "npm":
        npm = shutil.which("npm", path=path_str)
        if not npm:
            raise ValueError("环境中未找到 npm，请先安装 Node.js 或同步环境工具")
        ensure_npm_user_prefix(env)
        print("Installing t3@latest via npm official registry...", flush=True)
        res = subprocess.run(
            [npm, "install", "--registry=https://registry.npmjs.org", "--prefix", str(HOME / ".local"), "--global", "t3@latest"],
            env=env, stdin=subprocess.DEVNULL,
        )
        if res.returncode != 0:
            raise ValueError(f"npm 安装 T3 Code (t3) 失败（退出码 {res.returncode}）")
    elif channel == "script":
        curl = shutil.which("curl", path=path_str)
        if not curl:
            raise ValueError("环境中未找到 curl")
        script_url = "https://t3.codes/install.sh"
        print("Downloading official T3 Code installer from t3.codes...", flush=True)
        with tempfile.NamedTemporaryFile(prefix="cage-t3code-install-", suffix=".sh", delete=False) as tmp:
            script_path = tmp.name
        try:
            dl = subprocess.run(
                [curl, "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "-fsSL", "--connect-timeout", "20", "-o", script_path, script_url],
                env=env, stdin=subprocess.DEVNULL,
            )
            if dl.returncode != 0:
                raise ValueError(f"下载 T3 Code 安装脚本失败（curl 退出码 {dl.returncode}）")
            res = subprocess.run(["/bin/bash", script_path], env=env, stdin=subprocess.DEVNULL)
            if res.returncode != 0:
                raise ValueError(f"T3 Code 安装脚本执行失败（退出码 {res.returncode}）")
        finally:
            try:
                os.unlink(script_path)
            except OSError:
                pass
    else:
        raise ValueError(f"unsupported t3code install channel: {channel}")

    try:
        PROBE_CACHE_FILE.unlink(missing_ok=True)
    except OSError:
        pass
    refreshed_path = enriched_path()
    binary = shutil.which("t3", path=refreshed_path)
    if not binary:
        raise ValueError("安装完成但未在 PATH 中找到 t3 命令，请检查安装日志")
    version, _ = read_cli_version(binary, ["--version"], refreshed_path, {})
    return {"installed": True, "binary_path": binary, "version": version}


def _stop_managed_pid(pid, expected_identity="", wait_secs=3.0, require_identity=True):
    st = _pid_state(pid, expected_identity, require_identity=require_identity)
    if st in ("gone", "mismatch"):
        return True
    if st == "unknown":
        return False
    try:
        os.killpg(pid, signal.SIGTERM)
    except OSError:
        try:
            os.kill(pid, signal.SIGTERM)
        except OSError:
            return _pid_state(pid, expected_identity, require_identity=require_identity) in ("gone", "mismatch")
    deadline = time.monotonic() + wait_secs
    while time.monotonic() < deadline:
        st = _pid_state(pid, expected_identity, require_identity=require_identity)
        if st in ("gone", "mismatch"):
            return True
        time.sleep(0.08)
    st = _pid_state(pid, expected_identity, require_identity=require_identity)
    if st in ("gone", "mismatch"):
        return True
    if st == "unknown":
        return False
    try:
        os.killpg(pid, signal.SIGKILL)
    except OSError:
        try:
            os.kill(pid, signal.SIGKILL)
        except OSError:
            return _pid_state(pid, expected_identity, require_identity=require_identity) in ("gone", "mismatch")
    deadline = time.monotonic() + 0.5
    while time.monotonic() < deadline:
        st = _pid_state(pid, expected_identity, require_identity=require_identity)
        if st in ("gone", "mismatch"):
            return True
        time.sleep(0.05)
    return _pid_state(pid, expected_identity, require_identity=require_identity) in ("gone", "mismatch")


def _t3code_stop_locked(requested_port=None):
    state = read_json(T3CODE_STATE_FILE)
    if isinstance(state, dict):
        state_port = state.get("port")
        if (
            requested_port is not None
            and not isinstance(state_port, bool)
            and isinstance(state_port, int)
            and state_port != requested_port
        ):
            # Refuse to kill a managed instance bound to a different port.
            return {"running": False, "skipped_port_mismatch": True}
        sup_pid = state.get("supervisor_pid")
        sup_ident = str(state.get("supervisor_identity") or "")
        pid = state.get("pid")
        pid_ident = str(state.get("pid_identity") or "")
        has_sup = not isinstance(sup_pid, bool) and isinstance(sup_pid, int) and sup_pid > 0
        has_pid = not isinstance(pid, bool) and isinstance(pid, int) and pid > 0
        if has_sup:
            sup_st = _pid_state(sup_pid, sup_ident)
            if sup_st == "unknown":
                raise ValueError(f"无法验证 T3 Code 守护进程 ({sup_pid}) 身份，已保留运行状态文件")
            if sup_st == "matched":
                # Signal supervisor first so it cleanly forwards SIGTERM to child's process group.
                try:
                    os.kill(sup_pid, signal.SIGTERM)
                except OSError:
                    pass
                deadline = time.monotonic() + 2.5
                while time.monotonic() < deadline:
                    s_done = _pid_state(sup_pid, sup_ident) in ("gone", "mismatch")
                    p_done = (not has_pid) or (_pid_state(pid, pid_ident) in ("gone", "mismatch"))
                    if s_done and p_done:
                        break
                    time.sleep(0.08)
        if has_pid:
            pid_st = _pid_state(pid, pid_ident)
            if pid_st == "unknown":
                raise ValueError(f"无法验证 T3 Code 进程 ({pid}) 身份，已保留运行状态文件")
            if pid_st == "matched":
                if not _stop_managed_pid(pid, expected_identity=pid_ident, wait_secs=1.5):
                    raise ValueError(f"T3 Code 进程 ({pid}) 未能终止，已保留运行状态文件")
        if has_sup:
            sup_st = _pid_state(sup_pid, sup_ident)
            if sup_st == "unknown":
                raise ValueError(f"无法验证 T3 Code 守护进程 ({sup_pid}) 身份，已保留运行状态文件")
            if sup_st == "matched":
                if not _stop_managed_pid(sup_pid, expected_identity=sup_ident, wait_secs=1.0):
                    raise ValueError(f"T3 Code 守护进程 ({sup_pid}) 未能终止，已保留运行状态文件")
    T3CODE_STATE_FILE.unlink(missing_ok=True)
    T3CODE_PAIR_FILE.unlink(missing_ok=True)
    return {"running": False}


def t3code_stop(payload=None):
    req_port = None
    if isinstance(payload, dict) and payload.get("port") not in (None, "", 0):
        req_port = _requested_port(payload)
    with _t3code_lifecycle_lock():
        return _t3code_stop_locked(requested_port=req_port)


def _t3_help_has_subcommand(help_text, subcmd):
    if not help_text:
        return False
    return bool(re.search(
        rf"(?m)^\s*{re.escape(subcmd)}\b|\bcommands:\s*[\s\S]*\b{re.escape(subcmd)}\b|\bt3\s+{re.escape(subcmd)}\b",
        help_text,
        re.IGNORECASE,
    ))


def _build_t3code_command(binary, port, env):
    help_text = ""
    try:
        res = subprocess.run(
            [binary, "--help"],
            env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, timeout=4,
        )
        help_text = res.stdout or ""
    except (OSError, subprocess.SubprocessError):
        help_text = ""

    use_serve = _t3_help_has_subcommand(help_text, "serve")
    flag_help = help_text
    cmd = [binary]
    if use_serve:
        cmd.append("serve")
        try:
            sres = subprocess.run(
                [binary, "serve", "--help"],
                env=env, stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                text=True, timeout=4,
            )
            if sres.stdout:
                flag_help = sres.stdout
        except (OSError, subprocess.SubprocessError):
            pass

    if "--host" in flag_help:
        cmd.extend(["--host", "127.0.0.1"])
    if "--port" in flag_help:
        cmd.extend(["--port", str(port)])
    if "--no-browser" in flag_help:
        cmd.append("--no-browser")
    if "--base-dir" in flag_help:
        T3CODE_STATE_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
        cmd.extend(["--base-dir", str(T3CODE_STATE_DIR)])
    return cmd


def _mint_t3_pair_url(binary, port, path_str, budget_secs=2.5):
    """Attempt to mint a fresh per-browser pairing URL via `t3 pair` within a tight budget."""
    if not binary:
        return ""
    deadline = time.monotonic() + budget_secs
    env = os.environ.copy()
    env["PATH"] = path_str
    env["T3CODE_HOST"] = "127.0.0.1"
    env["T3CODE_PORT"] = str(port)
    env["T3CODE_NO_BROWSER"] = "1"

    pair_help = ""
    rem = max(0.3, min(0.9, deadline - time.monotonic()))
    try:
        phres = subprocess.run(
            [binary, "pair", "--help"],
            env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, timeout=rem,
        )
        if phres.returncode != 0:
            return ""
        pair_help = phres.stdout or ""
    except (OSError, subprocess.SubprocessError):
        return ""
    if not pair_help or ("unknown command" in pair_help.lower()) or ("unrecognized" in pair_help.lower()):
        return ""

    cmd = [binary, "pair"]
    if "--host" in pair_help:
        cmd.extend(["--host", "127.0.0.1"])
    if "--port" in pair_help:
        cmd.extend(["--port", str(port)])
    if "--base-dir" in pair_help:
        T3CODE_STATE_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
        cmd.extend(["--base-dir", str(T3CODE_STATE_DIR)])

    rem = deadline - time.monotonic()
    if rem <= 0.2:
        return ""
    try:
        res = subprocess.run(
            cmd,
            env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, timeout=rem,
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    if res.returncode != 0:
        return ""
    return _extract_t3_pair_url(res.stdout or "", port)


def _ensure_macos_claude_shim():
    """Create a self-stripping /bin/sh shim on macOS that routes `claude` through cage-claude-account."""
    shim_dir = T3CODE_ROOT / "shims"
    shim_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    helper = "/usr/local/libexec/cage-cli/bin/cage-claude-account"
    if not os.path.isfile(helper):
        helper = "/usr/local/bin/cage-claude-account"
    shim_path = shim_dir / "claude"
    q_dir = shlex.quote(str(shim_dir))
    q_helper = shlex.quote(helper)
    q_takeover = shlex.quote(str(MAGPIE_TAKEOVER_FILE))
    script = (
        "#!/bin/sh\n"
        f"if [ -f {q_takeover} ]; then\n"
        + _posix_strip_and_exec("claude")
        + "fi\n"
        f"SHIM_DIR={q_dir}\n"
        'CLEAN_PATH=""\n'
        'OLD_IFS="$IFS"\n'
        "IFS=:\n"
        'for dir in $PATH; do\n'
        '  case "$dir" in\n'
        '    ""|"$SHIM_DIR") continue ;;\n'
        "  esac\n"
        '  CLEAN_PATH="${CLEAN_PATH:+$CLEAN_PATH:}$dir"\n'
        "done\n"
        'IFS="$OLD_IFS"\n'
        'export PATH="$CLEAN_PATH"\n'
        f'exec {q_helper} run "$CAGE_CLAUDE_ACCOUNT_ID" "$@"\n'
    )
    atomic_write_text(shim_path, script, mode=0o700)
    return str(shim_dir)


def t3code_start(payload, restart=False):
    port = _requested_port(payload, default=13773)
    bindings = payload.get("bindings") or {}
    if not isinstance(bindings, dict):
        raise ValueError("invalid t3code bindings")
    bindings_hash = hashlib.sha256(
        json.dumps(bindings, sort_keys=True, ensure_ascii=False).encode("utf-8")
    ).hexdigest()[:16]

    with _t3code_lifecycle_lock():
        path_str = enriched_path()
        binary = shutil.which("t3", path=path_str)
        if not binary:
            raise ValueError("未找到 t3 命令，请先安装 T3 Code")
        version = _read_t3_version(binary, path_str)

        existing = read_json(T3CODE_STATE_FILE)
        if restart:
            _t3code_stop_locked()
        elif isinstance(existing, dict):
            old_pid = existing.get("pid")
            old_pid_ident = str(existing.get("pid_identity") or "")
            old_sup = existing.get("supervisor_pid")
            old_sup_ident = str(existing.get("supervisor_identity") or "")
            old_port = existing.get("port")
            if (
                not isinstance(old_pid, bool)
                and isinstance(old_pid, int)
                and old_port == port
                and existing.get("bindings_hash") == bindings_hash
                and _pid_matches(old_pid, old_pid_ident)
                and _http_responsive("127.0.0.1", port, timeout=0.45)
            ):
                return {
                    "installed": bool(binary),
                    "binary_path": binary,
                    "version": version,
                    "running": True,
                    "pid": old_pid,
                    "port": port,
                    "has_pair_url": bool(_read_pair_once(port)),
                }
            if (
                (not isinstance(old_pid, bool) and isinstance(old_pid, int) and _pid_matches(old_pid, old_pid_ident))
                or (not isinstance(old_sup, bool) and isinstance(old_sup, int) and _pid_matches(old_sup, old_sup_ident))
            ):
                _t3code_stop_locked()

        os.umask(0o077)
        T3CODE_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
        T3CODE_PAIR_FILE.unlink(missing_ok=True)

        # Build isolated provider routing for `t3 serve` WITHOUT polluting global
        # XDG_CONFIG_HOME / XDG_DATA_HOME / XDG_STATE_HOME or CODEX_HOME on the parent process.
        # Keep ~/.local/share/cage/auto-shims synced so "auto" bindings dynamically
        # pick healthy accounts per-invocation without restarting T3 Code!
        magpie_mode = any(
            isinstance(b, dict) and str(b.get("mode") or "").strip().lower() == "magpie"
            for b in bindings.values()
        )
        try:
            sync_auto_shims({"disable_all": True, "magpie_takeover": True} if magpie_mode else {})
        except Exception:
            pass
        env = os.environ.copy()
        env.pop("CAGE_CLI_ACCOUNT_ID", None)
        for k in list(env):
            if k.startswith("CAGE_CLI_ACCOUNT_"):
                env.pop(k, None)
        env.pop("CAGE_CLAUDE_ACCOUNT_ID", None)
        env.pop("CAGE_CLAUDE_SUBJECT", None)
        env.pop("CAGE_CODEX_ACCOUNT_ID", None)
        env.pop("CAGE_CODEX_SUBJECT", None)
        shim_prefixes = []
        include_auto_shims = False
        for agent_id, binding in bindings.items():
            if not isinstance(binding, dict):
                continue
            mode = str(binding.get("mode") or "").strip().lower()
            guest_id = str(binding.get("guest_id") or "default").strip() or "default"
            if mode in ("auto", "magpie"):
                include_auto_shims = True
                if agent_id == "claude":
                    env.pop("CAGE_CLAUDE_ACCOUNT_ID", None)
                    env.pop("CAGE_CLAUDE_SUBJECT", None)
                elif agent_id == "codex":
                    env.pop("CAGE_CODEX_ACCOUNT_ID", None)
                    env.pop("CAGE_CODEX_SUBJECT", None)
                    env.pop(_agent_account_env_var("codex"), None)
                elif CLI_ID_RE.match(str(agent_id)):
                    env.pop(_agent_account_env_var(str(agent_id)), None)
                continue
            if mode == "default":
                if agent_id == "claude":
                    env["CAGE_CLAUDE_ACCOUNT_ID"] = "default"
                    env.pop("CAGE_CLAUDE_SUBJECT", None)
                elif agent_id == "codex":
                    env["CAGE_CODEX_ACCOUNT_ID"] = "default"
                    env.pop("CAGE_CODEX_SUBJECT", None)
                    env[_agent_account_env_var("codex")] = "default"
                elif CLI_ID_RE.match(str(agent_id)):
                    env[_agent_account_env_var(str(agent_id))] = "default"
                continue
            if agent_id == "claude":
                if guest_id != "default" and not ACCOUNT_ID_RE.match(guest_id):
                    raise ValueError("invalid claude guest_id")
                if sys.platform == "darwin":
                    shim_prefixes.append(_ensure_macos_claude_shim())
                    if Path("/usr/local/libexec/cage-cli/bin").is_dir():
                        shim_prefixes.append("/usr/local/libexec/cage-cli/bin")
                else:
                    shim_prefixes.append("/usr/local/lib/cage-claude-bin")
                env["CAGE_CLAUDE_ACCOUNT_ID"] = guest_id
                subject = str(binding.get("subject") or "").strip()
                if subject:
                    if len(subject) > 128 or any(ch in subject for ch in ("\x00", "\r", "\n")):
                        raise ValueError("invalid claude subject")
                    env["CAGE_CLAUDE_SUBJECT"] = subject
                else:
                    env.pop("CAGE_CLAUDE_SUBJECT", None)
                if guest_id != "default":
                    claude_cfg = HOME / ".local/share/cage/claude-accounts" / guest_id / "config"
                    claude_cfg.mkdir(parents=True, exist_ok=True, mode=0o700)
            elif agent_id == "codex" and binding.get("codex_native"):
                if guest_id != "default" and not ACCOUNT_ID_RE.match(guest_id):
                    raise ValueError("invalid codex guest_id")
                shim_prefixes.append("/usr/local/lib/cage-codex-bin")
                env["CAGE_CODEX_ACCOUNT_ID"] = guest_id
                subject = str(binding.get("subject") or "").strip()
                if subject:
                    if len(subject) > 128 or any(ch in subject for ch in ("\x00", "\r", "\n")):
                        raise ValueError("invalid codex subject")
                    env["CAGE_CODEX_SUBJECT"] = subject
                else:
                    env.pop("CAGE_CODEX_SUBJECT", None)
                if guest_id != "default":
                    codex_home_dir = HOME / ".local/share/cage/codex-accounts" / guest_id / "home"
                    codex_home_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
            else:
                spec = binding.get("spec")
                if isinstance(spec, dict) and spec.get("id"):
                    prep = prepare_session({"spec": spec, "account_id": guest_id})
                    shim_prefixes.append(prep["shim_dir"])
                    env[_agent_account_env_var(spec["id"])] = guest_id

        if include_auto_shims and AUTO_SHIMS_DIR.is_dir():
            shim_prefixes.append(str(AUTO_SHIMS_DIR))

        dedup_prefixes = []
        for p in shim_prefixes:
            if p and p not in dedup_prefixes:
                dedup_prefixes.append(p)
        env["PATH"] = os.pathsep.join([*dedup_prefixes, path_str]) if dedup_prefixes else path_str
        env["T3CODE_HOST"] = "127.0.0.1"
        env["T3CODE_PORT"] = str(port)
        env["T3CODE_NO_BROWSER"] = "1"

        cmd = _build_t3code_command(binary, port, env)
        workspace = str(payload.get("workspace") or "").strip()
        cwd = workspace if workspace and Path(workspace).is_dir() else str(HOME)

        handshake_file = T3CODE_ROOT / f".sup-{os.getpid()}-{time.time_ns()}.json"
        sup_cfg = {
            "cmd": cmd,
            "port": port,
            "cwd": cwd,
            "log_file": str(T3CODE_LOG_FILE),
            "pair_file": str(T3CODE_PAIR_FILE),
            "handshake_file": str(handshake_file),
        }
        sup_proc = subprocess.Popen(
            [sys.executable, "-I", str(Path(__file__).resolve()), "t3code-supervise"],
            cwd=cwd,
            env=env,
            stdin=subprocess.PIPE,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            close_fds=True,
            start_new_session=True,
        )
        try:
            assert sup_proc.stdin is not None
            sup_proc.stdin.write(json.dumps(sup_cfg, ensure_ascii=False).encode("utf-8"))
            sup_proc.stdin.close()
        except OSError:
            pass

        child_pid = 0
        deadline = time.monotonic() + 12.0
        ready = False
        try:
            while time.monotonic() < deadline:
                if not child_pid and handshake_file.is_file():
                    hs = read_json(handshake_file)
                    if isinstance(hs, dict):
                        if hs.get("error"):
                            raise ValueError(f"T3 Code 启动失败：{hs['error']}")
                        cpid = hs.get("child_pid")
                        if not isinstance(cpid, bool) and isinstance(cpid, int) and cpid > 0:
                            child_pid = cpid
                rc = sup_proc.poll()
                if rc is not None:
                    tail = _redact_t3_log_tail(T3CODE_LOG_FILE)
                    suffix = f"：{tail}" if tail else ""
                    raise ValueError(f"T3 Code 启动失败（退出码 {rc}）{suffix}")
                if child_pid and not _pid_alive(child_pid):
                    try:
                        rc = sup_proc.wait(timeout=0.5)
                    except subprocess.TimeoutExpired:
                        rc = 1
                    tail = _redact_t3_log_tail(T3CODE_LOG_FILE)
                    suffix = f"：{tail}" if tail else ""
                    raise ValueError(f"T3 Code 启动失败（退出码 {rc}）{suffix}")
                if child_pid and _http_responsive("127.0.0.1", port, timeout=0.45):
                    ready = True
                    break
                time.sleep(0.1)

            if not ready:
                if child_pid:
                    _stop_managed_pid(child_pid, expected_identity=_pid_identity(child_pid), wait_secs=1.5, require_identity=False)
                _stop_managed_pid(sup_proc.pid, expected_identity=_pid_identity(sup_proc.pid), wait_secs=1.0, require_identity=False)
                T3CODE_PAIR_FILE.unlink(missing_ok=True)
                tail = _redact_t3_log_tail(T3CODE_LOG_FILE)
                suffix = f"：{tail}" if tail else ""
                raise ValueError(f"T3 Code 启动超时（端口 {port} 未就绪）{suffix}")

            # Allow supervisor pipe reader a brief window to capture initial pairing URL into pair.once.
            pair_deadline = time.monotonic() + 0.35
            while not _read_pair_once(port) and time.monotonic() < pair_deadline:
                time.sleep(0.05)

            state = {
                "pid": child_pid,
                "pid_identity": _pid_identity(child_pid),
                "supervisor_pid": sup_proc.pid,
                "supervisor_identity": _pid_identity(sup_proc.pid),
                "port": port,
                "version": version,
                "binary": binary,
                "started_at": int(time.time()),
                "bindings_hash": bindings_hash,
            }
            atomic_write_json(T3CODE_STATE_FILE, state, mode=0o600)
            return {
                "installed": bool(binary),
                "binary_path": binary,
                "version": version,
                "running": True,
                "pid": child_pid,
                "port": port,
                "has_pair_url": bool(_read_pair_once(port)),
            }
        finally:
            handshake_file.unlink(missing_ok=True)


def _t3code_open_url_locked(payload):
    state = read_json(T3CODE_STATE_FILE)
    default_port = 13773
    if isinstance(state, dict):
        sp = state.get("port")
        if not isinstance(sp, bool) and isinstance(sp, int) and 1024 <= sp <= 65535:
            default_port = sp
    requested_port = _requested_port(payload, default=default_port)

    path_str = enriched_path()
    binary = ""
    if isinstance(state, dict) and isinstance(state.get("binary"), str) and os.path.isfile(state["binary"]):
        binary = state["binary"]
    if not binary:
        binary = shutil.which("t3", path=path_str) or ""

    # 1. Prefer minting a brand-new short-lived pairing URL via `t3 pair` for this browser.
    minted = _mint_t3_pair_url(binary, requested_port, path_str)
    if minted:
        return {"url": minted, "paired_bootstrap": True}

    # 2. Fallback to ephemeral one-time startup pair.once file.
    consume = payload.get("consume", True) if isinstance(payload, dict) else True
    pair_url = _read_pair_once(requested_port)
    if pair_url:
        if consume:
            T3CODE_PAIR_FILE.unlink(missing_ok=True)
        return {"url": pair_url, "paired_bootstrap": True}

    return {"url": f"http://127.0.0.1:{requested_port}/", "paired_bootstrap": False}


def t3code_open_url(payload):
    with _t3code_lifecycle_lock():
        return _t3code_open_url_locked(payload)


def t3code_mint_pair(payload):
    with _t3code_lifecycle_lock():
        return _t3code_open_url_locked(payload)


# ---------------------------------------------------------------------------
# Managed Magpie (yetone/magpie) Guest Lifecycle
# ---------------------------------------------------------------------------

@contextmanager
def _magpie_lifecycle_lock():
    """Serialize all Magpie lifecycle and web key operations."""
    os.umask(0o077)
    MAGPIE_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(str(MAGPIE_LOCK_FILE), os.O_RDWR | os.O_CREAT, 0o600)
    os.chmod(MAGPIE_LOCK_FILE, 0o600)
    with os.fdopen(fd, "a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def _validate_magpie_port(port, label="Magpie 端口"):
    if isinstance(port, bool) or not isinstance(port, int) or not (1024 <= port <= 65535):
        raise ValueError(f"{label}须在 1024-65535 之间")
    return port


def _requested_magpie_port(payload, key="port", default=MAGPIE_DEFAULT_PORT, label="Magpie 端口"):
    raw = payload.get(key) if isinstance(payload, dict) else None
    if raw is None or raw == "" or raw == 0:
        return _validate_magpie_port(default, label=label)
    if isinstance(raw, bool):
        raise ValueError(f"{label}须在 1024-65535 之间")
    try:
        port = int(raw)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{label}须在 1024-65535 之间") from exc
    return _validate_magpie_port(port, label=label)


def _read_magpie_web_key():
    try:
        key = MAGPIE_KEY_FILE.read_text(encoding="utf-8", errors="ignore").strip()
    except OSError:
        return ""
    if MAGPIE_KEY_RE.match(key):
        return key
    return ""


def _ensure_magpie_web_key():
    """Load validated Magpie web auth key or generate a fresh 32-char hex key (0600)."""
    existing = _read_magpie_web_key()
    if existing:
        try:
            os.chmod(MAGPIE_KEY_FILE, 0o600)
        except OSError:
            pass
        return existing
    key = secrets.token_hex(16)
    atomic_write_text(MAGPIE_KEY_FILE, key + "\n", mode=0o600)
    return key


def _read_cached_cli_version(binary, path_str):
    if not binary:
        return ""
    cache = read_json(PROBE_CACHE_FILE, {}) or {}
    if not isinstance(cache, dict):
        cache = {}
    ver, dirty = read_cli_version(binary, ["--version"], path_str, cache)
    if dirty:
        try:
            atomic_write_json(PROBE_CACHE_FILE, cache, mode=0o600)
        except OSError:
            pass
    return ver


def magpie_supervise(cfg):
    """Detached supervisor for `magpie web` with live redaction and 64 KiB log cap."""
    os.umask(0o077)
    cmd = cfg["cmd"]
    cwd = cfg.get("cwd") or str(HOME)
    log_file = Path(cfg.get("log_file") or MAGPIE_LOG_FILE)
    handshake_file = Path(cfg["handshake_file"])

    if hasattr(signal, "SIGHUP"):
        signal.signal(signal.SIGHUP, signal.SIG_IGN)

    logger = _BoundedRedactedLog(log_file, max_bytes=MAGPIE_LOG_MAX_BYTES)
    child = None
    term_state = {"requested": False, "sent": False}

    def forward_term(_signum, _frame):
        term_state["requested"] = True
        if child is not None and not term_state["sent"]:
            term_state["sent"] = True
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except OSError:
                try:
                    child.terminate()
                except OSError:
                    pass

    signal.signal(signal.SIGTERM, forward_term)
    signal.signal(signal.SIGINT, forward_term)

    try:
        child = subprocess.Popen(
            cmd,
            cwd=cwd,
            env=os.environ.copy(),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
    except OSError as exc:
        logger.write_raw_line(f"Failed to start magpie: {exc}")
        logger.close()
        try:
            atomic_write_json(handshake_file, {"error": str(exc), "supervisor_pid": os.getpid()}, mode=0o600)
        except OSError:
            pass
        return 1

    if term_state["requested"] and not term_state["sent"]:
        forward_term(signal.SIGTERM, None)

    try:
        atomic_write_json(
            handshake_file,
            {"child_pid": child.pid, "supervisor_pid": os.getpid()},
            mode=0o600,
        )
    except OSError:
        pass

    try:
        assert child.stdout is not None
        secret_key = os.environ.get("MAGPIE_WEB_KEY", "")
        while True:
            raw = child.stdout.readline(65536)
            if not raw:
                break
            line = raw.decode("utf-8", errors="replace")
            if secret_key and secret_key in line:
                logger.write_raw_line("[REDACTED]\n")
                continue
            logger.write_raw_line(line)
    finally:
        if child.stdout is not None:
            try:
                child.stdout.close()
            except OSError:
                pass
        rc = child.wait()
        logger.close()
    return rc if isinstance(rc, int) else 0


def magpie_status(payload):
    requested_port = _requested_magpie_port(payload, key="port", default=MAGPIE_DEFAULT_PORT)
    gateway_port = _requested_magpie_port(
        payload, key="gateway_port", default=MAGPIE_DEFAULT_GATEWAY_PORT, label="Magpie Gateway 端口"
    )
    path_str = enriched_path()
    binary = shutil.which("magpie", path=path_str) or ""
    version = _read_cached_cli_version(binary, path_str) if binary else ""
    state = read_json(MAGPIE_STATE_FILE)
    running = False
    pid = 0
    if isinstance(state, dict):
        cand_pid = state.get("pid")
        cand_port = state.get("port")
        cand_ident = str(state.get("pid_identity") or "")
        if (
            not isinstance(cand_pid, bool)
            and isinstance(cand_pid, int)
            and cand_pid > 0
            and not isinstance(cand_port, bool)
            and isinstance(cand_port, int)
            and cand_port == requested_port
            and _pid_matches(cand_pid, cand_ident)
            and _http_responsive("127.0.0.1", requested_port, timeout=0.45)
        ):
            running = True
            pid = cand_pid
    gateway_running = _http_responsive("127.0.0.1", gateway_port, timeout=0.35)
    return {
        "installed": bool(binary),
        "binary_path": binary,
        "version": version,
        "running": running,
        "pid": pid if running else 0,
        "port": requested_port,
        "gateway_port": gateway_port,
        "gateway_running": gateway_running,
        "gateway_only": bool(state.get("gateway_only")) if running and isinstance(state, dict) else False,
    }


def magpie_install(payload):
    path_str = enriched_path()
    curl = shutil.which("curl", path=path_str)
    if not curl:
        raise ValueError("环境中未找到 curl")
    (HOME / ".local/bin").mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["MAGPIE_BIN"] = str(HOME / ".local/bin")
    env["PATH"] = path_str
    script_url = "https://usemagpie.ai/install.sh"
    print("Downloading official Magpie installer from usemagpie.ai...", flush=True)
    with tempfile.NamedTemporaryFile(prefix="cage-magpie-install-", suffix=".sh", delete=False) as tmp:
        script_path = tmp.name
    try:
        dl = subprocess.run(
            [curl, "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "-fsSL", "--connect-timeout", "20", "-o", script_path, script_url],
            env=env, stdin=subprocess.DEVNULL,
        )
        if dl.returncode != 0:
            raise ValueError(f"下载 Magpie 安装脚本失败（curl 退出码 {dl.returncode}）")
        install_cmd = ["/bin/sh", script_path] if sys.platform == "darwin" else ["/bin/sh", script_path, "--tui"]
        res = subprocess.run(install_cmd, env=env, stdin=subprocess.DEVNULL)
        if res.returncode != 0:
            raise ValueError(f"Magpie 安装脚本执行失败（退出码 {res.returncode}）")
    finally:
        try:
            os.unlink(script_path)
        except OSError:
            pass

    try:
        PROBE_CACHE_FILE.unlink(missing_ok=True)
    except OSError:
        pass
    refreshed_path = enriched_path()
    binary = shutil.which("magpie", path=refreshed_path)
    if not binary:
        raise ValueError("安装完成但未在 PATH 中找到 magpie 命令，请检查安装日志")
    version, _ = read_cli_version(binary, ["--version"], refreshed_path, {})
    return {"installed": True, "binary_path": binary, "version": version}


def _magpie_stop_locked(requested_port=None):
    state = read_json(MAGPIE_STATE_FILE)
    if isinstance(state, dict):
        state_port = state.get("port")
        if (
            requested_port is not None
            and not isinstance(state_port, bool)
            and isinstance(state_port, int)
            and state_port != requested_port
        ):
            return {"running": False, "skipped_port_mismatch": True}
        sup_pid = state.get("supervisor_pid")
        sup_ident = str(state.get("supervisor_identity") or "")
        pid = state.get("pid")
        pid_ident = str(state.get("pid_identity") or "")
        has_sup = not isinstance(sup_pid, bool) and isinstance(sup_pid, int) and sup_pid > 0
        has_pid = not isinstance(pid, bool) and isinstance(pid, int) and pid > 0
        if has_sup:
            sup_st = _pid_state(sup_pid, sup_ident)
            if sup_st == "unknown":
                raise ValueError(f"无法验证 Magpie 守护进程 ({sup_pid}) 身份，已保留运行状态文件")
            if sup_st == "matched":
                try:
                    os.kill(sup_pid, signal.SIGTERM)
                except OSError:
                    pass
                deadline = time.monotonic() + 2.5
                while time.monotonic() < deadline:
                    s_done = _pid_state(sup_pid, sup_ident) in ("gone", "mismatch")
                    p_done = (not has_pid) or (_pid_state(pid, pid_ident) in ("gone", "mismatch"))
                    if s_done and p_done:
                        break
                    time.sleep(0.08)
        if has_pid:
            pid_st = _pid_state(pid, pid_ident)
            if pid_st == "unknown":
                raise ValueError(f"无法验证 Magpie 进程 ({pid}) 身份，已保留运行状态文件")
            if pid_st == "matched":
                if not _stop_managed_pid(pid, expected_identity=pid_ident, wait_secs=1.5):
                    raise ValueError(f"Magpie 进程 ({pid}) 未能终止，已保留运行状态文件")
        if has_sup:
            sup_st = _pid_state(sup_pid, sup_ident)
            if sup_st == "unknown":
                raise ValueError(f"无法验证 Magpie 守护进程 ({sup_pid}) 身份，已保留运行状态文件")
            if sup_st == "matched":
                if not _stop_managed_pid(sup_pid, expected_identity=sup_ident, wait_secs=1.0):
                    raise ValueError(f"Magpie 守护进程 ({sup_pid}) 未能终止，已保留运行状态文件")
    MAGPIE_STATE_FILE.unlink(missing_ok=True)
    return {"running": False}


def magpie_stop(payload=None):
    req_port = None
    if isinstance(payload, dict) and payload.get("port") not in (None, "", 0):
        req_port = _requested_magpie_port(payload, key="port", default=MAGPIE_DEFAULT_PORT)
    with _magpie_lifecycle_lock():
        return _magpie_stop_locked(requested_port=req_port)


def magpie_uninstall(payload=None):
    with _magpie_lifecycle_lock():
        _magpie_stop_locked()
        path_str = enriched_path()
        binary = shutil.which("magpie", path=path_str) or ""
        if binary:
            env = os.environ.copy()
            env["PATH"] = path_str
            try:
                res = subprocess.run(
                    [binary, "uninstall", "--yes", "--keep-data"],
                    env=env,
                    stdin=subprocess.DEVNULL,
                    capture_output=True,
                    text=True,
                    timeout=20,
                    check=False,
                )
                if res.stdout:
                    print(res.stdout, end="", flush=True)
                if res.stderr:
                    print(res.stderr, end="", file=sys.stderr, flush=True)
            except (OSError, subprocess.SubprocessError) as exc:
                print(f"magpie uninstall helper exited with error: {exc}", file=sys.stderr, flush=True)
        local_bin = HOME / ".local/bin/magpie"
        if local_bin.is_file() or local_bin.is_symlink():
            try:
                local_bin.unlink(missing_ok=True)
            except OSError:
                pass
        if sys.platform == "darwin":
            for app_name in ("Magpie.app", "magpie.app"):
                user_app = HOME / "Applications" / app_name
                try:
                    if user_app.is_symlink():
                        user_app.unlink(missing_ok=True)
                    elif user_app.is_dir():
                        shutil.rmtree(user_app, ignore_errors=True)
                except OSError:
                    pass
        MAGPIE_STATE_FILE.unlink(missing_ok=True)
        MAGPIE_TAKEOVER_FILE.unlink(missing_ok=True)
        try:
            PROBE_CACHE_FILE.unlink(missing_ok=True)
        except OSError:
            pass
        remaining = shutil.which("magpie", path=enriched_path()) or ""
        if remaining:
            raise ValueError(f"检测到系统路径仍存在 magpie（{remaining}），未能完全卸载，请手动移除")
    return {"installed": False, "running": False}


def magpie_start(payload, restart=False):
    port = _requested_magpie_port(payload, key="port", default=MAGPIE_DEFAULT_PORT)
    gateway_port = _requested_magpie_port(
        payload, key="gateway_port", default=MAGPIE_DEFAULT_GATEWAY_PORT, label="Magpie Gateway 端口"
    )
    if port == gateway_port:
        raise ValueError(f"Magpie Web 端口 ({port}) 不能与 Gateway 端口 ({gateway_port}) 冲突")
    gateway_only = bool(payload.get("gateway_only")) if isinstance(payload, dict) else False

    with _magpie_lifecycle_lock():
        path_str = enriched_path()
        binary = shutil.which("magpie", path=path_str)
        if not binary:
            raise ValueError("未找到 magpie 命令，请先安装 Magpie")
        version = _read_cached_cli_version(binary, path_str)

        existing = read_json(MAGPIE_STATE_FILE)
        if restart:
            _magpie_stop_locked()
        elif isinstance(existing, dict):
            old_pid = existing.get("pid")
            old_pid_ident = str(existing.get("pid_identity") or "")
            old_sup = existing.get("supervisor_pid")
            old_sup_ident = str(existing.get("supervisor_identity") or "")
            old_port = existing.get("port")
            old_gw_only = bool(existing.get("gateway_only"))
            if (
                not isinstance(old_pid, bool)
                and isinstance(old_pid, int)
                and old_port == port
                and old_gw_only == gateway_only
                and _pid_matches(old_pid, old_pid_ident)
                and _http_responsive("127.0.0.1", port, timeout=0.45)
            ):
                return {
                    "installed": bool(binary),
                    "binary_path": binary,
                    "version": version,
                    "running": True,
                    "pid": old_pid,
                    "port": port,
                    "gateway_port": gateway_port,
                    "gateway_running": _http_responsive("127.0.0.1", gateway_port, timeout=0.35),
                    "gateway_only": gateway_only,
                }
            if (
                (not isinstance(old_pid, bool) and isinstance(old_pid, int) and _pid_matches(old_pid, old_pid_ident))
                or (not isinstance(old_sup, bool) and isinstance(old_sup, int) and _pid_matches(old_sup, old_sup_ident))
            ):
                _magpie_stop_locked()

        os.umask(0o077)
        MAGPIE_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
        web_key = _ensure_magpie_web_key()

        env = os.environ.copy()
        for k in list(env):
            if k.startswith("CAGE_CLI_ACCOUNT_"):
                env.pop(k, None)
        for k in (
            "CAGE_CLAUDE_ACCOUNT_ID", "CAGE_CLAUDE_SUBJECT",
            "CAGE_CODEX_ACCOUNT_ID", "CAGE_CODEX_SUBJECT",
            "CAGE_CLI_ACCOUNT_ID", "CAGE_CLI_ID", "CAGE_ORCA_SHIM_DIR",
            "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CLI_HOME",
        ):
            env.pop(k, None)
        env["PATH"] = path_str
        env["MAGPIE_WEB_KEY"] = web_key

        cmd = [binary, "web", "--addr", f"127.0.0.1:{port}", "--no-open"]
        if gateway_only:
            cmd.append("--gateway")
        workspace = str(payload.get("workspace") or "").strip() if isinstance(payload, dict) else ""
        cwd = workspace if workspace and Path(workspace).is_dir() else str(HOME)

        handshake_file = MAGPIE_ROOT / f".sup-{os.getpid()}-{time.time_ns()}.json"
        sup_cfg = {
            "cmd": cmd,
            "cwd": cwd,
            "log_file": str(MAGPIE_LOG_FILE),
            "handshake_file": str(handshake_file),
        }
        sup_proc = subprocess.Popen(
            [sys.executable, "-I", str(Path(__file__).resolve()), "magpie-supervise"],
            cwd=cwd,
            env=env,
            stdin=subprocess.PIPE,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            close_fds=True,
            start_new_session=True,
        )
        try:
            assert sup_proc.stdin is not None
            sup_proc.stdin.write(json.dumps(sup_cfg, ensure_ascii=False).encode("utf-8"))
            sup_proc.stdin.close()
        except OSError:
            pass

        child_pid = 0
        deadline = time.monotonic() + 12.0
        ready = False
        try:
            while time.monotonic() < deadline:
                if not child_pid and handshake_file.is_file():
                    hs = read_json(handshake_file)
                    if isinstance(hs, dict):
                        if hs.get("error"):
                            raise ValueError(f"Magpie 启动失败：{hs['error']}")
                        cpid = hs.get("child_pid")
                        if not isinstance(cpid, bool) and isinstance(cpid, int) and cpid > 0:
                            child_pid = cpid
                rc = sup_proc.poll()
                if rc is not None:
                    tail = _redact_t3_log_tail(MAGPIE_LOG_FILE)
                    suffix = f"：{tail}" if tail else ""
                    raise ValueError(f"Magpie 启动失败（退出码 {rc}）{suffix}")
                if child_pid and not _pid_alive(child_pid):
                    try:
                        rc = sup_proc.wait(timeout=0.5)
                    except subprocess.TimeoutExpired:
                        rc = 1
                    tail = _redact_t3_log_tail(MAGPIE_LOG_FILE)
                    suffix = f"：{tail}" if tail else ""
                    raise ValueError(f"Magpie 启动失败（退出码 {rc}）{suffix}")
                if child_pid and _http_responsive("127.0.0.1", port, timeout=0.45):
                    ready = True
                    break
                time.sleep(0.1)

            if not ready:
                if child_pid:
                    _stop_managed_pid(child_pid, expected_identity=_pid_identity(child_pid), wait_secs=1.5, require_identity=False)
                _stop_managed_pid(sup_proc.pid, expected_identity=_pid_identity(sup_proc.pid), wait_secs=1.0, require_identity=False)
                tail = _redact_t3_log_tail(MAGPIE_LOG_FILE)
                suffix = f"：{tail}" if tail else ""
                raise ValueError(f"Magpie 启动超时（端口 {port} 未就绪）{suffix}")

            state = {
                "pid": child_pid,
                "pid_identity": _pid_identity(child_pid),
                "supervisor_pid": sup_proc.pid,
                "supervisor_identity": _pid_identity(sup_proc.pid),
                "port": port,
                "gateway_port": gateway_port,
                "gateway_only": gateway_only,
                "version": version,
                "binary": binary,
                "started_at": int(time.time()),
            }
            atomic_write_json(MAGPIE_STATE_FILE, state, mode=0o600)
            return {
                "installed": bool(binary),
                "binary_path": binary,
                "version": version,
                "running": True,
                "pid": child_pid,
                "port": port,
                "gateway_port": gateway_port,
                "gateway_running": _http_responsive("127.0.0.1", gateway_port, timeout=0.35),
                "gateway_only": gateway_only,
            }
        finally:
            handshake_file.unlink(missing_ok=True)


def magpie_open_url(payload):
    with _magpie_lifecycle_lock():
        state = read_json(MAGPIE_STATE_FILE)
        default_port = MAGPIE_DEFAULT_PORT
        if isinstance(state, dict):
            sp = state.get("port")
            if not isinstance(sp, bool) and isinstance(sp, int) and 1024 <= sp <= 65535:
                default_port = sp
        requested_port = _requested_magpie_port(payload, key="port", default=default_port)
        key = _read_magpie_web_key()
        if key:
            return {"url": f"http://127.0.0.1:{requested_port}/?k={key}", "authenticated": True}
        return {"url": f"http://127.0.0.1:{requested_port}/", "authenticated": False}


# ---------------------------------------------------------------------------
# Herdr: herdr-core (hc), the environment's one terminal store
# ---------------------------------------------------------------------------
#
# Cage's terminals are hc terminals, so every hc client in the environment
# (the Herdr app through hc connect, hc in a shell) sees them, and Cage lists
# the others. Every hc command starts the core when it is down; whatever only
# looks (status, list, close, the attach loop) first checks the core's socket
# and never starts it. Only opening a terminal does.

HERDR_RELEASES = "https://releases.herdr.dev"
HC_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,31}$")
HC_ADDRESS_RE = re.compile(r"^t[0-9a-z]{1,15}$")
HC_SESSION_RE = re.compile(r"^[A-Za-z0-9._-]{1,64}$")
HC_VERSION_RE = re.compile(r"^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$")
HC_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
HERDR_LIST_MAX = 64
HERDR_HISTORY_LINES = 1000
HERDR_LABEL_KEYS = ("cage", "cage.project", "cage.kind", "cage.title")


@contextmanager
def _herdr_lifecycle_lock():
    """Serialize core start, terminal creation and install."""
    os.umask(0o077)
    HERDR_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(str(HERDR_LOCK_FILE), os.O_RDWR | os.O_CREAT, 0o600)
    os.chmod(HERDR_LOCK_FILE, 0o600)
    with os.fdopen(fd, "a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def _hc_binary(path_str=None):
    """herdr-core itself. A bare hc on PATH may be another program
    (herbstluftwm ships one), so Cage never runs hc by that name."""
    local = HOME / ".local/bin/herdr-core"
    if local.is_file() and os.access(local, os.X_OK):
        return str(local)
    return shutil.which("herdr-core", path=path_str or enriched_path()) or ""


def _hc_env(path_str):
    env = os.environ.copy()
    env["PATH"] = path_str
    # Cage's terminals have no parent, never the hc terminal this runs in.
    env.pop("HC_TERMINAL_ID", None)
    return env


def _hc_version(binary, env=None):
    """The version a working herdr-core reports ("hc 0.1.22"), or ""."""
    if not binary:
        return ""
    try:
        res = subprocess.run([binary, "--version"], env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, timeout=5)
    except (OSError, subprocess.SubprocessError):
        return ""
    out = (res.stdout or "").strip()
    if res.returncode != 0 or not out.startswith("hc "):
        return ""
    version = out[3:].split()[0] if out[3:].split() else ""
    return version if HC_VERSION_RE.match(version) else ""


def _hc_text(value, limit=120):
    """A guest-written string made safe to show: no control or format
    characters, at most limit characters."""
    if not isinstance(value, str):
        return ""
    clean = "".join(ch for ch in value if unicodedata.category(ch) not in ("Cc", "Cf", "Cs", "Co"))
    return clean.strip()[:limit]


def _hc_core_socket():
    """Where the running core listens, read from its lock file; "" when
    unknown. Unknown counts as not running: nothing here starts a core."""
    explicit = os.environ.get("HC_SOCKET", "")
    if explicit.startswith("/"):
        return explicit
    session = os.environ.get("HC_SESSION") or "default"
    if not HC_SESSION_RE.match(session):
        return ""
    roots = []
    xdg = os.environ.get("XDG_STATE_HOME", "")
    if xdg.startswith("/"):
        roots.append(Path(xdg))
    roots.append(HOME / ".local/state")
    runtime_dir = os.environ.get("XDG_RUNTIME_DIR", "")
    if runtime_dir.startswith("/"):
        roots.append(Path(runtime_dir))  # in case a Linux core keeps its lock there
    for root in roots:
        try:
            with open(root / "herdr-core" / session / "core.lock", "rb") as f:
                text = f.read(4096).decode("utf-8", "replace").strip()
        except OSError:
            continue
        line = text.splitlines()[0].strip() if text else ""
        if line.startswith("/"):
            return line
    return ""


def _hc_core_running(timeout=0.3):
    path = _hc_core_socket()
    if not path:
        return False
    probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    probe.settimeout(timeout)
    try:
        probe.connect(path)
        return True
    except OSError:
        return False
    finally:
        probe.close()


def _hc_json(binary, args, env, timeout=3.0):
    """Runs herdr-core; (exit code, its JSON object or None)."""
    try:
        res = subprocess.run([binary, *args], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE, text=True, timeout=timeout)
    except (OSError, subprocess.SubprocessError):
        return -1, None
    text = (res.stdout or "").strip() or (res.stderr or "").strip()
    try:
        data = json.loads(text) if text else None
    except ValueError:
        data = None
    return res.returncode, data if isinstance(data, dict) else None


def _hc_error(data, fallback):
    err = data.get("error") if isinstance(data, dict) else None
    message = _hc_text(err.get("message"), 200) if isinstance(err, dict) else ""
    return f"{fallback}：{message}" if message else fallback


def _hc_rpc(binary, env, requests, timeout=3.0):
    """Sends JSON-RPC requests through hc connect, one line each; results by id."""
    lines = "".join(json.dumps({"jsonrpc": "2.0", "id": i + 1, "method": m, "params": p}) + "\n"
                    for i, (m, p) in enumerate(requests))
    try:
        res = subprocess.run([binary, "connect"], env=env, input=lines, stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL, text=True, timeout=timeout)
    except (OSError, subprocess.SubprocessError):
        return {}
    results = {}
    for line in (res.stdout or "").splitlines()[:16]:
        try:
            msg = json.loads(line)
        except ValueError:
            continue
        if isinstance(msg, dict) and isinstance(msg.get("id"), int) and isinstance(msg.get("result"), dict):
            results[msg["id"]] = msg["result"]
    return results


def _hc_live_count(binary, env):
    rc, st = _hc_json(binary, ["server", "status", "--json"], env, timeout=3.0)
    live = st.get("live") if rc == 0 and st else None
    return live if isinstance(live, int) and live > 0 else 0


def _herdr_legacy_binary():
    """The Herdr TUI (herdr) Cage 0.1.21 installed into ~/.local/bin."""
    local = HOME / ".local/bin/herdr"
    return str(local) if local.is_file() and os.access(local, os.X_OK) else ""


def herdr_status(payload=None):
    path_str = enriched_path()
    binary = _hc_binary(path_str)
    version = _read_cached_cli_version(binary, path_str) if binary else ""
    running = bool(binary) and _hc_core_running()
    live, linger = 0, None
    if running:
        rc, st = _hc_json(binary, ["server", "status", "--json"], _hc_env(path_str), timeout=2.0)
        if rc == 0 and st:
            if isinstance(st.get("live"), int):
                live = max(0, st["live"])
            if isinstance(st.get("version"), str) and HC_VERSION_RE.match(st["version"]):
                version = st["version"]
            if isinstance(st.get("keeps_running_after_logout"), bool):
                linger = st["keeps_running_after_logout"]
    return {
        "installed": bool(binary and version),
        "binary_path": binary,
        "version": version,
        "server_running": running,
        "terminals_count": live,
        "keeps_running_after_logout": linger,
        "legacy_installed": bool(_herdr_legacy_binary()),
    }


def _herdr_fetch(curl, url, dest, env, max_time=300):
    res = subprocess.run(
        [curl, "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "-fsSL",
         "--connect-timeout", "20", "--max-time", str(max_time), "-o", str(dest), url],
        env=env, stdin=subprocess.DEVNULL,
    )
    if res.returncode != 0:
        raise ValueError(f"下载失败（curl 退出码 {res.returncode}）：{url}")


def _hc_newer(candidate, current):
    def parts(version):
        try:
            return tuple(int(x) for x in re.split(r"[-+]", version, maxsplit=1)[0].split("."))
        except ValueError:
            return ()
    return parts(candidate) > parts(current)


def _hc_link_hc(path_str):
    """hc beside herdr-core, only when no hc exists (the official rule)."""
    link = HOME / ".local/bin/hc"
    if not (HOME / ".local/bin/herdr-core").exists() or link.exists() or link.is_symlink():
        return
    if shutil.which("hc", path=path_str):
        return
    try:
        link.symlink_to("herdr-core")
    except OSError:
        pass


def _herdr_install_linux(existing, current, path_str, env):
    """The release build for this machine, checked against the SHA-256 the
    release manifest publishes, as the official install.sh does."""
    machine = os.uname().machine.lower()
    arch = {"x86_64": "x86_64", "amd64": "x86_64", "aarch64": "aarch64", "arm64": "aarch64"}.get(machine)
    if not arch:
        raise ValueError(f"herdr-core 没有适用于 {machine} 的版本")
    target = f"linux-{arch}"
    curl = shutil.which("curl", path=path_str)
    if not curl:
        raise ValueError("环境中未找到 curl")
    bin_dir = HOME / ".local/bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="cage-herdr-core-") as tmp:
        manifest_file = Path(tmp) / "manifest.json"
        print(f"Reading the herdr-core release manifest from {HERDR_RELEASES}...", flush=True)
        _herdr_fetch(curl, HERDR_RELEASES + "/core/latest.json", manifest_file, env, max_time=60)
        try:
            if manifest_file.stat().st_size > 1 << 20:
                raise ValueError("manifest too large")
            manifest = json.loads(manifest_file.read_text(encoding="utf-8"))
            entry = manifest["targets"][target]
            url, want, version = entry["url"], entry["sha256"], manifest["version"]
        except (OSError, ValueError, KeyError, TypeError):
            raise ValueError(f"herdr-core 发布清单里没有 {target} 的版本") from None
        if not (isinstance(url, str) and url.startswith(HERDR_RELEASES + "/core/") and url.endswith(".tar.gz")
                and not any(ch in url for ch in "?#") and isinstance(want, str) and HC_SHA256_RE.match(want)
                and isinstance(version, str) and HC_VERSION_RE.match(version)):
            raise ValueError("herdr-core 发布清单格式不正确")
        if existing and current and not _hc_newer(version, current):
            print(f"herdr-core {current} is current.", flush=True)
            return
        archive = Path(tmp) / "herdr-core.tar.gz"
        print(f"Downloading herdr-core {version} ({target})...", flush=True)
        _herdr_fetch(curl, url, archive, env)
        digest = hashlib.sha256()
        with open(archive, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                digest.update(chunk)
        if digest.hexdigest() != want:
            raise ValueError("下载的 herdr-core 校验失败（SHA-256 与官方发布清单不一致），请重试")
        print("SHA-256 matches the release manifest.", flush=True)
        staged = bin_dir / "herdr-core.staged"
        try:
            with tarfile.open(archive, "r:gz") as tar:
                member = tar.getmember("herdr-core")
                if not member.isfile() or member.size > 256 << 20:
                    raise ValueError("bad member")
                src = tar.extractfile(member)
                with open(staged, "wb") as out:
                    shutil.copyfileobj(src, out)
            os.chmod(staged, 0o755)
            if not _hc_version(str(staged), env):
                raise ValueError("下载的 herdr-core 无法在此环境运行")
            local = bin_dir / "herdr-core"
            if existing and Path(existing) == local:
                # The planned swap keeps running terminals alive.
                res = subprocess.run([existing, "update", "--from", str(staged)], env=env,
                                     stdin=subprocess.DEVNULL, timeout=120)
                if res.returncode != 0:
                    raise ValueError("herdr-core 更新失败，请查看操作详情")
            else:
                os.replace(staged, local)
        except (KeyError, OSError) as exc:
            raise ValueError(f"herdr-core 安装包无法解开：{exc}") from None
        finally:
            staged.unlink(missing_ok=True)
        print(f"herdr-core {version} installed into {bin_dir}.", flush=True)


def _herdr_install_official(path_str, env):
    """macOS: the official installer, which checks the SHA-256 and the
    release's code signature and installs Herdr Core.app."""
    curl = shutil.which("curl", path=path_str)
    if not curl:
        raise ValueError("环境中未找到 curl")
    with tempfile.TemporaryDirectory(prefix="cage-herdr-core-") as tmp:
        script = Path(tmp) / "install.sh"
        print("Downloading the official herdr-core installer from releases.herdr.dev...", flush=True)
        _herdr_fetch(curl, HERDR_RELEASES + "/core/install.sh", script, env, max_time=60)
        res = subprocess.run(["/bin/sh", str(script)], env={**env, "HC_INSTALL_DIR": str(HOME / ".local/bin")},
                             stdin=subprocess.DEVNULL, timeout=600)
        if res.returncode != 0:
            raise ValueError(f"herdr-core 官方安装脚本执行失败（退出码 {res.returncode}）")


def herdr_install(payload=None):
    """Installs herdr-core, or keeps the working one already here (the
    Herdr app may have put it there). update asks for a newer release."""
    update = isinstance(payload, dict) and payload.get("update") is True
    with _herdr_lifecycle_lock():
        path_str = enriched_path()
        env = _hc_env(path_str)
        existing = _hc_binary(path_str)
        current = _hc_version(existing, env)
        if existing and current and not update:
            print(f"herdr-core {current} is already installed at {existing}; using it.", flush=True)
            _hc_link_hc(path_str)
            return {"installed": True, "binary_path": existing, "version": current, "reused": True}
        if sys.platform == "darwin":
            _herdr_install_official(path_str, env)
        elif sys.platform.startswith("linux"):
            _herdr_install_linux(existing if current else "", current, path_str, env)
        else:
            raise ValueError("herdr-core 目前只支持 Linux 与 macOS 环境")
        try:
            PROBE_CACHE_FILE.unlink(missing_ok=True)
        except OSError:
            pass
        refreshed = enriched_path()
        _hc_link_hc(refreshed)
        binary = _hc_binary(refreshed)
        version = _hc_version(binary, env)
        if not version:
            raise ValueError("安装完成但 herdr-core 无法运行，请查看安装日志")
        return {"installed": True, "binary_path": binary, "version": version, "reused": False}


def herdr_uninstall(payload=None):
    """Removes the herdr-core in ~/.local/bin (one a package manager owns
    stays). Its state and config stay. Refuses while terminals run, unless
    forced: stopping the core ends them."""
    force = isinstance(payload, dict) and payload.get("force") is True
    with _herdr_lifecycle_lock():
        path_str = enriched_path()
        env = _hc_env(path_str)
        binary = _hc_binary(path_str)
        if binary and _hc_core_running():
            live = _hc_live_count(binary, env)
            if live and not force:
                raise ValueError(f"Herdr 里还有 {live} 个终端在运行，卸载会结束它们")
            _hc_json(binary, ["server", "stop", "--json"], env, timeout=10)
        local = HOME / ".local/bin/herdr-core"
        link = HOME / ".local/bin/hc"
        try:
            if link.is_symlink() and (os.readlink(link) == "herdr-core" or link.resolve() == local.resolve()):
                link.unlink()
        except OSError:
            pass
        try:
            if local.is_file() or local.is_symlink():
                local.unlink()
        except OSError:
            pass
        try:
            PROBE_CACHE_FILE.unlink(missing_ok=True)
        except OSError:
            pass
    return {"installed": bool(_hc_binary(enriched_path())), "server_running": False}


def herdr_start(payload=None):
    with _herdr_lifecycle_lock():
        path_str = enriched_path()
        binary = _hc_binary(path_str)
        if not binary:
            raise ValueError("未找到 herdr-core，请先安装 Herdr")
        if not _hc_core_running():
            rc, data = _hc_json(binary, ["server", "start", "--json"], _hc_env(path_str), timeout=10)
            if rc != 0:
                raise ValueError(_hc_error(data, "Herdr 服务没有启动"))
    return herdr_status({})


def herdr_stop(payload=None):
    """Stopping the core ends every program in it, so it refuses while
    terminals run unless forced."""
    force = isinstance(payload, dict) and payload.get("force") is True
    with _herdr_lifecycle_lock():
        path_str = enriched_path()
        binary = _hc_binary(path_str)
        if binary and _hc_core_running():
            env = _hc_env(path_str)
            live = _hc_live_count(binary, env)
            if live and not force:
                raise ValueError(f"Herdr 里还有 {live} 个终端在运行，停止服务会结束它们")
            _hc_json(binary, ["server", "stop", "--json"], env, timeout=10)
    return herdr_status({})


def herdr_restart(payload=None):
    """hc server restart keeps programs running; viewers re-attach."""
    with _herdr_lifecycle_lock():
        path_str = enriched_path()
        binary = _hc_binary(path_str)
        if not binary:
            raise ValueError("未找到 herdr-core，请先安装 Herdr")
        action = "restart" if _hc_core_running() else "start"
        rc, data = _hc_json(binary, ["server", action, "--json"], _hc_env(path_str), timeout=20)
        if rc != 0:
            raise ValueError(_hc_error(data, "Herdr 服务没有重启"))
    return herdr_status({})


def herdr_list(payload=None):
    """This machine's live hc terminals, for Cage's terminal list. Never
    starts the core; at most HERDR_LIST_MAX entries; strings cleaned."""
    path_str = enriched_path()
    binary = _hc_binary(path_str)
    if not binary or not _hc_core_running():
        return {"running": False, "terminals": []}
    results = _hc_rpc(binary, _hc_env(path_str), [("terminal.list", {})])
    listed = results.get(1)
    if not isinstance(listed, dict) or not isinstance(listed.get("terminals"), list):
        raise ValueError("无法读取 Herdr 终端列表")
    out = []
    for item in listed["terminals"]:
        if not isinstance(item, dict) or item.get("state") != "live":
            continue
        address = item.get("address")
        # Another machine's terminal (machine/t7) is never listed or opened.
        if not isinstance(address, str) or not HC_ADDRESS_RE.match(address):
            continue
        name = item.get("name")
        labels = item.get("labels") if isinstance(item.get("labels"), dict) else {}
        argv = item.get("argv") if isinstance(item.get("argv"), list) else []
        program = argv[0] if argv and isinstance(argv[0], str) else ""
        created = item.get("created_ms")
        out.append({
            "address": address,
            "name": name if isinstance(name, str) and HC_NAME_RE.match(name) else "",
            "title": _hc_text(item.get("title")),
            "program": _hc_text(program.rsplit("/", 1)[-1], 64),
            "cwd": _hc_text(item.get("cwd"), 1024),
            "created_ms": created if isinstance(created, int) and created > 0 else 0,
            "labels": {k: _hc_text(labels[k]) for k in HERDR_LABEL_KEYS if isinstance(labels.get(k), str)},
        })
        if len(out) >= HERDR_LIST_MAX:
            break
    return {"running": True, "terminals": out}


def _hc_target(value):
    target = str(value or "").strip()
    return target if HC_NAME_RE.match(target) or HC_ADDRESS_RE.match(target) else ""


def herdr_close(payload=None):
    """Ends a terminal Cage closed: kill, then rm. A core that is down is
    left down; its terminals ended with it."""
    target = _hc_target(payload.get("terminal") if isinstance(payload, dict) else "")
    if not target:
        raise ValueError("Herdr 终端名称无效")
    path_str = enriched_path()
    binary = _hc_binary(path_str)
    if not binary or not _hc_core_running():
        return {"closed": False, "running": False}
    env = _hc_env(path_str)
    rc_kill, _ = _hc_json(binary, ["kill", "--json", "--", target], env, timeout=10)
    rc_rm, _ = _hc_json(binary, ["rm", "--json", "--", target], env, timeout=5)
    return {"closed": rc_kill == 0 and rc_rm == 0, "running": True}


def herdr_legacy_status(payload=None):
    """The Herdr TUI from Cage 0.1.21: whether it runs and how many panes it
    holds. Every pane holds at least a shell, so any pane is a running
    program; a count it cannot read counts as running programs."""
    binary = _herdr_legacy_binary()
    if not binary:
        return {"installed": False, "server_running": False, "panes": 0, "busy": False}
    path_str = enriched_path()
    env = os.environ.copy()
    env["PATH"] = path_str
    running = _herdr_server_running(binary, env, timeout=2.0)
    panes = 0
    busy = False
    if running:
        resp = _herdr_run_json(binary, ["pane", "list"], env, timeout=2.0)
        result = resp.get("result") if isinstance(resp, dict) else None
        items = result.get("panes") if isinstance(result, dict) else None
        if isinstance(items, list):
            panes = len(items)
            busy = panes > 0
        else:
            busy = True
    return {
        "installed": True,
        "binary_path": binary,
        "version": _read_cached_cli_version(binary, path_str),
        "server_running": running,
        "panes": panes,
        "busy": busy,
    }


def herdr_legacy_uninstall(payload=None):
    """Stops the Herdr TUI server when it holds no panes (or when forced) and
    removes ~/.local/bin/herdr; ~/.config/herdr stays."""
    force = isinstance(payload, dict) and payload.get("force") is True
    status = herdr_legacy_status({})
    if not status["installed"]:
        return status
    if status["busy"] and not force:
        raise ValueError(f"旧版 Herdr 里还有 {status['panes'] or '一些'} 个终端在运行，卸载会结束它们")
    binary = status["binary_path"]
    if status["server_running"]:
        env = os.environ.copy()
        env["PATH"] = enriched_path()
        try:
            subprocess.run([binary, "server", "stop"], env=env, stdin=subprocess.DEVNULL,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5, check=False)
        except (OSError, subprocess.SubprocessError):
            pass
    try:
        Path(binary).unlink(missing_ok=True)
    except OSError:
        pass
    try:
        PROBE_CACHE_FILE.unlink(missing_ok=True)
    except OSError:
        pass
    return herdr_legacy_status({})


def _herdr_run_json(binary, args, env, timeout=2.0):
    try:
        res = subprocess.run(
            [binary, *args],
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if res.returncode != 0:
        return None
    out = (res.stdout or "").strip()
    if not out:
        return None
    try:
        data = json.loads(out)
        return data if isinstance(data, dict) else None
    except ValueError:
        for line in reversed(out.splitlines()):
            line = line.strip()
            if line.startswith("{") and line.endswith("}"):
                try:
                    data = json.loads(line)
                    if isinstance(data, dict):
                        return data
                except ValueError:
                    continue
        return None


def _herdr_server_running(binary, env, timeout=2.0):
    data = _herdr_run_json(binary, ["status", "server", "--json"], env, timeout=timeout)
    if not isinstance(data, dict):
        return False
    if data.get("running") is True or str(data.get("status") or "").lower() == "running":
        return True
    result = data.get("result")
    if isinstance(result, dict):
        if result.get("running") is True or str(result.get("status") or "").lower() == "running":
            return True
    return False


def _herdr_fallback_exec(shell, workspace, inner_cmd):
    target = workspace if workspace and Path(workspace).is_dir() else str(HOME)
    try:
        os.chdir(target)
    except OSError:
        pass
    if not (shell and os.path.isfile(shell) and os.access(shell, os.X_OK)):
        if sys.platform == "darwin" and os.path.isfile("/bin/zsh"):
            shell = "/bin/zsh"
        elif os.path.isfile("/bin/bash"):
            shell = "/bin/bash"
        else:
            shell = "/bin/sh"
    if inner_cmd:
        os.execv(shell, [shell, "-c", inner_cmd])
    os.execv(shell, [shell, "-l"])


# The viewer: hc attach, again whenever it returns while the terminal still
# lives (ctrl+\ detaches hc attach; a core restart ends its streams), so a
# Cage tab never turns into a dead or detached screen. It stops when the core
# is gone (no attach restarts it), the terminal finished, or attach keeps
# failing at once.
HERDR_ATTACH_LOOP = r'''hc=$1 t=$2 h=$3 sock=$4 n=0 since=$(date +%s)
while :; do
  "$hc" attach --history "$h" -- "$t"
  if [ -n "$sock" ] && [ ! -S "$sock" ]; then exit 0; fi
  case $("$hc" get --json -- "$t" 2>/dev/null) in
    *'"state":"live"'* | *'"state": "live"'*) ;;
    *) exit 0 ;;
  esac
  now=$(date +%s)
  if [ $((now - since)) -ge 10 ]; then since=$now n=0; fi
  n=$((n + 1))
  if [ "$n" -gt 5 ]; then exit 0; fi
done
'''


def _herdr_exec_viewer(binary, target, env, workspace, history):
    try:
        os.chdir(workspace)
    except OSError:
        pass
    os.execve("/bin/sh", ["/bin/sh", "-c", HERDR_ATTACH_LOOP, "cage-herdr-attach",
                          binary, target, str(history), _hc_core_socket()], env)


def _herdr_label(value, limit=64):
    return _hc_text(value, limit).replace("\n", " ")


# What hc runs first in a Cage terminal (herdr_attach). hc starts programs
# in the core's environment, which another hc client or a user service may
# have started with a different one: this takes the environment, folder and
# command herdr-attach saw from their private file, deletes it, keeps only
# hc's own HC_* variables for this terminal, and becomes the command the
# plain path would run. Small on purpose: python -I -S runs it before the
# terminal's first byte (about 10 ms, against about 50 ms for this file).
HERDR_EXEC_CODE = r"""
import json, os, stat, sys
path = sys.argv[1]
name = os.path.basename(path)
ours = name.startswith("run-") and name.endswith(".json")
spec = None
try:
    info = os.lstat(path)
    if ours and stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid():
        with open(path, encoding="utf-8") as f:
            spec = json.load(f)
except (OSError, ValueError):
    pass
finally:
    if ours:
        try:
            os.unlink(path)
        except OSError:
            pass
if not isinstance(spec, dict):
    sys.exit("Super Cage: 读不到这个终端的启动信息。")
env = {str(k): str(v) for k, v in (spec.get("env") or {}).items() if not str(k).startswith("HC_")}
env.update((k, v) for k, v in os.environ.items() if k.startswith("HC_"))
try:
    os.chdir(spec.get("cwd") or env.get("HOME") or "/")
except OSError:
    pass
shell = spec.get("shell") or "/bin/sh"
command = spec.get("command") or ""
argv = [shell, "-c", command] if command else [shell, "-l"]
try:
    os.execve(shell, argv, env)
except OSError:
    os.execve("/bin/sh", ["/bin/sh"] + argv[1:], env)
"""


def herdr_attach():
    """The interactive entry of a Cage terminal backed by herdr-core.

    CAGE_HERDR_MODE create (default): start the terminal CAGE_HERDR_NAME
    (unless it already lives) with the inner command, then view it. attach:
    view the live terminal CAGE_HERDR_TARGET (a name or tN), never starting a
    core. picker: hc's own picker. A create that fails runs the inner command
    in a plain shell instead, never a dead terminal."""
    workspace_raw = os.environ.pop("CAGE_HERDR_WORKSPACE", "").strip()
    workspace = workspace_raw if workspace_raw and Path(workspace_raw).is_dir() else str(HOME)
    project_label = os.environ.pop("CAGE_HERDR_PROJECT_LABEL", "").strip() or Path(workspace).name or "workspace"
    tab_label = os.environ.pop("CAGE_HERDR_TAB_LABEL", "").strip()
    kind = os.environ.pop("CAGE_HERDR_KIND", "").strip().lower() or "shell"
    inner_cmd = os.environ.pop("CAGE_HERDR_INNER_CMD", "").strip()
    default_shell = "/bin/zsh" if sys.platform == "darwin" else "/bin/bash"
    shell = os.environ.pop("CAGE_HERDR_SHELL", "").strip() or default_shell
    mode = os.environ.pop("CAGE_HERDR_MODE", "").strip().lower() or ("picker" if kind == "herdr" else "create")
    name = os.environ.pop("CAGE_HERDR_NAME", "").strip()
    target = _hc_target(os.environ.pop("CAGE_HERDR_TARGET", ""))
    try:
        history = max(0, min(int(os.environ.pop("CAGE_HERDR_HISTORY", "") or HERDR_HISTORY_LINES), 100000))
    except ValueError:
        history = HERDR_HISTORY_LINES

    path_str = enriched_path()
    binary = _hc_binary(path_str)
    env = _hc_env(path_str)
    if mode == "attach":
        if not binary or not target or not _hc_core_running():
            print("这个 Herdr 终端已经结束。", flush=True)
            return 0
        _herdr_exec_viewer(binary, target, env, workspace, history)
    nested = os.environ.get("HERDR_ENV") == "1" or bool(os.environ.get("HC_TERMINAL_ID"))
    if nested or not binary:
        _herdr_fallback_exec(shell, workspace, inner_cmd)
    if mode == "picker":
        try:
            os.chdir(workspace)
        except OSError:
            pass
        os.execve(binary, [binary], env)
    if not HC_NAME_RE.match(name):
        _herdr_fallback_exec(shell, workspace, inner_cmd)

    original_umask = os.umask(0o077)
    HERDR_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    now = time.time()
    try:
        for entry in HERDR_ROOT.iterdir():
            if entry.name.startswith("run-") and entry.name.endswith((".sh", ".json")) and entry.is_file():
                try:
                    if now - entry.stat().st_mtime > 300:
                        entry.unlink(missing_ok=True)
                except OSError:
                    pass
    except OSError:
        pass

    ok = False
    run_script = None
    # One lock around starting the core and the terminal: concurrent opens
    # never race the core's start or create the same name twice.
    with _herdr_lifecycle_lock():
        if not _hc_core_running():
            _hc_json(binary, ["server", "start", "--json"], env, timeout=10)
        rc, existing = _hc_json(binary, ["get", "--json", "--", name], env, timeout=3)
        if rc == 0 and existing and existing.get("state") == "live":
            ok = True  # the same Cage terminal, seen again: view it
        else:
            if rc == 0 and existing:
                _hc_json(binary, ["rm", "--json", "--", name], env, timeout=3)
            # The program runs in the core's environment, which another hc
            # client or a user service may have started: HERDR_EXEC_CODE gives
            # it Cage's instead, from a private file it deletes (never argv).
            run_script = HERDR_ROOT / f"run-{os.getpid()}-{secrets.token_hex(6)}.json"
            body = json.dumps({"env": {k: v for k, v in os.environ.items() if not k.startswith("HC_")},
                               "cwd": workspace, "shell": shell, "command": inner_cmd})
            args = ["run", "--bg", "--json", "--name", name, "--no-parent", "--no-notify", "--cwd", workspace,
                    "--label", "cage=1", "--label", f"cage.project={_herdr_label(project_label)}",
                    "--label", f"cage.kind={_herdr_label(kind, 32)}"]
            if tab_label:
                args += ["--label", f"cage.title={_herdr_label(tab_label)}"]
            try:
                atomic_write_text(run_script, body, mode=0o600)
                runner = [sys.executable or "python3", "-I", "-S", "-c", HERDR_EXEC_CODE, str(run_script)]
                rc, created = _hc_json(binary, [*args, "--", *runner], env, timeout=15)
                ok = rc == 0 and bool(created) and created.get("state") == "live"
            except OSError:
                ok = False
            if not ok:
                # It may have started after all (a slow core): never leave a
                # second copy running beside the fallback.
                _hc_json(binary, ["kill", "--json", "--", name], env, timeout=5)
                _hc_json(binary, ["rm", "--json", "--", name], env, timeout=5)
    os.umask(original_umask)
    if not ok:
        if run_script is not None:
            run_script.unlink(missing_ok=True)
        _herdr_fallback_exec(shell, workspace, inner_cmd)
    _herdr_exec_viewer(binary, name, env, workspace, history)


# ---------------------------------------------------------------------------
# Managed Orca Server (stablyai/orca orcad server-only) Guest Lifecycle
# ---------------------------------------------------------------------------

@contextmanager
def _orca_lifecycle_lock():
    """Serialize Orca Server install, uninstall, start, stop, pair and identity operations."""
    os.umask(0o077)
    ORCA_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(str(ORCA_LOCK_FILE), os.O_RDWR | os.O_CREAT, 0o600)
    os.chmod(ORCA_LOCK_FILE, 0o600)
    with os.fdopen(fd, "a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def _validate_orca_port(port):
    if isinstance(port, bool) or not isinstance(port, int) or not (1024 <= port <= 65535):
        raise ValueError("Orca Server 端口须在 1024-65535 之间")
    return port


def _requested_orca_port(payload, default=ORCA_DEFAULT_PORT):
    raw = payload.get("port") if isinstance(payload, dict) else None
    if raw is None or raw == "" or raw == 0:
        return _validate_orca_port(default)
    if isinstance(raw, bool):
        raise ValueError("Orca Server 端口须在 1024-65535 之间")
    try:
        port = int(raw)
    except (TypeError, ValueError) as exc:
        raise ValueError("Orca Server 端口须在 1024-65535 之间") from exc
    return _validate_orca_port(port)


def _orca_find_binary(path_str=None):
    """Locate Orca Server binary without ever colliding with Linux /usr/bin/orca (GNOME screen reader)."""
    current_bin = ORCA_CURRENT_LINK / "bin/orca-ide"
    if current_bin.is_file() and os.access(current_bin, os.X_OK):
        return str(current_bin)
    local_bin = HOME / ".local/bin/orca-ide"
    if local_bin.is_file() and os.access(local_bin, os.X_OK):
        return str(local_bin)
    found = shutil.which("orca-ide", path=path_str or enriched_path())
    if found:
        return found
    if sys.platform == "darwin":
        for app in (Path("/Applications/Orca.app"), HOME / "Applications/Orca.app"):
            cand = app / "Contents/Resources/bin/orca"
            if cand.is_file() and os.access(cand, os.X_OK):
                return str(cand)
    return ""


def _orca_read_version(binary, path_str=None):
    if not binary:
        return ""
    # Fast path (<0.1ms): read VERSION file bundled in official orca-server tarball.
    try:
        resolved = Path(binary).resolve()
        for parent in (resolved.parent.parent, ORCA_CURRENT_LINK):
            vf = parent / "VERSION"
            if vf.is_file():
                text = vf.read_text(encoding="utf-8", errors="ignore").strip().lstrip("v")
                if ORCA_VERSION_RE.match(text):
                    return text
            build_json = parent / "orca-local-build.json"
            if build_json.is_file():
                data = read_json(build_json)
                if isinstance(data, dict) and isinstance(data.get("version"), str):
                    v = data["version"].strip().lstrip("v")
                    if ORCA_VERSION_RE.match(v):
                        return v
    except OSError:
        pass
    return _read_cached_cli_version(binary, path_str or enriched_path())


def _orca_instance_id():
    """Persisted per-profile UUID so VM clones never share pairing identity."""
    try:
        val = ORCA_INSTANCE_ID_FILE.read_text(encoding="utf-8", errors="ignore").strip()
        if val and len(val) <= 64 and re.fullmatch(r"[A-Za-z0-9._-]+", val):
            return val
    except OSError:
        pass
    val = str(uuid.uuid4())
    atomic_write_text(ORCA_INSTANCE_ID_FILE, val + "\n", mode=0o600)
    return val


def _orca_ensure_guest_browser_wrapper():
    """Return an executable guest browser launcher for BROWSER so Orca's assembly.ts
    never routes OAuth URLs back to the host Desktop (anti-ban IP isolation)."""
    if os.path.isfile(OPEN_BIN) and os.access(OPEN_BIN, os.X_OK):
        return OPEN_BIN
    # Fallback wrapper invoking xdg-open / guest chromium, or safe no-op if headless.
    script = (
        "#!/bin/sh\n"
        "if command -v xdg-open >/dev/null 2>&1; then\n"
        '  exec xdg-open "$@"\n'
        "fi\n"
        "exit 0\n"
    )
    if not ORCA_BROWSER_WRAPPER.exists() or ORCA_BROWSER_WRAPPER.read_text(encoding="utf-8", errors="ignore") != script:
        atomic_write_text(ORCA_BROWSER_WRAPPER, script, mode=0o700)
    return str(ORCA_BROWSER_WRAPPER)


def _orca_tailscale_ip(path_str=None):
    ts = shutil.which("tailscale", path=path_str or enriched_path())
    if not ts:
        return ""
    try:
        res = subprocess.run(
            [ts, "ip", "-4"],
            env={**os.environ, "PATH": path_str or enriched_path()},
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            timeout=2.0,
        )
        if res.returncode != 0:
            return ""
        cgnat = ipaddress.IPv4Network("100.64.0.0/10")
        for line in (res.stdout or "").splitlines():
            cand = line.strip()
            if not cand:
                continue
            try:
                addr = ipaddress.IPv4Address(cand)
                if addr in cgnat:
                    return str(addr)
            except ValueError:
                continue
    except (OSError, subprocess.SubprocessError):
        pass
    return ""


def _orca_resolve_bind_ip(bind_mode, path_str=None):
    mode = str(bind_mode or "loopback").strip().lower()
    if mode == "loopback":
        return "127.0.0.1"
    if mode == "tailscale":
        ts_ip = _orca_tailscale_ip(path_str)
        if not ts_ip:
            raise ValueError("未检测到已连接的 Tailscale 100.x IPv4 地址，请先在「设置 → Tailscale」中连接私有网络，或切回仅本机回环模式")
        return ts_ip
    raise ValueError("Orca Server 绑定模式无效，仅支持 loopback 或 tailscale")


def _orca_prepare_bindings(bindings, path_str):
    """Prepare universal auto-shims and any explicit per-agent override shims for Orca.
    Returns (extra_env, shim_prefixes, orca_override_dir)."""
    magpie_mode = isinstance(bindings, dict) and any(
        isinstance(b, dict) and str(b.get("mode") or "").strip().lower() == "magpie"
        for b in bindings.values()
    )
    try:
        sync_auto_shims(
            {"ensure_shell_hook": True, "disable_all": True, "magpie_takeover": True}
            if magpie_mode
            else {"ensure_shell_hook": True}
        )
    except Exception:
        pass

    extra_env = {}
    shim_prefixes = []
    override_written = False
    q_takeover = shlex.quote(str(MAGPIE_TAKEOVER_FILE))

    # Rebuild ORCA_SHIMS_DIR cleanly on each start so stale explicit pins never linger.
    if ORCA_SHIMS_DIR.exists():
        shutil.rmtree(ORCA_SHIMS_DIR, ignore_errors=True)

    def write_override_shim(name, content):
        nonlocal override_written
        ORCA_SHIMS_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
        atomic_write_text(ORCA_SHIMS_DIR / name, content, mode=0o755)
        override_written = True

    if isinstance(bindings, dict):
        for agent_id, binding in bindings.items():
            if not isinstance(binding, dict):
                continue
            mode = str(binding.get("mode") or "").strip().lower()
            guest_id = str(binding.get("guest_id") or "default").strip() or "default"
            if mode in ("auto", "magpie"):
                # Dynamic live scheduling via ~/.local/share/cage/auto-shims (empty under Magpie takeover).
                continue
            if mode == "default":
                if agent_id == "claude":
                    extra_env["CAGE_CLAUDE_ACCOUNT_ID"] = "default"
                elif agent_id == "codex":
                    extra_env["CAGE_CODEX_ACCOUNT_ID"] = "default"
                    extra_env[_agent_account_env_var("codex")] = "default"
                elif CLI_ID_RE.match(str(agent_id)):
                    extra_env[_agent_account_env_var(str(agent_id))] = "default"
                continue
            if agent_id == "claude":
                if guest_id != "default" and not ACCOUNT_ID_RE.match(guest_id):
                    raise ValueError("invalid claude guest_id")
                helper = _claude_account_helper()
                subject = str(binding.get("subject") or "").strip()
                if subject and (len(subject) > 128 or any(ch in subject for ch in ("\x00", "\r", "\n"))):
                    raise ValueError("invalid claude subject")
                extra_env["CAGE_CLAUDE_ACCOUNT_ID"] = guest_id
                if subject:
                    extra_env["CAGE_CLAUDE_SUBJECT"] = subject
                if guest_id != "default":
                    (HOME / ".local/share/cage/claude-accounts" / guest_id / "config").mkdir(parents=True, exist_ok=True, mode=0o700)
                    write_override_shim(
                        "claude",
                        "#!/bin/sh\n"
                        f"if [ -f {q_takeover} ]; then\n"
                        + _posix_strip_and_exec("claude")
                        + "fi\n"
                        + f"export CAGE_CLAUDE_ACCOUNT_ID={shlex.quote(guest_id)}\n"
                        + (f"export CAGE_CLAUDE_SUBJECT={shlex.quote(subject)}\n" if subject else "")
                        + f'exec {shlex.quote(helper)} run {shlex.quote(guest_id)} "$@"\n',
                    )
            elif agent_id == "codex" and binding.get("codex_native"):
                if guest_id != "default" and not ACCOUNT_ID_RE.match(guest_id):
                    raise ValueError("invalid codex guest_id")
                helper = _codex_account_helper()
                subject = str(binding.get("subject") or "").strip()
                if subject and (len(subject) > 128 or any(ch in subject for ch in ("\x00", "\r", "\n"))):
                    raise ValueError("invalid codex subject")
                extra_env["CAGE_CODEX_ACCOUNT_ID"] = guest_id
                if subject:
                    extra_env["CAGE_CODEX_SUBJECT"] = subject
                if guest_id != "default":
                    (HOME / ".local/share/cage/codex-accounts" / guest_id / "home").mkdir(parents=True, exist_ok=True, mode=0o700)
                    write_override_shim(
                        "codex",
                        "#!/bin/sh\n"
                        f"if [ -f {q_takeover} ]; then\n"
                        + _posix_strip_and_exec("codex")
                        + "fi\n"
                        + f"export CAGE_CODEX_ACCOUNT_ID={shlex.quote(guest_id)}\n"
                        + (f"export CAGE_CODEX_SUBJECT={shlex.quote(subject)}\n" if subject else "")
                        + f'exec {shlex.quote(helper)} run {shlex.quote(guest_id)} "$@"\n',
                    )
            else:
                spec = binding.get("spec")
                if isinstance(spec, dict) and spec.get("id"):
                    prep = prepare_session({"spec": spec, "account_id": guest_id})
                    shim_prefixes.append(prep["shim_dir"])
                    extra_env[_agent_account_env_var(spec["id"])] = guest_id
                    if guest_id != "default":
                        for bin_name in [spec.get("binary", ""), *(spec.get("fallback_binaries") or [])]:
                            if not bin_name or "/" in bin_name:
                                continue
                            target_shim = Path(prep["shim_dir"]) / bin_name
                            if target_shim.is_file():
                                write_override_shim(
                                    bin_name,
                                    "#!/bin/sh\n"
                                    f"if [ -f {q_takeover} ]; then\n"
                                    + _posix_strip_and_exec(bin_name)
                                    + "fi\n"
                                    + f"export {_agent_account_env_var(spec['id'])}={shlex.quote(guest_id)}\n"
                                    f'exec {shlex.quote(str(target_shim))} "$@"\n',
                                )

    if override_written:
        shim_prefixes.insert(0, str(ORCA_SHIMS_DIR))
        extra_env["CAGE_ORCA_SHIM_DIR"] = str(ORCA_SHIMS_DIR)
    if AUTO_SHIMS_DIR.is_dir():
        shim_prefixes.append(str(AUTO_SHIMS_DIR))
    return extra_env, shim_prefixes, (str(ORCA_SHIMS_DIR) if override_written else "")


def _orca_base_env(path_str, extra_env=None, shim_prefixes=None, terminal_memory_max=""):
    ORCA_DATA_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
    env = os.environ.copy()
    for k in list(env):
        if k.startswith("CAGE_CLI_ACCOUNT_"):
            env.pop(k, None)
    env.pop("CAGE_CLI_ACCOUNT_ID", None)
    env.pop("CAGE_CLAUDE_ACCOUNT_ID", None)
    env.pop("CAGE_CLAUDE_SUBJECT", None)
    env.pop("CAGE_CODEX_ACCOUNT_ID", None)
    env.pop("CAGE_CODEX_SUBJECT", None)
    env.pop("CAGE_ORCA_SHIM_DIR", None)
    prefixes = []
    for p in (shim_prefixes or []):
        if p and p not in prefixes:
            prefixes.append(p)
    if str(AUTO_SHIMS_DIR) not in prefixes and AUTO_SHIMS_DIR.is_dir():
        prefixes.append(str(AUTO_SHIMS_DIR))
    env["PATH"] = os.pathsep.join([*prefixes, path_str]) if prefixes else path_str
    env["ORCA_USER_DATA"] = str(ORCA_DATA_DIR)
    env["ORCA_INSTANCE_ID"] = _orca_instance_id()
    env["DO_NOT_TRACK"] = "1"
    env["ORCA_TELEMETRY_DISABLED"] = "1"
    env["ORCA_BACKGROUND_LAUNCH"] = "1"
    env["BROWSER"] = _orca_ensure_guest_browser_wrapper()
    if sys.platform == "darwin":
        env["ORCA_SERVE_RUNTIME"] = "orcad"
    mem = str(terminal_memory_max or "").strip().upper()
    if mem and ORCA_MEM_LIMIT_RE.match(mem):
        env["ORCA_TERMINAL_MEMORY_MAX"] = mem
    if extra_env:
        env.update(extra_env)
    return env


def orca_supervise(cfg):
    """Detached supervisor for `orca-ide serve` with live redaction and 64 KiB log cap."""
    os.umask(0o077)
    cmd = cfg["cmd"]
    cwd = cfg.get("cwd") or str(HOME)
    log_file = Path(cfg.get("log_file") or ORCA_LOG_FILE)
    handshake_file = Path(cfg["handshake_file"])

    if hasattr(signal, "SIGHUP"):
        signal.signal(signal.SIGHUP, signal.SIG_IGN)

    logger = _BoundedRedactedLog(log_file, max_bytes=ORCA_LOG_MAX_BYTES)
    child = None
    term_state = {"requested": False, "sent": False}

    def forward_term(_signum, _frame):
        term_state["requested"] = True
        if child is not None and not term_state["sent"]:
            term_state["sent"] = True
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except OSError:
                try:
                    child.terminate()
                except OSError:
                    pass

    signal.signal(signal.SIGTERM, forward_term)
    signal.signal(signal.SIGINT, forward_term)

    try:
        child = subprocess.Popen(
            cmd,
            cwd=cwd,
            env=os.environ.copy(),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
    except OSError as exc:
        logger.write_raw_line(f"Failed to start orca-ide: {exc}")
        logger.close()
        try:
            atomic_write_json(handshake_file, {"error": str(exc), "supervisor_pid": os.getpid()}, mode=0o600)
        except OSError:
            pass
        return 1

    if term_state["requested"] and not term_state["sent"]:
        forward_term(signal.SIGTERM, None)

    try:
        atomic_write_json(
            handshake_file,
            {"child_pid": child.pid, "supervisor_pid": os.getpid()},
            mode=0o600,
        )
    except OSError:
        pass

    try:
        assert child.stdout is not None
        while True:
            raw = child.stdout.readline(65536)
            if not raw:
                break
            line = raw.decode("utf-8", errors="replace")
            logger.write_raw_line(line)
    finally:
        if child.stdout is not None:
            try:
                child.stdout.close()
            except OSError:
                pass
        rc = child.wait()
        logger.close()
    return rc if isinstance(rc, int) else 0


def orca_status(payload=None):
    payload = payload if isinstance(payload, dict) else {}
    requested_port = _requested_orca_port(payload, default=ORCA_DEFAULT_PORT)
    path_str = enriched_path()
    binary = _orca_find_binary(path_str)
    version = _orca_read_version(binary, path_str) if binary else ""
    state = read_json(ORCA_STATE_FILE)
    running = False
    pid = 0
    bind_ip = "127.0.0.1"
    bind_mode = "loopback"
    if isinstance(state, dict):
        cand_pid = state.get("pid")
        cand_port = state.get("port")
        cand_ident = str(state.get("pid_identity") or "")
        cand_bind_ip = str(state.get("bind_ip") or "127.0.0.1")
        cand_bind_mode = str(state.get("bind_mode") or "loopback")
        if (
            not isinstance(cand_pid, bool)
            and isinstance(cand_pid, int)
            and cand_pid > 0
            and not isinstance(cand_port, bool)
            and isinstance(cand_port, int)
            and cand_port == requested_port
            and _pid_matches(cand_pid, cand_ident)
            and _http_responsive(cand_bind_ip, requested_port, timeout=0.45)
        ):
            running = True
            pid = cand_pid
            bind_ip = cand_bind_ip
            bind_mode = cand_bind_mode
    has_rollback = False
    try:
        if ORCA_PREVIOUS_LINK.is_symlink() and (ORCA_PREVIOUS_LINK / "bin/orca-ide").is_file():
            has_rollback = True
    except OSError:
        pass
    return {
        "installed": bool(binary),
        "binary_path": binary,
        "version": version,
        "running": running,
        "pid": pid if running else 0,
        "port": requested_port,
        "bind_ip": bind_ip,
        "bind_mode": bind_mode,
        "has_rollback": has_rollback,
        "tailscale_ip": _orca_tailscale_ip(path_str),
        "platform": sys.platform,
    }


def _orca_fetch(curl, url, dest, env, max_time=300, headers=()):
    cmd = [
        curl, "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2",
        "-fsSL", "--connect-timeout", "20", "--max-time", str(max_time),
    ]
    for h in headers:
        cmd.extend(["-H", h])
    cmd.extend(["-o", str(dest), url])
    res = subprocess.run(cmd, env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True)
    if res.returncode != 0:
        err_tail = (res.stderr or "").strip().splitlines()[-1:] or [""]
        raise ValueError(f"下载失败（curl 退出码 {res.returncode}）：{url} {err_tail[0]}".strip())


def _orca_resolve_latest_version(curl, env, tmp_dir):
    meta_file = Path(tmp_dir) / "latest.json"
    _orca_fetch(
        curl, ORCA_RELEASES_API, meta_file, env, max_time=30,
        headers=("Accept: application/vnd.github+json", "User-Agent: super-cage"),
    )
    data = read_json(meta_file)
    tag = str(data.get("tag_name") or "").strip() if isinstance(data, dict) else ""
    ver = tag.lstrip("v")
    if not ORCA_VERSION_RE.match(ver):
        raise ValueError("无法解析 Orca Server 最新官方版本号")
    return ver


def _orca_safe_extract_tar(archive_path, dest_dir):
    """Safely extract an official orca-server tarball, blocking path traversal,
    escaping symlinks, device nodes, and setuid/setgid bits."""
    dest = Path(dest_dir).resolve()
    dest.mkdir(parents=True, exist_ok=True, mode=0o700)
    total_size = 0
    max_total = 1024 << 20  # 1 GiB upper bound
    with tarfile.open(archive_path, "r:gz") as tar:
        members = tar.getmembers()
        if len(members) > 50000:
            raise ValueError("安装包内文件数量超出安全上限")
        for m in members:
            name = m.name
            if not name or name.startswith("/") or "\x00" in name:
                raise ValueError(f"安装包包含非法绝对路径：{name}")
            parts = Path(name).parts
            if ".." in parts:
                raise ValueError(f"安装包包含路径穿越：{name}")
            target = (dest / name).resolve()
            if target != dest and dest not in target.parents:
                raise ValueError(f"安装包目标路径逃逸：{name}")
            if m.isdev() or m.isfifo():
                raise ValueError(f"安装包包含非法设备文件：{name}")
            if m.issym():
                if not m.linkname or m.linkname.startswith("/"):
                    raise ValueError(f"安装包包含非法绝对符号链接：{name} -> {m.linkname}")
                link_target = (target.parent / m.linkname).resolve()
                if link_target != dest and dest not in link_target.parents:
                    raise ValueError(f"安装包包含越界符号链接：{name} -> {m.linkname}")
            if m.islnk():
                if not m.linkname or m.linkname.startswith("/"):
                    raise ValueError(f"安装包包含非法硬链接：{name}")
                link_target = (dest / m.linkname).resolve()
                if link_target != dest and dest not in link_target.parents:
                    raise ValueError(f"安装包包含越界硬链接：{name}")
            if m.isfile():
                total_size += max(0, m.size)
                if total_size > max_total:
                    raise ValueError("安装包解压体积超出安全上限")
        for m in members:
            m.mode = m.mode & ~0o7000
            if m.isdir():
                m.mode = (m.mode & 0o777) or 0o755
            target_path = dest / m.name
            if m.isdir():
                target_path.mkdir(parents=True, exist_ok=True)
                os.chmod(target_path, m.mode)
            elif m.isfile():
                target_path.parent.mkdir(parents=True, exist_ok=True)
                src = tar.extractfile(m)
                if src is not None:
                    with src, open(target_path, "wb") as out:
                        shutil.copyfileobj(src, out)
                    os.chmod(target_path, m.mode & 0o777)
            elif m.issym():
                target_path.parent.mkdir(parents=True, exist_ok=True)
                if target_path.exists() or target_path.is_symlink():
                    target_path.unlink()
                target_path.symlink_to(m.linkname)
            elif m.islnk():
                target_path.parent.mkdir(parents=True, exist_ok=True)
                if target_path.exists() or target_path.is_symlink():
                    target_path.unlink()
                os.link(dest / m.linkname, target_path)


def _atomic_symlink(link_path, target):
    link_path = Path(link_path)
    link_path.parent.mkdir(parents=True, exist_ok=True)
    tmp_link = link_path.parent / f".{link_path.name}.tmp.{os.getpid()}.{time.time_ns()}"
    try:
        tmp_link.symlink_to(target)
        os.replace(tmp_link, link_path)
    finally:
        if tmp_link.is_symlink() or tmp_link.exists():
            try:
                tmp_link.unlink()
            except OSError:
                pass


def orca_install(payload=None):
    """Atomically install the official Orca Server (orcad server-only) release."""
    payload = payload if isinstance(payload, dict) else {}
    requested_version = str(payload.get("version") or "").strip().lstrip("v")
    if requested_version.lower() == "latest":
        requested_version = ""
    if requested_version and not ORCA_VERSION_RE.match(requested_version):
        raise ValueError("指定的 Orca Server 版本号格式无效")

    with _orca_lifecycle_lock():
        path_str = enriched_path()
        if sys.platform == "darwin":
            existing = _orca_find_binary(path_str)
            if existing:
                ver = _orca_read_version(existing, path_str)
                print(f"Detected Orca CLI ({ver or 'ready'}) at {existing}; reusing it.", flush=True)
                return {"installed": True, "binary_path": existing, "version": ver, "reused": True}
            raise ValueError("macOS 客机请先在客机内安装官方 Orca.app（/Applications/Orca.app），安装后 Super Cage 将自动托管其服务端运行时")

        if not sys.platform.startswith("linux"):
            raise ValueError("Orca Server 托管安装目前仅支持 Linux 与 macOS 环境")

        machine = os.uname().machine.lower()
        arch = {"x86_64": "x64", "amd64": "x64", "aarch64": "arm64", "arm64": "arm64"}.get(machine)
        if not arch:
            raise ValueError(f"Orca Server 官方服务端暂无适用于 {machine} 架构的构建")

        curl = shutil.which("curl", path=path_str)
        if not curl:
            raise ValueError("环境中未找到 curl，请先同步环境工具")

        env = os.environ.copy()
        env["PATH"] = path_str
        ORCA_RELEASES_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
        (HOME / ".local/bin").mkdir(parents=True, exist_ok=True)

        archive_name = f"orca-server-linux-{arch}.tar.gz"
        with tempfile.TemporaryDirectory(prefix="cage-orca-install-") as tmp:
            version = requested_version
            if not version:
                print("Resolving latest official Orca Server release...", flush=True)
                version = _orca_resolve_latest_version(curl, env, tmp)

            sums_file = Path(tmp) / "orca-server-sha256sums.txt"
            sums_url = f"{ORCA_RELEASE_DL_BASE}/v{version}/orca-server-sha256sums.txt"
            print(f"Downloading Orca Server v{version} checksums...", flush=True)
            try:
                _orca_fetch(curl, sums_url, sums_file, env, max_time=60)
            except ValueError:
                if requested_version:
                    print(f"Release v{version} not found on GitHub; falling back to latest release...", flush=True)
                    version = _orca_resolve_latest_version(curl, env, tmp)
                    sums_url = f"{ORCA_RELEASE_DL_BASE}/v{version}/orca-server-sha256sums.txt"
                    _orca_fetch(curl, sums_url, sums_file, env, max_time=60)
                else:
                    raise

            want_sha = ""
            for line in sums_file.read_text(encoding="utf-8", errors="ignore").splitlines():
                parts = line.strip().split()
                if len(parts) >= 2 and ORCA_SHA256_RE.match(parts[0].lower()):
                    fname = os.path.basename(parts[1].lstrip("*"))
                    if fname == archive_name:
                        want_sha = parts[0].lower()
                        break
            if not want_sha:
                raise ValueError(f"官方校验清单中未找到 {archive_name} 的 SHA-256 记录")

            archive_file = Path(tmp) / archive_name
            archive_url = f"{ORCA_RELEASE_DL_BASE}/v{version}/{archive_name}"
            print(f"Downloading official Orca Server v{version} ({arch})...", flush=True)
            _orca_fetch(curl, archive_url, archive_file, env, max_time=600)

            digest = hashlib.sha256()
            with open(archive_file, "rb") as fp:
                for chunk in iter(lambda: fp.read(1 << 20), b""):
                    digest.update(chunk)
            if digest.hexdigest().lower() != want_sha:
                raise ValueError("Orca Server 安装包 SHA-256 校验失败，已中止安装")
            print("SHA-256 verified against official release checksums.", flush=True)

            extract_root = Path(tmp) / "extracted"
            _orca_safe_extract_tar(archive_file, extract_root)

            # Locate unpacked server root containing bin/orca-ide (depth 0 or 1).
            server_src = None
            if (extract_root / "bin/orca-ide").is_file():
                server_src = extract_root
            else:
                for child in sorted(extract_root.iterdir()):
                    if child.is_dir() and (child / "bin/orca-ide").is_file():
                        server_src = child
                        break
            if server_src is None:
                raise ValueError("解压后的 Orca Server 目录缺少 bin/orca-ide 可执行入口")

            staged_dir = ORCA_RELEASES_DIR / f"{version}-{int(time.time())}"
            if staged_dir.exists():
                shutil.rmtree(staged_dir, ignore_errors=True)
            shutil.move(str(server_src), str(staged_dir))
            staged_bin = staged_dir / "bin/orca-ide"
            os.chmod(staged_bin, 0o755)

            # Smoke test under clean environment before flipping current symlink.
            clean_env = {"PATH": "/usr/bin:/bin", "HOME": str(HOME), "DO_NOT_TRACK": "1", "ORCA_TELEMETRY_DISABLED": "1"}
            try:
                smoke = subprocess.run(
                    [str(staged_bin), "--version"],
                    env=clean_env, stdin=subprocess.DEVNULL,
                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=15,
                )
            except (OSError, subprocess.SubprocessError) as exc:
                shutil.rmtree(staged_dir, ignore_errors=True)
                raise ValueError(f"Orca Server 自检失败：{exc}") from exc
            if smoke.returncode != 0:
                shutil.rmtree(staged_dir, ignore_errors=True)
                raise ValueError(f"Orca Server 无法在此环境运行（退出码 {smoke.returncode}）")

            prev_target = None
            try:
                if ORCA_CURRENT_LINK.is_symlink():
                    prev_target = ORCA_CURRENT_LINK.resolve()
            except OSError:
                prev_target = None

            if prev_target and prev_target.is_dir() and prev_target != staged_dir.resolve():
                _atomic_symlink(ORCA_PREVIOUS_LINK, str(prev_target))
            _atomic_symlink(ORCA_CURRENT_LINK, str(staged_dir))
            _atomic_symlink(HOME / ".local/bin/orca-ide", str(ORCA_CURRENT_LINK / "bin/orca-ide"))

            # Prune old releases beyond current and previous.
            keep = {staged_dir.resolve()}
            if prev_target and prev_target.is_dir():
                keep.add(prev_target.resolve())
            for entry in list(ORCA_RELEASES_DIR.iterdir()):
                try:
                    if entry.is_dir() and entry.resolve() not in keep:
                        shutil.rmtree(entry, ignore_errors=True)
                except OSError:
                    pass

        try:
            PROBE_CACHE_FILE.unlink(missing_ok=True)
        except OSError:
            pass

        final_bin = _orca_find_binary(enriched_path())
        final_ver = _orca_read_version(final_bin, enriched_path()) or version
        print(f"Orca Server v{final_ver} installed atomically at {final_bin}.", flush=True)
        return {"installed": True, "binary_path": final_bin, "version": final_ver, "reused": False}


def _orca_stop_locked(requested_port=None):
    state = read_json(ORCA_STATE_FILE)
    if isinstance(state, dict):
        state_port = state.get("port")
        if (
            requested_port is not None
            and not isinstance(state_port, bool)
            and isinstance(state_port, int)
            and state_port != requested_port
        ):
            return {"running": False, "skipped_port_mismatch": True}
        sup_pid = state.get("supervisor_pid")
        sup_ident = str(state.get("supervisor_identity") or "")
        pid = state.get("pid")
        pid_ident = str(state.get("pid_identity") or "")
        has_sup = not isinstance(sup_pid, bool) and isinstance(sup_pid, int) and sup_pid > 0
        has_pid = not isinstance(pid, bool) and isinstance(pid, int) and pid > 0
        if has_sup:
            sup_st = _pid_state(sup_pid, sup_ident)
            if sup_st == "unknown":
                raise ValueError(f"无法验证 Orca 守护进程 ({sup_pid}) 身份，已保留运行状态文件")
            if sup_st == "matched":
                try:
                    os.kill(sup_pid, signal.SIGTERM)
                except OSError:
                    pass
                deadline = time.monotonic() + 3.0
                while time.monotonic() < deadline:
                    s_done = _pid_state(sup_pid, sup_ident) in ("gone", "mismatch")
                    p_done = (not has_pid) or (_pid_state(pid, pid_ident) in ("gone", "mismatch"))
                    if s_done and p_done:
                        break
                    time.sleep(0.08)
        if has_pid:
            pid_st = _pid_state(pid, pid_ident)
            if pid_st == "unknown":
                raise ValueError(f"无法验证 Orca Server 进程 ({pid}) 身份，已保留运行状态文件")
            if pid_st == "matched":
                if not _stop_managed_pid(pid, expected_identity=pid_ident, wait_secs=2.0):
                    raise ValueError(f"Orca Server 进程 ({pid}) 未能终止，已保留运行状态文件")
        if has_sup:
            sup_st = _pid_state(sup_pid, sup_ident)
            if sup_st == "unknown":
                raise ValueError(f"无法验证 Orca 守护进程 ({sup_pid}) 身份，已保留运行状态文件")
            if sup_st == "matched":
                if not _stop_managed_pid(sup_pid, expected_identity=sup_ident, wait_secs=1.0):
                    raise ValueError(f"Orca 守护进程 ({sup_pid}) 未能终止，已保留运行状态文件")
    ORCA_STATE_FILE.unlink(missing_ok=True)
    return {"running": False}


def orca_stop(payload=None):
    req_port = None
    if isinstance(payload, dict) and payload.get("port") not in (None, "", 0):
        req_port = _requested_orca_port(payload)
    with _orca_lifecycle_lock():
        return _orca_stop_locked(requested_port=req_port)


def orca_uninstall(payload=None):
    payload = payload if isinstance(payload, dict) else {}
    purge_data = bool(payload.get("purge_data"))
    with _orca_lifecycle_lock():
        _orca_stop_locked()
        link = HOME / ".local/bin/orca-ide"
        try:
            if link.is_symlink():
                target = link.resolve()
                if ORCA_ROOT.resolve() in target.parents or target == ORCA_ROOT.resolve():
                    link.unlink()
        except OSError:
            pass
        for p in (ORCA_CURRENT_LINK, ORCA_PREVIOUS_LINK, ORCA_STATE_FILE, ORCA_BROWSER_WRAPPER):
            try:
                if p.is_symlink() or p.is_file():
                    p.unlink()
            except OSError:
                pass
        for d in (ORCA_RELEASES_DIR, ORCA_SHIMS_DIR):
            if d.exists():
                shutil.rmtree(d, ignore_errors=True)
        if purge_data:
            if ORCA_DATA_DIR.exists():
                shutil.rmtree(ORCA_DATA_DIR, ignore_errors=True)
            ORCA_INSTANCE_ID_FILE.unlink(missing_ok=True)
        try:
            PROBE_CACHE_FILE.unlink(missing_ok=True)
        except OSError:
            pass
    return {"installed": bool(_orca_find_binary(enriched_path())), "running": False}


def _orca_parse_json_output(stdout_text):
    text = (stdout_text or "").strip()
    if not text:
        return None
    try:
        data = json.loads(text)
        return data if isinstance(data, dict) else None
    except ValueError:
        for line in reversed(text.splitlines()):
            s = line.strip()
            if s.startswith("{") and s.endswith("}"):
                try:
                    data = json.loads(s)
                    if isinstance(data, dict):
                        return data
                except ValueError:
                    continue
    return None


def _orca_sync_repos_inner(binary, env, projects):
    if not isinstance(projects, list) or not projects:
        return {"added": [], "existing": 0}
    valid_paths = []
    seen = set()
    for raw in projects[:64]:
        p = str(raw or "").strip()
        if not p or not p.startswith("/") or "\x00" in p:
            continue
        path_obj = Path(p)
        if not path_obj.is_dir():
            continue
        rp = str(path_obj.resolve())
        if rp not in seen:
            seen.add(rp)
            valid_paths.append(rp)
    if not valid_paths:
        return {"added": [], "existing": 0}

    existing_paths = set()
    try:
        res = subprocess.run(
            [binary, "repo", "list", "--json"],
            env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=8,
        )
        data = _orca_parse_json_output(res.stdout)
        if isinstance(data, dict):
            result = data.get("result") if isinstance(data.get("result"), dict) else data
            repos = result.get("repos") or result.get("items") or []
            if isinstance(repos, list):
                for item in repos:
                    if isinstance(item, dict):
                        rp = item.get("path") or item.get("rootPath") or item.get("repoPath") or ""
                        if isinstance(rp, str) and rp:
                            try:
                                existing_paths.add(str(Path(rp).resolve()))
                            except OSError:
                                existing_paths.add(rp)
    except (OSError, subprocess.SubprocessError):
        pass

    added = []
    for rp in valid_paths:
        if rp in existing_paths:
            continue
        kind = "git" if (Path(rp) / ".git").exists() else "folder"
        try:
            res = subprocess.run(
                [binary, "repo", "add", "--path", rp, "--kind", kind, "--json"],
                env=env, stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=8,
            )
            if res.returncode == 0:
                added.append(rp)
        except (OSError, subprocess.SubprocessError):
            continue
    return {"added": added, "existing": len(existing_paths)}


def orca_sync_repos(payload=None):
    payload = payload if isinstance(payload, dict) else {}
    projects = payload.get("projects") or []
    path_str = enriched_path()
    binary = _orca_find_binary(path_str)
    if not binary:
        raise ValueError("未找到 Orca Server，请先安装")
    env = _orca_base_env(path_str)
    return _orca_sync_repos_inner(binary, env, projects)


def orca_start(payload=None, restart=False):
    payload = payload if isinstance(payload, dict) else {}
    port = _requested_orca_port(payload, default=ORCA_DEFAULT_PORT)
    bind_mode = str(payload.get("bind_mode") or "loopback").strip().lower()
    bindings = payload.get("bindings") or {}
    if not isinstance(bindings, dict):
        raise ValueError("invalid orca bindings")
    terminal_memory_max = str(payload.get("terminal_memory_max") or "").strip().upper()
    if terminal_memory_max and not ORCA_MEM_LIMIT_RE.match(terminal_memory_max):
        raise ValueError("终端内存上限格式无效（例如 2G、512M）")
    projects = payload.get("projects") or []

    bindings_hash = hashlib.sha256(
        json.dumps(
            {"bindings": bindings, "bind_mode": bind_mode, "mem": terminal_memory_max},
            sort_keys=True, ensure_ascii=False,
        ).encode("utf-8")
    ).hexdigest()[:16]

    with _orca_lifecycle_lock():
        path_str = enriched_path()
        binary = _orca_find_binary(path_str)
        if not binary:
            raise ValueError("未找到 Orca Server，请先安装")
        version = _orca_read_version(binary, path_str)
        bind_ip = _orca_resolve_bind_ip(bind_mode, path_str)

        existing = read_json(ORCA_STATE_FILE)
        if restart:
            _orca_stop_locked()
        elif isinstance(existing, dict):
            old_pid = existing.get("pid")
            old_pid_ident = str(existing.get("pid_identity") or "")
            old_sup = existing.get("supervisor_pid")
            old_sup_ident = str(existing.get("supervisor_identity") or "")
            old_port = existing.get("port")
            old_bind_ip = str(existing.get("bind_ip") or "127.0.0.1")
            if (
                not isinstance(old_pid, bool)
                and isinstance(old_pid, int)
                and old_port == port
                and old_bind_ip == bind_ip
                and existing.get("bindings_hash") == bindings_hash
                and _pid_matches(old_pid, old_pid_ident)
                and _http_responsive(bind_ip, port, timeout=0.45)
            ):
                if projects:
                    _orca_sync_repos_inner(binary, _orca_base_env(path_str), projects)
                return {
                    "installed": True,
                    "binary_path": binary,
                    "version": version,
                    "running": True,
                    "pid": old_pid,
                    "port": port,
                    "bind_ip": bind_ip,
                    "bind_mode": bind_mode,
                    "tailscale_ip": _orca_tailscale_ip(path_str),
                }
            if (
                (not isinstance(old_pid, bool) and isinstance(old_pid, int) and _pid_matches(old_pid, old_pid_ident))
                or (not isinstance(old_sup, bool) and isinstance(old_sup, int) and _pid_matches(old_sup, old_sup_ident))
            ):
                _orca_stop_locked()

        os.umask(0o077)
        ORCA_ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
        extra_env, shim_prefixes, _ = _orca_prepare_bindings(bindings, path_str)
        env = _orca_base_env(path_str, extra_env=extra_env, shim_prefixes=shim_prefixes, terminal_memory_max=terminal_memory_max)

        cmd = [binary, "serve", "--bind", bind_ip, "--port", str(port), "--no-pairing", "--refuse-public"]
        workspace = str(payload.get("workspace") or "").strip()
        cwd = workspace if workspace and Path(workspace).is_dir() else str(HOME)

        def launch_once():
            handshake_file = ORCA_ROOT / f".sup-{os.getpid()}-{time.time_ns()}.json"
            sup_cfg = {
                "cmd": cmd,
                "cwd": cwd,
                "log_file": str(ORCA_LOG_FILE),
                "handshake_file": str(handshake_file),
            }
            sup_proc = subprocess.Popen(
                [sys.executable, "-I", str(Path(__file__).resolve()), "orca-supervise"],
                cwd=cwd,
                env=env,
                stdin=subprocess.PIPE,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                close_fds=True,
                start_new_session=True,
            )
            try:
                assert sup_proc.stdin is not None
                sup_proc.stdin.write(json.dumps(sup_cfg, ensure_ascii=False).encode("utf-8"))
                sup_proc.stdin.close()
            except OSError:
                pass

            child_pid = 0
            deadline = time.monotonic() + 18.0
            try:
                while time.monotonic() < deadline:
                    if not child_pid and handshake_file.is_file():
                        hs = read_json(handshake_file)
                        if isinstance(hs, dict):
                            if hs.get("error"):
                                return False, 1, 0, sup_proc.pid, f"Orca Server 启动失败：{hs['error']}"
                            cpid = hs.get("child_pid")
                            if not isinstance(cpid, bool) and isinstance(cpid, int) and cpid > 0:
                                child_pid = cpid
                    rc = sup_proc.poll()
                    if rc is not None:
                        tail = _redact_t3_log_tail(ORCA_LOG_FILE)
                        suffix = f"：{tail}" if tail else ""
                        return False, rc, child_pid, sup_proc.pid, f"Orca Server 启动失败（退出码 {rc}）{suffix}"
                    if child_pid and not _pid_alive(child_pid):
                        try:
                            rc = sup_proc.wait(timeout=0.5)
                        except subprocess.TimeoutExpired:
                            rc = 1
                        tail = _redact_t3_log_tail(ORCA_LOG_FILE)
                        suffix = f"：{tail}" if tail else ""
                        return False, rc, child_pid, sup_proc.pid, f"Orca Server 启动失败（退出码 {rc}）{suffix}"
                    if child_pid and _http_responsive(bind_ip, port, timeout=0.45):
                        return True, 0, child_pid, sup_proc.pid, ""
                    time.sleep(0.12)

                if child_pid:
                    _stop_managed_pid(child_pid, expected_identity=_pid_identity(child_pid), wait_secs=1.5, require_identity=False)
                _stop_managed_pid(sup_proc.pid, expected_identity=_pid_identity(sup_proc.pid), wait_secs=1.0, require_identity=False)
                tail = _redact_t3_log_tail(ORCA_LOG_FILE)
                suffix = f"：{tail}" if tail else ""
                return False, -1, 0, 0, f"Orca Server 启动超时（{bind_ip}:{port} 未就绪）{suffix}"
            finally:
                handshake_file.unlink(missing_ok=True)

        ok, rc, child_pid, sup_pid, err_msg = launch_once()
        # Self-heal stale lock (Orca documented exit code 3) once automatically.
        if not ok and rc == 3:
            try:
                subprocess.run(
                    [binary, "serve", "--reclaim-stale-lock", "--data-root", str(ORCA_DATA_DIR), "--yes", "--json"],
                    env=env, stdin=subprocess.DEVNULL,
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10, check=False,
                )
            except (OSError, subprocess.SubprocessError):
                pass
            ok, rc, child_pid, sup_pid, err_msg = launch_once()

        if not ok:
            raise ValueError(err_msg)

        state = {
            "pid": child_pid,
            "pid_identity": _pid_identity(child_pid),
            "supervisor_pid": sup_pid,
            "supervisor_identity": _pid_identity(sup_pid),
            "port": port,
            "bind_ip": bind_ip,
            "bind_mode": bind_mode,
            "version": version,
            "binary": binary,
            "started_at": int(time.time()),
            "bindings_hash": bindings_hash,
        }
        atomic_write_json(ORCA_STATE_FILE, state, mode=0o600)

        if projects:
            _orca_sync_repos_inner(binary, env, projects)

        return {
            "installed": True,
            "binary_path": binary,
            "version": version,
            "running": True,
            "pid": child_pid,
            "port": port,
            "bind_ip": bind_ip,
            "bind_mode": bind_mode,
            "tailscale_ip": _orca_tailscale_ip(path_str),
        }


def orca_pair(payload=None):
    """Mint an on-demand pairing offer via `orca-ide pair` without restarting Orca Server."""
    payload = payload if isinstance(payload, dict) else {}
    mobile = bool(payload.get("mobile"))
    rotate = bool(payload.get("rotate"))
    address = str(payload.get("address") or "").strip()
    if address and (len(address) > 255 or any(ch in address for ch in ("\x00", "\r", "\n", " ", "\t"))):
        raise ValueError("配对地址格式无效")

    with _orca_lifecycle_lock():
        path_str = enriched_path()
        binary = _orca_find_binary(path_str)
        if not binary:
            raise ValueError("未找到 Orca Server，请先安装")
        env = _orca_base_env(path_str)
        cmd = [binary, "pair", "--json"]
        if mobile:
            cmd.append("--mobile")
        if rotate:
            cmd.append("--rotate")
        if address:
            cmd.extend(["--address", address])
        try:
            res = subprocess.run(
                cmd, env=env, stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=15,
            )
        except (OSError, subprocess.SubprocessError) as exc:
            raise ValueError(f"生成 Orca 配对码失败：{exc}") from exc
        data = _orca_parse_json_output(res.stdout) or _orca_parse_json_output(res.stderr)
        result = data.get("result") if isinstance(data, dict) and isinstance(data.get("result"), dict) else data
        if not isinstance(result, dict):
            raise ValueError(f"Orca 配对命令返回无效结果（退出码 {res.returncode}）")
        if result.get("available") is False or not result.get("pairingUrl"):
            reason = str(result.get("guidance") or result.get("reason") or "Orca Server 未就绪，请先启动服务")
            raise ValueError(reason)
        return {
            "available": True,
            "scope": str(result.get("scope") or ("mobile" if mobile else "desktop")),
            "pairing_url": str(result.get("pairingUrl") or ""),
            "endpoint": str(result.get("endpoint") or ""),
            "device_id": str(result.get("deviceId") or ""),
            "web_client_url": str(result.get("webClientUrl") or ""),
            "qr": str(result.get("qr") or ""),
        }


def orca_reset_identity(payload=None):
    """Stop Orca Server, rotate ORCA_INSTANCE_ID, and reset server pairing identity."""
    with _orca_lifecycle_lock():
        _orca_stop_locked()
        path_str = enriched_path()
        binary = _orca_find_binary(path_str)
        new_id = str(uuid.uuid4())
        atomic_write_text(ORCA_INSTANCE_ID_FILE, new_id + "\n", mode=0o600)
        if binary:
            env = _orca_base_env(path_str)
            try:
                subprocess.run(
                    [binary, "serve", "reset-identity", "--yes", "--json"],
                    env=env, stdin=subprocess.DEVNULL,
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15, check=False,
                )
            except (OSError, subprocess.SubprocessError):
                pass
    return {"ok": True, "instance_id": new_id}



def main(argv):
    if not argv:
        raise ValueError("missing subcommand")
    cmd = argv[0]
    if cmd == "exec-shim":
        if len(argv) < 3:
            raise ValueError("exec-shim requires spec_file and account_id")
        spec = read_json(argv[1])
        if not isinstance(spec, dict):
            raise ValueError("invalid shim spec")
        account_id = argv[2] or "default"
        return exec_agent(spec, account_id, argv[3:])

    if cmd == "auto-exec":
        if len(argv) < 3:
            raise ValueError("auto-exec requires agent_id and binary_name")
        return auto_exec_agent(argv[1], argv[2], argv[3:])

    if cmd == "t3code-supervise":
        raw_cfg = sys.stdin.read(65536)
        try:
            sys.stdin.close()
        except OSError:
            pass
        cfg = json.loads(raw_cfg) if raw_cfg.strip() else {}
        return t3code_supervise(cfg)

    if cmd == "magpie-supervise":
        raw_cfg = sys.stdin.read(65536)
        try:
            sys.stdin.close()
        except OSError:
            pass
        cfg = json.loads(raw_cfg) if raw_cfg.strip() else {}
        return magpie_supervise(cfg)

    if cmd == "orca-supervise":
        raw_cfg = sys.stdin.read(65536)
        try:
            sys.stdin.close()
        except OSError:
            pass
        cfg = json.loads(raw_cfg) if raw_cfg.strip() else {}
        return orca_supervise(cfg)

    if cmd == "herdr-attach":
        return herdr_attach()

    if cmd == "login":
        raw_env = os.environ.pop("CAGE_CLI_LOGIN_JSON", "")
        payload = json.loads(raw_env) if raw_env.strip() else json.loads(sys.stdin.read(2 << 20) or "{}")
        return run_login(payload)

    raw = sys.stdin.read(2 << 20)
    payload = json.loads(raw) if raw.strip() else {}
    if cmd == "probe-all":
        print(json.dumps(probe_all(payload), ensure_ascii=False))
        return 0
    if cmd == "install":
        res = install_agent(payload)
        print(json.dumps({"ok": True, **res}, ensure_ascii=False))
        return 0
    if cmd == "uninstall":
        print(json.dumps(uninstall_agent(payload), ensure_ascii=False))
        return 0
    if cmd == "snapshot":
        print(json.dumps(snapshot_account(payload["spec"], payload.get("account_id") or "default"), ensure_ascii=False))
        return 0
    if cmd == "set-api-key":
        print(json.dumps(set_api_key(payload), ensure_ascii=False))
        return 0
    if cmd == "update-api-key-meta":
        print(json.dumps(update_api_key_meta(payload), ensure_ascii=False))
        return 0
    if cmd == "clear-credentials":
        print(json.dumps(clear_credentials(payload), ensure_ascii=False))
        return 0
    if cmd == "prepare-session":
        print(json.dumps(prepare_session(payload), ensure_ascii=False))
        return 0
    if cmd == "sync-auto-shims":
        print(json.dumps(sync_auto_shims(payload), ensure_ascii=False))
        return 0
    if cmd == "orca-status":
        print(json.dumps(orca_status(payload), ensure_ascii=False))
        return 0
    if cmd == "orca-install":
        print(json.dumps(orca_install(payload), ensure_ascii=False))
        return 0
    if cmd == "orca-uninstall":
        print(json.dumps(orca_uninstall(payload), ensure_ascii=False))
        return 0
    if cmd == "orca-start":
        print(json.dumps(orca_start(payload, restart=False), ensure_ascii=False))
        return 0
    if cmd == "orca-restart":
        print(json.dumps(orca_start(payload, restart=True), ensure_ascii=False))
        return 0
    if cmd == "orca-stop":
        print(json.dumps(orca_stop(payload), ensure_ascii=False))
        return 0
    if cmd == "orca-pair":
        print(json.dumps(orca_pair(payload), ensure_ascii=False))
        return 0
    if cmd == "orca-sync-repos":
        print(json.dumps(orca_sync_repos(payload), ensure_ascii=False))
        return 0
    if cmd == "orca-reset-identity":
        print(json.dumps(orca_reset_identity(payload), ensure_ascii=False))
        return 0
    if cmd == "t3code-status":
        print(json.dumps(t3code_status(payload), ensure_ascii=False))
        return 0
    if cmd == "t3code-install":
        print(json.dumps(t3code_install(payload), ensure_ascii=False))
        return 0
    if cmd == "t3code-start":
        print(json.dumps(t3code_start(payload, restart=False), ensure_ascii=False))
        return 0
    if cmd == "t3code-restart":
        print(json.dumps(t3code_start(payload, restart=True), ensure_ascii=False))
        return 0
    if cmd == "t3code-stop":
        print(json.dumps(t3code_stop(payload), ensure_ascii=False))
        return 0
    if cmd == "t3code-open-url":
        print(json.dumps(t3code_open_url(payload), ensure_ascii=False))
        return 0
    if cmd == "t3code-mint-pair":
        print(json.dumps(t3code_mint_pair(payload), ensure_ascii=False))
        return 0
    if cmd == "magpie-status":
        print(json.dumps(magpie_status(payload), ensure_ascii=False))
        return 0
    if cmd == "magpie-install":
        print(json.dumps(magpie_install(payload), ensure_ascii=False))
        return 0
    if cmd == "magpie-uninstall":
        print(json.dumps(magpie_uninstall(payload), ensure_ascii=False))
        return 0
    if cmd == "magpie-start":
        print(json.dumps(magpie_start(payload, restart=False), ensure_ascii=False))
        return 0
    if cmd == "magpie-restart":
        print(json.dumps(magpie_start(payload, restart=True), ensure_ascii=False))
        return 0
    if cmd == "magpie-stop":
        print(json.dumps(magpie_stop(payload), ensure_ascii=False))
        return 0
    if cmd == "magpie-open-url":
        print(json.dumps(magpie_open_url(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-status":
        print(json.dumps(herdr_status(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-install":
        print(json.dumps(herdr_install(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-uninstall":
        print(json.dumps(herdr_uninstall(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-start":
        print(json.dumps(herdr_start(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-stop":
        print(json.dumps(herdr_stop(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-restart":
        print(json.dumps(herdr_restart(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-list":
        print(json.dumps(herdr_list(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-close":
        print(json.dumps(herdr_close(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-legacy-status":
        print(json.dumps(herdr_legacy_status(payload), ensure_ascii=False))
        return 0
    if cmd == "herdr-legacy-uninstall":
        print(json.dumps(herdr_legacy_uninstall(payload), ensure_ascii=False))
        return 0
    raise ValueError(f"unknown subcommand: {cmd}")


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except KeyboardInterrupt:
        sys.exit(130)
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
