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
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import urlparse

HOME = Path.home()
ACCOUNTS_ROOT = HOME / ".local/share/cage/agent-accounts"
SHIMS_ROOT = HOME / ".local/share/cage/agent-shims"
SHARED_ROOT = HOME / ".local/share/cage/agent-shared"
T3CODE_DIR = HOME / ".local/share/cage/t3code"
T3CODE_ROOT = T3CODE_DIR
T3CODE_STATE_DIR = T3CODE_ROOT / "state"
T3CODE_STATE_FILE = T3CODE_ROOT / "runtime.json"
T3CODE_LOG_FILE = T3CODE_ROOT / "server.log"
T3CODE_LOCK_FILE = T3CODE_ROOT / "lifecycle.lock"
T3CODE_PAIR_FILE = T3CODE_ROOT / "pair.once"
T3CODE_LOG_MAX_BYTES = 65536
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
    r"|token|api[_-]?key|secret|client_?secret|authorization|cookie|set-cookie)['\"]?\s*[:=]\s*\S+"
    r"|pair#|/pair[/?#]|t3://|t3code://|bearer\s+\S+)"
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
        # Never pick our own agent shims when resolving real binaries.
        if item.startswith(str(SHIMS_ROOT)) or item == "/usr/local/lib/cage-claude-bin":
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

    if agent_id == "gemini":
        accounts = read_json(cfg / "google_accounts.json")
        oauth = read_json(cfg / "oauth_creds.json")
        email = ""
        if isinstance(accounts, dict):
            active = accounts.get("active")
            if isinstance(active, str):
                email = active
            elif isinstance(active, dict):
                email = active.get("email") or ""
        has_token = isinstance(oauth, dict) and any(
            isinstance(oauth.get(k), str) and oauth.get(k).strip()
            for k in ("access_token", "refresh_token", "id_token")
        )
        if has_token:
            claims = decode_jwt_claims(oauth.get("id_token") or "")
            email = email or claims.get("email") or ""
            exp_ms = oauth.get("expiry_date") or 0
            exp = int(exp_ms / 1000) if isinstance(exp_ms, (int, float)) and exp_ms > 1e12 else int(exp_ms or 0)
            return {"logged_in": True, "auth_mode": "oauth", "email": str(email), "plan": "Google OAuth", "expires_at": exp}
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
    for rel in auth_spec.get("credential_files") or []:
        for root in (cfg, data_dir):
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
            f'    {q_shims}|{q_shims}/*|/usr/local/lib/cage-claude-bin) continue ;;\n'
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
    script = (
        "#!/bin/sh\n"
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
        env = os.environ.copy()
        env.pop("CAGE_CLI_ACCOUNT_ID", None)
        shim_prefixes = []
        for agent_id, binding in bindings.items():
            if not isinstance(binding, dict):
                continue
            guest_id = str(binding.get("guest_id") or "default").strip() or "default"
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
            else:
                spec = binding.get("spec")
                if isinstance(spec, dict) and spec.get("id"):
                    prep = prepare_session({"spec": spec, "account_id": guest_id})
                    shim_prefixes.append(prep["shim_dir"])
                    env[_agent_account_env_var(spec["id"])] = guest_id

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

    if cmd == "t3code-supervise":
        raw_cfg = sys.stdin.read(65536)
        try:
            sys.stdin.close()
        except OSError:
            pass
        cfg = json.loads(raw_cfg) if raw_cfg.strip() else {}
        return t3code_supervise(cfg)

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
    raise ValueError(f"unknown subcommand: {cmd}")


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except KeyboardInterrupt:
        sys.exit(130)
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
