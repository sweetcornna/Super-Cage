"""Regression fixtures for dsh's native identity and SSH-owned launch state."""
import ctypes
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("dsh_ssh", Path(__file__).with_name("dsh-ssh.py"))
dsh = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dsh)


class DshSSHTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.binding = {
            "workspace": "/work/a project/鲸",
            "home": str(self.root),
            "owner": "cage:ssh-device-12345678",
            "profile": "tui",
        }

    def test_native_entry_only(self):
        for args, options in [
            (["tui"], []),
            (["--profile", "tui", "resume"], ["--resume"]),
            (["--profile=tui", "--resume=abc-2"], ["--resume", "abc-2"]),
            (["tui", "--new"], ["--new"]),
        ]:
            self.assertEqual(dsh.native_tui(args), options)
        for args in [
            [], ["version"], ["plugin", "--profile", "tui"], ["tui", "a prompt"],
            ["tui", "--resume=../other"], ["tui", "--patch", "./custom.yml"],
        ]:
            self.assertIsNone(dsh.native_tui(args))

    def test_connection_binding(self):
        auth = self.root / "auth"
        auth.write_bytes(b"publickey ssh-ed25519 AAAA\n")
        with patch.dict(os.environ, {"SSH_CONNECTION": "client 42 host 22", "SSH_USER_AUTH": str(auth)}, clear=True):
            owner = dsh.ssh_owner()
            self.assertEqual(owner, "openssh:" + dsh.digest(b"ssh-ed25519 AAAA"))
            os.environ["SSH_CONNECTION"] = "changed 123 host 22"
            self.assertEqual(dsh.ssh_owner(), owner)
            auth.write_bytes(b"publickey ssh-ed25519 BBBB\n")
            self.assertNotEqual(dsh.ssh_owner(), owner)
            os.environ["CAGE_DSH_SSH_DEVICE"] = "a-valid-cage-device"
            self.assertEqual(dsh.ssh_owner(), "cage:a-valid-cage-device")
            del os.environ["CAGE_DSH_SSH_DEVICE"]
            auth.write_bytes(b"password\n")
            self.assertIsNone(dsh.ssh_owner())

    def test_exact_current_header_and_historical_generation(self):
        workspace = self.binding["workspace"]
        self.assertEqual(dsh.project_key(workspace), "--work-a~0020project-~9CB8--")
        self.assertEqual(dsh.project_key("/some/🐋"), "--some-~D83D~DC0B--")
        self.assertEqual(dsh.project_key("/a/../b"), "--a-..-b--")
        folder = self.root / "sessions" / dsh.project_key(workspace) / "session-uuid"
        folder.mkdir(parents=True)
        header = {"version": 0, "id": "session-uuid", "cwd": workspace, "type": "session"}
        old = folder / "session.jsonl"
        old.write_text(json.dumps(header) + "\n")
        self.assertTrue(dsh.confirmed_session(self.binding, "session-uuid"))
        self.assertFalse(dsh.confirmed_session(self.binding, "some-other-session"))
        self.assertFalse(dsh.confirmed_session(dict(self.binding, home=str(self.root / "other")), "session-uuid"))
        # Paths with the same dsh storage key still require the exact cwd header.
        self.assertFalse(dsh.confirmed_session(dict(self.binding, workspace=workspace.replace("/a ", "-a ")), "session-uuid"))
        active = folder / "session.v4.jsonl"
        active.write_text(json.dumps(dict(header, version=4, id="wrong-identity")) + "\n")
        self.assertFalse(dsh.confirmed_session(self.binding, "session-uuid"))
        active.write_text(json.dumps(dict(header, version=4)) + "\n" + "must not parse the transcript\n")
        self.assertTrue(dsh.confirmed_session(self.binding, "session-uuid"))
        active.unlink()
        active.symlink_to(old)
        self.assertFalse(dsh.confirmed_session(self.binding, "session-uuid"))

    def test_reads_only_the_first_native_zstandard_frame(self):
        try:
            zstd = ctypes.CDLL("libzstd.so.1")
        except OSError:
            self.skipTest("Linux libzstd is unavailable")
        zstd.ZSTD_compress.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int]
        zstd.ZSTD_compress.restype = ctypes.c_size_t

        def compress(value):
            output = ctypes.create_string_buffer(1024)
            count = zstd.ZSTD_compress(output, len(output), value, len(value), 3)
            self.assertLess(count, len(output))
            return output.raw[:count]

        header = json.dumps({
            "type": "session", "version": 4, "id": "session-uuid", "cwd": self.binding["workspace"],
        }).encode() + b"\n"
        first = compress(header)
        frames = io.BytesIO(first + compress(b"unrelated native event\n"))
        self.assertEqual(dsh.zstd_header(frames), header)
        self.assertEqual(frames.tell(), len(first))
        folder = self.root / "sessions" / dsh.project_key(self.binding["workspace"]) / "session-uuid"
        folder.mkdir(parents=True)
        (folder / "session.v4.jsonl.zstd").write_bytes(frames.getvalue())
        self.assertTrue(dsh.confirmed_session(self.binding, "session-uuid"))

    def test_auto_reconnect_requires_unique_active_original_device(self):
        workspace = self.root / "workspace"
        workspace.mkdir()
        binding = dict(self.binding, workspace=str(workspace))
        owner = binding["owner"]
        journal = self.root / "journal"
        journal.mkdir(mode=0o700)

        def note(key, recorded_binding, closed):
            directory = journal / (key * 64)
            directory.mkdir(mode=0o700, exist_ok=True)
            dsh.write_state(directory, {"version": 1, "binding": recorded_binding, "closed": closed})

        with patch.object(dsh, "state_root", return_value=journal), patch.dict(os.environ, {}, clear=True):
            note("a", binding, False)
            note("b", dict(binding, owner="cage:another-device"), False)
            self.assertEqual(dsh.automatic_binding(owner), binding)
            self.assertIsNone(dsh.automatic_binding("cage:unknowndevice"))
            os.environ["DSH_HOME"] = str(self.root / "different-profile")
            self.assertIsNone(dsh.automatic_binding(owner))
            del os.environ["DSH_HOME"]
            note("c", dict(binding, workspace=str(self.root)), False)
            self.assertIsNone(dsh.automatic_binding(owner))
            note("c", dict(binding, workspace=str(self.root)), True)
            self.assertEqual(dsh.automatic_binding(owner), binding)
            note("a", binding, True)
            self.assertIsNone(dsh.automatic_binding(owner))

    def test_selected_session_switch_and_close_fence(self):
        state = {"version": 1, "binding": self.binding, "closed": False, "generation": "pane1", "sequence": 0}
        dsh.write_state(self.root, state)

        def report(generation, session, sequence, agent="dsh"):
            return dsh.report(self.root, generation, {
                "method": "pane.report_agent_session",
                "params": {
                    "agent_session_id": session, "seq": sequence, "agent": agent,
                    "source": "custom:dsh-tui", "pane_id": generation,
                },
            })

        report("pane1", "first", 1)
        report("pane1", "second", 3)
        report("pane1", "stale", 2)
        report("pane2", "wrong-pane", 4)
        report("pane1", "wrong-agent", 4, agent="other")
        current = dsh.read_state(self.root)
        self.assertEqual(current["session_id"], "second")
        dsh.write_state(self.root, dict(current, closed=True))
        report("pane1", "late", 5)
        self.assertEqual(dsh.read_state(self.root)["session_id"], "second")
        self.assertEqual((self.root / "state.json").stat().st_mode & 0o777, 0o600)


if __name__ == "__main__":
    unittest.main()
