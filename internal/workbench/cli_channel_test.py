import concurrent.futures
import importlib.util
import io
import json
import re
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
            replay=channel.prepare_account_switch(request)
            self.assertEqual({key:replay[key] for key in result},result)
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

    def test_account_switch_refuses_busy_and_keeps_unfinished_tools(self):
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
        # A call that a limit interrupted has no result. The resumed CLI reports
        # it to the model as unknown (TestClaudeCrossAccountResumeOfUnfinishedTool).
        with patch.object(channel,"active_sessions",return_value=[]),patch.object(channel.subprocess,"run",return_value=completed):
            result=channel.prepare_account_switch(request)
        self.assertEqual(Path(result["transcript"]).read_bytes(),path.read_bytes())

    def test_account_switch_follows_the_session_into_subdirectories(self):
        # A session is identified by the folder it started in. Rows recorded in a
        # subdirectory or worktree of the project must not refuse the handoff.
        sub=self.project+"/packages/app"
        path=self.history(self.session,self.project,[{"type":"user","message":{"content":"start"}}])
        with path.open("a") as stream:
            stream.write(json.dumps({"cwd":sub,"uuid":str(uuid.uuid4()),"type":"assistant","message":{"content":"working below"}})+"\n")
        connection,switch_id,target=[str(uuid.uuid4()) for _ in range(3)]
        request={"cli_id":self.session,"config":str(self.home/".claude"),"connection":connection,"workspace":sub,"root":self.project,"target_cli_id":target,"switch_id":switch_id}
        channel.save(self.root/connection/"state.json",{"cli_id":self.session,"config":request["config"],"workspace":sub})
        completed=type("Result",(),{"returncode":0,"stdout":"[]"})()
        with patch.object(channel,"active_sessions",return_value=[]),patch.object(channel.subprocess,"run",return_value=completed):
            result=channel.prepare_account_switch(request)
        self.assertEqual(result["workspace"],self.project)
        self.assertEqual(Path(result["transcript"]).read_bytes(),path.read_bytes())
        # A history that started in another project still does not continue here.
        other=str(uuid.uuid4())
        self.history(other,str(self.home/"elsewhere"),[{"type":"user","message":{"content":"start"}}])
        connection=str(uuid.uuid4())
        request={**request,"cli_id":other,"connection":connection,"switch_id":str(uuid.uuid4()),"workspace":self.project}
        channel.save(self.root/connection/"state.json",{"cli_id":other,"config":request["config"],"workspace":self.project})
        with patch.object(channel,"active_sessions",return_value=[]),patch.object(channel.subprocess,"run",return_value=completed):
            with self.assertRaisesRegex(ValueError,"switch:project"): channel.prepare_account_switch(request)

    def test_account_switch_refusals_leave_the_source_running_and_unfenced(self):
        # Every check that can refuse runs before the source is fenced or
        # stopped; a failure after the fence lifts it again.
        self.history(self.session,str(self.home/"elsewhere"),[{"type":"user","message":{"content":"start"}}])
        connection,switch_id,target=[str(uuid.uuid4()) for _ in range(3)]
        request={"cli_id":self.session,"config":str(self.home/".claude"),"connection":connection,"workspace":self.project,"target_cli_id":target,"switch_id":switch_id}
        channel.save(self.root/connection/"state.json",{"cli_id":self.session,"config":request["config"],"workspace":self.project,"pid":os.getpid()})
        completed=type("Result",(),{"returncode":0,"stdout":"[]"})()
        with patch.object(channel,"active_sessions",return_value=[]),patch.object(channel.subprocess,"run",return_value=completed),patch.object(channel,"terminate_cli") as stop:
            with self.assertRaisesRegex(ValueError,"switch:project"): channel.prepare_account_switch(request)
            stop.assert_not_called()
        self.assertNotIn("account_switch",channel.read_json(self.root/connection/"state.json"))
        self.history(self.session,self.project,[{"type":"user","message":{"content":"start"}}])
        channel.save(self.root/connection/"state.json",{"cli_id":self.session,"config":request["config"],"workspace":self.project})
        with patch.object(channel,"active_sessions",return_value=[]),patch.object(channel.subprocess,"run",return_value=completed),patch.object(channel.os,"replace",side_effect=OSError("disk full")):
            with self.assertRaises(OSError): channel.prepare_account_switch(request)
        state=channel.read_json(self.root/connection/"state.json")
        self.assertNotIn("account_switch",state)
        self.assertFalse(state.get("closed"))
        self.assertFalse((self.root.parent/"cli-account-switches"/switch_id/"receipt.json").exists())

    def test_account_switch_replaces_a_stale_unprepared_fence(self):
        self.history(self.session,self.project,[{"type":"user","message":{"content":"start"}}])
        connection,stale,target=[str(uuid.uuid4()) for _ in range(3)]
        request={"cli_id":self.session,"config":str(self.home/".claude"),"connection":connection,"workspace":self.project,"target_cli_id":target,"switch_id":str(uuid.uuid4())}
        # An earlier helper fenced the session and then refused: no receipt.
        channel.save(self.root/connection/"state.json",{"cli_id":self.session,"config":request["config"],"workspace":self.project,"account_switch":stale})
        completed=type("Result",(),{"returncode":0,"stdout":"[]"})()
        with patch.object(channel,"active_sessions",return_value=[]),patch.object(channel.subprocess,"run",return_value=completed):
            result=channel.prepare_account_switch(request)
            self.assertTrue(Path(result["transcript"]).is_file())
            self.assertEqual(channel.read_json(self.root/connection/"state.json")["account_switch"],request["switch_id"])
            # A fence whose history is already frozen is still respected.
            other={**request,"switch_id":str(uuid.uuid4())}
            with self.assertRaisesRegex(ValueError,"switch:pending"): channel.prepare_account_switch(other)

    def test_descendants_follow_process_parentage(self):
        tree=subprocess.Popen(["/bin/sh","-c","sleep 30 & (sleep 30 & wait) & wait"])
        unrelated=subprocess.Popen(["/bin/sleep","30"])
        self.addCleanup(lambda:[p.kill() or p.wait() for p in (tree,unrelated)])
        deadline=time.monotonic()+5
        while len(channel.descendants(tree.pid))<3 and time.monotonic()<deadline:time.sleep(.05)
        found=channel.descendants(tree.pid)
        self.assertEqual(len(found),3,found)
        self.assertNotIn(str(unrelated.pid),[pid for pid,_ in found])
        self.assertTrue(all(mark==channel.process_fingerprint(pid) for pid,mark in found))
        self.assertEqual(channel.descendants(None),[])

    def switch_with_a_background_command(self,force):
        self.history(self.session,self.project,[{"type":"user","message":{"content":"start"}}])
        source=subprocess.Popen(["/bin/sh","-c","sleep 30 & wait"])
        sibling=subprocess.Popen(["/bin/sleep","30"])
        deadline=time.monotonic()+5
        while not channel.descendants(source.pid) and time.monotonic()<deadline:time.sleep(.05)
        child=int(channel.descendants(source.pid)[0][0])
        self.addCleanup(lambda:[p.kill() or p.wait() for p in (source,sibling)] and None)
        self.addCleanup(lambda:channel.live(child) and os.kill(child,9))
        connection,switch_id,target=[str(uuid.uuid4()) for _ in range(3)]
        request={"cli_id":self.session,"config":str(self.home/".claude"),"connection":connection,"workspace":self.project,"target_cli_id":target,"switch_id":switch_id,"force":force}
        channel.save(self.root/connection/"state.json",{"cli_id":self.session,"config":request["config"],"workspace":self.project,"pid":source.pid})
        rows=[{"pid":source.pid,"sessionId":self.session,"cwd":self.project,"status":"idle","kind":"interactive"}]
        def reap(pid,force=False):
            os.kill(int(pid),9 if force else 15)
            if int(pid)==source.pid:source.wait()
        with patch.object(channel,"active_sessions",return_value=rows),patch.object(channel,"terminate_cli",side_effect=reap):
            result=channel.prepare_account_switch(request)
        self.assertTrue(Path(result["transcript"]).is_file())
        self.assertIsNone(sibling.poll(),"an unrelated session of the account was stopped")
        return child

    def test_account_switch_stops_only_the_source_and_its_children(self):
        # A second, unrelated session of the same account never blocks and is
        # not stopped; the source's own background command stops with it when
        # the stop was confirmed or forced.
        child=self.switch_with_a_background_command(True)
        deadline=time.monotonic()+5
        while channel.live(child) and time.monotonic()<deadline:time.sleep(.05)
        self.assertFalse(channel.live(child),"the source's background command kept running")

    def test_unconfirmed_switch_leaves_background_commands_alone(self):
        # Nobody confirmed stopping the source's work: its children are not
        # killed (the host asks first whenever it knows of running tasks).
        child=self.switch_with_a_background_command(False)
        time.sleep(.3)
        self.assertTrue(channel.live(child),"an unconfirmed switch killed a background command")

    def test_account_switch_names_a_cli_running_outside_the_project(self):
        # A force cannot move a CLI that runs in another folder: the refusal
        # names that folder, and nothing is stopped or fenced.
        self.history(self.session,self.project,[{"type":"user","message":{"content":"start"}}])
        connection,switch_id,target=[str(uuid.uuid4()) for _ in range(3)]
        request={"cli_id":self.session,"config":str(self.home/".claude"),"connection":connection,"workspace":self.project,"target_cli_id":target,"switch_id":switch_id,"force":True}
        channel.save(self.root/connection/"state.json",{"cli_id":self.session,"config":request["config"],"workspace":self.project,"pid":4242})
        outside=str(self.home/"elsewhere")
        rows=[{"pid":4242,"sessionId":self.session,"cwd":outside,"status":"busy","kind":"interactive"}]
        with patch.object(channel,"active_sessions",return_value=rows),patch.object(channel,"terminate_cli") as stop:
            with self.assertRaisesRegex(ValueError,"^switch:outside:"+re.escape(outside)+"$"): channel.prepare_account_switch(request)
            stop.assert_not_called()
        self.assertNotIn("account_switch",channel.read_json(self.root/connection/"state.json"))

    def test_forced_switch_stops_a_working_source_and_returns_unsent_messages(self):
        # At a confirmed limit, or after the person confirmed, a working source
        # is stopped: approvals are dropped (they do not transfer) and a message
        # it never took comes back once; one already in the history does not.
        taken,unsent=str(uuid.uuid4()),str(uuid.uuid4())
        self.history(self.session,self.project,[
            {"type":"user","message":{"content":"first"}},
            {"type":"user","isMeta":True,"message":{"content":'<channel source="session" request_id="'+taken+'">\ntaken before the stop\n</channel>'}},
            {"type":"assistant","message":{"content":[{"type":"tool_use","id":"cut","name":"Bash","input":{"command":"sleep 300","description":"Long build"}}]}},
        ])
        connection,switch_id,target=[str(uuid.uuid4()) for _ in range(3)]
        request={"cli_id":self.session,"config":str(self.home/".claude"),"connection":connection,"workspace":self.project,"target_cli_id":target,"switch_id":switch_id}
        state={"cli_id":self.session,"config":request["config"],"workspace":self.project,"busy":True,"pending_send":unsent,
               "approvals":[{"id":"approval","relay_id":"relay","tool":"Bash","input":{}}],
               "sent":[{"id":taken,"text":"taken before the stop"},{"id":unsent,"text":"waiting behind the limit"}],"requests":{taken:"d1",unsent:"d2"}}
        channel.save(self.root/connection/"state.json",state)
        rows=[{"pid":4242,"sessionId":self.session,"cwd":self.project,"status":"waiting","kind":"interactive"}]
        with patch.object(channel,"active_sessions",return_value=rows),patch.object(channel,"terminate_cli") as stop:
            with self.assertRaisesRegex(ValueError,"switch:(busy|running)"): channel.prepare_account_switch(request)
            stop.assert_not_called()
            self.assertNotIn("account_switch",channel.read_json(self.root/connection/"state.json"))
            result=channel.prepare_account_switch({**request,"force":True})
        self.assertEqual(result["carry"],[{"id":unsent,"text":"waiting behind the limit"}])
        receipt=channel.read_json(Path(result["transcript"]).with_name("receipt.json"))
        self.assertNotIn("approvals",json.dumps(receipt))
        self.assertEqual(receipt["carry"],result["carry"])
        latest=channel.read_json(self.root/connection/"state.json")
        self.assertTrue(latest["closed"])
        self.assertEqual(latest["account_switch"],switch_id)
        self.assertEqual(channel.prepare_account_switch({**request,"force":True})["carry"],result["carry"])

    def test_account_resume_launch_keeps_the_interrupted_note(self):
        switch_id,connection=str(uuid.uuid4()),str(uuid.uuid4())
        frozen=self.root.parent/"cli-account-switches"/switch_id/"history.jsonl"
        frozen.parent.mkdir(parents=True)
        frozen.write_text("{}\n")
        channel.save(frozen.with_name("receipt.json"),{"binding":{},"requests":{"old":"digest"},"carry":[{"id":"x","text":"y"}]})
        note="\n\nInterrupted by the account change: Long build"
        with patch.object(channel,"migrate_cage_policy"), patch.object(channel.os,"execvpe"):
            channel.launch({"id":connection,"session_id":self.session,"prefix":["claude"],"resume_transcript":str(frozen),"switch_id":switch_id,"continue_after_switch":True,"note":note})
        state=channel.read_json(self.root/connection/"state.json")
        self.assertEqual(state["note"],note)
        self.assertTrue(state["resume_task"] and state["account_resume"])
        self.assertEqual(state["approvals"],[])
        self.assertEqual(state["requests"],{"old":"digest"})

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
        with patch.object(channel,"migrate_cage_policy"), patch.object(channel,"active_sessions",return_value=[]), patch.object(channel.os,"execvpe") as execute:
            channel.launch({"id":connection,"resume":self.session,"prefix":["claude"],"config":config})
        self.assertEqual(execute.call_args.args[1][-2:],["--permission-mode","manual"])
        self.assertTrue(channel.read_json(self.root / connection / "state.json")["resume_task"])

    def test_launch_resumes_a_frozen_history_by_a_jsonl_path(self):
        # The official CLI resumes a path only when it ends in .jsonl; any other
        # value opens its session picker as a search (seen with 2.1.330).
        switch_id,connection=str(uuid.uuid4()),str(uuid.uuid4())
        frozen=self.root.parent/"cli-account-switches"/switch_id/"history.jsonl"
        frozen.parent.mkdir(parents=True)
        frozen.write_text("{}\n")
        channel.save(frozen.with_name("receipt.json"),{"binding":{},"requests":{}})
        with patch.object(channel,"migrate_cage_policy"), patch.object(channel.os,"execvpe") as execute:
            channel.launch({"id":connection,"session_id":self.session,"prefix":["claude"],"resume_transcript":str(frozen),"switch_id":switch_id})
        args=execute.call_args.args[1]
        resumed=args[args.index("--resume")+1]
        self.assertTrue(resumed.endswith("/history.jsonl"),resumed)
        self.assertNotIn("cage",resumed.lower())
        self.assertEqual(Path(resumed).resolve(),frozen.resolve())

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

    @unittest.skipIf(os.name == "nt", "POSIX guest launcher")
    def test_default_shared_launch_ignores_guest_auto_shims(self):
        cage = self.home / ".local/share/cage"
        default_bin = self.home / "default-bin"
        shims = [cage / "auto-shims", cage / "orca/shims", cage / "t3code/shims"]
        with patch.dict(os.environ, {"PATH": os.pathsep.join(str(path) for path in [*shims, default_bin])}), \
                patch.object(channel, "migrate_cage_policy"), patch.object(channel.os, "execvpe") as execute:
            channel.launch({"id": str(uuid.uuid4()), "prefix": ["claude"]})
        self.assertEqual(execute.call_args.args[2]["PATH"], str(default_bin))

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
        with patch.object(channel, "active_sessions", return_value=[{"pid": 1}]):
            with self.assertRaises(ValueError): channel.purge_session(request)
        self.assertTrue(target.exists())
        with patch.object(channel, "active_sessions", return_value=[]):
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
