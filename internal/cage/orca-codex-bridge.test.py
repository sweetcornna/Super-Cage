import base64
import hashlib
import json
import os
from pathlib import Path
import platform
import runpy
import shlex
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
BRIDGE = runpy.run_path(str(HERE / "orca-codex-bridge.py"))["bridge"]
APP = Path("/Applications/Orca.app/Contents")
CLI = APP / "Resources/bin/orca"


def subject(email, organization):
    return hashlib.sha256(f"{email}\n{organization}".encode()).hexdigest()


def account_fixture(home, ident, email, organization):
    profile = home / ".local/share/cage/codex-accounts" / ident / "home"
    profile.mkdir(mode=0o700, parents=True)
    claims = {"email": email, "https://api.openai.com/auth": {"chatgpt_account_id": organization}}
    part = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    auth = profile / "auth.json"
    auth.write_text(json.dumps({"auth_mode": "chatgpt", "tokens": {
        "id_token": f"synthetic.{part}.signature", "account_id": organization,
        "access_token": "synthetic-access", "refresh_token": "synthetic-refresh",
    }}))
    auth.chmod(0o600)
    return subject(email, organization)


def installed_bundle_links(destination):
    assets = APP / "Resources/orcad-template"
    for item in assets.iterdir():
        if item.name not in ("node_modules", "targets"):
            (destination / item.name).symlink_to(item, target_is_directory=item.is_dir())
    modules = destination / "node_modules"
    modules.mkdir()
    for item in (assets / "node_modules").iterdir():
        if item.name != "node-pty":
            (modules / item.name).symlink_to(item, target_is_directory=item.is_dir())
    pty = modules / "node-pty"
    pty.mkdir()
    for item in (assets / "node_modules/node-pty").iterdir():
        if item.name != "build":
            (pty / item.name).symlink_to(item, target_is_directory=item.is_dir())
    architecture = {"arm64": "arm64", "x86_64": "x64"}[platform.machine()]
    native = assets / f"targets/darwin-{architecture}/node_modules/node-pty/build"
    (pty / "build").symlink_to(native, target_is_directory=True)


class OrcaCodexBridgeTests(unittest.TestCase):
    def test_orca_private_root_allows_native_umask_intermediates(self):
        with tempfile.TemporaryDirectory(prefix=".orca-test-", dir=".") as temp:
            data = Path(temp).resolve()
            root = data / "codex-accounts"
            account = root / "2debe93a-29ce-4430-add3-e8fe4e5b5bcc"
            home = account / "home"
            home.mkdir(mode=0o700, parents=True)
            for directory in (root, account):
                directory.chmod(0o775)
            verify = BRIDGE.__globals__["own_home"]
            self.assertEqual(verify(data, ("codex-accounts", account.name, "home")), home)
            home.chmod(0o775)
            self.assertIsNone(verify(data, ("codex-accounts", account.name, "home")))
            home.chmod(0o700)
            data.chmod(0o755)
            self.assertIsNone(verify(data, ("codex-accounts", account.name, "home")))

    def test_steady_metadata_does_not_run_the_cli_again(self):
        with tempfile.TemporaryDirectory(prefix=".orca-test-", dir=".") as temp:
            data = Path(temp).resolve()
            path = data / "profile-state.db-wal"
            request = {"accounts": {}}
            settings = dict(data=data, home=data, path=os.defpath, helper="")
            snapshot = {"activeAccountId": None, "accounts": []}
            mock = {"runtime_transport": lambda _: ("socket", "fixture", "synthetic-token"),
                    "local_rpc": lambda _data, method, *_: {"codex": snapshot} if method == "accounts.list" else {}}
            with patch.dict(BRIDGE.__globals__, mock):
                first = BRIDGE(request, **settings)
            with patch.dict(BRIDGE.__globals__, {**mock, "local_rpc": lambda *args: self.fail("steady state called Orca RPC")}):
                unchanged = BRIDGE({**request, "fingerprint": first["fingerprint"]}, **settings)
            self.assertTrue(unchanged["unchanged"])
            path.write_bytes(b"new synthetic state")
            with patch.dict(BRIDGE.__globals__, mock):
                changed = BRIDGE({**request, "fingerprint": first["fingerprint"]}, **settings)
            self.assertNotEqual(first["fingerprint"], changed["fingerprint"])

    def test_registration_waits_for_verification_after_restoring_selection(self):
        with tempfile.TemporaryDirectory(prefix=".orca-test-", dir=".") as temp:
            home = Path(temp).resolve()
            account = "a" * 24
            digest = account_fixture(home, account, "fixture@example.test", "tenant-a")
            orca_id = "2debe93a-29ce-4430-add3-e8fe4e5b5bcc"
            orca_home = home / "synthetic-orca-home"
            orca_home.mkdir(mode=0o700)
            selected = "system"
            reads_after_restore = 0
            imported = 0

            def request(_data, method, params, _runtime_id):
                nonlocal imported, selected
                if method == "accounts.addCodexFromHome":
                    imported += 1
                    selected = orca_id
                elif method == "accounts.selectCodex":
                    self.assertIsNone(params["accountId"])
                    selected = "system"
                else:
                    self.fail(f"unexpected RPC {method}")
                return {"activeAccountId": None, "accounts": []}

            def read(*_):
                nonlocal reads_after_restore
                if imported and selected == "system":
                    reads_after_restore += 1
                account_home = orca_home if reads_after_restore > 1 else None
                return selected, {orca_id: (digest, account_home)} if imported else {}

            with patch.dict(BRIDGE.__globals__, {"local_rpc": request, "read_native_accounts": read,
                                                "verify_profiles": lambda *args: True}):
                register = BRIDGE.__globals__["register_account"]
                result = register(account, {account: digest}, home, home, os.defpath, "",
                                  None, lambda _: None, "fixture-runtime")
            self.assertEqual(imported, 1)
            self.assertGreaterEqual(reads_after_restore, 2)
            self.assertTrue(result["registered"])
            self.assertFalse(result["selected"])

    @unittest.skipUnless(os.environ.get("CAGE_TEST_INSTALLED_ORCA") == "1" and sys.platform == "darwin"
                         and CLI.is_file(), "opt-in installed Orca protocol test")
    def test_installed_runtime_registration_switch_and_status(self):
        with tempfile.TemporaryDirectory(prefix=".o-", dir=".") as temp:
            root = Path(temp).resolve()
            home, bundle, temporary = (root / name for name in ("h", "r", "t"))
            data = home / "Library/Application Support/orca"
            for directory in (home, data, bundle, temporary):
                directory.mkdir(mode=0o700, parents=True)
            binary = home / ".local/bin/codex"
            binary.parent.mkdir(parents=True)
            elementary = runpy.run_path(str(HERE / "codex-account.test.py"))["FAKE_CODEX"]
            binary.write_text(f"#!{sys.executable}\n" + elementary)
            binary.chmod(0o700)
            a, b = "a" * 24, "b" * 24
            roster = {
                a: account_fixture(home, a, "fixture@example.test", "tenant-a"),
                b: account_fixture(home, b, "fixture@example.test", "tenant-b"),
            }
            policy = home / ".local/share/cage/codex-accounts/launch.json"
            policy.write_text(json.dumps({"mode": "manual", "managed": True, "selected_account_id": a,
                                          "accounts": [{"id": ident, "subject": digest} for ident, digest in roster.items()]}))
            installed_bundle_links(bundle)
            path = str(binary.parent) + os.pathsep + "/usr/bin:/bin:/usr/sbin:/sbin"
            env = {"HOME": str(home), "PATH": path, "SHELL": "/bin/sh", "TMPDIR": str(temporary),
                   "ORCA_USER_DATA": str(data), "ORCA_USER_DATA_PATH": str(data), "ORCA_ENVIRONMENT": "local",
                   "ORCA_INSTANCE_ID": "cage-orca-fixture", "ORCA_APP_VERSION": "1.4.278",
                   "DO_NOT_TRACK": "1", "ORCA_TELEMETRY_DISABLED": "1",
                   "ORCA_BACKGROUND_LAUNCH": "1", "NODE_OPTIONS": "--preserve-symlinks --preserve-symlinks-main",
                   "TEST_EMAIL": "fixture@example.test", "TEST_RATE_LIMITS": '{"rateLimits":{}}'}
            node = APP / "Helpers/Orca Agents.app/Contents/MacOS/Orca Agents"
            with (root / "server.log").open("w") as log:
                process = subprocess.Popen([node, bundle / "orcad-server.js", "--port", "0", "--no-pairing", "--json"],
                                           env=env, cwd=home, stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                                           start_new_session=True, umask=0o077)
                try:
                    self.verify_installed_runtime(process, env, home, data, path, roster, a, b)
                finally:
                    if process.poll() is None:
                        os.killpg(process.pid, signal.SIGTERM)
                        try:
                            process.wait(timeout=6)
                        except subprocess.TimeoutExpired:
                            os.killpg(process.pid, signal.SIGKILL)
                            process.wait()

    def verify_installed_runtime(self, process, env, home, data, path, roster, a, b):
        deadline = time.monotonic() + 10
        while not (data / "orca-runtime.json").is_file() and process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertTrue((data / "orca-runtime.json").is_file(), "installed runtime never became ready")

        def cli(*args):
            result = subprocess.run([CLI, "account", *args, "--agent", "codex", "--json"],
                                    env=env, cwd=home, capture_output=True, check=True, timeout=10)
            return json.loads(result.stdout)["result"]

        record = {}

        def write_record(value):
            record.clear()
            record.update(value)

        def bridge(**request):
            with patch.dict(os.environ, env, clear=True):
                return BRIDGE({"accounts": roster, **request}, data=data, home=home,
                              path=path, helper=str(HERE / "codex-account.py"),
                              registration_record=dict(record), write_record=write_record)

        self.assertEqual(cli("list")["accounts"], [])
        with patch.dict(os.environ, env, clear=True):
            runtime = runpy.run_path(str(HERE / "agent-runtime.py"))
            self.assertTrue(runtime["orca_codex_accounts"]({"accounts": {}, "managed_port": 0})["registration_supported"])
            runtime["_ensure_shell_shims_hook"]()
        self.assertFalse((home / ".local/share/cage/orca").exists())
        status = subprocess.run([sys.executable, HERE / "codex-account.py", "auto", "login", "status"],
                                env=env, cwd=home, capture_output=True, timeout=10)
        self.assertEqual(status.returncode, 0, status.stderr)
        self.assertEqual(status.stderr.strip(), b"Logged in using ChatGPT")
        self.assertEqual(subprocess.run(["codex", "login", "status"], env=env, cwd=home,
                                        capture_output=True, timeout=10).returncode, 1)
        shim = home / ".local/share/cage/auto-shims/codex"
        shim.parent.mkdir(parents=True)
        shim.write_text("#!/bin/sh\nexec " + shlex.join([sys.executable, str(HERE / "codex-account.py"), "auto"]) + ' "$@"\n')
        shim.chmod(0o700)
        project = home / "synthetic-project"
        project.mkdir()
        subprocess.run(["/usr/bin/git", "init", "--quiet", str(project)], env=env, cwd=home, check=True, timeout=10)
        doctor_env = {**env, "PATH": str(shim.parent) + os.pathsep + path}
        doctor = subprocess.run([CLI, "vm", "env", "doctor", "--path", str(project), "--json"],
                                env=doctor_env, cwd=project, capture_output=True, timeout=10)
        report = json.loads(doctor.stdout)
        checks = report.get("result", report)["checks"]
        codex = next(check for check in checks if "codex" in str(check).lower())
        self.assertIn("signed in", str(codex))
        self.assertEqual(codex["status"], "pass", codex)

        first = bridge(register=a)
        self.assertTrue(first["created"])
        self.assertFalse(first["selected"])
        listed = cli("list")
        self.assertIsNone(listed["activeAccountId"])
        a_orca = listed["accounts"][0]["id"]
        (data / "codex-accounts").chmod(0o775)
        (data / "codex-accounts" / a_orca).chmod(0o775)
        stamp = bridge()["fingerprint"]
        cli("select", "--account", a_orca)
        deadline = time.monotonic() + 3
        observed = bridge(fingerprint=stamp)
        while observed.get("unchanged") and time.monotonic() < deadline:
            time.sleep(0.05)
            observed = bridge(fingerprint=stamp)
        self.assertEqual(observed["cage_account_id"], a)
        self.assertFalse(bridge(register=a)["created"])

        second = bridge(register=b)
        self.assertTrue(second["created"])
        self.assertEqual(cli("list")["activeAccountId"], a_orca)
        b_orca = next(account["id"] for account in cli("list")["accounts"] if account["id"] != a_orca)
        changed = bridge(target=b, expected_orca_account_id=a_orca, expected_runtime_id=bridge()["runtime_id"])
        self.assertEqual(changed["cage_account_id"], b)
        self.assertEqual(cli("list")["activeAccountId"], b_orca)
        self.assertFalse(record)


if __name__ == "__main__":
    unittest.main()
