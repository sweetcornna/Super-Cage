import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import runpy
import shlex
import subprocess
import sys
import tempfile
import time
import tomllib
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
source = Path(__file__).with_name("codex-account.py")
spec = importlib.util.spec_from_file_location("codex_account", source)
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)
FIRST = "1" * 24
SECOND = "2" * 24

# Emulates the parts of the official CLI the helper uses: login status on
# stderr, device login, the stdio app server and the interactive launch.
FAKE_CODEX = r'''
import base64, json, os, sys, time
from pathlib import Path
home = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
args = sys.argv[1:]
def jwt(claims):
    part = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    return "header." + part + ".signature"
def auth():
    try:
        return json.loads((home / "auth.json").read_text())
    except FileNotFoundError:
        return None
if args[:2] == ["login", "status"]:
    data = auth()
    if not data:
        print("Not logged in", file=sys.stderr); sys.exit(1)
    if data.get("OPENAI_API_KEY"):
        print("API key configured - sk-***", file=sys.stderr); sys.exit(0)
    print("Logged in using ChatGPT", file=sys.stderr); sys.exit(0)
if args[:2] == ["login", "--device-auth"]:
    print("Open https://auth.openai.com/codex/device and enter ABCD-EFGH")
    claims = {"email": os.environ["TEST_EMAIL"], "https://api.openai.com/auth": {"chatgpt_plan_type": "plus", "chatgpt_account_id": "workspace-1"}}
    home.mkdir(parents=True, exist_ok=True)
    (home / "auth.json").write_text(json.dumps({"auth_mode": "chatgpt", "OPENAI_API_KEY": None, "tokens": {
        "id_token": jwt(claims), "access_token": "secret-access-token", "refresh_token": "secret-refresh-token", "account_id": "workspace-1"}}))
    sys.exit(0)
if args[:1] == ["app-server"]:
    if os.environ.get("TEST_APP_LOG"):
        with open(os.environ["TEST_APP_LOG"], "a") as log:
            log.write(os.getcwd() + "\n")
    for line in sys.stdin:
        message = json.loads(line)
        method, ident = message.get("method"), message.get("id")
        if ident is None:
            continue
        print(json.dumps({"method": "remoteControl/status/changed", "params": {}}), flush=True)
        if method == "initialize":
            result = {"codexHome": str(home), "userAgent": "test"}
        elif method == "account/read":
            data = auth() or {}
            if data.get("tokens"):
                result = {"account": {"type": "chatgpt", "email": os.environ.get("TEST_APP_EMAIL", os.environ["TEST_EMAIL"]), "planType": "pro"}, "requiresOpenaiAuth": True}
            else:
                result = {"account": {"type": "apiKey"} if data else None, "requiresOpenaiAuth": True}
        elif method == "account/rateLimits/read":
            print(json.dumps({"id": ident, "result": json.loads(os.environ["TEST_RATE_LIMITS"])}), flush=True)
            continue
        else:
            print(json.dumps({"id": ident, "error": {"code": -32601, "message": "unknown"}}), flush=True)
            continue
        print(json.dumps({"id": ident, "result": result}), flush=True)
    sys.exit(0)
capture = os.environ.get("TEST_CAPTURE")
if capture:
    Path(capture).write_text(json.dumps({"argv": args, "env": dict(os.environ), "cwd": os.getcwd()}))
'''


def rollout_line(seen, primary, secondary=None, limit_id="codex"):
    limits = {"limit_id": limit_id, "primary": primary, "secondary": secondary, "credits": None, "plan_type": "plus"}
    stamp = time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime(seen))
    return json.dumps({"timestamp": stamp, "type": "event_msg", "payload": {"type": "token_count", "info": {"total": 3}, "rate_limits": limits}})


class CodexAccountTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.home = Path(tmp.name).resolve()
        fake = self.home / ".local/bin/codex"
        fake.parent.mkdir(parents=True)
        fake.write_text("#!" + sys.executable + "\n" + FAKE_CODEX)
        fake.chmod(0o700)
        self.email = "owner@example.test"
        self.subject = hashlib.sha256((self.email + "\nworkspace-1").encode()).hexdigest()
        self.capture = self.home / "capture.json"
        self.env = {"HOME": str(self.home), "PATH": os.defpath, "TEST_EMAIL": self.email, "TMPDIR": str(self.home),
                    "CAGE_CODEX_SUBJECT": self.subject, "PYTHONDONTWRITEBYTECODE": "1", "TEST_CAPTURE": str(self.capture),
                    "TEST_RATE_LIMITS": json.dumps({"rateLimits": {"limitId": "codex", "primary": {"usedPercent": 37, "windowDurationMins": 300, "resetsAt": int(time.time()) + 3600},
                                                                     "secondary": {"usedPercent": 104, "windowDurationMins": 10080, "resetsAt": int(time.time()) + 86400}},
                                                    "rateLimitsByLimitId": None}),
                    "OPENAI_API_KEY": "sk-shell-private", "CODEX_HOME": str(self.home / "elsewhere")}

    def run_helper(self, mode, account, *args, env=None, check=True, cwd=None):
        return subprocess.run([sys.executable, str(source), mode, account, *args], env=env or self.env,
                              cwd=cwd or self.home, text=True, capture_output=True, check=check)

    def base(self, account=FIRST):
        return self.home / ".local/share/cage/codex-accounts" / account

    def launch_overrides(self, cwd, *args):
        """Run Codex through the helper and return its -c overrides as values."""
        self.run_helper("run", FIRST, *args, cwd=cwd)
        launch = json.loads(self.capture.read_text())
        argv = launch["argv"][:len(launch["argv"]) - len(args)]
        self.assertEqual(launch["argv"][len(argv):], list(args))
        overrides = {}
        for flag, value in zip(argv[::2], argv[1::2]):
            self.assertEqual(flag, "-c")
            key, raw = value.split("=", 1)
            overrides[key] = tomllib.loads("value = " + raw)["value"]
        return overrides, launch

    def login(self, account=FIRST, env=None):
        result = self.run_helper("login", account, env=env)
        self.assertIn("auth.openai.com/codex/device", result.stdout)
        return result

    def test_device_login_snapshot_never_exports_tokens(self):
        self.assertEqual(json.loads(self.run_helper("snapshot", FIRST).stdout), {"logged_in": False})
        self.login()
        auth = self.base() / "home/auth.json"
        self.assertTrue(auth.is_file())
        self.assertFalse((self.home / "elsewhere").exists())
        self.assertEqual(self.base().stat().st_mode & 0o777, 0o700)
        output = self.run_helper("snapshot", FIRST).stdout
        for secret in ("secret-access-token", "secret-refresh-token", "header."):
            self.assertNotIn(secret, output)
        summary = json.loads(output)
        self.assertEqual(summary, {"logged_in": True, "auth_mode": "chatgpt", "email": self.email, "organization": "workspace-1", "plan": "plus"})
        self.assertEqual(helper.subject(summary), self.subject)
        record = json.loads((self.base() / "identity.json").read_text())
        self.assertEqual(record["identity"], self.subject)
        self.assertNotEqual(self.run_helper("login", "default", check=False).returncode, 0)
        self.assertNotEqual(self.run_helper("snapshot", "../outside", check=False).returncode, 0)

    def test_api_key_login_is_not_a_subscription(self):
        home = self.base() / "home"
        home.mkdir(parents=True)
        (home / "auth.json").write_text(json.dumps({"auth_mode": "apikey", "OPENAI_API_KEY": "sk-file-private"}))
        output = self.run_helper("snapshot", FIRST).stdout
        self.assertNotIn("sk-file-private", output)
        self.assertEqual(json.loads(output), {"logged_in": False, "auth_mode": "api_key"})
        self.assertNotEqual(self.run_helper("run", FIRST, check=False).returncode, 0)

    def test_rollout_rate_limits_are_bound_to_the_current_identity(self):
        self.login()
        now = int(time.time())
        # Events recorded after the credential was written belong to this login.
        os.utime(self.base() / "home/auth.json", (now - 100, now - 100))
        self.run_helper("snapshot", FIRST)
        self.assertEqual(json.loads((self.base() / "identity.json").read_text())["since"], now - 100)
        day = self.base() / "home/sessions" / time.strftime("%Y/%m/%d", time.gmtime(now))
        day.mkdir(parents=True)
        reset = now + 3600
        lines = [rollout_line(now - 20, {"used_percent": 12.5, "window_minutes": 300, "resets_at": reset}),
                 rollout_line(now - 10, {"used_percent": 61.0, "window_minutes": 300, "resets_at": reset},
                              {"used_percent": 130.0, "window_minutes": 10080, "resets_at": now + 86400}),
                 rollout_line(now - 5, {"used_percent": 1.0, "window_minutes": 60, "resets_at": reset}, limit_id="codex_other"),
                 '{"truncated":']
        (day / "rollout-2026-11-19T00-00-00-a.jsonl").write_text("\n".join(lines) + "\n")
        batch = json.loads(self.run_helper("quotas", FIRST, SECOND).stdout)
        self.assertEqual(list(batch), [FIRST])
        quota = batch[FIRST]
        self.assertEqual(quota["identity"], self.subject)
        self.assertEqual(quota["primary"], {"used_percentage": 61.0, "resets_at": reset, "window_minutes": 300, "observed_at": now - 10})
        self.assertEqual(quota["secondary"]["used_percentage"], 100)
        self.assertEqual(json.loads(self.run_helper("snapshot", FIRST).stdout)["quota"]["primary"]["used_percentage"], 61.0)
        # A new identity in the same home never inherits the previous readings.
        other = {**self.env, "TEST_EMAIL": "other@example.test"}
        self.login(env=other)
        self.run_helper("snapshot", FIRST, env=other)
        self.assertEqual(json.loads(self.run_helper("quotas", FIRST).stdout), {})

    def test_usage_reads_app_server_and_rejects_identity_changes(self):
        self.login()
        app_log = self.home / "app.log"
        env = {**self.env, "TEST_APP_LOG": str(app_log)}
        report = json.loads(self.run_helper("usage", FIRST, self.subject, env=env).stdout)
        self.assertEqual(report["usage_status"], "reported")
        self.assertEqual(report["plan"], "pro")
        self.assertEqual(report["quota"]["primary"]["used_percentage"], 37)
        self.assertEqual(report["quota"]["primary"]["window_minutes"], 300)
        self.assertEqual(report["quota"]["secondary"]["used_percentage"], 100)
        self.assertNotIn(str(self.home / "project"), app_log.read_text())
        stored = json.loads((self.base() / "quota.json").read_text())
        self.assertEqual(stored["identity"], self.subject)
        other = hashlib.sha256(b"other@example.test\nworkspace-1").hexdigest()
        self.assertEqual(json.loads(self.run_helper("usage", FIRST, other).stdout)["usage_status"], "identity_changed")
        mismatch = {**self.env, "TEST_APP_EMAIL": "intruder@example.test"}
        self.assertEqual(json.loads(self.run_helper("usage", FIRST, self.subject, env=mismatch).stdout)["usage_status"], "identity_changed")
        broken = {**self.env, "TEST_RATE_LIMITS": "{\"rateLimits\": {\"limitId\": \"codex_other\", \"primary\": {\"usedPercent\": 5}}}"}
        report = json.loads(self.run_helper("usage", FIRST, self.subject, env=broken).stdout)
        self.assertEqual(report["usage_status"], "cached")
        self.assertEqual(report["quota"]["primary"]["used_percentage"], 37)

    def test_run_shares_local_customizations_but_not_login_or_routing(self):
        self.login()
        common = self.home / ".codex"
        (common / "prompts").mkdir(parents=True)
        (common / "prompts/review.md").write_text("shared prompt")
        (common / "AGENTS.md").write_text("shared instructions")
        (common / "skills/my-skill").mkdir(parents=True)
        (common / "skills/.system").mkdir(parents=True)
        (common / "auth.json").write_text('{"OPENAI_API_KEY": "sk-default-private"}')
        (common / "sessions").mkdir()
        (common / "config.toml").write_text('''
model = "gpt-shared"
model_provider = "private-proxy"
approval_policy = "never"
sandbox_mode = "danger-full-access"
openai_base_url = "https://proxy.invalid/v1"
cli_auth_credentials_store = "keyring"
[model_providers.private-proxy]
base_url = "https://proxy.invalid"
env_key = "PROXY_KEY"
[features]
web_search = true
fast_mode = true
[shell_environment_policy]
inherit = "all"
set = { FOO = "bar", OPENAI_API_KEY = "sk-in-config", HTTPS_PROXY = "http://proxy.invalid" }
[mcp_servers.docs]
command = "docs-mcp"
args = ["--quiet"]
[mcp_servers.github]
command = "github-mcp"
env = { GITHUB_PERSONAL_ACCESS_TOKEN = "ghp-private" }
[mcp_servers.remote]
url = "https://mcp.invalid"
[projects."/work/a.b"]
trust_level = "trusted"
[tui]
notifications = true
status_line_provider = "custom"
[profiles.work]
model_provider = "private-proxy"
''')
        account_home = self.base() / "home"
        (account_home / "config.toml").write_text('model = "gpt-account"\n[features]\nfast_mode = false\n')
        project = self.home / "project"
        (project / ".codex").mkdir(parents=True)
        (project / ".codex/config.toml").write_text('approval_policy = "on-request"\n')
        overrides, launch = self.launch_overrides(project, "--search", "resume")
        env = launch["env"]
        self.assertEqual(overrides, {
            "sandbox_mode": "danger-full-access",
            "features": {"web_search": True},
            "shell_environment_policy": {"inherit": "all", "set": {"FOO": "bar"}},
            "mcp_servers": {"docs": {"command": "docs-mcp", "args": ["--quiet"]}},
            "projects": {"/work/a.b": {"trust_level": "trusted"}},
            "tui": {"notifications": True},
        })
        self.assertEqual(Path(env["CODEX_HOME"]), account_home)
        self.assertNotIn("OPENAI_API_KEY", env)
        self.assertFalse([name for name in env if name.startswith("CAGE_")])
        self.assertEqual(Path(launch["cwd"]), project)
        self.assertEqual((account_home / "AGENTS.md").resolve(), (common / "AGENTS.md").resolve())
        self.assertEqual((account_home / "prompts/review.md").read_text(), "shared prompt")
        self.assertTrue((account_home / "skills/my-skill").is_symlink())
        self.assertFalse((account_home / "skills/.system").exists())
        self.assertNotIn("sk-default-private", (account_home / "auth.json").read_text())
        self.assertFalse((account_home / "sessions").is_symlink())
        # An account's own differing instructions are kept, not overwritten.
        (account_home / "AGENTS.md").unlink()
        (account_home / "AGENTS.md").write_text("account instructions")
        result = self.run_helper("run", FIRST, cwd=project)
        self.assertIn("AGENTS.md", result.stderr)
        self.assertEqual((account_home / "AGENTS.md").read_text(), "account instructions")

    def test_shared_config_keeps_credentials_private(self):
        self.login()
        common = self.home / ".codex"
        common.mkdir()
        (common / "config.toml").write_text('''
notify = ["curl", "-H", "Authorization: Bearer notify-secret", "https://hooks.invalid"]
[features]
web_search = true
apiKey = "camel-api-key"
[shell_environment_policy]
set = { GH_PAT = "pat-secret", DB_PASS = "pass-secret", apiKey = "camel-env-secret", authToken = "camel-token-secret", MY_TOOL = "ghp_abcdefghijklmnop", RUST_LOG = "info" }
[mcp_servers.flagged]
command = "github-mcp"
args = ["--token", "ghp_x"]
[mcp_servers.dashed]
command = "search-mcp"
args = ["--api-key", "dashed-secret"]
[mcp_servers.header]
command = "fetch-mcp"
args = ["--header", "Authorization: Bearer header-secret"]
[mcp_servers.assigned]
command = "env"
args = ["GITHUB_TOKEN=assigned-secret", "mcp"]
[mcp_servers.bare]
command = "github-mcp"
args = ["ghp_abcdefghijklmnop"]
[mcp_servers.forwarded]
command = "github-mcp"
env_vars = ["GITHUB_TOKEN"]
[mcp_servers.quiet_env]
command = "debug-mcp"
env = { DEBUG = "1" }
[mcp_servers.remote]
command = "remote-mcp"
http_headers = { "X-Team" = "header-value-secret" }
[mcp_servers.files]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/home/cage/projects"]
[tui]
notifications = true
clientSecret = "camel-client-secret"
''')
        project = self.home / "project"
        project.mkdir()
        overrides, launch = self.launch_overrides(project)
        self.assertEqual(overrides, {
            "features": {"web_search": True},
            "shell_environment_policy": {"set": {"RUST_LOG": "info"}},
            "mcp_servers": {"files": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/cage/projects"]}},
            "tui": {"notifications": True},
        })
        argv = "\n".join(launch["argv"])
        for secret in ("secret", "ghp_", "camel-api-key", "Bearer", "GITHUB_TOKEN", "DEBUG"):
            self.assertNotIn(secret, argv)
        # A notify command without credentials is still shared.
        (common / "config.toml").write_text('notify = ["notify-send", "Codex"]\n')
        self.assertEqual(self.launch_overrides(project)[0], {"notify": ["notify-send", "Codex"]})

    def test_secret_detection_splits_camel_case_and_flags(self):
        for name in ("apiKey", "authToken", "clientSecret", "bearer_token_env_var", "http_headers", "sessionCookie", "privateKey"):
            self.assertTrue(helper.private_config_name(name), name)
        for name in ("web_search", "trust_level", "notifications", "sandbox_mode", "approval_policy", "writable_roots",
                     "network_access", "persistence", "max_bytes", "startup_timeout_sec", "enabled_tools"):
            self.assertFalse(helper.private_config_name(name), name)
        for name in ("GH_PAT", "DB_PASS", "DB_PASSWD", "PGPASSWORD_FILE", "githubToken", "SESSION_ID", "MY_CREDS", "SIGNING_PRIVATE_KEY"):
            self.assertFalse(helper.shared_env_variable(name), name)
        for argument in ("--token", "--api-key", "--apiKey", "Authorization: Bearer x", "GITHUB_TOKEN=x", "HTTPS_PROXY=x",
                         "--password=x", "sk-abcdefghijkl", "ghp_abcdefghijklmnop"):
            self.assertTrue(helper.secret_argument(argument), argument)
        for argument in ("-y", "@modelcontextprotocol/server-filesystem", "/home/cage/projects", "--quiet", "LOG_LEVEL=debug", 3):
            self.assertFalse(helper.secret_argument(argument), argument)

    def test_run_requires_the_selected_identity(self):
        self.login()
        for env in ({**self.env, "CAGE_CODEX_SUBJECT": "0" * 64}, {k: v for k, v in self.env.items() if k != "CAGE_CODEX_SUBJECT"}):
            result = self.run_helper("run", FIRST, env=env, check=False)
            # A distinct status ends the terminal so the host re-checks the account.
            self.assertEqual(result.returncode, helper.ACCOUNT_REFUSED)
            self.assertIn("Codex", result.stderr)
            self.assertFalse(self.capture.exists())
        missing = {**self.env, "PATH": "/nonexistent"}
        (self.home / ".local/bin/codex").rename(self.home / "codex-moved")
        result = self.run_helper("run", FIRST, env=missing, check=False)
        self.assertEqual(result.returncode, 1)
        self.assertIn("未找到 Codex CLI", result.stderr)

    def test_inline_toml_values_round_trip(self):
        value = {"quote": 'say "hi"\\\n\t\x7f', "unicode": "中文", "nested": {"a.b": [1, 2.5, True]},
                 "when": tomllib.loads("t = 1979-05-27T07:32:00Z")["t"]}
        self.assertEqual(tomllib.loads("value = " + helper.toml_value(value))["value"], value)
        self.assertFalse(helper.shared_env_variable("OPENAI_ORG_ID"))
        self.assertFalse(helper.shared_env_variable("HTTP_PROXY"))
        self.assertTrue(helper.shared_env_variable("RUST_LOG"))

    def test_app_server_must_report_the_selected_codex_home(self):
        self.login()
        (self.home / "elsewhere").mkdir()
        with tempfile.TemporaryDirectory() as directory:
            env = {**self.env, "CODEX_HOME": str(self.base() / "home")}
            server = helper.AppServer(str(self.home / ".local/bin/codex"), env, directory, time.monotonic() + 5)
            server.home = self.home / "elsewhere"
            with self.assertRaises(helper.StatusError):
                with server:
                    self.fail("entered a session for another Codex home")
            self.assertIsNotNone(server.process.returncode)

    def test_resume_routes_rollouts_to_original_profile(self):
        self.login(FIRST)
        thread = "19b38927-d608-49d6-b690-870ef502e802"
        archive = "703d578a-20a5-4d67-924f-ad942e63221f"
        home = self.base(FIRST) / "home"
        session = home / "sessions/2026/10/03" / f"rollout-2026-10-03T07-09-01-{thread}_c88ebbc8-9649-4339-9f10-f6255aa748a8.jsonl"
        archived = home / "archived_sessions" / f"rollout-2026-10-03T07-09-01-{archive}.jsonl"
        for path, identifier in [(session, thread), (archived, archive)]:
            path.parent.mkdir(parents=True)
            path.write_text(json.dumps({"type": "session_meta", "timestamp": "2026-10-03T07:09:01.000Z",
                                        "payload": {"id": identifier, "cwd": "/previous/project", "source": "cli"}}) + "\n")
        schedule = self.base(FIRST).parent / "launch.json"
        old = {"id": FIRST, "subject": self.subject}
        new = {"id": SECOND, "subject": self.subject}
        schedule.write_text(json.dumps({"managed": True, "accounts": [new], "history_accounts": [old, new]}))
        env = {key: value for key, value in self.env.items() if key not in ("CODEX_HOME", "OPENAI_API_KEY", "CAGE_CODEX_SUBJECT")}
        for args in [("resume", "--all", thread), ("exec", "resume", "-c", "history.persistence='save-all'", thread),
                     ("resume", archive), ("unarchive", archive), ("resume", "--all"), ("resume", "--last")]:
            result = self.run_helper("auto", *args, env=env)
            self.assertEqual(result.returncode, 0)
            seen = json.loads(self.capture.read_text())
            self.assertEqual(Path(seen["env"]["CODEX_HOME"]), home)
            self.assertEqual(seen["argv"][-len(args):], list(args))
        other_path = self.base(SECOND) / "home/sessions/2026/10/03" / session.name
        other_path.parent.mkdir(parents=True)
        other_path.symlink_to(session)
        self.run_helper("auto", "resume", thread, env=env)
        self.assertEqual(Path(json.loads(self.capture.read_text())["env"]["CODEX_HOME"]), home)
        other_path.unlink()
        other_path.write_bytes(session.read_bytes())
        self.capture.unlink()
        ambiguous = self.run_helper("auto", "resume", thread, env=env, check=False)
        self.assertEqual(ambiguous.returncode, 78)
        self.assertFalse(self.capture.exists())
        self.assertIn('"cwd": "/previous/project"', archived.read_text())

    def test_auto_schedules_codex_skipping_exhausted_and_respecting_explicit_env(self):
        # Log in two distinct Codex accounts.
        self.login(FIRST)
        second_email = "second@example.test"
        second_subject = hashlib.sha256((second_email + "\nworkspace-1").encode()).hexdigest()
        second_env = {**self.env, "TEST_EMAIL": second_email, "CAGE_CODEX_SUBJECT": second_subject}
        self.login(SECOND, env=second_env)
        self.run_helper("snapshot", FIRST)
        self.run_helper("snapshot", SECOND, env=second_env)

        now = int(time.time())
        # Mark FIRST exhausted (100% primary) and SECOND healthy (20% primary).
        (self.base(FIRST) / "quota.json").write_text(json.dumps({
            "identity": self.subject,
            "observed_at": now,
            "primary": {"used_percentage": 100, "resets_at": now + 3600, "window_minutes": 300, "observed_at": now},
        }))
        (self.base(SECOND) / "quota.json").write_text(json.dumps({
            "identity": second_subject,
            "observed_at": now,
            "primary": {"used_percentage": 20, "resets_at": now + 3600, "window_minutes": 300, "observed_at": now},
        }))

        launch_file = self.home / ".local/share/cage/codex-accounts/launch.json"
        launch_file.write_text(json.dumps({
            "mode": "balanced",
            "accounts": [
                {"id": FIRST, "subject": self.subject, "preferred": False},
                {"id": SECOND, "subject": second_subject, "preferred": False},
            ],
        }))

        # Put a trap shim inside auto-shims to verify auto() strips Cage shim dirs (no recursion).
        auto_shims = self.home / ".local/share/cage/auto-shims"
        auto_shims.mkdir(parents=True, exist_ok=True)
        trap = auto_shims / "codex"
        trap.write_text("#!/bin/sh\necho recursion-trap >&2\nexit 99\n")
        trap.chmod(0o755)

        clean_env = {
            "HOME": str(self.home),
            "PATH": f"{auto_shims}:{self.home / '.local/bin'}:{os.defpath}",
            "TMPDIR": str(self.home),
            "PYTHONDONTWRITEBYTECODE": "1",
            "TEST_CAPTURE": str(self.capture),
            "TEST_EMAIL": second_email,
        }
        res = subprocess.run(
            [sys.executable, str(source), "auto", "--search", "hello"],
            env=clean_env, cwd=self.home, text=True, capture_output=True, check=True,
        )
        self.assertEqual(res.returncode, 0)
        seen = json.loads(self.capture.read_text())
        self.assertEqual(Path(seen["env"]["CODEX_HOME"]), self.base(SECOND) / "home")
        self.assertEqual(seen["argv"][-2:], ["--search", "hello"])

        # Explicit opt-out (CAGE_ACCOUNT_SCHEDULING=off, CODEX_HOME, or --version) bypasses scheduling.
        for label, extra_env, extra_args in (
            ("scheduling_off", {"CAGE_ACCOUNT_SCHEDULING": "off"}, ["prompt"]),
            ("explicit_home", {"CODEX_HOME": str(self.home / "custom-home")}, ["prompt"]),
            ("unscheduled_flag", {}, ["--version"]),
        ):
            self.capture.unlink(missing_ok=True)
            sub_env = {**clean_env, **extra_env}
            res2 = subprocess.run(
                [sys.executable, str(source), "auto", *extra_args],
                env=sub_env, cwd=self.home, text=True, capture_output=True, check=True,
            )
            self.assertEqual(res2.returncode, 0, label)
            seen2 = json.loads(self.capture.read_text())
            self.assertNotEqual(seen2["env"].get("CODEX_HOME"), str(self.base(SECOND) / "home"), label)

        # Magpie takeover marker bypasses scheduling even with valid launch.json present.
        takeover = self.home / ".local/share/cage/magpie/takeover"
        takeover.parent.mkdir(parents=True, exist_ok=True)
        takeover.write_text("1\n")
        self.capture.unlink(missing_ok=True)
        res3 = subprocess.run(
            [sys.executable, str(source), "auto", "--search", "magpie"],
            env=clean_env, cwd=self.home, text=True, capture_output=True, check=True,
        )
        self.assertEqual(res3.returncode, 0)
        seen3 = json.loads(self.capture.read_text())
        self.assertNotEqual(seen3["env"].get("CODEX_HOME"), str(self.base(SECOND) / "home"))
        self.assertEqual(seen3["argv"][-2:], ["--search", "magpie"])

    def orca_fixture(self):
        second_email = "second@example.test"
        second_subject = hashlib.sha256(f"{second_email}\nworkspace-1".encode()).hexdigest()
        self.login(FIRST)
        self.login(SECOND, env={**self.env, "TEST_EMAIL": second_email})
        self.run_helper("snapshot", FIRST)
        self.run_helper("snapshot", SECOND)
        data = self.home / ("Library/Application Support/orca" if sys.platform == "darwin" else ".config/orca")
        data.mkdir(parents=True, mode=0o700)
        native = []
        for guest_id, native_id in [(FIRST, "a14932ad-2140-42d1-a7d3-1206468de483"),
                                    (SECOND, "fd72af98-76e7-4601-b301-a05d207b248a")]:
            codex = data / "codex-accounts" / native_id / "home"
            codex.mkdir(parents=True, mode=0o700)
            codex.parent.chmod(0o775)
            codex.parent.parent.chmod(0o775)
            (codex / ".orca-managed-home").write_text(native_id + "\n")
            auth = codex / "auth.json"
            auth.write_bytes((self.base(guest_id) / "home/auth.json").read_bytes())
            auth.chmod(0o600)
            native.append(codex)
        env = {name: value for name, value in self.env.items() if name not in ("OPENAI_API_KEY", "CAGE_CODEX_SUBJECT")}
        env.update(TERM_PROGRAM="Orca", ORCA_USER_DATA_PATH=str(data), CODEX_HOME=str(native[0]))
        roster = [{"id": FIRST, "subject": self.subject}, {"id": SECOND, "subject": second_subject}]
        (self.base(FIRST).parent / "launch.json").write_text(json.dumps({"managed": True, "accounts": roster}))
        executable = self.home / ".local/bin/cage-codex-account"
        executable.write_text("#!/bin/sh\nexec " + shlex.join([sys.executable, str(source.resolve())]) + ' "$@"\n')
        executable.chmod(0o700)
        with patch.dict(os.environ, env, clear=True):
            runtime = runpy.run_path(str(source.with_name("agent-runtime.py")))
            sync = runtime["sync_auto_shims"]
            mask = os.umask(0o077)
            try:
                with patch.dict(sync.__globals__, {"_codex_account_helper": lambda: str(executable)}):
                    published = sync()
            finally:
                os.umask(mask)
        self.assertIn("codex", published["shims"])
        env["PATH"] = f"{published['dir']}{os.pathsep}{env['PATH']}"
        return env, native, roster

    def run_orca_codex(self, env, *args):
        return subprocess.run(["codex", *args], env=env, cwd=self.home,
                              text=True, capture_output=True, check=True, timeout=15)

    def orca_launch(self, env, *args):
        self.capture.unlink(missing_ok=True)
        self.run_orca_codex(env, *args)
        return json.loads(self.capture.read_text())

    def test_orca_injected_profile_obeys_native_launch_policy(self):
        env, native, roster = self.orca_fixture()
        schedule = self.base(FIRST).parent / "launch.json"
        choices = {"managed": True, "history_accounts": roster}
        schedule.write_text(json.dumps({**choices, "mode": "manual", "selected_account_id": SECOND,
                                        "accounts": [roster[1]]}))
        self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(self.base(SECOND) / "home"))

        now = int(time.time())
        quota = self.base(FIRST) / "quota.json"
        def usage(percent):
            quota.write_text(json.dumps({"identity": self.subject, "observed_at": now,
                                        "primary": {"used_percentage": percent, "resets_at": now + 3600,
                                                    "window_minutes": 300, "observed_at": now}}))

        usage(100)
        schedule.write_text(json.dumps({**choices, "mode": "exhaust", "selected_account_id": FIRST,
                                        "selected_account_revision": "new-selection", "accounts": roster}))
        self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(self.base(SECOND) / "home"))
        usage(20)
        self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(self.base(SECOND) / "home"))

        schedule.write_text(json.dumps({**choices, "mode": "smart", "accounts": roster, "active": {FIRST: 1}}))
        self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(self.base(SECOND) / "home"))
        for args in [("login",), ("logout",), ("--remote", "ws://127.0.0.1", "prompt")]:
            self.assertEqual(self.orca_launch(env, *args)["env"]["CODEX_HOME"], str(native[0]))
        status = self.run_orca_codex(env, "login", "status")
        self.assertIn("Logged in using ChatGPT", status.stderr)
        self.run_orca_codex({**env, "TEST_EMAIL": "reauth@example.test"}, "login", "--device-auth")
        self.assertEqual(helper.token_claims(native[0])["email"], "reauth@example.test")
        self.assertEqual(helper.token_claims(self.base(FIRST) / "home")["email"], self.email)

    def test_orca_profile_matching_preserves_explicit_and_unlinked_homes(self):
        env, native, roster = self.orca_fixture()
        schedule = self.base(FIRST).parent / "launch.json"
        settings = {"managed": True, "mode": "manual", "selected_account_id": SECOND,
                    "accounts": [roster[1]], "history_accounts": roster}
        schedule.write_text(json.dumps(settings))
        for extra in ({"TERM_PROGRAM": ""}, {"ORCA_USER_DATA_PATH": str(self.home / "another-orca")},
                      {"CODEX_HOME": str(self.home / "custom-home")}, {"OPENAI_BASE_URL": "https://proxy.example.test"},
                      {"CODEX_API_KEY": "synthetic-proxy-key"}, {"CAGE_CODEX_ACCOUNT_ID": "default"},
                      {"CAGE_CLI_ACCOUNT_CODEX": "default"}, {"CAGE_ACCOUNT_SCHEDULING": "off"}):
            with self.subTest(extra=extra):
                seen = self.orca_launch({**env, **extra}, "prompt")["env"]
                self.assertEqual(seen["CODEX_HOME"], extra.get("CODEX_HOME", str(native[0])))
        fixed = {**env, "CAGE_CODEX_ACCOUNT_ID": FIRST, "CAGE_CODEX_SUBJECT": self.subject}
        self.assertEqual(self.orca_launch(fixed, "prompt")["env"]["CODEX_HOME"], str(self.base(FIRST) / "home"))

        for history in ([roster[1]], [*roster, {"id": "3" * 24, "subject": self.subject}]):
            schedule.write_text(json.dumps({**settings, "history_accounts": history}))
            self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(native[0]))
        schedule.write_text(json.dumps(settings))
        marker = native[0] / ".orca-managed-home"
        marker.write_text("unknown")
        self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(native[0]))
        marker.write_text(native[0].parent.name + "\n")

        auth = native[0] / "auth.json"
        original = auth.read_bytes()
        other_tenant = json.loads(original)
        claims = {"email": self.email, "https://api.openai.com/auth": {"chatgpt_account_id": "other-tenant"}}
        part = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
        other_tenant["tokens"]["id_token"] = f"header.{part}.signature"
        auth.write_text(json.dumps(other_tenant))
        self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(native[0]))
        auth.write_bytes(original)
        auth.chmod(0o644)
        self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(native[0]))
        auth.chmod(0o600)
        alias = native[0].parent.parent / "6f86468c-eaf6-4931-b1c8-617f02166907" / "home"
        alias.parent.mkdir()
        alias.symlink_to(native[0], target_is_directory=True)
        self.assertEqual(self.orca_launch({**env, "CODEX_HOME": str(alias)}, "prompt")["env"]["CODEX_HOME"], str(alias))
        takeover = self.home / ".local/share/cage/magpie/takeover"
        takeover.parent.mkdir()
        takeover.write_text("1")
        self.assertEqual(self.orca_launch(env, "prompt")["env"]["CODEX_HOME"], str(native[0]))

    def test_orca_resume_keeps_rollouts_in_their_original_native_or_cage_home(self):
        env, native, roster = self.orca_fixture()
        schedule = self.base(FIRST).parent / "launch.json"
        schedule.write_text(json.dumps({"managed": True, "mode": "manual", "selected_account_id": FIRST,
                                        "accounts": [roster[0]], "history_accounts": roster}))
        cases = [
            ("0efd8832-fd5b-46bc-9991-7611c98d875f", native[0], "sessions/2026/10/04", ("resume",)),
            ("f39fe4c2-791a-4553-9a0c-0851eebe17b0", native[1], "archived_sessions", ("unarchive",)),
            ("6e673a4f-84f7-45a3-8ada-6ed47fa42be6", self.base(SECOND) / "home", "sessions/2026/10/04", ("exec", "resume")),
        ]
        for thread, home, directory, _ in cases:
            path = home / directory / f"rollout-2026-10-04T07-09-01-{thread}.jsonl"
            path.parent.mkdir(parents=True)
            path.write_text("{}\n")
        for thread, home, directory, args in cases:
            with self.subTest(thread=thread):
                seen = self.orca_launch(env, *args, thread)
                self.assertEqual(seen["env"]["CODEX_HOME"], str(home))
                self.assertEqual(seen["argv"][-len(args)-1:], [*args, thread])
                self.assertTrue((home / directory / f"rollout-2026-10-04T07-09-01-{thread}.jsonl").exists())


if __name__ == "__main__":
    unittest.main()
