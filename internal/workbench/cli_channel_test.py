import concurrent.futures
import importlib.util
import io
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import tempfile
import time
import unittest
import uuid
from unittest.mock import patch

sys.dont_write_bytecode = True
source = Path(__file__).with_name("cli_channel.py").resolve()
spec = importlib.util.spec_from_file_location("channel", source)
channel = importlib.util.module_from_spec(spec)
spec.loader.exec_module(channel)


class ChannelTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        # macOS /var and /private/var identify one directory. Match the
        # production config resolver so catalog order cannot duplicate it.
        self.home = Path(temporary.name).resolve()
        self.env = {**os.environ, "HOME": str(self.home), "CLAUDE_CONFIG_DIR": str(self.home / ".claude"), "TMPDIR": str(self.home)}
        environment = patch.dict(os.environ, self.env)
        environment.start()
        self.addCleanup(environment.stop)
        self.root = self.home / ".local/share/cage/cli-channels"
        root_patch = patch.object(channel, "ROOT", self.root)
        root_patch.start()
        self.addCleanup(root_patch.stop)
        self.session = str(uuid.uuid4())
        self.project = str(self.home / "project")
        channel._scans.clear()
        channel._children.clear()

    def history(self, session, workspace, messages):
        path = self.home / ".claude/projects/project" / (session + ".jsonl")
        path.parent.mkdir(parents=True, exist_ok=True)
        rows = [{"cwd": workspace, "uuid": str(uuid.uuid4()), **message} for message in messages]
        path.write_text("\n".join(json.dumps(row) for row in rows) + "\n")
        return path

    def test_channel_body_preserves_markup(self):
        request = str(uuid.uuid4())
        body = "show </channel> literally\n<channel cage_request=\"other\">example</channel>"
        self.assertEqual(channel.channel_message('<channel source="cage" cage_request="' + request + '">\n' + body + '\n</channel>'), (request, body))
        self.assertEqual(channel.channel_message('<channel source="session" request_id="' + request + '">\n' + body + '\n</channel>'), (request, body))
        self.assertEqual(channel.clip_text("你好abc", 4), "你")

    def test_slash_command_titles_hide_transcript_markup(self):
        command = "<command-message>model</command-message>\n<command-name>/model</command-name>\n<command-args>opus</command-args>"
        path = self.history(self.session, self.project, [
            {"type": "user", "message": {"role": "user", "content": command}},
            {"type": "user", "message": {"role": "user", "content": "<local-command-stdout>Set model to opus</local-command-stdout>"}},
            {"type": "user", "message": {"role": "user", "content": "继续上次的任务"}},
        ])
        self.assertEqual(channel.history_summary(path), (self.project, "/model"))

    def test_account_switch_preserves_tools_and_receipt(self):
        tool = {"type":"tool_use","id":"tool-completed","name":"Bash","input":{"command":"already completed"}}
        path = self.history(self.session,self.project,[
            {"type":"user","message":{"content":"perform task"}},
            {"type":"assistant","message":{"content":[tool]}},
            {"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-completed","content":"done"}]}},
        ])
        connection,switch_id,target = [str(uuid.uuid4()) for _ in range(3)]
        request = {"cli_id":self.session,"config":str(self.home / ".claude"),"connection":connection,"workspace":self.project,"target_cli_id":target,"switch_id":switch_id}
        channel.save(self.root/connection/"state.json",{"cli_id":self.session,"config":request["config"],"workspace":self.project,"busy":False,"requests":{"accepted":"digest"}})
        completed = type("Result",(),{"returncode":0,"stdout":"[]"})()
        original = path.read_bytes()
        with patch.object(channel,"active_sessions",return_value=[]),patch.object(channel.subprocess,"run",return_value=completed):
            result=channel.prepare_account_switch(request)
            self.assertEqual(Path(result["transcript"]).read_bytes(),original)
            self.assertEqual(channel.prepare_account_switch(request),result)
        receipt=channel.read_json(Path(result["transcript"]).with_name("receipt.json"))
        self.assertEqual(receipt["requests"],{"accepted":"digest"})
        self.assertEqual(path.read_bytes(),original)
        payload={"hook_event_name":"PreToolUse","session_id":self.session}
        with patch.object(channel.sys,"stdin",io.StringIO(json.dumps(payload))):
            denied=channel.hook(connection)
        self.assertEqual(denied["hookSpecificOutput"]["permissionDecision"],"deny")
        self.assertTrue(channel.cancel_account_switch(request)["ok"])
        self.assertFalse(Path(result["transcript"]).exists())
        self.assertTrue(path.exists())

    def test_account_switch_refuses_busy_and_unresolved_tools(self):
        path=self.history(self.session,self.project,[{"type":"assistant","message":{"content":[{"type":"tool_use","id":"uncertain","name":"Bash","input":{}}]}}])
        connection,switch_id,target=[str(uuid.uuid4()) for _ in range(3)]
        request={"cli_id":self.session,"config":str(self.home/".claude"),"connection":connection,"workspace":self.project,"target_cli_id":target,"switch_id":switch_id}
        state={"cli_id":self.session,"config":request["config"],"workspace":self.project,"busy":True}
        channel.save(self.root/connection/"state.json",state)
        with patch.object(channel.os,"kill") as kill:
            with self.assertRaises(ValueError): channel.prepare_account_switch(request)
            kill.assert_not_called()
        state["busy"]=False
        channel.save(self.root/connection/"state.json",state)
        completed=type("Result",(),{"returncode":0,"stdout":"[]"})()
        with patch.object(channel,"active_sessions",return_value=[]),patch.object(channel.subprocess,"run",return_value=completed):
            with self.assertRaisesRegex(ValueError,"工具"): channel.prepare_account_switch(request)
        self.assertTrue(path.exists())
        self.assertFalse((self.root.parent/"cli-account-switches"/switch_id/"history.jsonl").exists())

    def test_failure_hook_reports_kind_and_new_turn_clears_it(self):
        payload={"hook_event_name":"StopFailure","session_id":self.session,"error":"rate_limit","error_details":"private upstream diagnostic"}
        with patch.object(channel.sys,"stdin",io.StringIO(json.dumps(payload))):channel.hook(self.session)
        state=channel.read_json(self.root/self.session/"state.json")
        self.assertEqual(state["account_failure"],"rate_limit")
        self.assertNotIn("private upstream",json.dumps(state))
        with patch.object(channel.sys,"stdin",io.StringIO(json.dumps({**payload,"hook_event_name":"UserPromptSubmit"}))):channel.hook(self.session)
        self.assertNotIn("account_failure",channel.read_json(self.root/self.session/"state.json"))

    def test_first_display_delta_after_launch(self):
        with channel.state_file(self.session) as state:
            state["display"] = None
        payload = {"hook_event_name":"MessageDisplay","session_id":self.session,"message_id":"reply","index":0,"delta":"hello"}
        with patch.object(channel.sys,"stdin",io.StringIO(json.dumps(payload))):
            channel.hook(self.session)
        self.assertEqual(channel.read_json(self.root / self.session / "state.json")["display"]["text"],"hello")

    def test_ready_channel_wins_over_a_newer_unready_duplicate(self):
        config = str(channel.config_directory("default"))
        ready, starting = str(uuid.uuid4()), str(uuid.uuid4())
        channel.save(self.root / ready / "state.json", {"cli_id":self.session,"config":config,"pid":os.getpid(),"channel_pid":os.getpid(),"updated_at":1})
        channel.save(self.root / starting / "state.json", {"cli_id":self.session,"config":config,"pid":os.getpid(),"channel_pid":None,"updated_at":2})
        self.assertEqual(channel.connection_for(self.session,Path(config))[0],ready)

    def test_launch_retains_pending_permission_mode(self):
        config = str(channel.config_directory("default"))
        channel.save(channel.handoff_receipt(config,self.session),{"resume_task":True,"permission_mode":"manual"})
        connection = str(uuid.uuid4())
        with patch.object(channel,"migrate_cage_policy"), patch.object(channel,"active_session",return_value=False), patch.object(channel.os,"execvpe") as execute:
            channel.launch({"id":connection,"resume":self.session,"prefix":["claude"],"config":config})
        self.assertEqual(execute.call_args.args[1][-2:],["--permission-mode","manual"])
        self.assertTrue(channel.read_json(self.root / connection / "state.json")["resume_task"])

    def test_launch_selects_the_configured_session_channel(self):
        connection = str(uuid.uuid4())
        with patch.object(channel, "migrate_cage_policy"), patch.object(channel.os, "execvpe") as execute:
            channel.launch({"id": connection, "prefix": ["claude"]})
        args = execute.call_args.args[1]
        config = channel.read_json(Path(args[args.index("--mcp-config") + 1]))
        selected = args[args.index("--dangerously-load-development-channels") + 1]
        self.assertEqual(selected, "server:" + connection)
        self.assertEqual(list(config["mcpServers"]), [connection])
        settings = channel.read_json(Path(args[args.index("--settings") + 1]))
        self.assertNotIn("cage", json.dumps(config).lower())
        self.assertNotIn("cage", json.dumps(settings).lower())
        self.assertFalse(any(key.upper().startswith("CAGE_") for key in execute.call_args.args[2]))

    def test_launch_adds_bound_directories_as_literal_arguments(self):
        directory = self.home / "backend ' $literal"
        directory.mkdir()
        connection = str(uuid.uuid4())
        with patch.object(channel, "migrate_cage_policy"), patch.object(channel.os, "execvpe") as execute:
            channel.launch({"id": connection, "prefix": ["claude"], "additional_directories": [str(directory)]})
        args = execute.call_args.args[1]
        self.assertEqual(args[args.index("--add-dir") + 1], str(directory))
        self.assertFalse(any(key.upper().startswith("CAGE_") for key in execute.call_args.args[2]))
        with patch.object(channel, "migrate_cage_policy"), patch.object(channel.os, "execvpe") as execute:
            with self.assertRaisesRegex(ValueError, "项目文件夹未连接"):
                channel.launch({"id": str(uuid.uuid4()), "prefix": ["claude"], "additional_directories": [str(directory / "missing")]})
            execute.assert_not_called()

    @unittest.skipIf(os.name == "nt", "POSIX executable fixture")
    def test_bound_directory_reaches_isolated_child_and_host_write(self):
        default = Path(self.project)
        default.mkdir()
        extra = self.home / "backend ' $literal"
        extra.mkdir()
        binaries = self.home / "bin"
        binaries.mkdir()
        executable = binaries / "claude"
        executable.write_text("#!" + sys.executable + "\n" +
            "import json,os,sys\nfrom pathlib import Path\n" +
            "directory=Path(sys.argv[sys.argv.index('--add-dir')+1])\n" +
            "(directory/'from-child.txt').write_text('shared')\n" +
            "print(json.dumps({'cwd':os.getcwd(),'args':sys.argv[1:],'env':dict(os.environ)}))\n")
        executable.chmod(0o700)
        env = {**self.env, "PATH": str(binaries) + os.pathsep + os.environ.get("PATH", os.defpath), "CAGE_TEST_INTERNAL": "must-stay-internal"}
        request = {"id": str(uuid.uuid4()), "prefix": ["claude"], "additional_directories": [str(extra)]}
        result = subprocess.run([sys.executable, "-I", str(source), "launch", json.dumps(request)],
            cwd=default, env=env, capture_output=True, text=True, timeout=10, check=True)
        received = json.loads(result.stdout)
        self.assertEqual(received["cwd"], str(default))
        self.assertEqual(received["args"][received["args"].index("--add-dir")+1], str(extra))
        self.assertFalse(any(key.upper().startswith("CAGE_") for key in received["env"]))
        self.assertEqual((extra / "from-child.txt").read_text(), "shared")

    def test_handoff_preserves_executing_tools(self):
        request = {"cli_id":self.session, "config":str(self.home / ".claude"), "workspace":self.project}
        for status in ("busy", None):
            rows = [{"sessionId":self.session, "pid":1234, "cwd":self.project, "status":status, "kind":"interactive"}]
            with patch.object(channel, "active_sessions", return_value=rows), patch.object(channel.os, "kill") as kill:
                self.assertTrue(channel.handoff_session(request)["running"])
                kill.assert_not_called()

    def test_handoff_rechecks_idle_identity_and_waits_for_exit(self):
        request = {"cli_id":self.session, "config":str(self.home / ".claude"), "workspace":self.project}
        row = {"sessionId":self.session, "pid":1234, "cwd":self.project, "status":"idle", "kind":"interactive"}
        with patch.object(channel, "active_sessions", return_value=[row]), patch.object(channel, "live", return_value=False), patch.object(channel.os, "kill") as kill:
            self.assertFalse(channel.handoff_session(request)["running"])
            kill.assert_called_once_with(1234, channel.signal.SIGTERM)
        with patch.object(channel, "active_sessions", side_effect=[[row], [{**row, "status":"busy"}]]), patch.object(channel.os, "kill") as kill:
            self.assertTrue(channel.handoff_session(request)["running"])
            kill.assert_not_called()

    def test_handoff_waiting_task_requires_fresh_approval(self):
        request = {"cli_id":self.session, "config":str(self.home / ".claude"), "workspace":self.project}
        row = {"sessionId":self.session, "pid":1234, "cwd":self.project, "status":"waiting", "kind":"interactive"}
        with patch.object(channel, "active_sessions", return_value=[row]), patch.object(channel, "live", return_value=False), patch.object(channel.os, "kill") as kill:
            self.assertFalse(channel.handoff_session(request)["running"])
            kill.assert_called_once_with(1234, channel.signal.SIGTERM)
        self.assertTrue(channel.read_json(channel.handoff_receipt(request["config"], self.session))["resume_task"])
        with patch.object(channel, "active_sessions", return_value=[{**row, "cwd":self.project + "-other"}]), patch.object(channel.os, "kill") as kill:
            with self.assertRaises(ValueError): channel.handoff_session(request)
            kill.assert_not_called()

    @patch.object(channel, "running_sessions", return_value=[])
    def test_catalog_activity_requires_live_busy_process(self, _registry):
        self.history(self.session, self.project, [{"type":"user", "message":{"content":"hello"}}])
        connection = str(uuid.uuid4())
        with channel.state_file(connection) as state:
            state.update(cli_id=self.session, config=str(self.home / ".claude"), workspace=self.project,
                         pid=os.getpid(), busy=True)
        request = {"accounts":["default"], "workspaces":{self.project:self.project}}
        item = channel.discover(request)[0]
        self.assertTrue(item["active"])
        self.assertTrue(item["running"])
        self.assertTrue(channel.view(self.home / ".claude", self.session)["active"])
        with channel.state_file(connection) as state:
            state["busy"] = False
        item = channel.discover(request)[0]
        self.assertFalse(item["active"], "idle CLI was shown as active")
        self.assertTrue(item["running"], "idle owned process was shown as stopped")
        with channel.state_file(connection) as state:
            state.update(busy=True, closed=True)
        item = channel.discover(request)[0]
        self.assertFalse(item["active"], "closed CLI retained activity")
        self.assertFalse(item["running"])

    def test_discovery_reports_external_idle_process_and_clears_after_exit(self):
        self.history(self.session, self.project, [{"type":"user", "message":{"content":"hello"}}])
        other = str(uuid.uuid4())
        self.history(other, self.project, [{"type":"user", "message":{"content":"other"}}])
        request = {"accounts":["default"], "workspaces":{self.project:self.project}}
        registry = [{"sessionId": self.session, "pid": os.getpid(), "status":"idle"}]
        completed = subprocess.CompletedProcess([], 0, json.dumps(registry), "")
        with patch.object(channel.subprocess, "run", return_value=completed) as run:
            found = {item["cli_id"]: item for item in channel.discover(request)}
            self.assertTrue(found[self.session]["running"])
            self.assertFalse(found[self.session]["active"])
            self.assertFalse(found[other]["running"])
            self.assertEqual(run.call_count, 1, "registry must be read once per config, not per history")
            self.assertEqual(run.call_args.kwargs["env"]["CLAUDE_CONFIG_DIR"], str(self.home / ".claude"))
            completed.stdout = "[]"
            self.assertFalse(any(item["running"] for item in channel.discover(request)))

    def test_discovery_identifies_desktop_from_live_registration_only(self):
        self.history(self.session, self.project, [{"type":"user", "entrypoint":"claude-desktop", "message":{"content":"hello"}}])
        request = {"accounts":["default"], "workspaces":{self.project:self.project}}
        row = {"sessionId":self.session, "pid":os.getpid(), "startedAt":123, "status":"idle"}
        registry = self.home / ".claude/sessions" / (str(os.getpid()) + ".json")
        channel.save(registry, {**row, "entrypoint":"claude-desktop"})
        with patch.object(channel, "running_sessions", return_value=[row]):
            for entrypoint in ("claude-desktop", "claude-desktop-3p", "local-agent", "local_agent", "remote_desktop", "cli", "claude-vscode", "sdk-ts", "", "future-client"):
                with self.subTest(entrypoint=entrypoint):
                    channel.save(registry, {**row, "entrypoint":entrypoint})
                    self.assertEqual(channel.discover(request)[0]["claude_desktop"], entrypoint in ("claude-desktop", "claude-desktop-3p", "local-agent", "local_agent", "remote_desktop"))
            for invalid in ({**row, "startedAt":122}, {**row, "sessionId":str(uuid.uuid4())}, {**row, "pid":1}):
                channel.save(registry, {**invalid, "entrypoint":"claude-desktop"})
                self.assertFalse(channel.discover(request)[0]["claude_desktop"], "stale registration claimed Desktop ownership")
            registry.write_text("invalid json")
            self.assertFalse(channel.discover(request)[0]["claude_desktop"])
            registry.unlink()
            self.assertFalse(channel.discover(request)[0]["claude_desktop"], "transcript origin was used as live ownership")
            row["entrypoint"] = "claude-desktop"
            self.assertTrue(channel.discover(request)[0]["claude_desktop"], "explicit CLI source was ignored")
        with patch.object(channel, "running_sessions", return_value=[]):
            self.assertFalse(channel.discover(request)[0]["claude_desktop"])

    def test_purge_removes_only_target_transcripts_and_retries(self):
        target = self.history(self.session, self.project, [{"type":"user", "message":{"content":"delete this"}}])
        other = self.history(str(uuid.uuid4()), self.project, [{"type":"user", "message":{"content":"keep this"}}])
        child = target.parent / self.session / "subagents" / "agent-fixture.jsonl"
        child.parent.mkdir(parents=True)
        child.write_text("child history")
        project_file = self.home / "project" / "main.py"
        project_file.parent.mkdir()
        project_file.write_text("project content")
        request = {"cli_id":self.session, "account":"default", "workspace":self.project, "expect_history":True}
        with patch.object(channel, "active_session", return_value=True):
            with self.assertRaises(ValueError): channel.purge_session(request)
        self.assertTrue(target.exists())
        with patch.object(channel, "active_session", return_value=False):
            with self.assertRaises(ValueError): channel.purge_session({**request, "workspace":self.project + "-other"})
            self.assertTrue(channel.purge_session(request)["ok"])
            self.assertTrue(channel.purge_session(request)["ok"])
        self.assertFalse(target.exists())
        self.assertFalse(child.parent.parent.exists())
        self.assertTrue(other.exists())
        self.assertEqual(project_file.read_text(), "project content")

    def test_only_cage_default_policy_is_migrated(self):
        policy = self.home / "managed-settings.json"
        original = {"permissions": {"defaultMode": "bypassPermissions"}, "tui": "fullscreen"}
        policy.write_text(json.dumps(original))
        def path(value):
            return policy if str(value) == "/etc/claude-code/managed-settings.json" else Path(value)
        def migrate(args, **kwargs):
            code = args[-1].replace("/etc/claude-code/managed-settings.json", str(policy))
            exec(code, {"__name__": "migration_test"})
        with patch.object(channel, "Path", path), patch.object(channel.sys, "platform", "linux"), patch.object(channel.subprocess, "run", side_effect=migrate) as run:
            self.assertTrue(channel.migrate_cage_policy())
            self.assertTrue(json.loads(policy.read_text())["channelsEnabled"])
            backup = policy.with_name("managed-settings.json.cage-before-channels")
            self.assertEqual(json.loads(backup.read_text()), original)
            blocked = {**original, "channelsEnabled": False}
            policy.write_text(json.dumps(blocked))
            channel.migrate_cage_policy()
            self.assertEqual(json.loads(policy.read_text()), blocked)
            self.assertEqual(run.call_count, 1)

    @patch.object(channel, "running_sessions", return_value=[])
    def test_history_projects_and_channel_identity(self, _registry):
        request = str(uuid.uuid4())
        self.history(self.session, self.project + "/nested", [
            {"type": "user", "message": {"content": "desktop question"}},
            {"type": "assistant", "message": {"content": [{"type": "text", "text": "desktop answer"}]}},
            {"type": "assistant", "message": {"model":"<synthetic>", "content": [{"type": "text", "text": "No response requested."}]}},
            {"type": "user", "isMeta": True, "message": {"content": "system metadata"}},
            {"type": "user", "isMeta": True, "message": {"content": '<channel source="cage" cage_request="' + request + '">\nphone question\n</channel>'}},
            {"type": "user", "message": {"content": [{"type": "tool_result", "content": "private tool output"}]}},
            {"type": "assistant", "message": {"content": [{"type": "text", "text": "phone answer"}]}},
        ])
        self.history(str(uuid.uuid4()), self.project + "-other", [{"type": "user", "message": {"content": "outside project"}}])
        found = channel.discover({"accounts": ["default"], "workspaces": [self.project]})
        self.assertEqual([item["cli_id"] for item in found], [self.session])
        view = channel.view(channel.config_directory("default"), self.session)
        self.assertFalse(view["can_send"])
        self.assertEqual(len(view["turns"]), 2)
        self.assertEqual(view["turns"][1]["id"], request)
        self.assertEqual(view["turns"][1]["prompt"], "phone question")
        self.assertEqual(view["turns"][1]["text"], "phone answer")
        self.assertNotIn("private tool output", json.dumps(view))

    def test_history_pairs_tool_only_rows_and_keeps_task_notice(self):
        self.history(self.session, self.project, [
            {"type": "user", "message": {"content": "run"}},
            {"type": "assistant", "message": {"content": [
                {"type": "tool_use", "id": "tool-1", "name": "Bash", "input": {"description": "检查结果", "command": "private-command"}},
                {"type": "tool_use", "id": "tool-2", "name": "Read", "input": {"file_path": "result.md"}},
            ]}},
            {"type": "user", "message": {"content": [
                {"type": "tool_result", "tool_use_id": "tool-2", "content": "private-output"},
                {"type": "tool_result", "tool_use_id": "tool-1", "is_error": True},
            ]}},
            {"type": "user", "isMeta": True, "message": {"content": "<task-notification><summary>完成</summary></task-notification>"}},
        ])
        view = channel.view(channel.config_directory("default"), self.session)
        self.assertEqual(len(view["turns"]), 2)
        tools = view["turns"][0]["tools"]
        self.assertEqual([(t["name"], t["status"], t["detail"]) for t in tools],
                         [("Bash", "failed", "检查结果"), ("Read", "completed", "result.md")])
        self.assertTrue(view["turns"][1]["prompt"].startswith("<task-notification>"))
        self.assertNotIn("private-", json.dumps(view))

    def task_history(self, rows, started=1795000000):
        # Synthetic Claude Code rows in the shapes recorded by 2.1.330.
        stamp = lambda offset: time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(started + offset)) + ".250Z"
        return self.history(self.session, self.project, [{**row, "timestamp": stamp(row.pop("at", 0))} for row in rows])

    def live_registry(self, started_ms=1794999000000):
        registry = self.home / ".claude/sessions" / (str(os.getpid()) + ".json")
        registry.parent.mkdir(parents=True, exist_ok=True)
        registry.write_text(json.dumps({"pid": os.getpid(), "sessionId": self.session, "startedAt": started_ms}))
        return registry

    def notice(self, tool, task, status, at):
        text = "<task-notification>\n<task-id>%s</task-id>\n<tool-use-id>%s</tool-use-id>\n<output-file>/tmp/x/%s.output</output-file>\n<status>%s</status>\n<summary>Background command finished</summary>\n</task-notification>" % (task, tool, task, status)
        return {"type": "user", "at": at, "origin": {"kind": "task-notification"}, "message": {"role": "user", "content": text}}

    def view_now(self):
        return channel.view(channel.config_directory("default"), self.session)

    def test_background_command_runs_until_its_notice(self):
        self.live_registry()
        rows = [
            {"type": "user", "at": 0, "message": {"role": "user", "content": "build it"}},
            {"type": "assistant", "at": 1, "message": {"content": [{"type": "tool_use", "id": "toolu_bg1", "name": "Bash", "input": {"command": "private-command", "description": "构建发布包", "run_in_background": True}}]}},
            {"type": "user", "at": 1.5, "message": {"content": [{"type": "tool_result", "tool_use_id": "toolu_bg1", "content": "Command running in background with ID: bk1a2b3c"}]},
             "toolUseResult": {"backgroundTaskId": "bk1a2b3c", "interrupted": False}},
            {"type": "assistant", "at": 2, "message": {"content": [{"type": "tool_use", "id": "repl_bg2", "name": "Bash", "input": {"command": "private-command", "timeout": 1000}}]}},
            {"type": "user", "at": 3, "message": {"content": [{"type": "tool_result", "tool_use_id": "repl_bg2", "content": ""}]},
             "toolUseResult": {"backgroundTaskId": "bz9y8x7w", "timedOutAfterMs": 1000}},
            {"type": "assistant", "at": 4, "message": {"content": [{"type": "text", "text": "started"}]}},
            {"type": "system", "subtype": "turn_duration", "at": 4},
        ]
        path = self.task_history([dict(row) for row in rows])
        view = self.view_now()
        tools = view["turns"][0]["tools"]
        self.assertEqual([(t["name"], t["status"], t.get("kind"), t.get("state")) for t in tools],
                         [("Bash", "completed", "background_command", "running"), ("Bash", "completed", "background_command", "running")])
        self.assertEqual(tools[0]["detail"], "构建发布包")
        self.assertNotIn("detail", tools[1])
        self.assertEqual(tools[0]["started_at"], 1795000001.25)
        self.assertNotIn("ended_at", tools[0])
        self.assertEqual(view["tasks"]["count"], 2)
        self.assertEqual([item["id"] for item in view["tasks"]["running"]], ["repl_bg2", "toolu_bg1"])
        self.assertNotIn("private-", json.dumps(view))
        # The notice opens its own turn; it ends the first command by tool-use-id
        # and the second by task-id only.
        with path.open("a") as stream:
            for row in (self.notice("toolu_bg1", "bk1a2b3c", "completed", 30), self.notice("", "bz9y8x7w", "failed", 31)):
                stamp = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(1795000000 + row.pop("at"))) + ".250Z"
                stream.write(json.dumps({**row, "uuid": str(uuid.uuid4()), "cwd": self.project, "timestamp": stamp}) + "\n")
        view = self.view_now()
        tools = view["turns"][0]["tools"]
        self.assertEqual([(t["state"], t.get("ended_at")) for t in tools], [("completed", 1795000030.25), ("failed", 1795000031.25)])
        self.assertNotIn("tasks", view)
        self.assertEqual(len(view["turns"]), 3)
        self.assertTrue(view["turns"][1]["prompt"].startswith("<task-notification>"))
        self.assertNotIn("tools", view["turns"][1])

    def test_stopped_killed_and_failed_launches(self):
        self.live_registry()
        rows = [
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
        ]
        self.task_history(rows)
        view = self.view_now()
        tools = {t["id"]: t for t in view["turns"][0]["tools"]}
        self.assertEqual((tools["t-stop"]["state"], tools["t-stop"]["ended_at"]), ("stopped", 1795000006.25))
        self.assertEqual((tools["t-kill"]["state"], tools["t-kill"]["ended_at"]), ("stopped", 1795000007.25))
        self.assertEqual((tools["t-refused"]["status"], tools["t-refused"]["state"]), ("failed", "failed"))
        self.assertNotIn("kind", tools["t-plain"])
        self.assertNotIn("kind", tools["t-taskstop"])
        self.assertNotIn("tasks", view)

    def subagent_files(self, agent, tool, calls, finished=None):
        folder = self.home / ".claude/projects/project" / self.session / "subagents"
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
        rows = [
            {"type": "user", "at": 0, "message": {"role": "user", "content": "look around"}},
            {"type": "assistant", "at": 1, "message": {"content": [
                {"type": "tool_use", "id": "repl_agent1", "name": "Agent", "input": {"description": "梳理目录", "prompt": "private-prompt", "subagent_type": "Explore", "run_in_background": True}},
                {"type": "tool_use", "id": "toolu_fore", "name": "Agent", "input": {"description": "前台检查", "prompt": "private-prompt"}},
            ]}},
            {"type": "user", "at": 1.2, "message": {"content": [{"type": "tool_result", "tool_use_id": "repl_agent1", "content": ""}]},
             "toolUseResult": {"status": "async_launched", "isAsync": True, "agentId": "a0123456789abcdef", "description": "梳理目录"}},
        ]
        self.task_history(rows)
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
        rows = [
            {"type": "user", "at": 0, "message": {"role": "user", "content": "go"}},
            {"type": "assistant", "at": 1, "message": {"content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "x", "run_in_background": True}}]}},
            {"type": "user", "at": 2, "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "content": ""}]}, "toolUseResult": {"backgroundTaskId": "b1"}},
        ]
        self.task_history(rows)
        view = self.view_now()
        self.assertEqual(view["turns"][0]["tools"][0]["state"], "stopped")
        self.assertNotIn("tasks", view)
        # A process started after the launch (a resumed session) did not start it.
        self.live_registry(started_ms=(1795000000 + 60) * 1000)
        self.assertEqual(self.view_now()["turns"][0]["tools"][0]["state"], "stopped")
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
        original = channel.child_activity
        with patch.object(channel, "child_activity", side_effect=lambda path: (read.append(path), original(path))[1]):
            view = self.view_now()
        self.assertEqual(len(read), channel.CHILD_LIMIT)
        self.assertEqual(view["tasks"]["count"], 20)
        self.assertEqual(len(view["tasks"]["running"]), 16)
        self.assertEqual(view["tasks"]["running"][0]["id"], "agent-19")
        self.assertEqual(sum(1 for t in view["turns"][0]["tools"] if "now" in t), 16)

    def test_old_transcripts_keep_todays_output(self):
        rows = [
            {"type": "user", "message": {"content": "run"}},
            {"type": "assistant", "message": {"content": [
                {"type": "tool_use", "id": "tool-1", "name": "Bash", "input": {"description": "检查结果", "command": "private-command"}},
                {"type": "tool_use", "id": "tool-2", "name": "Read", "input": {"file_path": "result.md"}},
            ]}},
            {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "tool-2", "content": "x"}, {"type": "tool_result", "tool_use_id": "tool-1", "is_error": True}]}},
            {"type": "user", "isMeta": True, "message": {"content": "<task-notification><summary>完成</summary></task-notification>"}},
        ]
        self.history(self.session, self.project, rows)
        reads = []
        with patch.object(channel, "process_since", side_effect=lambda *a: reads.append(a)), patch.object(channel, "child_activity", side_effect=lambda *a: reads.append(a)):
            view = self.view_now()
        self.assertEqual(reads, [], "a transcript without tasks reads nothing more")
        self.assertNotIn("tasks", view)
        for turn in view["turns"]:
            for tool in turn.get("tools", []):
                self.assertEqual(set(tool), {"id", "name", "status", "detail"} if "detail" in tool else {"id", "name", "status"})

    def test_discovery_counts_tasks_of_live_sessions_incrementally(self):
        rows = [
            {"type": "user", "at": 0, "message": {"role": "user", "content": "go"}},
            {"type": "assistant", "at": 1, "message": {"content": [{"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "x", "run_in_background": True}},
                                                                   {"type": "tool_use", "id": "t2", "name": "Agent", "input": {"description": "d", "prompt": "p"}}]}},
            {"type": "user", "at": 2, "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "content": ""}]}, "toolUseResult": {"backgroundTaskId": "b1"}},
        ]
        path = self.task_history(rows)
        request = {"workspaces": [self.project], "accounts": ["default"]}
        with patch.object(channel, "running_sessions", return_value=[]):
            self.assertNotIn("running_tasks", channel.discover(request)[0])
            self.assertEqual(channel._scans, {}, "a session without a live process is not read")
        self.live_registry()
        live = [{"pid": os.getpid(), "sessionId": self.session}]
        with patch.object(channel, "running_sessions", return_value=live):
            self.assertEqual(channel.discover(request)[0]["running_tasks"], 2)
            offset = channel._scans[str(path)][1]
            self.assertEqual(offset, path.stat().st_size)
            with path.open("a") as stream:
                stream.write(json.dumps({**self.notice("t1", "b1", "completed", 0), "uuid": str(uuid.uuid4())}) + "\n")
            item = channel.discover(request)[0]
            self.assertEqual(item["running_tasks"], 1)
            self.assertEqual(channel._scans[str(path)][1], path.stat().st_size)

    def test_concurrent_send_and_stale_approval(self):
        with channel.state_file(self.session) as state:
            state.update(cli_id=self.session, config=str(channel.config_directory("default")), workspace=self.project, pid=os.getpid())
        process = subprocess.Popen([sys.executable, "-I", str(source), "serve", self.session],
                                   env=self.env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        def stop():
            process.stdin.close()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            process.stdout.close()
            process.stderr.close()
        self.addCleanup(stop)
        def event(message):
            process.stdin.write(json.dumps({"jsonrpc": "2.0", **message}) + "\n")
            process.stdin.flush()
        event({"id": 1, "method": "initialize", "params": {"protocolVersion": "2024-11-05"}})
        handshake = json.loads(process.stdout.readline())["result"]
        self.assertIn("claude/channel/permission", handshake["capabilities"]["experimental"])
        self.assertNotIn("cage", json.dumps(handshake).lower())
        self.assertNotIn("guest", json.dumps(handshake).lower())
        event({"method": "notifications/initialized"})
        def rpc(**fields):
            result = subprocess.run([sys.executable, "-I", str(source), "rpc", "-"], env=self.env, text=True,
                                    input=json.dumps({"cli_id": self.session, "connection": self.session, **fields}), capture_output=True, check=True)
            return json.loads(result.stdout)
        request = str(uuid.uuid4())
        with concurrent.futures.ThreadPoolExecutor(2) as workers:
            replies = list(workers.map(lambda _: rpc(action="send", request_id=request, text="once"), range(2)))
        self.assertEqual(replies, [{"ok": True}, {"ok": True}])
        sent = json.loads(process.stdout.readline())
        self.assertEqual(sent["params"]["meta"]["request_id"], request)
        self.assertFalse(select.select([process.stdout], [], [], 0.1)[0])
        self.assertIn("error", rpc(action="send", request_id=request, text="changed"))
        event({"method": "notifications/claude/channel/permission_request", "params": {
            "request_id": "abcde", "tool_name": "Bash", "input_preview": '{"command":"echo test"}'}})
        path = self.root / self.session / "state.json"
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline and not channel.read_json(path).get("approvals"):
            time.sleep(0.01)
        approval = channel.read_json(path)["approvals"][0]
        self.assertIn("error", rpc(action="approve", approval_id=str(uuid.uuid4()), allow=True))
        self.assertEqual(rpc(action="approve", approval_id=approval["id"], allow=False), {"ok": True})
        self.assertEqual(json.loads(process.stdout.readline())["params"], {"request_id": "abcde", "behavior": "deny"})
        self.assertIn("error", rpc(action="approve", approval_id=approval["id"], allow=True))
        with channel.state_file(self.session) as state:
            state["resume_task"] = True
        self.assertEqual(rpc(action="resume"), {"ok":True})
        self.assertEqual(rpc(action="resume"), {"ok":True})
        resumed = json.loads(process.stdout.readline())
        self.assertEqual(resumed["params"]["meta"], {"resume":"true"})
        self.assertFalse(select.select([process.stdout], [], [], 0.1)[0], "resume was injected twice")


if __name__ == "__main__":
    unittest.main()
