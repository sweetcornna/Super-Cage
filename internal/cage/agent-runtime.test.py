#!/usr/bin/python3
"""Unit tests for the universal guest Agent CLI runtime."""
import base64
import json
import os
import shlex
import shutil
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
        for stop_cmd in ("t3code-stop", "magpie-stop", "herdr-stop", "orca-stop"):
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

    def _hc_stub(self):
        """A herdr-core stand-in: a core that is a unix socket listener,
        terminals in a JSON file, every call logged."""
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        short = Path(tempfile.mkdtemp(prefix="hc"))
        self.addCleanup(shutil.rmtree, short, True)
        state = self.home / "hc-state.json"
        calls = self.home / "hc-calls.jsonl"
        stub = bin_dir / "herdr-core"
        stub.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib, signal, socket, subprocess, sys, time\n"
            "STATE = pathlib.Path(os.environ['HC_STUB_STATE'])\n"
            "CALLS = pathlib.Path(os.environ['HC_STUB_CALLS'])\n"
            "SOCK = os.environ['HC_SOCKET']\n"
            "def load():\n"
            "    return json.loads(STATE.read_text()) if STATE.exists() else {'terminals': [], 'next': 1, 'pid': 0, 'attaches': 0}\n"
            "def save(st):\n"
            "    STATE.write_text(json.dumps(st))\n"
            "def log(**entry):\n"
            "    with CALLS.open('a') as f:\n"
            "        f.write(json.dumps(entry) + '\\n')\n"
            "def out(obj):\n"
            "    print(json.dumps(obj, separators=(',', ':')))\n"
            "def running():\n"
            "    s = socket.socket(socket.AF_UNIX)\n"
            "    try:\n"
            "        s.connect(SOCK)\n"
            "        return True\n"
            "    except OSError:\n"
            "        return False\n"
            "    finally:\n"
            "        s.close()\n"
            "def start():\n"
            "    if running():\n"
            "        return\n"
            "    code = 'import os,socket,sys\\ns=socket.socket(socket.AF_UNIX)\\ntry: os.unlink(sys.argv[1])\\nexcept OSError: pass\\ns.bind(sys.argv[1]); s.listen(16)\\nwhile True:\\n  c,_=s.accept(); c.close()'\n"
            "    p = subprocess.Popen([sys.executable, '-c', code, SOCK], start_new_session=True, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)\n"
            "    st = load(); st['pid'] = p.pid; save(st)\n"
            "    for _ in range(200):\n"
            "        if running():\n"
            "            break\n"
            "        time.sleep(0.01)\n"
            "    log(cmd='core-start')\n"
            "def find(st, t):\n"
            "    for term in st['terminals']:\n"
            "        if t in (term['address'], term.get('name')):\n"
            "            return term\n"
            "    return None\n"
            "args = sys.argv[1:]\n"
            "log(cmd=' '.join(args[:2]) if args else 'picker', argv=args, nested=os.environ.get('HC_TERMINAL_ID', ''))\n"
            "if args == ['--version']:\n"
            "    print('hc 0.1.22'); sys.exit(0)\n"
            "if not args:\n"
            "    print('PICKER:' + os.getcwd()); sys.exit(0)\n"
            "if args[:2] == ['server', 'stop']:\n"
            "    st = load()\n"
            "    if st.get('pid'):\n"
            "        try: os.kill(st['pid'], signal.SIGTERM)\n"
            "        except OSError: pass\n"
            "    try: os.unlink(SOCK)\n"
            "    except OSError: pass\n"
            "    for term in st['terminals']:\n"
            "        term['state'] = 'finished'\n"
            "    st['pid'] = 0; save(st); out({'stopped': True}); sys.exit(0)\n"
            "start()\n"
            "st = load()\n"
            "live = [t for t in st['terminals'] if t['state'] == 'live']\n"
            "if args[:2] == ['server', 'status']:\n"
            "    out({'version': '0.1.22', 'live': len(live), 'keeps_running_after_logout': True}); sys.exit(0)\n"
            "if args[:2] in (['server', 'start'], ['server', 'restart']):\n"
            "    out({'started': True}); sys.exit(0)\n"
            "if args == ['connect']:\n"
            "    for line in sys.stdin:\n"
            "        req = json.loads(line)\n"
            "        terms = live + [{'address': 'box/t9', 'name': 'remote', 'state': 'live'}]\n"
            "        out({'jsonrpc': '2.0', 'id': req['id'], 'result': {'terminals': terms, 'machines': [], 'hidden_finished': 0}})\n"
            "    sys.exit(0)\n"
            "target = args[-1]\n"
            "if args[0] == 'get':\n"
            "    term = find(st, target)\n"
            "    if not term:\n"
            "        out({'error': {'kind': 'not_found', 'message': 'no terminal'}}); sys.exit(3)\n"
            "    out(term); sys.exit(0)\n"
            "if args[0] == 'run':\n"
            "    if os.environ.get('HC_STUB_RUN_FAIL') == '1':\n"
            "        st['terminals'].append({'address': 't99', 'name': args[args.index('--name') + 1], 'state': 'live', 'labels': {}})\n"
            "        save(st); out({'error': {'kind': 'internal', 'message': 'registry save failed'}}); sys.exit(1)\n"
            "    sep = args.index('--')\n"
            "    opts, argv = args[:sep], args[sep + 1:]\n"
            "    labels = dict(opts[i + 1].split('=', 1) for i, a in enumerate(opts) if a == '--label')\n"
            "    name = opts[opts.index('--name') + 1]\n"
            "    cwd = opts[opts.index('--cwd') + 1]\n"
            "    address = 't' + str(st['next']); st['next'] += 1\n"
            "    term = {'address': address, 'name': name, 'state': 'live', 'labels': labels, 'cwd': cwd, 'argv': argv, 'title': ''}\n"
            "    st['terminals'].append(term); save(st)\n"
            "    # hc runs programs in the core's environment, not the caller's.\n"
            "    core_env = {'PATH': '/usr/bin:/bin', 'HOME': os.environ['HOME'], 'CORE_ONLY': '1', 'LANG': 'C',\n"
            "                'HC_TERMINAL_ID': address, 'HC_SESSION': 'stub-core'}\n"
            "    subprocess.run(argv, cwd=cwd, env=core_env, stdin=subprocess.DEVNULL, check=False)\n"
            "    out(term); sys.exit(0)\n"
            "if args[0] == 'attach':\n"
            "    st['attaches'] += 1\n"
            "    term = find(st, target)\n"
            "    print('ATTACHED:' + target + ':' + os.getcwd())\n"
            "    if term and not (os.environ.get('HC_STUB_DETACH_FIRST') == '1' and st['attaches'] == 1):\n"
            "        term['state'] = 'finished'\n"
            "    save(st); sys.exit(0)\n"
            "if args[0] == 'kill':\n"
            "    term = find(st, target)\n"
            "    if term: term['state'] = 'finished'\n"
            "    save(st); out(term or {}); sys.exit(0 if term else 3)\n"
            "if args[0] == 'rm':\n"
            "    term = find(st, target)\n"
            "    st['terminals'] = [t for t in st['terminals'] if t is not term]\n"
            "    save(st); out({'removed': term['address']} if term else {}); sys.exit(0 if term else 3)\n"
            "sys.exit(2)\n"
        )
        stub.chmod(0o755)
        env = {**self.env, "HC_SOCKET": str(short / "hc.sock"), "HC_STUB_STATE": str(state), "HC_STUB_CALLS": str(calls)}
        env.pop("HC_SESSION", None)
        env.pop("HC_TERMINAL_ID", None)

        def read_calls():
            if not calls.exists():
                return []
            return [json.loads(line) for line in calls.read_text().splitlines()]

        def stop_core():
            if stub.exists():
                subprocess.run([str(stub), "server", "stop"], env=env, capture_output=True)
        self.addCleanup(stop_core)
        return stub, env, read_calls, state

    def _attach(self, env, **extra):
        return subprocess.run(
            [sys.executable, "-I", str(RUNTIME), "herdr-attach"],
            env={**env, **extra}, text=True, capture_output=True, timeout=30,
        )

    def test_herdr_core_lifecycle_attach_and_close(self):
        stub, env, read_calls, state = self._hc_stub()
        proj = self.home / "proj"
        proj.mkdir()

        # Looking never starts the core: status, list, close, attach-mode.
        st0 = json.loads(self.run_rt("herdr-status", {}, env=env).stdout)
        self.assertTrue(st0["installed"])
        self.assertEqual(st0["version"], "0.1.22")
        self.assertFalse(st0["server_running"])
        self.assertEqual(json.loads(self.run_rt("herdr-list", {}, env=env).stdout), {"running": False, "terminals": []})
        self.assertEqual(json.loads(self.run_rt("herdr-close", {"terminal": "cage-0123456789abcdef"}, env=env).stdout)["running"], False)
        down = self._attach(env, CAGE_HERDR_MODE="attach", CAGE_HERDR_TARGET="t1", CAGE_HERDR_WORKSPACE=str(proj))
        self.assertIn("已经结束", down.stdout)
        self.assertEqual([c for c in read_calls() if c["cmd"] != "--version"], [])
        bad = self.run_rt("herdr-close", {"terminal": "--help"}, env=env, check=False)
        self.assertNotEqual(bad.returncode, 0)
        self.assertIn("名称无效", bad.stderr)

        # Create: one core start, a named, labelled terminal with no parent
        # and no notes, the inner command in a self-deleting script, then the
        # viewer loop (hc attach) until the terminal finishes.
        marker = self.home / "inner-ran"
        name = "cage-0123456789abcdef"
        res = self._attach(env, CAGE_HERDR_MODE="create", CAGE_HERDR_NAME=name, CAGE_HERDR_WORKSPACE=str(proj),
                           CAGE_HERDR_PROJECT_LABEL="proj", CAGE_HERDR_TAB_LABEL="Shell\x1b[31m 1", CAGE_HERDR_KIND="shell",
                           CAGE_HERDR_INNER_CMD=f"printf ok > '{marker}'; [ -z \"$CAGE_HERDR_INNER_CMD\" ] && printf clean >> '{marker}'",
                           CAGE_HERDR_SHELL="/bin/sh")
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn(f"ATTACHED:{name}:{proj.resolve()}", res.stdout)
        self.assertEqual(marker.read_text(), "okclean")
        calls = read_calls()
        self.assertEqual(sum(1 for c in calls if c["cmd"] == "core-start"), 1)
        run = next(c for c in calls if c["cmd"].startswith("run"))
        argv = run["argv"]
        for flag in ("--bg", "--json", "--no-parent", "--no-notify"):
            self.assertIn(flag, argv)
        self.assertEqual(argv[argv.index("--name") + 1], name)
        self.assertEqual(argv[argv.index("--cwd") + 1], str(proj))
        labels = [argv[i + 1] for i, a in enumerate(argv) if a == "--label"]
        self.assertEqual(labels, ["cage=1", "cage.project=proj", "cage.kind=shell", "cage.title=Shell[31m 1"])
        runner = argv[argv.index("--") + 1:]
        self.assertEqual(runner[1:4], ["-I", "-S", "-c"])
        self.assertFalse(Path(runner[-1]).exists(), "the launch file must delete itself")
        self.assertNotIn("printf", " ".join(argv), "the command stays out of argv")
        attach = next(c for c in calls if c["cmd"].startswith("attach"))
        self.assertEqual(attach["argv"], ["attach", "--history", "1000", "--", name])
        self.assertFalse(any(c["cmd"].startswith("machine") for c in calls))

        # ctrl+\ (hc attach detaches): the loop attaches again while it lives.
        st = json.loads(state.read_text())
        st["attaches"] = 0
        st["terminals"][0]["state"] = "live"
        state.write_text(json.dumps(st))
        again = self._attach({**env, "HC_STUB_DETACH_FIRST": "1"}, CAGE_HERDR_MODE="attach", CAGE_HERDR_TARGET=name,
                             CAGE_HERDR_WORKSPACE=str(proj))
        self.assertEqual(again.stdout.count(f"ATTACHED:{name}"), 2, again.stdout)

        # Re-open with the same name while it lives: no second program.
        st = json.loads(state.read_text())
        st["terminals"][0]["state"] = "live"
        state.write_text(json.dumps(st))
        before = sum(1 for c in read_calls() if c["cmd"].startswith("run"))
        self._attach(env, CAGE_HERDR_MODE="create", CAGE_HERDR_NAME=name, CAGE_HERDR_WORKSPACE=str(proj),
                     CAGE_HERDR_INNER_CMD=f"printf twice >> '{marker}'", CAGE_HERDR_SHELL="/bin/sh")
        self.assertEqual(sum(1 for c in read_calls() if c["cmd"].startswith("run")), before)
        self.assertEqual(marker.read_text(), "okclean")

        # List: live local terminals with their cage.* labels, never another machine's.
        st = json.loads(state.read_text())
        st["terminals"][0]["state"] = "live"
        state.write_text(json.dumps(st))
        listed = json.loads(self.run_rt("herdr-list", {}, env=env).stdout)
        self.assertTrue(listed["running"])
        self.assertEqual([t["address"] for t in listed["terminals"]], ["t1"])
        self.assertEqual(listed["terminals"][0]["name"], name)
        self.assertEqual(listed["terminals"][0]["labels"]["cage.kind"], "shell")
        status = json.loads(self.run_rt("herdr-status", {}, env=env).stdout)
        self.assertTrue(status["server_running"])
        self.assertEqual(status["terminals_count"], 1)

        # Stop refuses while terminals run, unless forced.
        refused = self.run_rt("herdr-stop", {}, env=env, check=False)
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("1 个终端在运行", refused.stderr)

        # Close: kill, then rm.
        closed = json.loads(self.run_rt("herdr-close", {"terminal": name}, env=env).stdout)
        self.assertTrue(closed["closed"])
        self.assertEqual([c["cmd"] for c in read_calls()][-2:], ["kill --json", "rm --json"])
        self.assertEqual(json.loads(state.read_text())["terminals"], [])

        stopped = json.loads(self.run_rt("herdr-stop", {"force": True}, env=env).stdout)
        self.assertFalse(stopped["server_running"])

        # A run that fails ends what it may have started, then falls back.
        res = self._attach({**env, "HC_STUB_RUN_FAIL": "1"}, CAGE_HERDR_MODE="create", CAGE_HERDR_NAME="cage-1111111111111111",
                           CAGE_HERDR_WORKSPACE=str(proj), CAGE_HERDR_INNER_CMD="printf FALLBACK_RAN", CAGE_HERDR_SHELL="/bin/sh")
        self.assertIn("FALLBACK_RAN", res.stdout)
        self.assertEqual([c["cmd"] for c in read_calls()][-2:], ["kill --json", "rm --json"])

        # Nested in an hc terminal, a bad name, or no herdr-core: the plain shell.
        nested = self._attach({**env, "HC_TERMINAL_ID": "t3"}, CAGE_HERDR_MODE="create", CAGE_HERDR_NAME="cage-2222222222222222",
                              CAGE_HERDR_WORKSPACE=str(proj), CAGE_HERDR_INNER_CMD="printf NESTED_PLAIN", CAGE_HERDR_SHELL="/bin/sh")
        self.assertIn("NESTED_PLAIN", nested.stdout)
        bad_name = self._attach(env, CAGE_HERDR_MODE="create", CAGE_HERDR_NAME="--help", CAGE_HERDR_WORKSPACE=str(proj),
                                CAGE_HERDR_INNER_CMD="printf BAD_NAME_PLAIN", CAGE_HERDR_SHELL="/bin/sh")
        self.assertIn("BAD_NAME_PLAIN", bad_name.stdout)
        picker = self._attach(env, CAGE_HERDR_KIND="herdr", CAGE_HERDR_WORKSPACE=str(proj))
        self.assertIn(f"PICKER:{proj.resolve()}", picker.stdout)
        stub.unlink()
        missing = self._attach(env, CAGE_HERDR_MODE="create", CAGE_HERDR_NAME="cage-3333333333333333",
                               CAGE_HERDR_WORKSPACE=str(proj), CAGE_HERDR_INNER_CMD="printf MISSING_PLAIN", CAGE_HERDR_SHELL="/bin/sh")
        self.assertIn("MISSING_PLAIN", missing.stdout)

    def test_herdr_program_gets_cages_environment_not_the_cores(self):
        stub, env, read_calls, state = self._hc_stub()
        proj = self.home / "proj"
        proj.mkdir()
        dump = self.home / "program-env.json"
        cage_env = {**env, "LANG": "zh_CN.UTF-8", "TZ": "Asia/Shanghai", "BROWSER": "/opt/cage/browser",
                    "CLAUDE_CONFIG_DIR": str(self.home / "claude-account-2"), "CAGE_TEST_SECRET": "s3cr3t-value",
                    "PATH": f"{self.home}/cage-bin:{env['PATH']}", "HC_SESSION": "from-the-caller"}
        program = f"import json, os; open({str(dump)!r}, 'w').write(json.dumps(dict(os.environ)))"
        res = self._attach(cage_env, CAGE_HERDR_MODE="create", CAGE_HERDR_NAME="cage-4444444444444444",
                           CAGE_HERDR_WORKSPACE=str(proj), CAGE_HERDR_SHELL="/bin/sh",
                           CAGE_HERDR_INNER_CMD=f"exec {shlex.quote(sys.executable)} -I -c {shlex.quote(program)}")
        self.assertEqual(res.returncode, 0, res.stderr)
        seen = json.loads(dump.read_text())
        for key in ("LANG", "TZ", "BROWSER", "CLAUDE_CONFIG_DIR", "CAGE_TEST_SECRET", "PATH", "HOME"):
            self.assertEqual(seen.get(key), cage_env[key], key)
        self.assertNotIn("CORE_ONLY", seen, "the core's environment leaked into the program")
        self.assertFalse(any(k.startswith("CAGE_HERDR_") for k in seen), "Cage's launch variables leaked")
        # hc's own variables for this terminal stay hc's.
        self.assertEqual(seen.get("HC_TERMINAL_ID"), "t1")
        self.assertEqual(seen.get("HC_SESSION"), "stub-core")
        # Nothing secret in what hc keeps: argv, labels, names.
        self.assertNotIn("s3cr3t-value", state.read_text())
        self.assertNotIn("s3cr3t-value", json.dumps(read_calls()))
        self.assertEqual(list((self.home / ".local/share/cage/herdr").glob("run-*")), [], "the launch file was left behind")

    def test_herdr_legacy_tui_status_and_uninstall(self):
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        panes = self.home / "legacy-panes.json"
        panes.write_text(json.dumps({"result": {"panes": [{"pane_id": "w1:p1"}, {"pane_id": "w1:p2"}]}}))
        legacy = bin_dir / "herdr"
        legacy.write_text(
            f"#!{sys.executable}\n"
            "import json, pathlib, sys\n"
            f"PANES = pathlib.Path({str(panes)!r})\n"
            "args = sys.argv[1:]\n"
            "if args == ['--version']:\n"
            "    print('herdr 0.9.5'); sys.exit(0)\n"
            "if args == ['status', 'server', '--json']:\n"
            "    print(json.dumps({'running': PANES.exists()})); sys.exit(0)\n"
            "if args == ['pane', 'list']:\n"
            "    print(PANES.read_text()); sys.exit(0)\n"
            "if args == ['server', 'stop']:\n"
            "    PANES.unlink(missing_ok=True); sys.exit(0)\n"
            "sys.exit(1)\n"
        )
        legacy.chmod(0o755)
        config = self.home / ".config/herdr/session.json"
        config.parent.mkdir(parents=True)
        config.write_text("{}")
        st = json.loads(self.run_rt("herdr-legacy-status", {}).stdout)
        self.assertEqual((st["installed"], st["server_running"], st["panes"], st["busy"]), (True, True, 2, True))
        self.assertTrue(json.loads(self.run_rt("herdr-status", {}).stdout)["legacy_installed"])
        refused = self.run_rt("herdr-legacy-uninstall", {}, check=False)
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("2 个终端", refused.stderr)
        self.assertTrue(legacy.exists())
        done = json.loads(self.run_rt("herdr-legacy-uninstall", {"force": True}).stdout)
        self.assertFalse(done["installed"])
        self.assertFalse(legacy.exists())
        self.assertTrue(config.exists(), "~/.config/herdr stays")
        self.assertFalse(json.loads(self.run_rt("herdr-status", {}).stdout)["legacy_installed"])

    def test_herdr_core_install_reuse_and_release_checksum(self):
        import hashlib
        import importlib.util
        import io
        import tarfile
        saved_home = os.environ.get("HOME")
        os.environ["HOME"] = str(self.home)
        try:
            spec = importlib.util.spec_from_file_location("agent_runtime_hc_install", RUNTIME)
            rt = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(rt)
        finally:
            os.environ["HOME"] = saved_home
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        served = self.home / "served"
        served.mkdir()
        curl_log = self.home / "curl.log"
        # curl stand-in: serves files by the URL's last path segment.
        fake_curl = bin_dir / "curl"
        fake_curl.write_text(
            "#!/bin/sh\n"
            "echo \"$@\" >> '" + str(curl_log) + "'\n"
            "out=''; url=''\n"
            "while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift 2;; https://*) url=$1; shift;; *) shift;; esac; done\n"
            "cp '" + str(served) + "'/\"$(basename \"$url\")\" \"$out\"\n"
        )
        fake_curl.chmod(0o755)

        def build(version):
            body = ("#!/bin/sh\n[ \"$1\" = --version ] && echo 'hc " + version + "' && exit 0\n"
                    "[ \"$1\" = update ] && echo \"$@\" >> '" + str(self.home) + "/update.log' && exit 0\nexit 1\n").encode()
            buf = io.BytesIO()
            with tarfile.open(fileobj=buf, mode="w:gz") as tar:
                info = tarfile.TarInfo("herdr-core")
                info.size, info.mode = len(body), 0o755
                tar.addfile(info, io.BytesIO(body))
            return buf.getvalue()

        machine = os.uname().machine.lower()
        target = "linux-" + {"x86_64": "x86_64", "amd64": "x86_64"}.get(machine, "aarch64")

        def publish(version, archive, sha=None, url=None):
            (served / "herdr-core.tar.gz").write_bytes(archive)
            (served / "latest.json").write_text(json.dumps({"version": version, "targets": {target: {
                "url": url or f"https://releases.herdr.dev/core/{version}/herdr-core.tar.gz",
                "sha256": sha or hashlib.sha256(archive).hexdigest()}}}))

        path_str = f"{bin_dir}:/usr/bin:/bin"
        env = {**os.environ, "HOME": str(self.home), "PATH": path_str}

        # A damaged download is refused and nothing is installed.
        publish("0.1.22", build("0.1.22"), sha="0" * 64)
        with self.assertRaisesRegex(ValueError, "SHA-256"):
            rt._herdr_install_linux("", "", path_str, env)
        self.assertFalse((bin_dir / "herdr-core").exists())
        # A manifest pointing anywhere but the release host is refused.
        publish("0.1.22", build("0.1.22"), url="https://evil.example/herdr-core.tar.gz")
        with self.assertRaisesRegex(ValueError, "格式"):
            rt._herdr_install_linux("", "", path_str, env)
        # The checked release installs, with hc beside it.
        publish("0.1.22", build("0.1.22"))
        rt._herdr_install_linux("", "", path_str, env)
        self.assertEqual(rt._hc_version(str(bin_dir / "herdr-core")), "0.1.22")
        rt._hc_link_hc(path_str)
        self.assertEqual(os.readlink(bin_dir / "hc"), "herdr-core")
        self.assertIn("--proto =https", curl_log.read_text())
        # A working herdr-core is kept: no download at all.
        curl_log.unlink()
        res = json.loads(self.run_rt("herdr-install", {}).stdout.strip().splitlines()[-1])
        self.assertTrue(res["reused"])
        self.assertFalse(curl_log.exists())
        # update with a newer release swaps it the planned way (update --from).
        publish("0.1.23", build("0.1.23"))
        rt._herdr_install_linux(str(bin_dir / "herdr-core"), "0.1.22", path_str, env)
        self.assertIn("update --from", (self.home / "update.log").read_text())
        # update with nothing newer downloads only the manifest.
        curl_log.unlink()
        rt._herdr_install_linux(str(bin_dir / "herdr-core"), "0.1.23", path_str, env)
        self.assertEqual(len(curl_log.read_text().splitlines()), 1)

    def test_antigravity_oauth_snapshot_default_and_xdg_subdirs(self):
        antigravity_spec = {
            "id": "antigravity",
            "name": "Antigravity CLI",
            "binary": "agy",
            "isolation": {
                "mode": "xdg",
                "default_config_paths": ["~/.gemini/antigravity-cli", "~/.gemini/config"],
            },
            "auth": {
                "credential_files": [
                    "oauth_creds.json",
                    "google_accounts.json",
                    "jetski_state.pbtxt",
                    "credentials.json",
                ],
            },
        }
        exp_ms = int((time.time() + 3600) * 1000)

        # 1. Isolated XDG account with oauth_creds.json inside config/antigravity-cli/
        acc1 = "e" * 24
        acc1_dir = self.home / ".local/share/cage/agent-accounts/antigravity" / acc1 / "config/antigravity-cli"
        acc1_dir.mkdir(parents=True)
        (acc1_dir / "oauth_creds.json").write_text(json.dumps({
            "access_token": "ya29.super-secret-token-1",
            "refresh_token": "1//super-secret-refresh-1",
            "id_token": make_jwt({"email": "alice@example.com"}),
            "expiry_date": exp_ms,
        }))
        (acc1_dir / "google_accounts.json").write_text(json.dumps({"active": "alice@example.com", "old": []}))

        snap1 = json.loads(self.run_rt("snapshot", {"spec": antigravity_spec, "account_id": acc1}).stdout)
        self.assertTrue(snap1["logged_in"], f"XDG antigravity-cli oauth_creds not detected: {snap1}")
        self.assertEqual(snap1["auth_mode"], "oauth")
        self.assertEqual(snap1["email"], "alice@example.com")
        self.assertEqual(snap1["plan"], "Google OAuth")
        self.assertEqual(snap1["expires_at"], int(exp_ms / 1000))
        self.assertNotIn("ya29.super-secret-token-1", json.dumps(snap1))
        self.assertNotIn("super-secret-refresh-1", json.dumps(snap1))

        # 2. Isolated XDG account with jetski_state.pbtxt under data/antigravity-cli/
        acc2 = "f" * 24
        acc2_dir = self.home / ".local/share/cage/agent-accounts/antigravity" / acc2 / "data/antigravity-cli"
        acc2_dir.mkdir(parents=True)
        (acc2_dir / "jetski_state.pbtxt").write_text('user_email: "bob@example.com"\naccount_id: "12345"\n')
        snap2 = json.loads(self.run_rt("snapshot", {"spec": antigravity_spec, "account_id": acc2}).stdout)
        self.assertTrue(snap2["logged_in"], f"XDG jetski_state.pbtxt not detected: {snap2}")
        self.assertEqual(snap2["email"], "bob@example.com")

        # 3. Default ~/.gemini/antigravity-cli directory extracts email from google_accounts.json / id_token.
        def_dir = self.home / ".gemini/antigravity-cli"
        def_dir.mkdir(parents=True)
        (def_dir / "oauth_creds.json").write_text(json.dumps({
            "access_token": "ya29.default-secret",
            "id_token": make_jwt({"email": "default@example.com"}),
            "expiry_date": exp_ms,
        }))
        snap_def = json.loads(self.run_rt("snapshot", {"spec": antigravity_spec, "account_id": "default"}).stdout)
        self.assertTrue(snap_def["logged_in"])
        self.assertEqual(snap_def["email"], "default@example.com")
        self.assertEqual(snap_def["plan"], "Google OAuth")
        self.assertNotIn("ya29.default-secret", json.dumps(snap_def))

        # 4. google_accounts.json alone without tokens must NOT report logged_in.
        acc3 = "1" * 24
        acc3_dir = self.home / ".local/share/cage/agent-accounts/antigravity" / acc3 / "config/antigravity-cli"
        acc3_dir.mkdir(parents=True)
        (acc3_dir / "google_accounts.json").write_text(json.dumps({"active": "stale@example.com", "old": []}))
        snap3 = json.loads(self.run_rt("snapshot", {"spec": antigravity_spec, "account_id": acc3}).stdout)
        self.assertFalse(snap3["logged_in"])

        # 5. Generic XDG agent stores credentials under config/<app_name>/.
        custom_xdg = {
            "id": "demo-xdg",
            "name": "Demo XDG",
            "binary": "demo-xdg",
            "isolation": {"mode": "xdg", "default_config_paths": ["~/.config/demo-app"]},
            "auth": {"credential_files": ["auth.json"]},
        }
        acc4 = "2" * 24
        acc4_dir = self.home / ".local/share/cage/agent-accounts/demo-xdg" / acc4 / "config/demo-app"
        acc4_dir.mkdir(parents=True)
        (acc4_dir / "auth.json").write_text(json.dumps({"access_token": "tok_demo_123", "email": "demo@example.com"}))
        snap4 = json.loads(self.run_rt("snapshot", {"spec": custom_xdg, "account_id": acc4}).stdout)
        self.assertTrue(snap4["logged_in"], f"Generic XDG subdir credential not detected: {snap4}")
        self.assertEqual(snap4["email"], "demo@example.com")

    def test_sync_auto_shims_and_auto_exec_all_clis_with_project_pins_and_rr(self):
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        fake_oc = bin_dir / "opencode"
        fake_oc.write_text(
            f"#!{sys.executable}\n"
            "import json, os, sys\n"
            "print(json.dumps({\n"
            "    'XDG_CONFIG_HOME': os.environ.get('XDG_CONFIG_HOME', ''),\n"
            "    'CAGE_CLI_ACCOUNT_OPENCODE': os.environ.get('CAGE_CLI_ACCOUNT_OPENCODE', ''),\n"
            "    'ANTHROPIC_API_KEY': os.environ.get('ANTHROPIC_API_KEY', ''),\n"
            "    'ARGS': sys.argv[1:],\n"
            "}))\n"
        )
        fake_oc.chmod(0o755)

        acc1, acc2, acc3 = "1" * 24, "2" * 24, "3" * 24
        oc_spec = {
            "id": "opencode",
            "name": "OpenCode",
            "binary": "opencode",
            "isolation": {"mode": "xdg"},
            "auth": {"api_key_env_vars": ["ANTHROPIC_API_KEY"]},
        }
        self.run_rt("set-api-key", {"spec": oc_spec, "account_id": acc1, "api_key": "sk-ant-acc1-secret"})
        self.run_rt("set-api-key", {"spec": oc_spec, "account_id": acc2, "api_key": "sk-ant-acc2-secret"})
        self.run_rt("set-api-key", {"spec": oc_spec, "account_id": acc3, "api_key": "sk-ant-acc3-secret"})

        # Seed Claude & native Codex launch.json so sync-auto-shims creates all three shims.
        claude_launch = self.home / ".local/share/cage/claude-accounts/launch.json"
        claude_launch.parent.mkdir(parents=True, exist_ok=True)
        claude_launch.write_text(json.dumps({"mode": "balanced", "accounts": [{"id": acc1, "subject": "a" * 64, "preferred": True}]}))
        codex_launch = self.home / ".local/share/cage/codex-accounts/launch.json"
        codex_launch.parent.mkdir(parents=True, exist_ok=True)
        codex_launch.write_text(json.dumps({"mode": "balanced", "accounts": [{"id": acc2, "subject": "b" * 64, "preferred": False}]}))

        pinned_proj = self.home / "workspace/pinned-repo"
        pinned_sub = pinned_proj / "pkg/sub"
        pinned_sub.mkdir(parents=True)
        other_proj = self.home / "workspace/other-repo"
        other_proj.mkdir(parents=True)

        now = int(time.time())
        sync_res = json.loads(self.run_rt("sync-auto-shims", {
            "schedule": {
                "agents": {
                    "opencode": {
                        "mode": "balanced",
                        "project_pins": {str(pinned_proj): f"account:{acc2}"},
                        "spec": oc_spec,
                        "candidates": [
                            {"id": acc1, "guest_id": acc1, "score": 90.0},
                            {"id": acc2, "guest_id": acc2, "score": 88.0},
                            {"id": acc3, "guest_id": acc3, "score": 99.0, "cooldown_until": now + 3600},
                        ],
                    },
                },
            },
        }).stdout)
        self.assertTrue(sync_res["ok"])
        self.assertEqual(sync_res["shims"], ["claude", "codex", "opencode"])

        auto_dir = Path(sync_res["dir"])
        for name in ("claude", "codex", "opencode"):
            self.assertTrue((auto_dir / name).is_file())
            self.assertTrue(os.access(auto_dir / name, os.X_OK))
        # Shell rc hooks idempotently installed for login-shell PATH preservation.
        self.assertIn("# >>> super-cage-auto-shims >>>", (self.home / ".bashrc").read_text())
        self.assertIn("# >>> super-cage-auto-shims >>>", (self.home / ".profile").read_text())

        shim_env = {k: v for k, v in self.env.items() if not k.startswith("XDG_") and k != "ANTHROPIC_API_KEY"}
        shim_env["PATH"] = f"{auto_dir}:{self.env['PATH']}"

        # Consecutive balanced invocations rotate acc1 -> acc2 and skip cooled-down acc3.
        r1 = json.loads(subprocess.check_output(["opencode", "run", "task1"], cwd=other_proj, env=shim_env, text=True, timeout=10))
        r2 = json.loads(subprocess.check_output(["opencode", "run", "task2"], cwd=other_proj, env=shim_env, text=True, timeout=10))
        self.assertEqual(r1["CAGE_CLI_ACCOUNT_OPENCODE"], acc1)
        self.assertEqual(r1["ANTHROPIC_API_KEY"], "sk-ant-acc1-secret")
        self.assertTrue(r1["XDG_CONFIG_HOME"].endswith(f"/opencode/{acc1}/config"))
        self.assertEqual(r2["CAGE_CLI_ACCOUNT_OPENCODE"], acc2)
        self.assertEqual(r2["ANTHROPIC_API_KEY"], "sk-ant-acc2-secret")
        rr_file = self.home / ".local/share/cage/agent-accounts/opencode/.auto-rr"
        self.assertEqual(stat.S_IMODE(rr_file.stat().st_mode), 0o600)

        # Project pin forces acc2 inside pinned_proj subtree even when RR would pick acc1.
        r_pin = json.loads(subprocess.check_output(["opencode", "run", "pinned"], cwd=pinned_sub, env=shim_env, text=True, timeout=10))
        self.assertEqual(r_pin["CAGE_CLI_ACCOUNT_OPENCODE"], acc2)
        self.assertEqual(r_pin["ARGS"], ["run", "pinned"])

        # Bypass on --version or CAGE_ACCOUNT_SCHEDULING=off runs real binary without account isolation.
        r_ver = json.loads(subprocess.check_output(["opencode", "--version"], cwd=other_proj, env=shim_env, text=True, timeout=10))
        self.assertEqual(r_ver["CAGE_CLI_ACCOUNT_OPENCODE"], "")
        self.assertEqual(r_ver["ANTHROPIC_API_KEY"], "")
        off_env = {**shim_env, "CAGE_ACCOUNT_SCHEDULING": "off"}
        r_off = json.loads(subprocess.check_output(["opencode", "run"], cwd=other_proj, env=off_env, text=True, timeout=10))
        self.assertEqual(r_off["CAGE_CLI_ACCOUNT_OPENCODE"], "")
        self.assertEqual(r_off["ANTHROPIC_API_KEY"], "")

        # Emptying schedules cleans up stale shims cleanly.
        claude_launch.unlink()
        codex_launch.unlink()
        sync_empty = json.loads(self.run_rt("sync-auto-shims", {"schedule": {"agents": {}}}).stdout)
        self.assertEqual(sync_empty["shims"], [])
        self.assertEqual(list(auto_dir.iterdir()), [])

    def test_orca_server_atomic_install_lifecycle_pair_and_stale_lock_recovery(self):
        import hashlib
        import importlib.util
        import io
        import tarfile
        from unittest.mock import patch

        # --- Part A: Atomic Linux tarball installer (checksum, traversal, rollback) ---
        saved_home = os.environ.get("HOME")
        os.environ["HOME"] = str(self.home)
        try:
            spec = importlib.util.spec_from_file_location("agent_runtime_orca_install", RUNTIME)
            rt = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(rt)
        finally:
            os.environ["HOME"] = saved_home

        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        served = self.home / "orca-served"
        served.mkdir()
        fake_curl = bin_dir / "curl"
        fake_curl.write_text(
            "#!/bin/sh\n"
            "out=''; url=''\n"
            "while [ $# -gt 0 ]; do\n"
            "  case \"$1\" in\n"
            "    -o) out=\"$2\"; shift 2 ;;\n"
            "    -H|--proto|--proto-redir|--connect-timeout|--max-time) shift 2 ;;\n"
            "    https://*) url=\"$1\"; shift ;;\n"
            "    *) shift ;;\n"
            "  esac\n"
            "done\n"
            "name=$(basename \"$url\")\n"
            "if [ \"$name\" = latest ]; then name=latest.json; fi\n"
            "if [ ! -f '" + str(served) + "'/\"$name\" ]; then echo missing \"$name\" >&2; exit 22; fi\n"
            "cp '" + str(served) + "'/\"$name\" \"$out\"\n"
        )
        fake_curl.chmod(0o755)

        machine = os.uname().machine.lower()
        arch = {"x86_64": "x64", "amd64": "x64", "aarch64": "arm64", "arm64": "arm64"}.get(machine, "arm64")
        archive_name = f"orca-server-linux-{arch}.tar.gz"

        def make_orca_tarball(version, evil_member=None):
            buf = io.BytesIO()
            with tarfile.open(fileobj=buf, mode="w:gz") as tar:
                ver_bytes = (version + "\n").encode()
                vi = tarfile.TarInfo(f"orca-server-{version}/VERSION")
                vi.size, vi.mode = len(ver_bytes), 0o644
                tar.addfile(vi, io.BytesIO(ver_bytes))

                script = f"#!/bin/sh\n[ \"$1\" = --version ] && echo 'orca {version}' && exit 0\nexit 0\n".encode()
                bi = tarfile.TarInfo(f"orca-server-{version}/bin/orca-ide")
                bi.size, bi.mode = len(script), 0o755
                tar.addfile(bi, io.BytesIO(script))
                if evil_member:
                    ei = tarfile.TarInfo(evil_member)
                    ei.size, ei.mode = 4, 0o644
                    tar.addfile(ei, io.BytesIO(b"evil"))
            return buf.getvalue()

        def publish_orca(version, archive_bytes, sha=None):
            (served / "latest.json").write_text(json.dumps({"tag_name": f"v{version}"}))
            (served / archive_name).write_bytes(archive_bytes)
            digest = sha or hashlib.sha256(archive_bytes).hexdigest()
            (served / "orca-server-sha256sums.txt").write_text(f"{digest}  {archive_name}\n")

        with patch.object(rt.sys, "platform", "linux"), patch.dict(os.environ, {"HOME": str(self.home), "PATH": f"{bin_dir}:/usr/bin:/bin"}):
            # 1. Bad SHA-256 must abort cleanly without creating current symlink.
            publish_orca("1.4.277", make_orca_tarball("1.4.277"), sha="0" * 64)
            with self.assertRaisesRegex(ValueError, "SHA-256"):
                rt.orca_install({})
            self.assertFalse((self.home / ".local/share/cage/orca/current").exists())

            # 2. Path traversal member must be refused.
            publish_orca("1.4.277", make_orca_tarball("1.4.277", evil_member="../escape.txt"))
            with self.assertRaisesRegex(ValueError, "路径穿越"):
                rt.orca_install({})
            self.assertFalse((self.home / ".local/share/cage/orca/current").exists())

            # 3. Clean v1.4.277 install creates current + ~/.local/bin/orca-ide.
            publish_orca("1.4.277", make_orca_tarball("1.4.277"))
            ins1 = rt.orca_install({})
            self.assertTrue(ins1["installed"])
            self.assertEqual(ins1["version"], "1.4.277")
            self.assertTrue((self.home / ".local/bin/orca-ide").is_symlink())
            time.sleep(1.05)  # distinct second for staged_dir timestamp

            # 4. Upgrade to v1.4.278 atomically swaps current and retains previous rollback.
            publish_orca("1.4.278", make_orca_tarball("1.4.278"))
            ins2 = rt.orca_install({})
            self.assertEqual(ins2["version"], "1.4.278")
            self.assertTrue((self.home / ".local/share/cage/orca/previous").is_symlink())
            self.assertIn("1.4.277", str((self.home / ".local/share/cage/orca/previous").resolve()))
            self.assertTrue(rt.orca_status({})["has_rollback"])

        # --- Part B: Full subprocess lifecycle, stale-lock auto-heal, bindings, pair, repo sync, uninstall ---
        port = free_port()
        env_dump = self.home / "orca-seen-env.json"
        reclaim_marker = self.home / "orca-reclaimed.flag"
        first_fail_marker = self.home / "orca-first-fail.flag"
        repos_file = self.home / "orca-repos.json"
        reset_marker = self.home / "orca-reset.flag"
        proj_dir = self.home / "projects/demo-app"
        (proj_dir / ".git").mkdir(parents=True)

        # Install mock `orca-ide` directly into managed current/bin/orca-ide.
        cur_bin = self.home / ".local/share/cage/orca/current/bin/orca-ide"
        cur_bin.write_text(
            f"#!{sys.executable}\n"
            "import http.server, json, os, pathlib, signal, socketserver, sys\n"
            "args = sys.argv[1:]\n"
            "if args == ['--version']:\n"
            "    print('orca 1.4.278')\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['serve', 'reset-identity']:\n"
            f"    pathlib.Path({str(reset_marker)!r}).write_text('reset')\n"
            "    print(json.dumps({'ok': True}))\n"
            "    sys.exit(0)\n"
            "if args[:1] == ['serve'] and '--reclaim-stale-lock' in args:\n"
            f"    pathlib.Path({str(reclaim_marker)!r}).write_text(' '.join(args))\n"
            "    print(json.dumps({'ok': True}))\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['repo', 'list']:\n"
            f"    rf = pathlib.Path({str(repos_file)!r})\n"
            "    items = json.loads(rf.read_text()) if rf.exists() else []\n"
            "    print(json.dumps({'ok': True, 'result': {'repos': [{'path': p} for p in items]}}))\n"
            "    sys.exit(0)\n"
            "if args[:2] == ['repo', 'add']:\n"
            f"    rf = pathlib.Path({str(repos_file)!r})\n"
            "    items = json.loads(rf.read_text()) if rf.exists() else []\n"
            "    p = args[args.index('--path') + 1] if '--path' in args else ''\n"
            "    if p and p not in items:\n"
            "        items.append(p)\n"
            "    rf.write_text(json.dumps(items))\n"
            "    print(json.dumps({'ok': True, 'result': {'path': p}}))\n"
            "    sys.exit(0)\n"
            "if args[:1] == ['pair']:\n"
            "    addr = args[args.index('--address') + 1] if '--address' in args else '127.0.0.1:16768'\n"
            "    mobile = '--mobile' in args\n"
            "    print(json.dumps({'ok': True, 'result': {\n"
            "        'available': True,\n"
            "        'scope': 'mobile' if mobile else 'desktop',\n"
            "        'pairingUrl': f'orca://pair?endpoint=ws://{addr}&token=minted_pair_tok',\n"
            "        'endpoint': f'ws://{addr}',\n"
            "        'deviceId': 'dev-orca-123',\n"
            "        'qr': 'QR_TEXT',\n"
            "    }}))\n"
            "    sys.exit(0)\n"
            "if args[:1] == ['serve']:\n"
            f"    ff = pathlib.Path({str(first_fail_marker)!r})\n"
            f"    if not ff.exists() and not pathlib.Path({str(reclaim_marker)!r}).exists():\n"
            "        ff.write_text('1')\n"
            "        print('stale lock detected', flush=True)\n"
            "        sys.exit(3)\n"
            "    bind = args[args.index('--bind') + 1]\n"
            "    port = int(args[args.index('--port') + 1])\n"
            f"    pathlib.Path({str(env_dump)!r}).write_text(json.dumps({{\n"
            "        'args': args,\n"
            "        'PATH': os.environ.get('PATH', ''),\n"
            "        'ORCA_USER_DATA': os.environ.get('ORCA_USER_DATA', ''),\n"
            "        'ORCA_INSTANCE_ID': os.environ.get('ORCA_INSTANCE_ID', ''),\n"
            "        'DO_NOT_TRACK': os.environ.get('DO_NOT_TRACK', ''),\n"
            "        'ORCA_TELEMETRY_DISABLED': os.environ.get('ORCA_TELEMETRY_DISABLED', ''),\n"
            "        'ORCA_BACKGROUND_LAUNCH': os.environ.get('ORCA_BACKGROUND_LAUNCH', ''),\n"
            "        'ORCA_TERMINAL_MEMORY_MAX': os.environ.get('ORCA_TERMINAL_MEMORY_MAX', ''),\n"
            "        'BROWSER': os.environ.get('BROWSER', ''),\n"
            "        'CAGE_CLAUDE_ACCOUNT_ID': os.environ.get('CAGE_CLAUDE_ACCOUNT_ID', ''),\n"
            "    }))\n"
            "    class H(http.server.BaseHTTPRequestHandler):\n"
            "        def do_GET(self):\n"
            "            self.send_response(200); self.end_headers(); self.wfile.write(b'ok')\n"
            "        def log_message(self, *a): pass\n"
            "    socketserver.TCPServer.allow_reuse_address = True\n"
            "    srv = socketserver.TCPServer((bind, port), H)\n"
            "    signal.signal(signal.SIGTERM, lambda *a: sys.exit(0))\n"
            "    print('Orca server listening on 127.0.0.1', flush=True)\n"
            "    print('Secret leak orca://pair?token=top_secret_pair_999', flush=True)\n"
            "    print('deviceToken=dev_secret_888', flush=True)\n"
            "    srv.serve_forever()\n"
        )
        cur_bin.chmod(0o755)

        started = json.loads(self.run_rt("orca-start", {
            "port": port,
            "bind_mode": "loopback",
            "terminal_memory_max": "2G",
            "workspace": str(self.home),
            "projects": [str(proj_dir)],
            "bindings": {
                "claude": {"mode": "default"},
                "opencode": {"mode": "auto"},
            },
        }).stdout)
        self.assertTrue(started["installed"])
        self.assertTrue(started["running"])
        self.assertEqual(started["port"], port)
        self.assertEqual(started["bind_ip"], "127.0.0.1")
        # Stale-lock exit code 3 automatically ran --reclaim-stale-lock and succeeded!
        self.assertTrue(reclaim_marker.is_file())
        self.assertIn("--reclaim-stale-lock", reclaim_marker.read_text())

        seen = json.loads(env_dump.read_text())
        self.assertIn("--no-pairing", seen["args"])
        self.assertIn("--refuse-public", seen["args"])
        self.assertEqual(seen["DO_NOT_TRACK"], "1")
        self.assertEqual(seen["ORCA_TELEMETRY_DISABLED"], "1")
        self.assertEqual(seen["ORCA_BACKGROUND_LAUNCH"], "1")
        self.assertEqual(seen["ORCA_TERMINAL_MEMORY_MAX"], "2G")
        self.assertTrue(seen["ORCA_USER_DATA"].endswith("/orca/data"))
        self.assertTrue(bool(seen["ORCA_INSTANCE_ID"]))
        self.assertTrue(bool(seen["BROWSER"]))
        self.assertEqual(seen["CAGE_CLAUDE_ACCOUNT_ID"], "default")
        self.assertIn("auto-shims", seen["PATH"])

        # Project auto-sync added proj_dir once.
        self.assertEqual(json.loads(repos_file.read_text()), [str(proj_dir.resolve())])
        sync2 = json.loads(self.run_rt("orca-sync-repos", {"projects": [str(proj_dir)]}).stdout)
        self.assertEqual(sync2["added"], [])
        self.assertEqual(sync2["existing"], 1)

        # Log file is 0600 and redacts pairing URLs & device tokens.
        log_file = self.home / ".local/share/cage/orca/server.log"
        self.assertEqual(stat.S_IMODE(log_file.stat().st_mode), 0o600)
        log_text = log_file.read_text()
        self.assertIn("Orca server listening", log_text)
        self.assertNotIn("top_secret_pair_999", log_text)
        self.assertNotIn("dev_secret_888", log_text)

        # Status & on-demand pairing offer.
        st = json.loads(self.run_rt("orca-status", {"port": port}).stdout)
        self.assertTrue(st["running"])
        self.assertEqual(st["pid"], started["pid"])
        pair = json.loads(self.run_rt("orca-pair", {"mobile": True, "rotate": True, "address": "100.64.0.9:16768"}).stdout)
        self.assertTrue(pair["available"])
        self.assertEqual(pair["scope"], "mobile")
        self.assertIn("100.64.0.9:16768", pair["pairing_url"])

        # Reset identity rotates ORCA_INSTANCE_ID and stops server.
        old_id = seen["ORCA_INSTANCE_ID"]
        reset = json.loads(self.run_rt("orca-reset-identity", {}).stdout)
        self.assertTrue(reset["ok"])
        self.assertNotEqual(reset["instance_id"], old_id)
        self.assertTrue(reset_marker.is_file())
        self.assertFalse(json.loads(self.run_rt("orca-status", {"port": port}).stdout)["running"])

        # Uninstall preserves data/ by default, purges data/ when purge_data=True.
        data_marker = self.home / ".local/share/cage/orca/data/keep.txt"
        data_marker.write_text("keep")
        self.run_rt("orca-uninstall", {"purge_data": False})
        self.assertFalse((self.home / ".local/bin/orca-ide").exists())
        self.assertTrue(data_marker.is_file())
        self.run_rt("orca-uninstall", {"purge_data": True})
        self.assertFalse((self.home / ".local/share/cage/orca/data").exists())


if __name__ == "__main__":
    unittest.main()
