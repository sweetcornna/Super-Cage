import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import uuid
from unittest.mock import patch

sys.dont_write_bytecode = True
here = Path(__file__).resolve().parent


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class DiscoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.home = Path(temporary.name).resolve()
        self.config = self.home / ".claude"
        self.bin = self.home / "bin"
        self.bin.mkdir()
        self.runs = self.home / "claude-runs"
        self.agents = self.home / "agents.json"
        self.agents.write_text("[]")
        claude = self.bin / "claude"
        claude.write_text("#!/bin/sh\necho run >> %s\ncat %s\n" % (self.runs, self.agents))
        claude.chmod(0o700)
        env = {**os.environ, "HOME": str(self.home), "CLAUDE_CONFIG_DIR": str(self.config), "PATH": str(self.bin) + ":/usr/bin:/bin"}
        environment = patch.dict(os.environ, env)
        environment.start()
        self.addCleanup(environment.stop)
        self.root = self.home / ".local/share/cage/cli-channels"
        self.plain = load("plain_channel", here / "cli_channel.py")
        self.channel = load("resident_channel", here / "cli_channel.py")
        for module in (self.plain, self.channel):
            root = patch.object(module, "ROOT", self.root)
            root.start()
            self.addCleanup(root.stop)
        self.summaries = []
        original = self.channel.history_summary
        self.channel.history_summary = lambda path: (self.summaries.append(path), original(path))[1]
        self.discovery = load("discovery", here / "cli_discovery.py")
        self.discovery.install(self.channel)
        self.project = str(self.home / "project")
        self.request = {"accounts": ["default"], "workspaces": {self.project: "/host/project"}}

    def history(self, session, workspace, text="hello"):
        path = self.config / "projects/project" / (session + ".jsonl")
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a") as stream:
            stream.write(json.dumps({"cwd": workspace, "type": "user", "uuid": str(uuid.uuid4()), "message": {"content": text}}) + "\n")
        return path

    def state(self, connection, **value):
        self.plain.save(self.root / connection / "state.json", value)

    def register(self, pid, session, **extra):
        folder = self.config / "sessions"
        folder.mkdir(parents=True, exist_ok=True)
        (folder / ("%d.json" % pid)).write_text(json.dumps({"pid": pid, "sessionId": session, "startedAt": 1, "status": "idle", **extra}))

    def claude_runs(self):
        return self.runs.read_text().count("run") if self.runs.exists() else 0

    def test_matches_channel_discover(self):
        sessions = [str(uuid.uuid4()) for _ in range(4)]
        self.history(sessions[0], self.project)
        self.history(sessions[1], self.project + "/nested")
        self.history(sessions[2], str(self.home / "elsewhere"))
        connection, older, state_only = str(uuid.uuid4()), str(uuid.uuid4()), str(uuid.uuid4())
        self.state(connection, cli_id=sessions[0], config=str(self.config), workspace=self.project, pid=os.getpid(), channel_pid=os.getpid(), busy=True, updated_at=2)
        self.state(older, cli_id=sessions[0], config=str(self.config), workspace=self.project, pid=None, updated_at=5)
        self.state(state_only, cli_id=sessions[3], config=str(self.config), workspace=self.project, pid=os.getpid(), updated_at=3)
        self.agents.write_text(json.dumps([{"pid": os.getpid(), "sessionId": sessions[1], "entrypoint": "claude-desktop"}]))
        self.register(os.getpid(), sessions[1], entrypoint="claude-desktop")
        expected = self.plain.discover(self.request)
        for _ in range(2):
            found, hashed = self.discovery.discover(self.request)
            self.assertEqual(found, expected)
            self.assertEqual(hashed, self.discovery.digest(expected))
        self.assertEqual({item["cli_id"]: item["connection"] for item in found if item["cli_id"] == sessions[0]}, {sessions[0]: connection})
        self.assertTrue(any(item["cli_id"] == sessions[1] and item["claude_desktop"] for item in found))

    def test_reads_only_what_changed(self):
        session = str(uuid.uuid4())
        path = self.history(session, self.project)
        self.discovery.discover(self.request)
        self.assertEqual((len(self.summaries), self.claude_runs()), (1, 1))
        self.discovery.discover(self.request)
        self.assertEqual((len(self.summaries), self.claude_runs()), (1, 1), "an unchanged guest was read again")
        self.history(session, self.project, "renamed")
        self.discovery.discover(self.request)
        self.assertEqual(len(self.summaries), 2, "an appended transcript was not read")
        self.assertEqual(self.claude_runs(), 1)
        self.register(os.getpid(), session)
        self.agents.write_text(json.dumps([{"pid": os.getpid(), "sessionId": session}]))
        found, _ = self.discovery.discover(self.request)
        self.assertEqual(self.claude_runs(), 2, "a new registered process did not refresh the listing")
        self.assertTrue(found[0]["running"])
        self.register(os.getpid(), session, status="busy", statusUpdatedAt=9)
        self.discovery.discover(self.request)
        self.assertEqual(self.claude_runs(), 2, "a status change reran the listing")
        with patch.object(self.discovery, "AGENTS_MAX_AGE", 0):
            self.discovery.discover(self.request)
        self.assertEqual(self.claude_runs(), 3, "the listing outlived its age limit")
        self.assertEqual(path.exists(), True)

    def test_cached_listing_drops_an_exited_process(self):
        session = str(uuid.uuid4())
        self.history(session, self.project)
        child = subprocess.Popen(["sleep", "30"])
        self.addCleanup(child.kill)
        self.agents.write_text(json.dumps([{"pid": child.pid, "sessionId": session}]))
        found, _ = self.discovery.discover(self.request)
        self.assertTrue(found[0]["running"])
        child.kill()
        child.wait()
        found, _ = self.discovery.discover(self.request)
        self.assertFalse(found[0]["running"])

    def test_watch_answers_on_change_timeout_and_cancel(self):
        session = str(uuid.uuid4())
        self.history(session, self.project)
        _, known = self.discovery.discover(self.request)
        cancelled = threading.Event()
        quiet = self.discovery.handle("watch", dict(self.request, known=known, timeout=0.3, cancelled=cancelled))
        self.assertEqual(quiet, {"hash": known, "changed": False})
        timer = threading.Timer(0.3, lambda: self.history(session, self.project, "more"))
        timer.start()
        self.addCleanup(timer.cancel)
        started = time.monotonic()
        changed = self.discovery.handle("watch", dict(self.request, known=known, timeout=10, cancelled=cancelled))
        self.assertTrue(changed["changed"])
        self.assertLess(time.monotonic() - started, 3)
        _, known = self.discovery.discover(self.request)
        threading.Timer(0.2, cancelled.set).start()
        started = time.monotonic()
        self.assertFalse(self.discovery.handle("watch", dict(self.request, known=known, timeout=10, cancelled=cancelled))["changed"])
        self.assertLess(time.monotonic() - started, 3)

    def test_view_matches_channel_and_waits_for_its_session(self):
        session, other = str(uuid.uuid4()), str(uuid.uuid4())
        self.history(session, self.project)
        self.history(other, self.project)
        connection = str(uuid.uuid4())
        self.state(connection, cli_id=session, config=str(self.config), workspace=self.project, pid=os.getpid(), channel_pid=os.getpid(), updated_at=1)
        request = {"cli_id": session, "config": str(self.config), "connection": connection, "account": "default"}
        view = self.discovery.handle("view", dict(request, cancelled=threading.Event()))
        key = view.pop("watch_key")
        self.assertEqual(view, self.plain.view(self.config, session))
        quiet = self.discovery.handle("view-wait", dict(request, key=key, timeout=0.3, cancelled=threading.Event()))
        self.assertEqual(quiet, {"key": key, "changed": False})
        self.history(other, self.project, "unrelated")
        self.assertFalse(self.discovery.handle("view-wait", dict(request, key=key, timeout=0.3, cancelled=threading.Event()))["changed"])
        self.state(connection, cli_id=session, config=str(self.config), workspace=self.project, pid=os.getpid(), channel_pid=os.getpid(), updated_at=2, busy=True)
        self.assertTrue(self.discovery.handle("view-wait", dict(request, key=key, timeout=5, cancelled=threading.Event()))["changed"])
        key = self.discovery.handle("view", dict(request, cancelled=threading.Event()))["watch_key"]
        self.history(session, self.project, "reply")
        self.assertTrue(self.discovery.handle("view-wait", dict(request, key=key, timeout=5, cancelled=threading.Event()))["changed"])

    def test_server_framing(self):
        code = self.home / ".local/share/cage/cli-channel-code"
        code.mkdir(parents=True)
        name = "a" * 64
        shutil.copy(here / "cli_channel.py", code / (name + ".py"))
        session = str(uuid.uuid4())
        self.history(session, self.project)
        digest = hashlib.sha256((code / (name + ".py")).read_bytes()).hexdigest()
        server = subprocess.Popen([sys.executable, "-I", str(here / "cli_discovery.py"), name, digest], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=os.environ)
        def stop():
            server.kill()
            server.wait()
            server.stdin.close()
            server.stdout.close()
        self.addCleanup(stop)
        self.assertEqual(server.stdout.readline(), b"CAGE-GUEST-SESSION 1\n")

        def send(ident, mode, value):
            payload = json.dumps({"mode": mode, "input": value}).encode()
            server.stdin.write(b"R %d %d\n" % (ident, len(payload)) + payload)
            server.stdin.flush()

        def receive():
            _, ident, exit, out, err = server.stdout.readline().split()
            data = server.stdout.read(int(out) + int(err))
            return int(ident), int(exit), data[:int(out)]

        send(1, "discover", self.request)
        ident, exit, output = receive()
        reply = json.loads(output)
        self.assertEqual((ident, exit, [item["cli_id"] for item in reply["sessions"]]), (1, 0, [session]))
        send(2, "watch", dict(self.request, known=reply["hash"], timeout=20))
        send(3, "nonsense", {})
        self.assertEqual(receive()[:2], (3, 1))
        # A request that fails in the channel helper's own code is refused on
        # its own; the process keeps serving.
        self.agents.write_text("not a listing")
        self.register(os.getpid(), session)
        send(4, "discover", self.request)
        self.assertEqual(receive()[:2], (4, 6))
        self.agents.write_text("[]")
        send(5, "discover", self.request)
        self.assertEqual(receive()[:2], (5, 0))
        started = time.monotonic()
        server.stdin.write(b"C 2\n")
        server.stdin.flush()
        ident, exit, output = receive()
        self.assertEqual((ident, exit, json.loads(output)["changed"]), (2, 0, False))
        self.assertLess(time.monotonic() - started, 5)
        server.stdin.write(b"garbage\n")
        server.stdin.flush()
        self.assertEqual(server.wait(5), 0)

    def test_retires_after_its_lifetime(self):
        code = self.home / ".local/share/cage/cli-channel-code"
        code.mkdir(parents=True)
        name = "c" * 64
        shutil.copy(here / "cli_channel.py", code / (name + ".py"))
        digest = hashlib.sha256((code / (name + ".py")).read_bytes()).hexdigest()
        server = subprocess.Popen([sys.executable, "-I", str(here / "cli_discovery.py"), name, digest, "1"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=os.environ)
        def stop():
            server.kill()
            server.wait()
            server.stdin.close()
            server.stdout.close()
        self.addCleanup(stop)
        self.assertEqual(server.stdout.readline(), b"CAGE-GUEST-SESSION 1\n")
        payload = json.dumps({"mode": "discover", "input": self.request}).encode()
        server.stdin.write(b"R 1 %d\n" % len(payload) + payload)
        server.stdin.flush()
        _, ident, exit, out, err = server.stdout.readline().split()
        self.assertEqual((ident, exit), (b"1", b"0"))
        server.stdout.read(int(out) + int(err))
        time.sleep(1.1)
        server.stdin.write(b"R 2 %d\n" % len(payload) + payload)
        server.stdin.flush()
        line = server.stdout.readline()
        self.assertTrue(line.startswith(b"D"), line)
        self.assertEqual(line, b"D 2 4 0 0\n")
        server.stdin.close()
        self.assertEqual(server.wait(5), 0)

    def test_codex_discover_runs_the_installed_codex_helper(self):
        source = (here / "codex_sessions.py").read_bytes()
        name = hashlib.sha256(source).hexdigest()
        with patch.object(self.discovery, "codex_name", name), patch.object(self.discovery, "codex", None):
            with self.assertRaises(self.discovery.Missing):
                self.discovery.handle("codex-discover", {"accounts": ["default"], "workspaces": [self.project]})
            folder = self.home / ".local/share/cage/codex-session-code"
            folder.mkdir(parents=True)
            (folder / (name + ".py")).write_bytes(source + b"# changed\n")
            with self.assertRaises(self.discovery.Missing):
                self.discovery.handle("codex-discover", {"accounts": ["default"], "workspaces": [self.project]})
            (folder / (name + ".py")).write_bytes(source)
            codex = load("plain_codex", here / "codex_sessions.py")
            thread = str(uuid.uuid4())
            rollout = self.home / ".codex/sessions/2026/11/21" / ("rollout-2026-11-21T00-00-00-" + thread + ".jsonl")
            rollout.parent.mkdir(parents=True)
            rollout.write_text(json.dumps({"type": "session_meta", "payload": {"id": thread, "cwd": self.project}}) + "\n" +
                               json.dumps({"type": "response_item", "payload": {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "codex title"}]}}) + "\n")
            request = {"accounts": ["default"], "workspaces": [self.project]}
            expected = codex.discover(request)
            self.assertEqual(self.discovery.handle("codex-discover", dict(request)), expected)
            self.assertEqual([item["thread_id"] for item in expected], [thread])
            self.assertEqual(self.discovery.handle("codex-discover", {"accounts": ["default"], "workspaces": [self.project], "bad": None}), expected)

    def test_refuses_to_start_without_the_intact_channel_helper(self):
        code = self.home / ".local/share/cage/cli-channel-code"
        code.mkdir(parents=True)
        name = "b" * 64
        for digest in ("0" * 64, None):
            if digest is None:
                (code / (name + ".py")).write_text("import sys\nsys.stdin.read()\n")
                digest = "0" * 64
            result = subprocess.run([sys.executable, "-I", str(here / "cli_discovery.py"), name, digest], capture_output=True, env=os.environ, timeout=10, stdin=subprocess.DEVNULL)
            self.assertEqual((result.returncode, result.stdout), (3, b""))


if __name__ == "__main__":
    unittest.main()
