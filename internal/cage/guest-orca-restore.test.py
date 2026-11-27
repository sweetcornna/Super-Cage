import os
from pathlib import Path
import runpy
import tempfile
import unittest
from unittest.mock import patch


HELPER = Path(__file__).with_name("guest-orca-restore.py")
EXECUTE = runpy.run_path(str(HELPER))["execute"]


class GuestOrcaRestoreTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = Path(self.tmp.name).resolve()
        self.workspace = self.home / "quant"
        self.workspace.mkdir()
        self.managed = self.home / ".local/share/cage/orca/data"
        self.bin = self.home / "bin"
        self.bin.mkdir()
        self.binary = self.bin / "orca-ide"
        self.binary.write_text("#!/bin/sh\nexit 1\n")
        self.binary.chmod(0o700)
        self.calls = []
        self.runtime_id = "fixture-orca-instance"
        self.run_id = (self.runtime_id, 10, 11, os.getpid())
        self.addCleanup(patch.stopall)
        patch.dict(EXECUTE.__globals__, {"_socket_identity": self.socket_identity}).start()

    def socket_identity(self, data, transport):
        self.last_data = data
        return self.run_id

    def request(self, operation, *, fixture=None, **changes):
        data = {"operation": operation, "runtime": "independent",
                "workspace": str(self.workspace), "runtime_id": self.runtime_id, **changes}
        return EXECUTE(data, home=self.home, managed_data=self.managed,
                       managed_binary=self.binary, search_path=str(self.bin),
                       transport=None, read_cli=fixture or self.cli)

    def cli(self, binary, args, env, home, wait):
        self.assertEqual(str(self.binary), binary)
        self.assertEqual(home, self.home)
        self.assertEqual("local", env["ORCA_ENVIRONMENT"])
        self.assertNotIn("ORCA_PAIRING_CODE", env)
        self.calls.append(args)
        if args[:2] == ["worktree", "show"]:
            return {"worktree": {"id": "quant-id", "path": str(self.workspace)}}
        if args[:2] == ["agent", "list"]:
            return {"agents": [{"kind": "terminal", "agent": "codex", "hostId": "local",
                                "title": "hidden text", "snippet": "hidden content"},
                               {"kind": "terminal", "agent": "codex", "hostId": "remote"}]}
        if args[:2] == ["terminal", "create"]:
            return {"terminal": {"handle": "term_fixture", "worktreeId": "quant-id", "secret": "hidden"}}
        if args[:2] == ["terminal", "wait"]:
            self.assertTrue(wait)
            return {"wait": {"handle": "term_fixture", "condition": "exit", "status": "exited",
                             "satisfied": True, "exitCode": 0, "extra": "hidden"}}
        return {"terminals": []}

    def test_scoped_discovery_and_redaction(self):
        self.assertEqual(self.request("probe")["runtime_id"], self.runtime_id)
        self.assertEqual(self.last_data, self.home / ".config/orca")
        self.assertEqual(self.request("probe", runtime="managed")["runtime_id"], self.runtime_id)
        self.assertEqual(self.last_data, self.managed)
        self.assertEqual(self.request("agents")["result"]["agents"],
                         [{"kind": "terminal", "agent": "codex", "hostId": "local"}])
        self.assertEqual(self.calls, [["agent", "list", "--worktree", "path:" + str(self.workspace)]])

    def test_guarded_resume_has_no_caller_supplied_shell_or_remote_target(self):
        origin = {"worktree": "quant-id", "subject": "a" * 64, "account": "b" * 24,
                  "thread": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}
        self.assertEqual(self.request("resume", **origin, command="echo changed"), {"runtime_id": ""})
        self.assertEqual(self.request("resume", **{**origin, "account": "b;other"}), {"runtime_id": ""})
        self.assertEqual(self.request("resume", **origin, runtime_id="other"), {"runtime_id": ""})
        self.assertEqual(self.calls, [])
        result = self.request("resume", **origin)["result"]["terminal"]
        self.assertEqual(result, {"handle": "term_fixture", "worktreeId": "quant-id"})
        self.assertEqual(self.calls[0], ["worktree", "show", "--worktree", "id:quant-id"])
        self.assertEqual(self.calls[1], ["terminal", "create", "--worktree", "id:quant-id", "--command",
                                         "exec env CAGE_CODEX_SUBJECT=" + origin["subject"] +
                                         " /usr/local/bin/cage-codex-account run " + origin["account"] +
                                         " resume " + origin["thread"], "--keep-open"])

    def test_instance_switch_and_incomplete_inventory_are_indeterminate(self):
        def replace(*_):
            self.run_id = (self.runtime_id, 10, 19, os.getpid())
            return {"terminals": []}

        self.assertEqual(self.request("terminals", fixture=replace), {"runtime_id": ""})
        self.assertEqual(self.request("terminals", fixture=lambda *_: {
            "terminals": [], "truncated": True,
        }), {"runtime_id": ""})
        self.assertEqual(self.request("wait", handle="term_fixture")["result"]["wait"], {
            "handle": "term_fixture", "condition": "exit", "status": "exited", "satisfied": True, "exitCode": 0,
        })


if __name__ == "__main__":
    unittest.main()
