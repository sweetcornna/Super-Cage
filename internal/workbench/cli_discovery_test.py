import hashlib
import importlib.util
import json
import os
import re
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
        self.session = str(uuid.uuid4())

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

    def test_view_wait_wakes_when_a_running_subagent_writes(self):
        session, connection = str(uuid.uuid4()), str(uuid.uuid4())
        path = self.history(session, self.project, "look around")
        stamp = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()) + ".000Z"
        with path.open("a") as stream:
            stream.write(json.dumps({"cwd": self.project, "type": "assistant", "uuid": str(uuid.uuid4()), "timestamp": stamp, "message": {"content": [
                {"type": "tool_use", "id": "toolu_child", "name": "Agent", "input": {"description": "梳理目录", "prompt": "p"}}]}}) + "\n")
        folder = path.parent / session / "subagents"
        folder.mkdir(parents=True)
        (folder / "agent-a1b2c3.meta.json").write_text(json.dumps({"toolUseId": "toolu_child", "agentType": "Explore"}))
        child = folder / "agent-a1b2c3.jsonl"
        child.write_text(json.dumps({"type": "assistant", "isSidechain": True, "message": {"content": [{"type": "tool_use", "id": "c1", "name": "Read", "input": {"file_path": "a.py"}}]}}) + "\n")
        self.state(connection, cli_id=session, config=str(self.config), workspace=self.project, pid=os.getpid(), channel_pid=os.getpid(), updated_at=1)
        request = {"cli_id": session, "config": str(self.config), "connection": connection, "account": "default"}
        view = self.discovery.handle("view", dict(request, cancelled=threading.Event()))
        key = view.pop("watch_key")
        self.assertEqual(self.without_tasks(view), self.without_tasks(self.plain.view(self.config, session)))
        self.assertEqual(view["tasks"]["running"][0]["now"], {"name": "Read", "detail": "a.py"})
        self.assertEqual(self.discovery.view_key(self.config, session), key)
        self.assertFalse(self.discovery.handle("view-wait", dict(request, key=key, timeout=0.3, cancelled=threading.Event()))["changed"])
        def append(text):
            with child.open("a") as stream:
                stream.write(text)
        threading.Timer(0.2, append, [json.dumps({"type": "assistant", "isSidechain": True, "message": {"content": [{"type": "tool_use", "id": "c2", "name": "Grep", "input": {"pattern": "x"}}]}}) + "\n"]).start()
        started = time.monotonic()
        self.assertTrue(self.discovery.handle("view-wait", dict(request, key=key, timeout=5, cancelled=threading.Event()))["changed"])
        self.assertLess(time.monotonic() - started, 3)
        view = self.discovery.handle("view", dict(request, cancelled=threading.Event()))
        self.assertEqual(view["tasks"]["running"][0]["now"], {"name": "Grep", "detail": "x"})
        # Once the child has ended its files are no longer part of the key.
        (folder / "agent-a1b2c3.meta.json").write_text(json.dumps({"toolUseId": "toolu_child", "finished": True}))
        view = self.discovery.handle("view", dict(request, cancelled=threading.Event()))
        self.assertNotIn("tasks", view)
        key = view["watch_key"]
        append("{}\n")
        self.assertFalse(self.discovery.handle("view-wait", dict(request, key=key, timeout=0.3, cancelled=threading.Event()))["changed"])

    TASK_FIELDS = ("kind", "state", "agent_type", "started_at", "ended_at", "now", "calls", "calls_partial")

    def without_tasks(self, view):
        # The channel helper's answer: no task fields, and its own updated_at.
        view = json.loads(json.dumps(view))
        view.pop("tasks", None)
        view.pop("updated_at", None)
        for turn in view.get("turns", []):
            for tool in turn.get("tools", []):
                for field in self.TASK_FIELDS:
                    tool.pop(field, None)
        return view

    def task_history(self, rows, started=1795000000):
        # Synthetic Claude Code rows in the shapes recorded by 2.1.330.
        path = self.config / "projects/project" / (self.session + ".jsonl")
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a") as stream:
            for row in rows:
                row = dict(row)
                stamp = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(started + row.pop("at", 0))) + ".250Z"
                stream.write(json.dumps({"cwd": self.project, "uuid": str(uuid.uuid4()), "timestamp": stamp, **row}) + "\n")
        return path

    def live_registry(self, started_ms=1794999000000):
        self.register(os.getpid(), self.session, startedAt=started_ms)

    def notice(self, tool, task, status, at):
        text = "<task-notification>\n<task-id>%s</task-id>\n<tool-use-id>%s</tool-use-id>\n<output-file>/tmp/x/%s.output</output-file>\n<status>%s</status>\n<summary>Background command finished</summary>\n</task-notification>" % (task, tool, task, status)
        return {"type": "user", "at": at, "origin": {"kind": "task-notification"}, "message": {"role": "user", "content": text}}

    def view_now(self):
        view = self.discovery.view({"cli_id": self.session, "config": str(self.config)})
        view.pop("watch_key")
        return view

    def test_background_command_runs_until_its_notice(self):
        self.live_registry()
        path = self.task_history([
            {"type": "user", "at": 0, "message": {"role": "user", "content": "build it"}},
            {"type": "assistant", "at": 1, "message": {"content": [{"type": "tool_use", "id": "toolu_bg1", "name": "Bash", "input": {"command": "private-command", "description": "构建发布包", "run_in_background": True}}]}},
            {"type": "user", "at": 1.5, "message": {"content": [{"type": "tool_result", "tool_use_id": "toolu_bg1", "content": "Command running in background with ID: bk1a2b3c"}]},
             "toolUseResult": {"backgroundTaskId": "bk1a2b3c", "interrupted": False}},
            {"type": "assistant", "at": 2, "message": {"content": [{"type": "tool_use", "id": "repl_bg2", "name": "Bash", "input": {"command": "private-command", "timeout": 1000}}]}},
            {"type": "user", "at": 3, "message": {"content": [{"type": "tool_result", "tool_use_id": "repl_bg2", "content": ""}]},
             "toolUseResult": {"backgroundTaskId": "bz9y8x7w", "timedOutAfterMs": 1000}},
            {"type": "assistant", "at": 4, "message": {"content": [{"type": "text", "text": "started"}]}},
            {"type": "system", "subtype": "turn_duration", "at": 4},
        ])
        view = self.view_now()
        tools = view["turns"][0]["tools"]
        self.assertEqual([(t["name"], t["status"], t.get("kind"), t.get("state")) for t in tools],
                         [("Bash", "completed", "background_command", "running"), ("Bash", "completed", "background_command", "running")])
        self.assertEqual(tools[0]["started_at"], 1795000001.25)
        self.assertNotIn("ended_at", tools[0])
        self.assertEqual(view["tasks"]["count"], 2)
        self.assertEqual([item["id"] for item in view["tasks"]["running"]], ["repl_bg2", "toolu_bg1"])
        # The strip's label is the card's label.
        self.assertEqual(view["tasks"]["running"][1].get("detail"), tools[0].get("detail"))
        self.assertNotIn("private-", json.dumps(view))
        self.assertEqual(self.without_tasks(view), self.without_tasks(self.plain.view(self.config, self.session)))
        # The notice opens its own turn; it ends the first command by tool-use-id
        # and the second by task-id only. Only the appended rows are read.
        offset = self.discovery.task_scans[str(path)][1]
        self.assertEqual(offset, path.stat().st_size)
        self.task_history([self.notice("toolu_bg1", "bk1a2b3c", "completed", 30), self.notice("", "bz9y8x7w", "failed", 31)])
        view = self.view_now()
        tools = view["turns"][0]["tools"]
        self.assertEqual([(t["state"], t.get("ended_at")) for t in tools], [("completed", 1795000030.25), ("failed", 1795000031.25)])
        self.assertNotIn("tasks", view)
        self.assertEqual(len(view["turns"]), 3)
        self.assertNotIn("tools", view["turns"][1])

    def test_stopped_killed_and_failed_launches(self):
        self.live_registry()
        self.task_history([
            {"type": "user", "at": 0, "message": {"role": "user", "content": "go"}},
            {"type": "assistant", "at": 1, "message": {"content": [
                {"type": "tool_use", "id": "t-stop", "name": "Bash", "input": {"command": "x", "run_in_background": True}},
                {"type": "tool_use", "id": "t-kill", "name": "Bash", "input": {"command": "x", "run_in_background": True}},
                {"type": "tool_use", "id": "t-refused", "name": "Bash", "input": {"command": "x", "run_in_background": True}},
                {"type": "tool_use", "id": "t-plain", "name": "Bash", "input": {"command": "x", "timeout": 1000}},
            ]}},
            {"type": "user", "at": 2, "message": {"content": [{"type": "tool_result", "tool_use_id": "t-stop", "content": ""}]}, "toolUseResult": {"backgroundTaskId": "bstop1"}},
            {"type": "user", "at": 2, "message": {"content": [{"type": "tool_result", "tool_use_id": "t-kill", "content": ""}]}, "toolUseResult": {"backgroundTaskId": "bkill1"}},
            {"type": "user", "at": 2, "message": {"content": [{"type": "tool_result", "tool_use_id": "t-refused", "is_error": True, "content": "not available"}]}},
            {"type": "user", "at": 2, "message": {"content": [{"type": "tool_result", "tool_use_id": "t-plain", "content": "ok"}]}, "toolUseResult": {"interrupted": False}},
            {"type": "assistant", "at": 5, "message": {"content": [{"type": "tool_use", "id": "t-taskstop", "name": "TaskStop", "input": {"task_id": "bstop1"}}]}},
            {"type": "user", "at": 6, "message": {"content": [{"type": "tool_result", "tool_use_id": "t-taskstop", "content": ""}]},
             "toolUseResult": {"task_id": "bstop1", "task_type": "local_bash", "message": "Successfully stopped task"}},
            self.notice("t-kill", "bkill1", "killed", 7),
        ])
        view = self.view_now()
        tools = {t["id"]: t for t in view["turns"][0]["tools"]}
        self.assertEqual((tools["t-stop"]["state"], tools["t-stop"]["ended_at"]), ("stopped", 1795000006.25))
        self.assertEqual((tools["t-kill"]["state"], tools["t-kill"]["ended_at"]), ("stopped", 1795000007.25))
        self.assertEqual((tools["t-refused"]["status"], tools["t-refused"]["state"]), ("failed", "failed"))
        self.assertNotIn("kind", tools["t-plain"])
        self.assertNotIn("kind", tools["t-taskstop"])
        self.assertNotIn("tasks", view)

    def subagent_files(self, agent, tool, calls, finished=None):
        folder = self.config / "projects/project" / self.session / "subagents"
        folder.mkdir(parents=True, exist_ok=True)
        meta = {"agentType": "general-purpose", "description": "x", "toolUseId": tool, "spawnDepth": 1}
        if finished is not None:
            meta["finished"] = finished
        (folder / ("agent-" + agent + ".meta.json")).write_text(json.dumps(meta))
        rows = [{"type": "user", "isSidechain": True, "agentId": agent, "message": {"role": "user", "content": "private-prompt"}}]
        for name, args in calls:
            rows.append({"type": "assistant", "isSidechain": True, "agentId": agent, "message": {"content": [{"type": "tool_use", "id": str(uuid.uuid4()), "name": name, "input": args}]}})
            rows.append({"type": "user", "isSidechain": True, "agentId": agent, "message": {"content": [{"type": "tool_result", "tool_use_id": "x", "content": "private-output"}]}})
        rows.append({"type": "api-request-blob", "agentId": agent, "message": {"content": [{"type": "tool_use", "id": "blob", "name": "Blob", "input": {}}]}})
        history = folder / ("agent-" + agent + ".jsonl")
        history.write_text("".join(json.dumps(row) + "\n" for row in rows))
        return history

    def test_subagent_now_line_count_and_finish(self):
        self.live_registry()
        self.task_history([
            {"type": "user", "at": 0, "message": {"role": "user", "content": "look around"}},
            {"type": "assistant", "at": 1, "message": {"content": [
                {"type": "tool_use", "id": "repl_agent1", "name": "Agent", "input": {"description": "梳理目录", "prompt": "private-prompt", "subagent_type": "Explore", "run_in_background": True}},
                {"type": "tool_use", "id": "toolu_fore", "name": "Agent", "input": {"description": "前台检查", "prompt": "private-prompt"}},
            ]}},
            {"type": "user", "at": 1.2, "message": {"content": [{"type": "tool_result", "tool_use_id": "repl_agent1", "content": ""}]},
             "toolUseResult": {"status": "async_launched", "isAsync": True, "agentId": "a0123456789abcdef", "description": "梳理目录"}},
        ])
        history = self.subagent_files("a0123456789abcdef", "repl_agent1", [("Read", {"file_path": "src/app.py"}), ("Bash", {"command": "private-command", "description": "统计行数"}), ("Poll", {})])
        # The foreground one has no result yet: its meta file names it.
        self.subagent_files("f0123456789abcdef", "toolu_fore", [("Grep", {"pattern": "TODO"})])
        view = self.view_now()
        tools = {t["id"]: t for t in view["turns"][0]["tools"]}
        background, foreground = tools["repl_agent1"], tools["toolu_fore"]
        self.assertEqual((background["kind"], background["state"], background["agent_type"]), ("background_agent", "running", "Explore"))
        self.assertEqual(background["now"], {"name": "Bash", "detail": "统计行数"})
        self.assertEqual(background["calls"], 3)
        self.assertNotIn("calls_partial", background)
        self.assertEqual((foreground["kind"], foreground["state"], foreground["agent_type"]), ("subagent", "running", "general-purpose"))
        self.assertEqual((foreground["now"], foreground["calls"]), ({"name": "Grep", "detail": "TODO"}, 1))
        self.assertEqual(view["tasks"]["count"], 2)
        self.assertNotIn("private-", json.dumps(view))
        before = view["updated_at"]
        time.sleep(0.02)
        with history.open("a") as stream:
            stream.write(json.dumps({"type": "assistant", "isSidechain": True, "message": {"content": [{"type": "tool_use", "id": "n", "name": "Edit", "input": {"file_path": "src/app.py", "old_string": "private-content"}}]}}) + "\n")
        view = self.view_now()
        background = next(t for t in view["turns"][0]["tools"] if t["id"] == "repl_agent1")
        self.assertEqual((background["now"], background["calls"]), ({"name": "Edit", "detail": "src/app.py"}, 4))
        self.assertGreater(view["updated_at"], before)
        meta = history.with_name("agent-a0123456789abcdef.meta.json")
        meta.write_text(json.dumps({**json.loads(meta.read_text()), "finished": True}))
        view = self.view_now()
        background = next(t for t in view["turns"][0]["tools"] if t["id"] == "repl_agent1")
        self.assertEqual(background["state"], "completed")
        self.assertIn("ended_at", background)
        self.assertEqual(view["tasks"]["count"], 1)

    def test_children_live_no_longer_than_their_process(self):
        self.task_history([
            {"type": "user", "at": 0, "message": {"role": "user", "content": "go"}},
            {"type": "assistant", "at": 1, "message": {"content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "x", "run_in_background": True}}]}},
            {"type": "user", "at": 2, "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "content": ""}]}, "toolUseResult": {"backgroundTaskId": "b1"}},
        ])
        view = self.view_now()
        self.assertEqual(view["turns"][0]["tools"][0]["state"], "stopped")
        self.assertNotIn("tasks", view)
        # A process started after the launch (a resumed session) did not start it.
        self.live_registry(started_ms=(1795000000 + 60) * 1000)
        self.assertEqual(self.view_now()["turns"][0]["tools"][0]["state"], "stopped")
        # The cached index was not changed by an answer.
        self.live_registry()
        self.assertEqual(self.view_now()["turns"][0]["tools"][0]["state"], "running")

    def test_running_children_are_capped(self):
        self.live_registry()
        launches = [{"type": "tool_use", "id": "agent-%02d" % i, "name": "Agent", "input": {"description": "任务 %d" % i, "prompt": "p"}} for i in range(20)]
        rows = [{"type": "user", "at": 0, "message": {"role": "user", "content": "fan out"}}]
        rows += [{"type": "assistant", "at": 1 + i, "message": {"content": [launch]}} for i, launch in enumerate(launches)]
        self.task_history(rows)
        for i in range(20):
            self.subagent_files("%017x" % i, "agent-%02d" % i, [("Read", {"file_path": "f%d" % i})])
        read = []
        original = self.discovery.child_activity
        with patch.object(self.discovery, "child_activity", side_effect=lambda path: (read.append(path), original(path))[1]):
            view = self.view_now()
        self.assertEqual(len(read), self.discovery.CHILD_LIMIT)
        self.assertEqual(view["tasks"]["count"], 20)
        self.assertEqual(len(view["tasks"]["running"]), 16)
        self.assertEqual(view["tasks"]["running"][0]["id"], "agent-19")
        self.assertEqual(sum(1 for t in view["turns"][0]["tools"] if "now" in t), 16)

    def test_old_transcripts_keep_todays_output(self):
        self.live_registry()
        self.task_history([
            {"type": "user", "message": {"content": "run"}},
            {"type": "assistant", "message": {"content": [
                {"type": "tool_use", "id": "tool-1", "name": "Bash", "input": {"description": "检查结果", "command": "private-command"}},
                {"type": "tool_use", "id": "tool-2", "name": "Read", "input": {"file_path": "result.md"}},
            ]}},
            {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "tool-2", "content": "x"}, {"type": "tool_result", "tool_use_id": "tool-1", "is_error": True}]}},
            {"type": "user", "isMeta": True, "message": {"content": "<task-notification><summary>完成</summary></task-notification>"}},
        ])
        reads = []
        with patch.object(self.discovery, "process_since", side_effect=lambda *a: reads.append(a)), patch.object(self.discovery, "child_activity", side_effect=lambda *a: reads.append(a)):
            view = self.view_now()
        self.assertEqual(reads, [], "a transcript without tasks reads nothing more")
        self.assertEqual(view, self.plain.view(self.config, self.session))
        found, _ = self.discovery.discover(self.request)
        self.assertEqual(found, self.plain.discover(self.request))

    def test_discovery_counts_tasks_of_live_sessions_incrementally(self):
        path = self.task_history([
            {"type": "user", "at": 0, "message": {"role": "user", "content": "go"}},
            {"type": "assistant", "at": 1, "message": {"content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "x", "run_in_background": True}},
                                                                   {"type": "tool_use", "id": "t2", "name": "Agent", "input": {"description": "d", "prompt": "p"}}]}},
            {"type": "user", "at": 2, "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "content": ""}]}, "toolUseResult": {"backgroundTaskId": "b1"}},
        ])
        found, _ = self.discovery.discover(self.request)
        self.assertNotIn("running_tasks", found[0])
        self.assertEqual(self.discovery.task_scans, {}, "a session without a live process is not read")
        self.live_registry()
        self.agents.write_text(json.dumps([{"pid": os.getpid(), "sessionId": self.session}]))
        found, hashed = self.discovery.discover(self.request)
        self.assertEqual(found[0]["running_tasks"], 2)
        self.assertEqual(self.discovery.task_scans[str(path)][1], path.stat().st_size)
        self.task_history([self.notice("t1", "b1", "completed", 30)])
        found, changed = self.discovery.discover(self.request)
        self.assertEqual(found[0]["running_tasks"], 1)
        self.assertNotEqual(hashed, changed)
        self.assertEqual(self.discovery.task_scans[str(path)][1], path.stat().st_size)

    def recorded(self, session, cwds):
        # A transcript filed, as the CLI does, under its starting folder's name.
        folder = self.config / "projects" / re.sub(r"[^A-Za-z0-9]", "-", cwds[0])
        folder.mkdir(parents=True, exist_ok=True)
        path = folder / (session + ".jsonl")
        with path.open("w") as stream:
            for index, cwd in enumerate(cwds):
                stream.write(json.dumps({"cwd": cwd, "type": "user", "uuid": str(uuid.uuid4()), "message": {"content": "prompt %d" % index}}) + "\n")
        return path

    def test_a_session_whose_cwd_left_the_project_stays_in_it(self):
        inside, outside, elsewhere, neighbour = (str(uuid.uuid4()) for _ in range(4))
        # Resumed from a subfolder, from a folder outside the project, a
        # session that was never the project's, and one from an ungranted
        # folder whose name the CLI files under the same folder name.
        self.recorded(inside, [self.project, self.project + "/sub"])
        self.recorded(outside, [self.project, str(self.home / "other")])
        self.recorded(elsewhere, [str(self.home / "other")])
        collider = str(Path(self.project).parent) + "-" + Path(self.project).name
        self.assertEqual(re.sub(r"[^A-Za-z0-9]", "-", collider), re.sub(r"[^A-Za-z0-9]", "-", self.project))
        self.recorded(neighbour, [collider, collider + "/x"])
        plain = {item["cli_id"]: item["workspace"] for item in self.plain.discover(self.request)}
        self.assertEqual(plain, {inside: self.project + "/sub"}, "the channel helper drops the session that left")
        found = {item["cli_id"]: item["workspace"] for item in self.discovery.discover(self.request)[0]}
        self.assertEqual(found, {inside: self.project + "/sub", outside: self.project})
        # Its view names the project too when the workbench says which one.
        request = {"cli_id": outside, "config": str(self.config)}
        self.assertEqual(self.discovery.view(dict(request))["workspace"], str(self.home / "other"))
        self.assertEqual(self.discovery.view(dict(request, project=self.project))["workspace"], self.project)
        self.assertEqual(self.discovery.view({"cli_id": elsewhere, "config": str(self.config), "project": self.project})["workspace"], str(self.home / "other"))
        self.assertEqual(self.discovery.view({"cli_id": neighbour, "config": str(self.config), "project": self.project})["workspace"], collider + "/x")

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
