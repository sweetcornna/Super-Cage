"""Exercise the production Windows launch policy with fixture child processes."""
from contextlib import contextmanager
import ctypes
import json
import os
from pathlib import Path
import runpy
import shlex
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch


with patch.object(ctypes, "WinDLL", create=True, return_value=MagicMock()):
    launch = runpy.run_path(Path(__file__).with_name("windows_cli_platform.py"))["launch_cli"]


@unittest.skipIf(os.name == "nt", "Fixture shell runs on POSIX without ConPTY")
class NativeLaunchPolicy(unittest.TestCase):
    def run_fixture(self, account_id, subject, actual_subject, refused=False):
        with tempfile.TemporaryDirectory() as path:
            root = Path(path)
            fixture = root / "child.py"
            fixture.write_text("""import json, os, sys
from pathlib import Path
with Path(os.environ['FIXTURE_LOG']).open('a') as record:
    record.write(json.dumps([sys.argv[1:], dict(os.environ)]) + '\\n')
if sys.argv[1] == 'probe':
    print(os.environ['FIXTURE_ACTUAL_SUBJECT'])
""")
            shell = root / "shell"
            shell.write_text("#!/bin/sh\nexec " + shlex.join([sys.executable, str(fixture), "shell"]) + ' "$@"\n')
            shell.chmod(0o700)
            log = root / "log"
            clean_env = {"FIXTURE_LOG": str(log), "FIXTURE_ACTUAL_SUBJECT": actual_subject,
                         "CLAUDE_CONFIG_DIR": str(root / account_id), "PATH": os.defpath}
            account = {
                "profile": lambda selected: root / selected,
                "cli_environment": lambda base: dict(clean_env, CLAUDE_CONFIG_DIR=str(base)),
                "shared_profile_resources": lambda base: None,
                "shared_launch_arguments": lambda base, args, settings: args,
                "identity": lambda binary, env, cwd, settings: subprocess.check_output(
                    [binary, str(fixture), "probe"], env=env, text=True).strip(),
                "settings_flags": lambda args: (),
                "subject": lambda reading: reading,
                "_runtime": {"path": str},
            }
            state = {"closed": False, "busy": True}

            @contextmanager
            def state_file(_):
                yield state

            class Process:
                def __init__(self, args, env):
                    self.child = subprocess.Popen(args, env=env)
                    self.pid = self.child.pid

                def wait(self):
                    return self.child.wait(timeout=5)

                def close(self):
                    if self.child.poll() is None:
                        self.child.kill()
                    self.child.wait(timeout=5)

            controller = {
                "CliProcess": Process,
                "_controller_context": {"CAGE_CLI_ACCOUNT_HELPER": "fixture-helper", "CAGE_CLI_SHELL": str(shell),
                                        "CAGE_CLI_PYTHON": sys.executable, "CAGE_CLI_BINARY": sys.executable},
            }
            request = {"account": account_id, "subject": subject}
            args = [sys.executable, str(fixture), "launch"]
            with patch.dict(launch.__globals__, controller), patch.object(runpy, "run_path", return_value=account), \
                    patch.dict(os.environ, {"ANTHROPIC_API_KEY": "unrelated-fixture-provider"}):
                if refused:
                    with self.assertRaises(ValueError):
                        launch(args, request, "fixture-connection", state_file)
                else:
                    self.assertEqual(launch(args, request, "fixture-connection", state_file), 0)
            self.assertTrue(state["closed"])
            self.assertFalse(state["busy"])
            self.assertEqual("pid" in state, not refused)
            return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []

    def test_verified_profile_survives_the_return_to_powershell(self):
        account = "a" * 24
        subject = "b" * 64
        records = self.run_fixture(account, subject, subject)
        self.assertEqual([args[0] for args, _ in records], ["probe", "launch", "shell"])
        self.assertNotIn("CAGE_CLAUDE_SUBJECT", records[1][1])
        self.assertTrue(all(Path(env["CLAUDE_CONFIG_DIR"]).name == account for _, env in records))
        args, env = records[-1]
        self.assertIn("-NoProfile", args)
        self.assertIn("$env:CAGE_CLI_ACCOUNT_HELPER run $env:CAGE_CLAUDE_ACCOUNT_ID", args[-1])
        self.assertEqual(env["CAGE_CLAUDE_SUBJECT"], subject)
        self.assertEqual(env["CAGE_CLAUDE_ACCOUNT_ID"], account)
        self.assertNotIn("ANTHROPIC_API_KEY", env)

    def test_refusal_never_starts_claude_or_a_fallback_shell(self):
        account = "a" * 24
        self.assertEqual(self.run_fixture(account, "", "", refused=True), [])
        records = self.run_fixture(account, "b" * 64, "c" * 64, refused=True)
        self.assertEqual([args[0] for args, _ in records], ["probe"])

    def test_unbound_default_keeps_an_ordinary_terminal(self):
        records = self.run_fixture("default", "", "")
        self.assertEqual([args[0] for args, _ in records], ["launch", "shell"])
        self.assertNotIn("CAGE_CLAUDE_SUBJECT", records[-1][1])
        self.assertEqual(records[-1][0], ["shell", "-NoLogo"])


if __name__ == "__main__":
    unittest.main()
