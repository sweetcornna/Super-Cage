#!/usr/bin/python3
"""Unit tests for the universal guest Agent CLI runtime."""
import base64
import json
import os
from pathlib import Path
import socket
import stat
import subprocess
import sys
import tempfile
import time
import unittest

RUNTIME = Path(__file__).with_name("agent-runtime.py")


def make_jwt(claims):
    header = base64.urlsafe_b64encode(b'{"alg":"RS256"}').decode().rstrip("=")
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    return f"{header}.{body}.signature-secret"


def free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class AgentRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = Path(self.tmp.name)
        self.env = {**os.environ, "HOME": str(self.home), "PATH": f"{self.home / '.local/bin'}:{os.environ.get('PATH', '')}"}

    def tearDown(self):
        # Ensure any mock servers started by a test are stopped cleanly.
        for stop_cmd in ("t3code-stop", "magpie-stop", "herdr-stop"):
            try:
                self.run_rt(stop_cmd, {}, check=False)
            except Exception:
                pass
        self.tmp.cleanup()

    def run_rt(self, subcmd, payload, extra_args=(), check=True, env=None):
        res = subprocess.run(
            [sys.executable, "-I", str(RUNTIME), subcmd, *extra_args],
            input=json.dumps(payload),
            text=True,
            capture_output=True,
            env=env or self.env,
            timeout=20,
        )
        if check and res.returncode != 0:
            self.fail(f"{subcmd} failed ({res.returncode}): stdout={res.stdout} stderr={res.stderr}")
        return res

    def test_fast_npm_package_version_and_prerequisite_cache_reuse(self):
        # Create a fake npm-installed codex CLI whose binary would fail if executed,
        # proving the fast package.json reader resolves version in <1ms without spawning Node.
        pkg_dir = self.home / ".local/lib/node_modules/@openai/codex"
        bin_dir = pkg_dir / "bin"
        bin_dir.mkdir(parents=True)
        (pkg_dir / "package.json").write_text(json.dumps({"name": "@openai/codex", "version": "1.2.3"}))
        real_bin = bin_dir / "codex.js"
        real_bin.write_text("#!/bin/sh\necho should-not-run >&2; exit 99\n")
        real_bin.chmod(0o755)
        link_dir = self.home / ".local/bin"
        link_dir.mkdir(parents=True)
        (link_dir / "codex").symlink_to(real_bin)

        # Create a counting fake `uv` prerequisite to verify warm probe-all spawns ZERO subprocesses.
        counter = self.home / "uv-calls.log"
        fake_uv = link_dir / "uv"
        fake_uv.write_text(f"#!/bin/sh\necho x >> '{counter}'\necho 'uv 0.6.12'\n")
        fake_uv.chmod(0o755)

        specs = [
            {"id": "codex", "binary": "codex", "version_args": ["--version"], "isolation": {"mode": "env_dir", "default_config_paths": ["~/.codex"]}},
            {"id": "missing-agent", "binary": "definitely-not-installed-xyz", "version_args": ["--version"]},
        ]
        res1 = self.run_rt("probe-all", {"specs": specs})
        data1 = json.loads(res1.stdout)
        self.assertTrue(data1["agents"]["codex"]["installed"])
        self.assertEqual(data1["agents"]["codex"]["version"], "1.2.3")
        self.assertFalse(data1["agents"]["missing-agent"]["installed"])
        self.assertTrue(data1["prerequisites"]["uv"]["available"])
        self.assertEqual(data1["prerequisites"]["uv"]["version"], "0.6.12")
        self.assertEqual(len(counter.read_text().splitlines()), 1)

        cache_file = self.home / ".cache/cage/agent-probe-cache.json"
        self.assertTrue(cache_file.is_file())
        cache_data = json.loads(cache_file.read_text())
        self.assertTrue(any(k.startswith("prereq:uv:") for k in cache_data))

        # Warm probe-all must hit cache for prerequisites and CLI versions without invoking fake_uv again.
        res2 = self.run_rt("probe-all", {"specs": specs})
        data2 = json.loads(res2.stdout)
        self.assertEqual(data2["prerequisites"]["uv"]["version"], "0.6.12")
        self.assertEqual(data2["agents"]["codex"]["version"], "1.2.3")
        self.assertEqual(len(counter.read_text().splitlines()), 1)

    def test_codex_file_credentials_store_and_5h_7d_quota_telemetry(self):
        account_id = "a" * 24
        codex_spec = {
            "id": "codex",
            "name": "OpenAI Codex",
            "binary": "codex",
            "isolation": {"mode": "env_dir", "env_var": "CODEX_HOME", "default_config_paths": ["~/.codex"]},
            "auth": {"strip_env_on_oauth": ["OPENAI_API_KEY"]},
        }
        codex_cfg = self.home / ".local/share/cage/agent-accounts/codex" / account_id / "config"
        codex_cfg.mkdir(parents=True)
        # Pre-seed a config.toml with user settings and a [table] section to verify top-level idempotent insertion.
        (codex_cfg / "config.toml").write_text('model = "o3"\n\n[mcp_servers.demo]\ncommand = "demo"\n')

        prep = json.loads(self.run_rt("prepare-session", {"spec": codex_spec, "account_id": account_id}).stdout)
        cfg_text = (codex_cfg / "config.toml").read_text()
        self.assertIn('cli_auth_credentials_store = "file"', cfg_text)
        self.assertLess(cfg_text.index('cli_auth_credentials_store = "file"'), cfg_text.index('[mcp_servers.demo]'))
        self.assertIn('model = "o3"', cfg_text)
        self.assertIn('[mcp_servers.demo]', cfg_text)
        self.assertTrue(Path(prep["prepared_stamp"]).is_file())
        self.assertEqual(prep["env_exports"], {"CODEX_HOME": str(codex_cfg)})

        # Idempotent re-run must not duplicate cli_auth_credentials_store.
        self.run_rt("prepare-session", {"spec": codex_spec, "account_id": account_id})
        self.assertEqual((codex_cfg / "config.toml").read_text().count("cli_auth_credentials_store"), 1)

        # Write auth.json + nested sessions/YYYY/MM/DD/rollout-*.jsonl with >64KB filler + rate_limits.
        secret_token = make_jwt({
            "email": "dev@example.com",
            "https://api.openai.com/auth": {"chatgpt_plan_type": "pro", "chatgpt_account_id": "org-123"},
            "exp": 1999999999,
        })
        (codex_cfg / "auth.json").write_text(json.dumps({
            "auth_mode": "chatgpt",
            "access_token": secret_token,
            "refresh_token": "rt_super_secret_token",
        }))
        now = int(time.time())
        old_dir = codex_cfg / "sessions/2025/01/01"
        old_dir.mkdir(parents=True)
        (old_dir / "rollout-2025-01-01T00-00-00-old.jsonl").write_text(
            json.dumps({"rate_limits": {"primary": {"used_percent": 99, "resets_at": now + 9999}}}) + "\n"
        )
        new_dir = codex_cfg / "sessions/2026/11/23"
        new_dir.mkdir(parents=True)
        rollout = new_dir / "rollout-2026-11-23T09-30-00-latest.jsonl"
        filler = (json.dumps({"type": "message", "text": "TOP_SECRET_USER_PROMPT_" + ("x" * 200)}) + "\n") * 400
        event = json.dumps({
            "timestamp": now - 10,
            "type": "event_msg",
            "payload": {
                "type": "token_count",
                "prompt": "NEVER_LEAK_THIS_PROMPT",
                "rate_limits": {
                    "primary": {"used_percent": 27.5, "resets_at": now + 7200},
                    "secondary": {"used_percent": 61.0, "resets_at": now + 86400 * 3},
                },
            },
        })
        rollout.write_text(filler + event + "\n")

        snap = json.loads(self.run_rt("snapshot", {"spec": codex_spec, "account_id": account_id}).stdout)
        self.assertTrue(snap["logged_in"])
        self.assertEqual(snap["email"], "dev@example.com")
        self.assertEqual(snap["plan"], "pro")
        self.assertIn("quota", snap)
        self.assertEqual(snap["quota"]["five_hour"]["used_percentage"], 27.5)
        self.assertEqual(snap["quota"]["five_hour"]["resets_at"], now + 7200)
        self.assertEqual(snap["quota"]["seven_day"]["used_percentage"], 61.0)
        self.assertEqual(snap["quota"]["seven_day"]["resets_at"], now + 86400 * 3)
        dumped = json.dumps(snap)
        self.assertNotIn("rt_super_secret_token", dumped)
        self.assertNotIn("signature-secret", dumped)
        self.assertNotIn("NEVER_LEAK_THIS_PROMPT", dumped)
        self.assertNotIn("TOP_SECRET_USER_PROMPT", dumped)

    def test_false_positive_login_prevention_and_copilot_subdir(self):
        account_id = "d" * 24

        # 1. Gemini: google_accounts.json alone WITHOUT oauth_creds.json token must NOT be logged_in.
        gemini_cfg = self.home / ".local/share/cage/agent-accounts/gemini" / account_id / ".gemini"
        gemini_cfg.mkdir(parents=True)
        (gemini_cfg / "google_accounts.json").write_text(json.dumps({"active": "stale@example.com"}))
        gemini_spec = {"id": "gemini", "isolation": {"mode": "env_home", "env_var": "GEMINI_CLI_HOME", "config_subdir": ".gemini"}}
        gsnap1 = json.loads(self.run_rt("snapshot", {"spec": gemini_spec, "account_id": account_id}).stdout)
        self.assertFalse(gsnap1["logged_in"])

        # With valid oauth_creds.json token, Gemini becomes logged_in.
        (gemini_cfg / "oauth_creds.json").write_text(json.dumps({
            "access_token": "ya29.secret",
            "id_token": make_jwt({"email": "gem@example.com"}),
        }))
        gsnap2 = json.loads(self.run_rt("snapshot", {"spec": gemini_spec, "account_id": account_id}).stdout)
        self.assertTrue(gsnap2["logged_in"])
        self.assertEqual(gsnap2["email"], "stale@example.com")
        self.assertNotIn("ya29.secret", json.dumps(gsnap2))

        # 2. Copilot: github-copilot/hosts.json detection.
        cop_cfg = self.home / ".local/share/cage/agent-accounts/copilot" / account_id / "config/github-copilot"
        cop_cfg.mkdir(parents=True)
        (cop_cfg / "hosts.json").write_text(json.dumps({"github.com": {"user": "octocat", "oauth_token": "ghu_secret"}}))
        cop_spec = {"id": "copilot", "isolation": {"mode": "env_dir", "env_var": "COPILOT_HOME"}}
        csnap = json.loads(self.run_rt("snapshot", {"spec": cop_spec, "account_id": account_id}).stdout)
        self.assertTrue(csnap["logged_in"])
        self.assertEqual(csnap["username"], "octocat")
        self.assertNotIn("ghu_secret", json.dumps(csnap))

        # 3. Generic JSON without auth fields & .env with only comments/empty value must NOT be logged_in.
        vibe_cfg = self.home / ".local/share/cage/agent-accounts/vibe" / account_id / "config"
        vibe_cfg.mkdir(parents=True)
        (vibe_cfg / "auth.json").write_text(json.dumps({"theme": "dark", "version": 2, "token": ""}))
        (vibe_cfg / ".env").write_text("# comment only\nMISTRAL_API_KEY=\n")
        vibe_spec = {"id": "vibe", "isolation": {"mode": "env_dir", "env_var": "VIBE_HOME"}, "auth": {"credential_files": [".env", "auth.json"]}}
        vsnap1 = json.loads(self.run_rt("snapshot", {"spec": vibe_spec, "account_id": account_id}).stdout)
        self.assertFalse(vsnap1["logged_in"])

        # Populating a real key in .env makes it logged_in.
        (vibe_cfg / ".env").write_text("MISTRAL_API_KEY=ms-real-key\n")
        vsnap2 = json.loads(self.run_rt("snapshot", {"spec": vibe_spec, "account_id": account_id}).stdout)
        self.assertTrue(vsnap2["logged_in"])

        # Generic JSON with non-empty token field makes it logged_in.
        cursor_cfg = self.home / ".local/share/cage/agent-accounts/cursor" / account_id / "config"
        cursor_cfg.mkdir(parents=True)
        (cursor_cfg / "auth.json").write_text(json.dumps({"theme": "light"}))
        cursor_spec = {"id": "cursor", "isolation": {"mode": "env_dir", "env_var": "CURSOR_CONFIG_DIR"}, "auth": {"credential_files": ["auth.json"]}}
        self.assertFalse(json.loads(self.run_rt("snapshot", {"spec": cursor_spec, "account_id": account_id}).stdout)["logged_in"])
        (cursor_cfg / "auth.json").write_text(json.dumps({"access_token": "tok_live", "email": "u@cursor.com"}))
        cur_snap = json.loads(self.run_rt("snapshot", {"spec": cursor_spec, "account_id": account_id}).stdout)
        self.assertTrue(cur_snap["logged_in"])
        self.assertEqual(cur_snap["email"], "u@cursor.com")
        self.assertNotIn("tok_live", json.dumps(cur_snap))

        # 4. OpenCode XDG multi-provider detection.
        oc_data = self.home / ".local/share/cage/agent-accounts/opencode" / account_id / "data/opencode"
        oc_data.mkdir(parents=True)
        (oc_data / "auth.json").write_text(json.dumps({"anthropic": {"key": "sk-secret"}, "openai": {"key": "sk-2"}}))
        oc_spec = {"id": "opencode", "isolation": {"mode": "xdg"}}
        osnap = json.loads(self.run_rt("snapshot", {"spec": oc_spec, "account_id": account_id}).stdout)
        self.assertTrue(osnap["logged_in"])
        self.assertIn("anthropic", osnap["provider"])
        self.assertNotIn("sk-secret", json.dumps(osnap))

    def test_xdg_shared_resource_symlinks_use_app_subdir(self):
        account_id = "e" * 24
        (self.home / ".config/amp").mkdir(parents=True)
        (self.home / ".config/amp/AGENTS.md").write_text("# Amp Shared Rules\n")
        amp_spec = {
            "id": "amp",
            "name": "Amp",
            "binary": "amp",
            "isolation": {
                "mode": "xdg",
                "default_config_paths": ["~/.config/amp", "~/.local/share/amp"],
                "shared_resources": ["AGENTS.md"],
            },
            "auth": {"strip_env_on_oauth": ["AMP_API_KEY"]},
        }
        prep = json.loads(self.run_rt("prepare-session", {"spec": amp_spec, "account_id": account_id}).stdout)
        xdg_cfg = Path(prep["config_dir"])
        self.assertTrue((xdg_cfg / "amp/AGENTS.md").is_symlink())
        self.assertEqual((xdg_cfg / "amp/AGENTS.md").resolve(), (self.home / ".config/amp/AGENTS.md").resolve())
        self.assertFalse((xdg_cfg / "AGENTS.md").exists())

    def test_pure_shell_shim_api_key_update_meta_and_secret_isolation(self):
        account_id = "b" * 24
        spec = {
            "id": "codex",
            "name": "OpenAI Codex",
            "binary": "codex",
            "isolation": {"mode": "env_dir", "env_var": "CODEX_HOME", "default_config_paths": ["~/.codex"], "shared_resources": ["AGENTS.md"]},
            "auth": {"api_key_env_vars": ["OPENAI_API_KEY"], "base_url_env_var": "OPENAI_BASE_URL", "model_env_var": "OPENAI_MODEL", "strip_env_on_oauth": ["OPENAI_API_KEY"]},
        }
        (self.home / ".codex").mkdir(parents=True)
        (self.home / ".codex/AGENTS.md").write_text("# Shared Team Rules\n")

        meta = json.loads(self.run_rt("set-api-key", {
            "spec": spec,
            "account_id": account_id,
            "api_key": "sk-test-secret-9876",
            "base_url": "https://gateway.example.com/v1",
            "model": "o3",
            "provider": "GatewayA",
        }).stdout)
        self.assertTrue(meta["logged_in"])
        self.assertEqual(meta["key_hint"], "sk-t...9876")
        self.assertNotIn("sk-test-secret-9876", json.dumps(meta))
        cred_path = self.home / ".local/share/cage/agent-accounts/codex" / account_id / "credentials.json"
        env_path = self.home / ".local/share/cage/agent-accounts/codex" / account_id / "profile.env"
        self.assertEqual(stat.S_IMODE(cred_path.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(env_path.stat().st_mode), 0o600)

        # Update API key metadata WITHOUT resending raw api_key.
        updated = json.loads(self.run_rt("update-api-key-meta", {
            "spec": spec,
            "account_id": account_id,
            "base_url": "https://gateway2.example.com/v1",
            "model": "gpt-5",
            "provider": "GatewayB",
        }).stdout)
        self.assertEqual(updated["base_url"], "https://gateway2.example.com/v1")
        self.assertEqual(updated["model"], "gpt-5")
        self.assertEqual(updated["provider"], "GatewayB")
        self.assertEqual(updated["key_hint"], "sk-t...9876")
        self.assertNotIn("sk-test-secret-9876", json.dumps(updated))
        self.assertEqual(stat.S_IMODE(cred_path.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(env_path.stat().st_mode), 0o600)

        # Install a fake pure-shell codex binary so the pure-shell shim executes with ZERO Python!
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        fake = bin_dir / "codex"
        fake.write_text(
            "#!/bin/sh\n"
            'printf \'{"CODEX_HOME":"%s","OPENAI_API_KEY":"%s","OPENAI_BASE_URL":"%s","OPENAI_MODEL":"%s","ARGS":"%s"}\\n\' '
            '"$CODEX_HOME" "$OPENAI_API_KEY" "$OPENAI_BASE_URL" "$OPENAI_MODEL" "$*"\n'
        )
        fake.chmod(0o755)

        prep = json.loads(self.run_rt("prepare-session", {"spec": spec, "account_id": account_id}).stdout)
        shim = Path(prep["shim_dir"]) / "codex"
        self.assertTrue(shim.is_file())
        self.assertIn("profile.env", shim.read_text())
        self.assertTrue((Path(prep["config_dir"]) / "AGENTS.md").is_symlink())
        self.assertNotIn("sk-test-secret-9876", json.dumps(prep))

        # Execute via pure-shell shim — verify updated metadata and preserved secret key!
        run_env = {**self.env, "PATH": f"{prep['shim_dir']}:{self.env['PATH']}", "CAGE_CLI_ACCOUNT_ID": account_id}
        res = subprocess.run([str(shim), "--flag", "hello"], text=True, capture_output=True, env=run_env, timeout=10)
        self.assertEqual(res.returncode, 0, res.stderr)
        seen = json.loads(res.stdout)
        self.assertEqual(seen["OPENAI_API_KEY"], "sk-test-secret-9876")
        self.assertEqual(seen["OPENAI_BASE_URL"], "https://gateway2.example.com/v1")
        self.assertEqual(seen["OPENAI_MODEL"], "gpt-5")
        self.assertEqual(seen["CODEX_HOME"], prep["config_dir"])
        self.assertEqual(seen["ARGS"], "--flag hello")

        # OAuth profile strips stray OPENAI_API_KEY from shell env via pure-shell profile.env.
        oauth_id = "c" * 24
        prep2 = json.loads(self.run_rt("prepare-session", {"spec": spec, "account_id": oauth_id}).stdout)
        run_env2 = {**self.env, "PATH": f"{prep2['shim_dir']}:{self.env['PATH']}", "CAGE_CLI_ACCOUNT_ID": oauth_id, "OPENAI_API_KEY": "sk-stray-shell-key"}
        res2 = subprocess.run([str(Path(prep2["shim_dir"]) / "codex")], text=True, capture_output=True, env=run_env2, timeout=10)
        self.assertEqual(res2.returncode, 0, res2.stderr)
        seen2 = json.loads(res2.stdout)
        self.assertEqual(seen2["OPENAI_API_KEY"], "")
        self.assertEqual(seen2["CODEX_HOME"], prep2["config_dir"])

        # Clearing credentials removes secret from profile.env immediately.
        self.run_rt("clear-credentials", {"spec": spec, "account_id": account_id})
        self.assertNotIn("sk-test-secret-9876", env_path.read_text())

        # flag_dir mode passes config_flag via pure-shell shim without Python!
        flag_id = "f" * 24
        flag_spec = {
            "id": "mytool",
            "name": "My Tool",
            "binary": "mytool",
            "isolation": {"mode": "flag_dir", "config_flag": "--config-dir"},
            "auth": {},
        }
        fake_flag = bin_dir / "mytool"
        fake_flag.write_text('#!/bin/sh\nprintf "%s\\n" "$*"\n')
        fake_flag.chmod(0o755)
        fprep = json.loads(self.run_rt("prepare-session", {"spec": flag_spec, "account_id": flag_id}).stdout)
        fenv = {**self.env, "PATH": f"{fprep['shim_dir']}:{self.env['PATH']}", "CAGE_CLI_ACCOUNT_ID": flag_id}
        fres = subprocess.run([str(Path(fprep["shim_dir"]) / "mytool"), "run"], text=True, capture_output=True, env=fenv, timeout=10)
        self.assertEqual(fres.returncode, 0, fres.stderr)
        self.assertEqual(fres.stdout.strip(), f"--config-dir {fprep['config_dir']} run")

    def test_uninstall_script_user_bin_and_refuses_system_bin(self):
        # User-owned script binary under ~/.factory/bin gets removed cleanly.
        factory_bin = self.home / ".factory/bin"
        factory_bin.mkdir(parents=True)
        droid = factory_bin / "droid"
        droid.write_text("#!/bin/sh\necho droid 1.0.0\n")
        droid.chmod(0o755)
        spec = {"id": "droid", "binary": "droid"}
        res = json.loads(self.run_rt("uninstall", {"spec": spec, "channel": {"kind": "script"}}).stdout)
        self.assertTrue(res["ok"])
        self.assertFalse(droid.exists())

        # System binary (e.g. /bin/sh) must NEVER be deleted and must return an error.
        sys_spec = {"id": "sysbin", "binary": "sh"}
        fail = self.run_rt("uninstall", {"spec": sys_spec, "channel": {"kind": "script"}}, check=False)
        self.assertNotEqual(fail.returncode, 0)
        self.assertTrue(Path("/bin/sh").exists())

    def test_t3code_lifecycle_mock_server_and_one_time_pair_url(self):
        port = free_port()
        other_port = free_port()
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        env_dump = self.home / "t3-seen-env.json"

        # Install fake codex and opencode binaries so we can prove t3 serve invokes
        # each provider through its own pure-shell shim without parent XDG/CODEX_HOME pollution.
        fake_codex = bin_dir / "codex"
        fake_codex.write_text('#!/bin/sh\nprintf \'{"CODEX_HOME":"%s","XDG_CONFIG_HOME":"%s"}\\n\' "$CODEX_HOME" "$XDG_CONFIG_HOME"\n')
        fake_codex.chmod(0o755)
        fake_opencode = bin_dir / "opencode"
        fake_opencode.write_text('#!/bin/sh\nprintf \'{"XDG_CONFIG_HOME":"%s","XDG_DATA_HOME":"%s"}\\n\' "$XDG_CONFIG_HOME" "$XDG_DATA_HOME"\n')
        fake_opencode.chmod(0o755)

        # Mock `t3` CLI WITHOUT `pair` subcommand (tests pair.once fallback + log redaction).
        mock_t3 = bin_dir / "t3"
        mock_t3.write_text(
            f"#!{sys.executable}\n"
            "import http.server, json, os, signal, socketserver, sys\n"
            "args = sys.argv[1:]\n"
            "if args == ['--version']:\n"
            "    print('t3 0.9.4')\n"
            "    sys.exit(0)\n"
            "if args == ['--help']:\n"
            "    print('Usage: t3 <command>\\nCommands:\\n  serve  Start server\\n')\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['serve', '--help']:\n"
            "    print('Options: --host <host> --port <port> --no-browser --base-dir <dir>')\n"
            "    sys.exit(0)\n"
            "port = int(os.environ.get('T3CODE_PORT', '13773'))\n"
            "for i, a in enumerate(args):\n"
            "    if a == '--port' and i + 1 < len(args):\n"
            "        port = int(args[i + 1])\n"
            f"with open({str(env_dump)!r}, 'w') as f:\n"
            "    json.dump({\n"
            "        'args': args,\n"
            "        'PATH': os.environ.get('PATH', ''),\n"
            "        'CODEX_HOME': os.environ.get('CODEX_HOME', ''),\n"
            "        'XDG_CONFIG_HOME': os.environ.get('XDG_CONFIG_HOME', ''),\n"
            "        'XDG_DATA_HOME': os.environ.get('XDG_DATA_HOME', ''),\n"
            "        'XDG_STATE_HOME': os.environ.get('XDG_STATE_HOME', ''),\n"
            "        'CAGE_CLAUDE_ACCOUNT_ID': os.environ.get('CAGE_CLAUDE_ACCOUNT_ID', ''),\n"
            "        'CAGE_CLAUDE_SUBJECT': os.environ.get('CAGE_CLAUDE_SUBJECT', ''),\n"
            "        'CAGE_CLI_ACCOUNT_CODEX': os.environ.get('CAGE_CLI_ACCOUNT_CODEX', ''),\n"
            "        'CAGE_CLI_ACCOUNT_OPENCODE': os.environ.get('CAGE_CLI_ACCOUNT_OPENCODE', ''),\n"
            "        'CAGE_CLI_ACCOUNT_ID': os.environ.get('CAGE_CLI_ACCOUNT_ID', ''),\n"
            "        'T3CODE_NO_BROWSER': os.environ.get('T3CODE_NO_BROWSER', ''),\n"
            "    }, f)\n"
            "class H(http.server.BaseHTTPRequestHandler):\n"
            "    def do_GET(self):\n"
            "        self.send_response(200); self.end_headers(); self.wfile.write(b'ok')\n"
            "    def log_message(self, *a): pass\n"
            "socketserver.TCPServer.allow_reuse_address = True\n"
            "srv = socketserver.TCPServer(('127.0.0.1', port), H)\n"
            "signal.signal(signal.SIGTERM, lambda *a: sys.exit(0))\n"
            "print('T3 Code server ready on 127.0.0.1', flush=True)\n"
            "print(f'T3 Code listening. Pair at http://127.0.0.1:{port}/pair#token=secret_boot_tok_123', flush=True)\n"
            "print('Token: raw_secret_token_line_456', flush=True)\n"
            "print('App link: t3://pair?code=deep_secret_789', flush=True)\n"
            "print('Alt link: t3code://pair?code=deep_secret_999', flush=True)\n"
            "print('Leak sk-supersecretkey12345 and eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig', flush=True)\n"
            "print('Google key AIzaSyB1234567890abcdefghijklmno and GitHub ghp_1234567890abcdefghijklmnopqrst', flush=True)\n"
            "print('Config api_key=my_custom_vendor_key_999', flush=True)\n"
            "print('{\"api_key\": \"json_key_111\", \"secret\": \"json_sec_222\", \"authorization\": \"Basic json_auth_333\", \"cookie\": \"sid=json_ck_444\"}', flush=True)\n"
            "print('▀ ▄ ▀ ▄', flush=True)\n"
            "print('████████████  ▄▄▄▄▄▄  ▀▀▀▀▀▀', flush=True)\n"
            "print('░▒▓█▀▄▌▐████████████', flush=True)\n"
            "srv.serve_forever()\n"
        )
        mock_t3.chmod(0o755)

        # 1. Status before start.
        st0 = json.loads(self.run_rt("t3code-status", {"port": port}).stdout)
        self.assertTrue(st0["installed"])
        self.assertEqual(st0["binary_path"], str(mock_t3))
        self.assertEqual(st0["version"], "0.9.4")
        self.assertFalse(st0["running"])
        self.assertEqual(st0["pid"], 0)
        self.assertEqual(st0["port"], port)
        self.assertFalse(st0["has_pair_url"])

        # 2. Start with bound claude (with subject) + codex (env_dir) + opencode (xdg).
        codex_id = "1" * 24
        claude_id = "2" * 24
        opencode_id = "3" * 24
        claude_subject = "a" * 64
        codex_spec = {"id": "codex", "name": "OpenAI Codex", "binary": "codex", "isolation": {"mode": "env_dir", "env_var": "CODEX_HOME"}, "auth": {}}
        opencode_spec = {"id": "opencode", "name": "OpenCode", "binary": "opencode", "isolation": {"mode": "xdg"}, "auth": {}}
        # Ensure clean parent XDG vars before start so we prove t3code-start does NOT inject them.
        start_env = {k: v for k, v in self.env.items() if not k.startswith("XDG_") and k != "CODEX_HOME"}
        started = json.loads(self.run_rt("t3code-start", {
            "port": port,
            "workspace": str(self.home),
            "bindings": {
                "claude": {"guest_id": claude_id, "subject": claude_subject},
                "codex": {"guest_id": codex_id, "spec": codex_spec},
                "opencode": {"guest_id": opencode_id, "spec": opencode_spec},
            },
        }, env=start_env).stdout)
        self.assertTrue(started["installed"])
        self.assertEqual(started["binary_path"], str(mock_t3))
        self.assertTrue(started["running"])
        self.assertEqual(started["port"], port)
        self.assertEqual(started["version"], "0.9.4")
        self.assertTrue(started["has_pair_url"])
        self.assertNotIn("secret_boot_tok_123", json.dumps(started))

        # Verify state (0600, ZERO tokens), log (0600, redacted secrets & QR), and pair.once (0600).
        state_file = self.home / ".local/share/cage/t3code/runtime.json"
        log_file = self.home / ".local/share/cage/t3code/server.log"
        pair_file = self.home / ".local/share/cage/t3code/pair.once"
        self.assertEqual(stat.S_IMODE(state_file.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(log_file.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(pair_file.stat().st_mode), 0o600)

        state_raw = state_file.read_text()
        state_obj = json.loads(state_raw)
        self.assertEqual(state_obj["pid"], started["pid"])
        self.assertGreater(state_obj["supervisor_pid"], 0)
        self.assertEqual(state_obj["port"], port)
        self.assertEqual(state_obj["version"], "0.9.4")
        self.assertEqual(state_obj["binary"], str(mock_t3))
        self.assertNotIn("pair_url", state_obj)
        self.assertNotIn("secret_boot_tok_123", state_raw)

        log_text = log_file.read_text()
        self.assertIn("T3 Code server ready on 127.0.0.1", log_text)
        for forbidden in (
            "secret_boot_tok_123",
            "raw_secret_token_line_456",
            "deep_secret_789",
            "deep_secret_999",
            "sk-supersecretkey12345",
            "eyJhbGciOiJIUzI1NiJ9",
            "AIzaSyB1234567890",
            "ghp_1234567890",
            "my_custom_vendor_key_999",
            "json_key_111",
            "json_sec_222",
            "json_auth_333",
            "json_ck_444",
            "▀ ▄",
            "t3://",
            "t3code://",
            "pair#token=",
            "Token:",
            "████",
            "░▒▓",
        ):
            self.assertNotIn(forbidden, log_text, f"secret leaked in server.log: {forbidden}")

        # Verify bindings env on t3 serve: Claude subject + per-agent vars WITHOUT polluting parent XDG_* or CODEX_HOME.
        seen_env = json.loads(env_dump.read_text())
        self.assertIn("serve", seen_env["args"])
        self.assertIn("--no-browser", seen_env["args"])
        self.assertEqual(seen_env["CAGE_CLAUDE_ACCOUNT_ID"], claude_id)
        self.assertEqual(seen_env["CAGE_CLAUDE_SUBJECT"], claude_subject)
        self.assertEqual(seen_env["CAGE_CLI_ACCOUNT_CODEX"], codex_id)
        self.assertEqual(seen_env["CAGE_CLI_ACCOUNT_OPENCODE"], opencode_id)
        self.assertEqual(seen_env["CAGE_CLI_ACCOUNT_ID"], "")
        self.assertEqual(seen_env["CODEX_HOME"], "")
        self.assertEqual(seen_env["XDG_CONFIG_HOME"], "")
        self.assertEqual(seen_env["XDG_DATA_HOME"], "")
        self.assertEqual(seen_env["XDG_STATE_HOME"], "")
        self.assertEqual(seen_env["T3CODE_NO_BROWSER"], "1")

        # Verify invoking codex and opencode using t3 serve's exact env routes each through its own shim!
        child_env = {**start_env, **seen_env}
        child_env.pop("args", None)
        codex_out = json.loads(subprocess.check_output(["codex"], env=child_env, text=True, timeout=10))
        self.assertTrue(codex_out["CODEX_HOME"].endswith(f"/codex/{codex_id}/config"))
        self.assertEqual(codex_out["XDG_CONFIG_HOME"], "")
        opencode_out = json.loads(subprocess.check_output(["opencode"], env=child_env, text=True, timeout=10))
        self.assertTrue(opencode_out["XDG_CONFIG_HOME"].endswith(f"/opencode/{opencode_id}/config"))
        self.assertTrue(opencode_out["XDG_DATA_HOME"].endswith(f"/opencode/{opencode_id}/data"))

        # 3. Status while running: matches requested port, rejects port mismatch!
        st1 = json.loads(self.run_rt("t3code-status", {"port": port}).stdout)
        self.assertTrue(st1["installed"])
        self.assertTrue(st1["running"])
        self.assertEqual(st1["pid"], started["pid"])
        self.assertEqual(st1["port"], port)
        self.assertTrue(st1["has_pair_url"])
        self.assertNotIn("secret_boot_tok_123", json.dumps(st1))
        self.assertNotIn("pair_url", st1)

        st_mismatch = json.loads(self.run_rt("t3code-status", {"port": other_port}).stdout)
        self.assertTrue(st_mismatch["installed"])
        self.assertFalse(st_mismatch["running"])
        self.assertEqual(st_mismatch["pid"], 0)
        self.assertEqual(st_mismatch["port"], other_port)
        self.assertFalse(st_mismatch["has_pair_url"])

        # 4. t3code-open-url with consume=False preserves pair.once; consume=True (default) unlinks pair.once!
        peek = json.loads(self.run_rt("t3code-open-url", {"port": port, "consume": False}).stdout)
        self.assertTrue(peek["paired_bootstrap"])
        self.assertEqual(peek["url"], f"http://127.0.0.1:{port}/pair#token=secret_boot_tok_123")
        self.assertTrue(pair_file.is_file())

        open1 = json.loads(self.run_rt("t3code-open-url", {"port": port}).stdout)
        self.assertTrue(open1["paired_bootstrap"])
        self.assertEqual(open1["url"], f"http://127.0.0.1:{port}/pair#token=secret_boot_tok_123")
        self.assertFalse(pair_file.exists())

        open2 = json.loads(self.run_rt("t3code-open-url", {"port": port}).stdout)
        self.assertFalse(open2["paired_bootstrap"])
        self.assertEqual(open2["url"], f"http://127.0.0.1:{port}/")

        st2 = json.loads(self.run_rt("t3code-status", {"port": port}).stdout)
        self.assertTrue(st2["running"])
        self.assertFalse(st2["has_pair_url"])

        # 5. Restart replaces PID cleanly, returns installed=True, and yields fresh pair.once.
        restarted = json.loads(self.run_rt("t3code-restart", {
            "port": port,
            "workspace": str(self.home),
            "bindings": {"claude": {"guest_id": claude_id, "subject": claude_subject}},
        }).stdout)
        self.assertTrue(restarted["installed"])
        self.assertEqual(restarted["binary_path"], str(mock_t3))
        self.assertTrue(restarted["running"])
        self.assertNotEqual(restarted["pid"], started["pid"])
        self.assertTrue(restarted["has_pair_url"])

        # 5b. Stop with mismatched port MUST NOT stop the running instance!
        skip_stop = json.loads(self.run_rt("t3code-stop", {"port": other_port}).stdout)
        self.assertTrue(skip_stop.get("skipped_port_mismatch"))
        st_still = json.loads(self.run_rt("t3code-status", {"port": port}).stdout)
        self.assertTrue(st_still["running"])

        # 5c. Verify runtime.json records pid_identity & missing identity does NOT match on liveness alone.
        st_json = json.loads(state_file.read_text())
        self.assertTrue(bool(st_json.get("pid_identity")))
        # Write a copy without pid_identity and verify t3code-status refuses to trust an unverified PID.
        unverified = dict(st_json)
        unverified["pid_identity"] = ""
        state_file.write_text(json.dumps(unverified))
        st_unverified = json.loads(self.run_rt("t3code-status", {"port": port}).stdout)
        self.assertFalse(st_unverified["running"])
        # Restore valid state_file with pid_identity before stopping cleanly.
        state_file.write_text(json.dumps(st_json))

        # 6. Stop terminates managed server & supervisor cleanly and cleans runtime.json + pair.once.
        stopped = json.loads(self.run_rt("t3code-stop", {"port": port}).stdout)
        self.assertFalse(stopped["running"])
        st3 = json.loads(self.run_rt("t3code-status", {"port": port}).stdout)
        self.assertFalse(st3["running"])
        self.assertEqual(st3["pid"], 0)
        self.assertFalse(state_file.exists())
        self.assertFalse(pair_file.exists())

    def test_t3code_pair_minting_and_bounded_log_cap(self):
        port = free_port()
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        mint_counter = self.home / "mint-count.txt"
        mint_args_dump = self.home / "mint-args.json"

        # Mock `t3` CLI WITH `t3 pair` support AND noisy >100 KiB startup output.
        mock_t3 = bin_dir / "t3"
        mock_t3.write_text(
            f"#!{sys.executable}\n"
            "import http.server, json, os, pathlib, signal, socketserver, sys\n"
            "args = sys.argv[1:]\n"
            "if args == ['--version']:\n"
            "    print('t3 0.10.0')\n"
            "    sys.exit(0)\n"
            "if args == ['--help']:\n"
            "    print('Usage: t3 <command>\\nCommands:\\n  serve  Start server\\n  pair   Mint pairing URL\\n')\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['serve', '--help']:\n"
            "    print('Options: --host <host> --port <port> --no-browser --base-dir <dir>')\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['pair', '--help']:\n"
            "    print('Options: --host <host> --port <port> --base-dir <dir>')\n"
            "    sys.exit(0)\n"
            "port = int(os.environ.get('T3CODE_PORT', '13773'))\n"
            "for i, a in enumerate(args):\n"
            "    if a == '--port' and i + 1 < len(args):\n"
            "        port = int(args[i + 1])\n"
            "if args and args[0] == 'pair':\n"
            f"    cf = pathlib.Path({str(mint_counter)!r})\n"
            "    n = int(cf.read_text()) + 1 if cf.exists() else 1\n"
            "    cf.write_text(str(n))\n"
            f"    pathlib.Path({str(mint_args_dump)!r}).write_text(json.dumps({{'args': args, 'T3CODE_PORT': os.environ.get('T3CODE_PORT', ''), 'T3CODE_HOST': os.environ.get('T3CODE_HOST', '')}}))\n"
            "    print(f'http://127.0.0.1:{port}/pair#token=minted_tok_{n}')\n"
            "    sys.exit(0)\n"
            "class H(http.server.BaseHTTPRequestHandler):\n"
            "    def do_GET(self):\n"
            "        self.send_response(200); self.end_headers(); self.wfile.write(b'ok')\n"
            "    def log_message(self, *a): pass\n"
            "socketserver.TCPServer.allow_reuse_address = True\n"
            "srv = socketserver.TCPServer(('127.0.0.1', port), H)\n"
            "signal.signal(signal.SIGTERM, lambda *a: sys.exit(0))\n"
            "print(f'http://127.0.0.1:{port}/pair#token=initial_once_tok', flush=True)\n"
            "for i in range(1200):\n"
            "    print(f'log-line-{i:04d}-' + ('a' * 100), flush=True)\n"
            "srv.serve_forever()\n"
        )
        mock_t3.chmod(0o755)

        started = json.loads(self.run_rt("t3code-start", {"port": port, "workspace": str(self.home)}).stdout)
        self.assertTrue(started["running"])
        self.assertTrue(started["installed"])
        pair_file = self.home / ".local/share/cage/t3code/pair.once"
        log_file = self.home / ".local/share/cage/t3code/server.log"
        self.assertTrue(pair_file.is_file())

        # Minting via t3code-open-url & t3code-mint-pair returns distinct fresh URLs and leaves pair.once untouched!
        m1 = json.loads(self.run_rt("t3code-open-url", {"port": port}).stdout)
        self.assertTrue(m1["paired_bootstrap"])
        self.assertEqual(m1["url"], f"http://127.0.0.1:{port}/pair#token=minted_tok_1")
        self.assertTrue(pair_file.is_file())

        m2 = json.loads(self.run_rt("t3code-mint-pair", {"port": port}).stdout)
        self.assertTrue(m2["paired_bootstrap"])
        self.assertEqual(m2["url"], f"http://127.0.0.1:{port}/pair#token=minted_tok_2")
        self.assertTrue(pair_file.is_file())

        mint_seen = json.loads(mint_args_dump.read_text())
        self.assertEqual(mint_seen["T3CODE_PORT"], str(port))
        self.assertEqual(mint_seen["T3CODE_HOST"], "127.0.0.1")
        self.assertIn("--base-dir", mint_seen["args"])

        # Give supervisor a moment to finish draining the 1200 log lines and verify 64 KiB cap.
        time.sleep(0.2)
        self.assertLessEqual(log_file.stat().st_size, 65536)
        self.assertEqual(stat.S_IMODE(log_file.stat().st_mode), 0o600)
        self.assertNotIn("initial_once_tok", log_file.read_text())

    def test_hashed_prepared_stamp_cleanup_and_profile_env_literal_quoting(self):
        account_id = "7" * 24
        pwn_marker = self.home / "pwned-by-shell-expansion"
        spec = {
            "id": "codex",
            "name": "OpenAI Codex",
            "binary": "codex",
            "isolation": {
                "mode": "env_dir",
                "env_var": "CODEX_HOME",
                "extra_env": {
                    "CUSTOM_LITERAL": f"$(touch {pwn_marker}); `touch {pwn_marker}`; $HOME 'quoted'",
                },
            },
            "auth": {
                "api_key_env_vars": ["OPENAI_API_KEY"],
                "base_url_env_var": "OPENAI_BASE_URL",
                "model_env_var": "OPENAI_MODEL",
            },
        }
        tricky_key = f"sk-key$(touch {pwn_marker})'quoted'$HOME"
        tricky_model = f"gpt-5-$(touch {pwn_marker})'m'"
        tricky_url = f"https://gateway.example.com/v1?x=$(touch+{pwn_marker})"

        self.run_rt("set-api-key", {
            "spec": spec,
            "account_id": account_id,
            "api_key": tricky_key,
            "base_url": tricky_url,
            "model": tricky_model,
            "provider": "LiteralTest",
        })

        # 1. Default deterministic hashed stamp.
        prep1 = json.loads(self.run_rt("prepare-session", {"spec": spec, "account_id": account_id}).stdout)
        stamp1 = Path(prep1["prepared_stamp"])
        self.assertTrue(stamp1.is_file())
        self.assertTrue(stamp1.name.startswith(".prepared-"))
        self.assertEqual(stamp1.name, f".prepared-{prep1['prep_hash']}")

        # 2. Explicit prep_hash writes new stamp and removes stale .prepared-* stamps!
        prep2 = json.loads(self.run_rt("prepare-session", {
            "spec": spec,
            "account_id": account_id,
            "prep_hash": "deadbeef12345678",
        }).stdout)
        stamp2 = Path(prep2["prepared_stamp"])
        self.assertEqual(stamp2.name, ".prepared-deadbeef12345678")
        self.assertTrue(stamp2.is_file())
        self.assertFalse(stamp1.exists())

        # Invalid prep_hash (path traversal) must be rejected.
        bad = self.run_rt("prepare-session", {
            "spec": spec,
            "account_id": account_id,
            "prep_hash": "../../evil",
        }, check=False)
        self.assertNotEqual(bad.returncode, 0)

        # 3. Verify sourcing profile.env via /bin/sh performs ZERO shell expansion!
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        fake = bin_dir / "codex"
        fake.write_text(
            f"#!{sys.executable}\n"
            "import json, os\n"
            "print(json.dumps({k: os.environ.get(k, '') for k in ('OPENAI_API_KEY', 'OPENAI_BASE_URL', 'OPENAI_MODEL', 'CUSTOM_LITERAL')}))\n"
        )
        fake.chmod(0o755)
        run_env = {**self.env, "PATH": f"{prep2['shim_dir']}:{self.env['PATH']}", "CAGE_CLI_ACCOUNT_CODEX": account_id}
        out = json.loads(subprocess.check_output([str(Path(prep2["shim_dir"]) / "codex")], env=run_env, text=True, timeout=10))
        self.assertFalse(pwn_marker.exists(), "shell expansion occurred while sourcing profile.env!")
        self.assertEqual(out["OPENAI_API_KEY"], tricky_key)
        self.assertEqual(out["OPENAI_BASE_URL"], tricky_url)
        self.assertEqual(out["OPENAI_MODEL"], tricky_model)
        self.assertEqual(out["CUSTOM_LITERAL"], f"$(touch {pwn_marker}); `touch {pwn_marker}`; $HOME 'quoted'")

    def test_run_login_sets_isolation_env_and_allowed_hosts(self):
        account_id = "9" * 24
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        captured = self.home / "login-env.json"
        fake = bin_dir / "droid"
        fake.write_text(
            f"#!{sys.executable}\n"
            "import json, os\n"
            f"open({str(captured)!r}, 'w').write(json.dumps({{'login': os.environ.get('CAGE_CLAUDE_LOGIN', ''), 'hosts': os.environ.get('CAGE_LOGIN_ALLOWED_HOSTS', ''), 'home': os.environ.get('FACTORY_HOME', '')}}))\n"
        )
        fake.chmod(0o755)
        spec = {
            "id": "droid",
            "name": "Factory Droid",
            "binary": "droid",
            "isolation": {"mode": "env_dir", "env_var": "FACTORY_HOME"},
            "auth": {"login_args": ["login"], "oauth_domains": ["app.factory.ai", "auth.factory.ai"]},
        }
        self.run_rt("login", {"spec": spec, "account_id": account_id})
        seen = json.loads(captured.read_text())
        self.assertEqual(seen["login"], account_id)
        self.assertEqual(seen["hosts"], "app.factory.ai,auth.factory.ai")
        self.assertTrue(seen["home"].endswith(f"/droid/{account_id}/config"))

    def test_magpie_lifecycle_mock_server_auth_redaction_and_uninstall(self):
        port = free_port()
        gw_port = free_port()
        other_port = free_port()
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        seen_dump = self.home / "magpie-seen.json"
        uninstall_log = self.home / "magpie-uninstall.log"

        mock_magpie = bin_dir / "magpie"
        mock_magpie.write_text(
            f"#!{sys.executable}\n"
            "import http.server, json, os, pathlib, signal, socketserver, sys, threading\n"
            "args = sys.argv[1:]\n"
            "if args == ['--version']:\n"
            "    print('magpie v0.1.20260417')\n"
            "    sys.exit(0)\n"
            "if args and args[0] == 'uninstall':\n"
            f"    pathlib.Path({str(uninstall_log)!r}).write_text(json.dumps(args))\n"
            "    sys.exit(0)\n"
            "port = 3430\n"
            "for i, a in enumerate(args):\n"
            "    if a == '--addr' and i + 1 < len(args):\n"
            "        port = int(args[i + 1].rsplit(':', 1)[1])\n"
            f"gw_port = {gw_port}\n"
            "web_key = os.environ.get('MAGPIE_WEB_KEY', '')\n"
            f"pathlib.Path({str(seen_dump)!r}).write_text(json.dumps({{'args': args, 'MAGPIE_WEB_KEY': web_key}}))\n"
            "class H(http.server.BaseHTTPRequestHandler):\n"
            "    def do_GET(self):\n"
            "        self.send_response(200); self.end_headers(); self.wfile.write(b'ok')\n"
            "    def log_message(self, *a): pass\n"
            "socketserver.TCPServer.allow_reuse_address = True\n"
            "web_srv = socketserver.TCPServer(('127.0.0.1', port), H)\n"
            "gw_srv = socketserver.TCPServer(('127.0.0.1', gw_port), H)\n"
            "t = threading.Thread(target=gw_srv.serve_forever, daemon=True)\n"
            "t.start()\n"
            "signal.signal(signal.SIGTERM, lambda *a: sys.exit(0))\n"
            "print('Magpie Web UI ready on 127.0.0.1', flush=True)\n"
            "print(f'Open http://127.0.0.1:{port}/?k={web_key}', flush=True)\n"
            "print(f'Auth MAGPIE_WEB_KEY={web_key}', flush=True)\n"
            "print(f'Cookie magpie_web_{port}={web_key}', flush=True)\n"
            "print(f'Query http://127.0.0.1:{port}/api?foo=1&k=another_secret_key_99', flush=True)\n"
            "web_srv.serve_forever()\n"
        )
        mock_magpie.chmod(0o755)

        # 1. Status before start.
        st0 = json.loads(self.run_rt("magpie-status", {"port": port, "gateway_port": gw_port}).stdout)
        self.assertTrue(st0["installed"])
        self.assertEqual(st0["binary_path"], str(mock_magpie))
        self.assertEqual(st0["version"], "0.1.20260417")
        self.assertFalse(st0["running"])
        self.assertEqual(st0["pid"], 0)
        self.assertEqual(st0["port"], port)
        self.assertEqual(st0["gateway_port"], gw_port)
        self.assertFalse(st0["gateway_running"])
        self.assertFalse(st0["gateway_only"])

        # Open URL before key exists falls back to unauthenticated base URL.
        open0 = json.loads(self.run_rt("magpie-open-url", {"port": port}).stdout)
        self.assertFalse(open0["authenticated"])
        self.assertEqual(open0["url"], f"http://127.0.0.1:{port}/")

        # Port colliding with gateway port must be rejected.
        collide_default = self.run_rt("magpie-start", {"port": 3425}, check=False)
        self.assertNotEqual(collide_default.returncode, 0)
        collide_custom = self.run_rt("magpie-start", {"port": gw_port, "gateway_port": gw_port}, check=False)
        self.assertNotEqual(collide_custom.returncode, 0)

        # 2. Start Magpie and verify key (0600), env-only passing, log redaction, and gateway detection.
        started = json.loads(self.run_rt("magpie-start", {
            "port": port,
            "gateway_port": gw_port,
            "workspace": str(self.home),
        }).stdout)
        self.assertTrue(started["installed"])
        self.assertEqual(started["binary_path"], str(mock_magpie))
        self.assertEqual(started["version"], "0.1.20260417")
        self.assertTrue(started["running"])
        self.assertGreater(started["pid"], 0)
        self.assertEqual(started["port"], port)
        self.assertEqual(started["gateway_port"], gw_port)
        self.assertTrue(started["gateway_running"])
        self.assertFalse(started["gateway_only"])

        key_file = self.home / ".local/share/cage/magpie/web.key"
        state_file = self.home / ".local/share/cage/magpie/runtime.json"
        log_file = self.home / ".local/share/cage/magpie/server.log"
        self.assertEqual(stat.S_IMODE(key_file.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(state_file.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(log_file.stat().st_mode), 0o600)

        web_key = key_file.read_text().strip()
        self.assertEqual(len(web_key), 32)
        self.assertNotIn(web_key, json.dumps(started))
        self.assertNotIn(web_key, state_file.read_text())

        seen = json.loads(seen_dump.read_text())
        self.assertEqual(seen["MAGPIE_WEB_KEY"], web_key)
        self.assertEqual(seen["args"], ["web", "--addr", f"127.0.0.1:{port}", "--no-open"])
        self.assertNotIn(web_key, " ".join(seen["args"]))

        time.sleep(0.15)
        log_text = log_file.read_text()
        self.assertIn("Magpie Web UI ready on 127.0.0.1", log_text)
        for forbidden in (web_key, "another_secret_key_99", "MAGPIE_WEB_KEY=", f"magpie_web_{port}=", "?k=", "&k="):
            self.assertNotIn(forbidden, log_text, f"secret leaked in magpie server.log: {forbidden}")

        # 3. Open URL returns authenticated URL with web.key.
        open1 = json.loads(self.run_rt("magpie-open-url", {}).stdout)
        self.assertTrue(open1["authenticated"])
        self.assertEqual(open1["url"], f"http://127.0.0.1:{port}/?k={web_key}")

        # 4. Status matching vs mismatched port & unverified PID identity.
        st1 = json.loads(self.run_rt("magpie-status", {"port": port, "gateway_port": gw_port}).stdout)
        self.assertTrue(st1["running"])
        self.assertEqual(st1["pid"], started["pid"])
        self.assertTrue(st1["gateway_running"])

        st_mismatch = json.loads(self.run_rt("magpie-status", {"port": other_port, "gateway_port": gw_port}).stdout)
        self.assertFalse(st_mismatch["running"])
        self.assertEqual(st_mismatch["pid"], 0)
        self.assertTrue(st_mismatch["gateway_running"])

        state_obj = json.loads(state_file.read_text())
        self.assertTrue(bool(state_obj.get("pid_identity")))
        unverified = dict(state_obj, pid_identity="")
        state_file.write_text(json.dumps(unverified))
        self.assertFalse(json.loads(self.run_rt("magpie-status", {"port": port, "gateway_port": gw_port}).stdout)["running"])
        state_file.write_text(json.dumps(state_obj))

        # 5. Idempotent start reuses existing PID; restart replaces PID & applies gateway_only flag.
        reused = json.loads(self.run_rt("magpie-start", {"port": port, "gateway_port": gw_port}).stdout)
        self.assertEqual(reused["pid"], started["pid"])

        restarted = json.loads(self.run_rt("magpie-restart", {
            "port": port,
            "gateway_port": gw_port,
            "gateway_only": True,
        }).stdout)
        self.assertTrue(restarted["running"])
        self.assertNotEqual(restarted["pid"], started["pid"])
        self.assertTrue(restarted["gateway_only"])
        seen2 = json.loads(seen_dump.read_text())
        self.assertIn("--gateway", seen2["args"])
        self.assertEqual(seen2["MAGPIE_WEB_KEY"], web_key)

        # 6. Stop rejects mismatched port, then stops cleanly on matching port.
        skip_stop = json.loads(self.run_rt("magpie-stop", {"port": other_port}).stdout)
        self.assertTrue(skip_stop.get("skipped_port_mismatch"))
        self.assertTrue(json.loads(self.run_rt("magpie-status", {"port": port, "gateway_port": gw_port}).stdout)["running"])

        stopped = json.loads(self.run_rt("magpie-stop", {"port": port}).stdout)
        self.assertFalse(stopped["running"])
        self.assertFalse(state_file.exists())
        st_after = json.loads(self.run_rt("magpie-status", {"port": port, "gateway_port": gw_port}).stdout)
        self.assertFalse(st_after["running"])
        self.assertFalse(st_after["gateway_running"])

        # 7. Uninstall stops running process, invokes `magpie uninstall --yes --keep-data`, and removes binary.
        self.run_rt("magpie-start", {"port": port, "gateway_port": gw_port})
        uninst = json.loads(self.run_rt("magpie-uninstall", {}).stdout)
        self.assertFalse(uninst["installed"])
        self.assertFalse(uninst["running"])
        self.assertEqual(json.loads(uninstall_log.read_text()), ["uninstall", "--yes", "--keep-data"])
        self.assertFalse(mock_magpie.exists())
        self.assertFalse(state_file.exists())
        st_uninst = json.loads(self.run_rt("magpie-status", {"port": port, "gateway_port": gw_port}).stdout)
        self.assertFalse(st_uninst["installed"])
        self.assertFalse(st_uninst["running"])

    def test_herdr_lifecycle_theme_and_interactive_attach(self):
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        proj_dir = self.home / "my-project"
        proj_dir.mkdir(parents=True, exist_ok=True)
        mock_state_file = self.home / "herdr-mock-state.json"
        mock_calls_file = self.home / "herdr-mock-calls.jsonl"
        mock_state_file.write_text(json.dumps({
            "running": False,
            "server_pid": 0,
            "workspaces": [],
            "agents": [],
            "next_id": 1,
        }))

        mock_herdr = bin_dir / "herdr"
        mock_herdr.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib, shlex, signal, stat, subprocess, sys, time\n"
            f"STATE = pathlib.Path({str(mock_state_file)!r})\n"
            f"CALLS = pathlib.Path({str(mock_calls_file)!r})\n"
            "def load_state():\n"
            "    return json.loads(STATE.read_text())\n"
            "def save_state(st):\n"
            "    STATE.write_text(json.dumps(st))\n"
            "def log_call(entry):\n"
            "    with CALLS.open('a') as f:\n"
            "        f.write(json.dumps(entry) + '\\n')\n"
            "args = sys.argv[1:]\n"
            "if args == ['--version']:\n"
            "    print('herdr 0.4.2')\n"
            "    sys.exit(0)\n"
            "if args == ['status', 'server', '--json']:\n"
            "    st = load_state()\n"
            "    running = bool(st.get('running'))\n"
            "    print(json.dumps({'running': running, 'status': 'running' if running else 'stopped'}))\n"
            "    sys.exit(0)\n"
            "if args == ['server']:\n"
            "    st = load_state()\n"
            "    st['running'] = True\n"
            "    st['server_pid'] = os.getpid()\n"
            "    save_state(st)\n"
            "    log_call({'cmd': 'server'})\n"
            "    def stop_sig(*_a):\n"
            "        s = load_state()\n"
            "        s['running'] = False\n"
            "        s['server_pid'] = 0\n"
            "        save_state(s)\n"
            "        sys.exit(0)\n"
            "    signal.signal(signal.SIGTERM, stop_sig)\n"
            "    while True:\n"
            "        time.sleep(0.2)\n"
            "if args == ['server', 'stop']:\n"
            "    st = load_state()\n"
            "    pid = st.get('server_pid') or 0\n"
            "    st['running'] = False\n"
            "    st['server_pid'] = 0\n"
            "    save_state(st)\n"
            "    log_call({'cmd': 'server-stop'})\n"
            "    if pid > 0:\n"
            "        try:\n"
            "            os.kill(pid, signal.SIGTERM)\n"
            "        except OSError:\n"
            "            pass\n"
            "    sys.exit(0)\n"
            "if args == ['server', 'reload-config']:\n"
            "    log_call({'cmd': 'reload-config'})\n"
            "    sys.exit(0)\n"
            "if args == ['workspace', 'list']:\n"
            "    st = load_state()\n"
            "    print(json.dumps({'result': {'workspaces': st.get('workspaces', [])}}))\n"
            "    sys.exit(0)\n"
            "if args == ['agent', 'list']:\n"
            "    st = load_state()\n"
            "    print(json.dumps({'result': {'agents': st.get('agents', [])}}))\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['workspace', 'create']:\n"
            "    st = load_state()\n"
            "    nid = st.get('next_id', 1)\n"
            "    st['next_id'] = nid + 1\n"
            "    def opt(flag):\n"
            "        for i, a in enumerate(args):\n"
            "            if a == flag and i + 1 < len(args):\n"
            "                return args[i + 1]\n"
            "            if a.startswith(flag + '='):\n"
            "                return a.split('=', 1)[1]\n"
            "        return ''\n"
            "    label = opt('--label')\n"
            "    cwd = opt('--cwd')\n"
            "    ws = {'workspace_id': f'ws-{nid}', 'label': label, 'cwd': cwd}\n"
            "    st['workspaces'].append(ws)\n"
            "    save_state(st)\n"
            "    log_call({'cmd': 'workspace-create', 'args': args})\n"
            "    print(json.dumps({'result': {'workspace': ws, 'tab': {'tab_id': f'tab-{nid}'}, 'root_pane': {'pane_id': f'pane-{nid}'}}}))\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['tab', 'rename']:\n"
            "    log_call({'cmd': 'tab-rename', 'args': args})\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['tab', 'create']:\n"
            "    st = load_state()\n"
            "    nid = st.get('next_id', 1)\n"
            "    st['next_id'] = nid + 1\n"
            "    save_state(st)\n"
            "    log_call({'cmd': 'tab-create', 'args': args})\n"
            "    print(json.dumps({'result': {'tab': {'tab_id': f'tab-{nid}'}, 'root_pane': {'pane_id': f'pane-{nid}'}}}))\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['pane', 'run']:\n"
            "    pane_id = args[2]\n"
            "    shell_cmd = args[3]\n"
            "    parts = shlex.split(shell_cmd)\n"
            "    script_path = pathlib.Path(parts[-1])\n"
            "    mode = stat.S_IMODE(script_path.stat().st_mode)\n"
            "    body = script_path.read_text()\n"
            "    res = subprocess.run(['/bin/sh', '-c', shell_cmd], check=False)\n"
            "    log_call({\n"
            "        'cmd': 'pane-run',\n"
            "        'pane_id': pane_id,\n"
            "        'shell_cmd': shell_cmd,\n"
            "        'mode': mode,\n"
            "        'body': body,\n"
            "        'rc': res.returncode,\n"
            "        'deleted_after': not script_path.exists(),\n"
            "    })\n"
            "    sys.exit(0)\n"
            "if not args:\n"
            "    leaked = [k for k in os.environ if k.startswith('CAGE_HERDR_')]\n"
            "    log_call({'cmd': 'attach', 'cwd': os.getcwd(), 'leaked': leaked})\n"
            "    print(f'HERDR_ATTACHED:{os.getcwd()}')\n"
            "    sys.exit(0)\n"
            "sys.exit(1)\n"
        )
        mock_herdr.chmod(0o755)

        # 1. Status while stopped.
        st0 = json.loads(self.run_rt("herdr-status", {}).stdout)
        self.assertTrue(st0["installed"])
        self.assertEqual(st0["binary_path"], str(mock_herdr))
        self.assertEqual(st0["version"], "0.4.2")
        self.assertFalse(st0["server_running"])
        self.assertEqual(st0["workspaces_count"], 0)
        self.assertEqual(st0["agents_count"], 0)
        self.assertEqual(st0["blocked_agents_count"], 0)

        # 2. Start server and verify workspace/agent/blocked counts.
        started = json.loads(self.run_rt("herdr-start", {"workspace": str(proj_dir)}).stdout)
        self.assertTrue(started["server_running"])
        st_data = json.loads(mock_state_file.read_text())
        st_data["workspaces"] = [{"workspace_id": "ws-init", "label": "init"}]
        st_data["agents"] = [
            {"id": "a1", "status": "running"},
            {"id": "a2", "status": "blocked"},
            {"id": "a3", "agent_status": "BLOCKED"},
        ]
        mock_state_file.write_text(json.dumps(st_data))
        st1 = json.loads(self.run_rt("herdr-status", {}).stdout)
        self.assertTrue(st1["server_running"])
        self.assertEqual(st1["workspaces_count"], 1)
        self.assertEqual(st1["agents_count"], 3)
        self.assertEqual(st1["blocked_agents_count"], 2)

        # 3. Apply theme: preserves existing TOML sections/comments and triggers server reload-config.
        cfg_file = self.home / ".config/herdr/config.toml"
        cfg_file.parent.mkdir(parents=True, exist_ok=True)
        cfg_file.write_text(
            "# Top-level user comment\n"
            "[server]\n"
            "port = 9999\n"
            "\n"
            "[other]\n"
            'name = "keep-this-name"\n'
        )
        th1 = json.loads(self.run_rt("herdr-apply-theme", {"theme": "tokyo-night"}).stdout)
        self.assertEqual(th1, {"ok": True, "theme": "tokyo-night"})
        self.assertEqual(stat.S_IMODE(cfg_file.stat().st_mode), 0o600)
        cfg1 = cfg_file.read_text()
        self.assertIn("# Top-level user comment", cfg1)
        self.assertIn("port = 9999", cfg1)
        self.assertIn('[other]\nname = "keep-this-name"', cfg1)
        self.assertIn('[theme]\nname = "tokyo-night"', cfg1)

        # Updating existing [theme] replaces name in-place without touching [other] or duplicating [theme].
        th2 = json.loads(self.run_rt("herdr-apply-theme", {"theme": "catppuccin"}).stdout)
        self.assertEqual(th2, {"ok": True, "theme": "catppuccin"})
        cfg2 = cfg_file.read_text()
        self.assertEqual(cfg2.count("[theme]"), 1)
        self.assertIn('name = "catppuccin"', cfg2)
        self.assertNotIn('name = "tokyo-night"', cfg2)
        self.assertIn('[other]\nname = "keep-this-name"', cfg2)

        # Default theme removes the managed [theme] override while keeping other sections; unknown theme is rejected.
        th_def = json.loads(self.run_rt("herdr-apply-theme", {"theme": "default"}).stdout)
        self.assertEqual(th_def, {"ok": True, "theme": "default"})
        cfg_def = cfg_file.read_text()
        self.assertNotIn('name = "catppuccin"', cfg_def)
        self.assertIn("# Top-level user comment", cfg_def)
        self.assertIn("port = 9999", cfg_def)
        self.assertIn('[other]\nname = "keep-this-name"', cfg_def)
        bad_theme = self.run_rt("herdr-apply-theme", {"theme": "nonexistent-theme"}, check=False)
        self.assertNotEqual(bad_theme.returncode, 0)

        calls = [json.loads(line) for line in mock_calls_file.read_text().splitlines() if line.strip()]
        self.assertTrue(any(c.get("cmd") == "reload-config" for c in calls))

        # 4. Stop server cleanly.
        stopped = json.loads(self.run_rt("herdr-stop", {}).stdout)
        self.assertFalse(stopped["server_running"])

        # Reset mock workspaces and call log before testing herdr-attach takeover flow.
        st_data = json.loads(mock_state_file.read_text())
        st_data["workspaces"] = []
        mock_state_file.write_text(json.dumps(st_data))
        mock_calls_file.write_text("")

        # Seed a stale run-*.sh older than 5 minutes to verify pruning.
        herdr_root = self.home / ".local/share/cage/herdr"
        herdr_root.mkdir(parents=True, exist_ok=True)
        stale_script = herdr_root / "run-stale-old.sh"
        stale_script.write_text("#!/bin/sh\n")
        old_ts = time.time() - 600
        os.utime(stale_script, (old_ts, old_ts))

        # 5a. herdr-attach autostarts server, creates workspace, renames tab, runs self-deleting 0700 script.
        marker1 = self.home / "claude-ran.txt"
        attach_env1 = {
            **self.env,
            "CAGE_HERDR_WORKSPACE": str(proj_dir),
            "CAGE_HERDR_PROJECT_LABEL": "my-project",
            "CAGE_HERDR_TAB_LABEL": "Claude",
            "CAGE_HERDR_KIND": "claude",
            "CAGE_HERDR_INNER_CMD": f"printf 'CLAUDE_OK' > '{marker1}'",
            "CAGE_HERDR_SHELL": "/bin/sh",
        }
        res_a1 = subprocess.run(
            [sys.executable, "-I", str(RUNTIME), "herdr-attach"],
            env=attach_env1,
            text=True,
            capture_output=True,
            timeout=15,
        )
        self.assertEqual(res_a1.returncode, 0, res_a1.stderr)
        self.assertIn("HERDR_ATTACHED:", res_a1.stdout)
        attached_cwd = res_a1.stdout.strip().split("HERDR_ATTACHED:", 1)[1]
        self.assertEqual(Path(attached_cwd).resolve(), proj_dir.resolve())
        self.assertFalse(stale_script.exists(), "stale run-*.sh should be pruned")
        self.assertEqual(marker1.read_text(), "CLAUDE_OK")

        calls1 = [json.loads(line) for line in mock_calls_file.read_text().splitlines() if line.strip()]
        cmds1 = [c["cmd"] for c in calls1]
        self.assertIn("server", cmds1)
        self.assertIn("workspace-create", cmds1)
        self.assertIn("tab-rename", cmds1)
        pane_runs = [c for c in calls1 if c["cmd"] == "pane-run"]
        self.assertEqual(len(pane_runs), 1)
        self.assertEqual(pane_runs[0]["mode"], 0o700)
        self.assertTrue(pane_runs[0]["deleted_after"])
        self.assertIn('rm -f -- "$0"', pane_runs[0]["body"])
        attach_calls = [c for c in calls1 if c["cmd"] == "attach"]
        self.assertEqual(attach_calls[-1]["leaked"], [])

        # 5b. Second attach for same project reuses workspace via `tab create` instead of creating a new workspace.
        mock_calls_file.write_text("")
        marker2 = self.home / "codex-ran.txt"
        attach_env2 = {
            **self.env,
            "CAGE_HERDR_WORKSPACE": str(proj_dir),
            "CAGE_HERDR_PROJECT_LABEL": "my-project",
            "CAGE_HERDR_TAB_LABEL": "Codex",
            "CAGE_HERDR_KIND": "codex",
            "CAGE_HERDR_INNER_CMD": f"printf 'CODEX_OK' > '{marker2}'",
            "CAGE_HERDR_SHELL": "/bin/sh",
        }
        res_a2 = subprocess.run(
            [sys.executable, "-I", str(RUNTIME), "herdr-attach"],
            env=attach_env2,
            text=True,
            capture_output=True,
            timeout=15,
        )
        self.assertEqual(res_a2.returncode, 0, res_a2.stderr)
        self.assertEqual(marker2.read_text(), "CODEX_OK")
        calls2 = [json.loads(line) for line in mock_calls_file.read_text().splitlines() if line.strip()]
        cmds2 = [c["cmd"] for c in calls2]
        self.assertNotIn("workspace-create", cmds2)
        self.assertIn("tab-create", cmds2)
        self.assertIn("pane-run", cmds2)

        # 5c. Fallback when HERDR_ENV=1 runs inner command directly via shell without invoking herdr.
        mock_calls_file.write_text("")
        attach_env3 = {
            **self.env,
            "HERDR_ENV": "1",
            "CAGE_HERDR_WORKSPACE": str(proj_dir),
            "CAGE_HERDR_KIND": "claude",
            "CAGE_HERDR_INNER_CMD": "printf 'FALLBACK_NESTED'",
            "CAGE_HERDR_SHELL": "/bin/sh",
        }
        res_a3 = subprocess.run(
            [sys.executable, "-I", str(RUNTIME), "herdr-attach"],
            env=attach_env3,
            text=True,
            capture_output=True,
            timeout=10,
        )
        self.assertEqual(res_a3.returncode, 0, res_a3.stderr)
        self.assertEqual(res_a3.stdout, "FALLBACK_NESTED")
        self.assertEqual(mock_calls_file.read_text().strip(), "")

        # 5d. Uninstall stops server, removes ~/.local/bin/herdr, preserves ~/.config/herdr/config.toml.
        uninst = json.loads(self.run_rt("herdr-uninstall", {}).stdout)
        self.assertEqual(uninst, {"installed": False, "server_running": False})
        self.assertFalse(mock_herdr.exists())
        self.assertTrue(cfg_file.is_file())
        self.assertIn('name = "keep-this-name"', cfg_file.read_text())

        # 5e. Fallback when herdr binary is missing executes inner command cleanly.
        attach_env4 = {
            **self.env,
            "CAGE_HERDR_WORKSPACE": str(proj_dir),
            "CAGE_HERDR_KIND": "codex",
            "CAGE_HERDR_INNER_CMD": "printf 'FALLBACK_MISSING'",
            "CAGE_HERDR_SHELL": "/bin/sh",
        }
        res_a4 = subprocess.run(
            [sys.executable, "-I", str(RUNTIME), "herdr-attach"],
            env=attach_env4,
            text=True,
            capture_output=True,
            timeout=10,
        )
        self.assertEqual(res_a4.returncode, 0, res_a4.stderr)
        self.assertEqual(res_a4.stdout, "FALLBACK_MISSING")

    def test_herdr_settings_keys_follow_config(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location("agent_runtime_settings_keys", RUNTIME)
        rt = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(rt)
        config = self.home / "herdr-config.toml"
        saved = os.environ.get("HERDR_CONFIG_PATH")
        os.environ["HERDR_CONFIG_PATH"] = str(config)
        self.addCleanup(lambda: os.environ.pop("HERDR_CONFIG_PATH", None) if saved is None else os.environ.__setitem__("HERDR_CONFIG_PATH", saved))
        cases = [
            # No config: first-run setup shows first, and ends in Settings.
            (None, {"onboarding": True, "chunks": ["\x02", "s"], "shortcut": "ctrl+b s"}),
            ("onboarding = false\n", {"onboarding": False, "chunks": ["\x02", "s"], "shortcut": "ctrl+b s"}),
            ('onboarding = false\n[keys]\nprefix = ["ctrl+space", "ctrl+s"]\nsettings = "prefix+comma"\n',
             {"onboarding": False, "chunks": ["\x00", ","], "shortcut": "ctrl+space comma"}),
            ('onboarding = false\n[keys]\nsettings = ["cmd+comma", "alt+s"]\n',
             {"onboarding": False, "chunks": ["\x1bs"], "shortcut": "alt+s"}),
            # A key no terminal can type: only its label, for the user to press.
            ('onboarding = false\n[keys]\nsettings = "cmd+comma"\n',
             {"onboarding": False, "chunks": None, "shortcut": "cmd+comma"}),
            # Herdr reads an invalid config as its defaults.
            ("onboarding = = nope\n", {"onboarding": False, "chunks": ["\x02", "s"], "shortcut": "ctrl+b s"}),
        ]
        for text, want in cases:
            if text is None:
                config.unlink(missing_ok=True)
            else:
                config.write_text(text)
            self.assertEqual(rt._herdr_settings_keys(), want, text)
        # Without tomllib (a macOS guest's Python 3.9) the same simple lines read the same.
        saved_module = sys.modules.get("tomllib")
        sys.modules["tomllib"] = None
        try:
            for text, want in cases[1:5]:
                config.write_text(text)
                self.assertEqual(rt._herdr_settings_keys(), want, text)
        finally:
            if saved_module is None:
                sys.modules.pop("tomllib", None)
            else:
                sys.modules["tomllib"] = saved_module
        self.assertIsNone(rt._herdr_key_bytes("super+s"))
        self.assertIsNone(rt._herdr_key_bytes("ctrl+shift+s"))
        self.assertEqual(rt._herdr_key_bytes("shift+n"), "N")
        self.assertEqual(rt._herdr_key_bytes("ctrl+["), "\x1b")


if __name__ == "__main__":
    unittest.main()
