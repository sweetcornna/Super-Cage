import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
import uuid
import warnings
from unittest.mock import patch

sys.dont_write_bytecode = True
source = Path(__file__).with_name("codex_sessions.py").resolve()
spec = importlib.util.spec_from_file_location("codex_sessions", source)
sessions = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sessions)

# A stand-in for the Codex CLI. It records its invocation and appends the
# records a real exec turn writes to its rollout. All content is synthetic.
FAKE_CODEX = """#!{python}
import json, os, pathlib, sys, time, uuid
args = sys.argv[1:]
home = pathlib.Path(os.environ.get("CODEX_HOME") or pathlib.Path.home() / ".codex")
level = os.environ.get("FAKE_CODEX_FLAGS", "all")
flags, commands = level == "all", level in ("all", "old")
options = "  -C, --cd <DIR>\\n      --add-dir <DIR>\\n      --no-daemon\\n" if flags else ""
if args == ["--version"]:
    print("codex-cli 0.171.0")
    sys.exit(0)
if args[-1:] == ["--help"]:
    with open(os.environ["FAKE_CODEX_CALLS"] + ".help", "a") as calls:
        calls.write(" ".join(args) + "\\n")
    if args == ["--help"]:
        print("Codex CLI\\n\\nCommands:\\n  exec    Run Codex non-interactively\\n" + ("  resume  Resume a previous session (picker by default; use --last to continue\\n          the most recent)\\n  delete  Permanently delete a saved session\\n" if commands else "") + "  help    Print this message\\n\\nOptions:\\n" + options)
    elif args[0] == "resume":
        print("Usage: codex resume [OPTIONS] [SESSION_ID]\\n" + options)
    elif args[0] == "delete":
        print("Usage: codex delete [OPTIONS] <SESSION>\\n" + ("      --force\\n" if flags else ""))
    sys.exit(0)
with open(os.environ["FAKE_CODEX_CALLS"], "a") as calls:
    calls.write(json.dumps({{"argv": args, "cwd": os.getcwd(), "subject": os.environ.get("CAGE_CODEX_SUBJECT", ""), "pid": os.getpid(), "home": str(home)}}) + "\\n")
if args[:1] == ["delete"]:
    for path in home.glob("sessions/*/*/*/rollout-*-" + args[-1] + "*.jsonl*"):
        path.unlink()
    sys.exit(0)
if args[:1] != ["exec"]:
    # The interactive TUI: it stays until the test lets it quit.
    while not os.path.exists(os.environ["FAKE_CODEX_CALLS"] + ".quit"):
        time.sleep(0.02)
    sys.exit(0)
mode = os.environ.get("FAKE_CODEX_MODE", "reply")
text = sys.stdin.read()
if mode == "wait":
    time.sleep(30)
if mode == "fail":
    print(json.dumps({{"type": "turn.failed", "error": {{"message": "synthetic usage limit"}}}}), flush=True)
    sys.exit(1)
if "resume" in args:
    thread = args[args.index("resume") + 1]
    path = next(home.glob("sessions/*/*/*/rollout-*-" + thread + ".jsonl"))
else:
    thread = str(uuid.uuid4())
    path = home / "sessions/2026/11/20" / ("rollout-2026-11-20T10-00-00-" + thread + ".jsonl")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({{"type": "session_meta", "payload": {{"id": thread, "cwd": os.getcwd(), "source": "exec", "originator": "codex_exec"}}}}) + "\\n")
print(json.dumps({{"type": "thread.started", "thread_id": thread}}), flush=True)
def row(kind, payload):
    with path.open("a") as stream:
        stream.write(json.dumps({{"type": kind, "payload": payload}}) + "\\n")
row("event_msg", {{"type": "task_started", "turn_id": "turn"}})
row("response_item", {{"type": "message", "role": "user", "content": [{{"type": "input_text", "text": text}}]}})
row("event_msg", {{"type": "item_completed", "item": {{"type": "UserMessage", "content": [{{"type": "text", "text": text}}]}}}})
if mode == "sleep":
    time.sleep(30)
row("response_item", {{"type": "message", "role": "assistant", "content": [{{"type": "output_text", "text": "reply: " + text}}]}})
row("event_msg", {{"type": "task_complete", "last_agent_message": "reply: " + text}})
"""


def record(kind, payload):
    return {"timestamp": "2026-11-20T10:00:00.000Z", "type": kind, "payload": payload}


class CodexSessionTests(unittest.TestCase):
    def setUp(self):
        # send() deliberately leaves its detached supervisor running.
        warnings.filterwarnings("ignore", "subprocess [0-9]+ is still running", ResourceWarning)
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.home = Path(temporary.name).resolve()
        self.project = self.home / "project"
        self.project.mkdir()
        bin_directory = self.home / "bin"
        bin_directory.mkdir()
        fake = bin_directory / "codex"
        fake.write_text(FAKE_CODEX.format(python=sys.executable))
        fake.chmod(0o755)
        self.calls = self.home / "calls.jsonl"
        self.env = {**os.environ, "HOME": str(self.home), "PATH": str(bin_directory) + os.pathsep + os.environ.get("PATH", os.defpath),
                    "FAKE_CODEX_CALLS": str(self.calls), "TMPDIR": str(self.home)}
        self.env.pop("CODEX_HOME", None)
        environment = patch.dict(os.environ, self.env, clear=True)
        environment.start()
        self.addCleanup(environment.stop)
        for name, value in (("ROOT", self.home / ".local/share/cage/codex-sessions"), ("ACCOUNTS", self.home / ".local/share/cage/codex-accounts")):
            patcher = patch.object(sessions, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.addCleanup(self.stop_runs)

    def stop_runs(self):
        for path in sessions.ROOT.glob("*/state.json"):
            run = sessions.read_json(path, {}).get("run") or {}
            if sessions.alive(run):
                try:
                    os.killpg(run["pid"], 9)
                except OSError:
                    pass

    def rollout(self, rows, thread=None, home=None, day="2026/11/20", now=False):
        thread = thread or str(uuid.uuid4())
        path = (home or self.home / ".codex") / "sessions" / day / ("rollout-2026-11-20T10-00-00-" + thread + ".jsonl")
        path.parent.mkdir(parents=True, exist_ok=True)
        meta = record("session_meta", {"id": thread, "cwd": str(self.project), "originator": "codex-tui", "source": "cli",
                                       "base_instructions": {"text": "synthetic base instructions"}})
        if now:
            meta["timestamp"] = time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime())
        path.write_text("".join(json.dumps(row) + "\n" for row in [meta, *rows]))
        return thread, path

    def turn_rows(self, prompt, reply, tools=()):
        rows = [record("event_msg", {"type": "task_started", "turn_id": "turn"}),
                record("response_item", {"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "synthetic developer policy"}]}),
                record("response_item", {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "<environment_context>synthetic</environment_context>"}]}),
                record("response_item", {"type": "message", "role": "user", "content": [{"type": "input_text", "text": prompt}]}),
                record("event_msg", {"type": "item_completed", "item": {"type": "UserMessage", "content": [{"type": "text", "text": prompt}]}}),
                record("world_state", {"full": True, "state": {"tools": {"synthetic": "description"}}}),
                record("response_item", {"type": "reasoning", "summary": [], "encrypted_content": "synthetic-encrypted"}),
                record("response_item", {"type": "agent_message", "author": "/root/worker", "recipient": "/root", "content": [{"type": "input_text", "text": "synthetic inter-agent note"}]}),
                record("response_item", {"type": "message", "role": "assistant", "phase": "commentary", "content": [{"type": "output_text", "text": "checking"}]})]
        rows += tools
        rows += [record("event_msg", {"type": "token_count", "info": {}}),
                 record("response_item", {"type": "message", "role": "assistant", "phase": "final_answer", "content": [{"type": "output_text", "text": reply}]}),
                 record("event_msg", {"type": "item_completed", "item": {"type": "AgentMessage", "content": [{"type": "Text", "text": reply}]}}),
                 record("event_msg", {"type": "task_complete", "last_agent_message": reply})]
        return rows

    def wait(self, request, predicate, timeout=15):
        deadline = time.monotonic() + timeout
        while True:
            result = sessions.view(request)
            if predicate(result):
                return result
            if time.monotonic() > deadline:
                self.fail("view did not reach the expected state: " + json.dumps(result, ensure_ascii=False))
            time.sleep(0.1)

    def test_transcript_keeps_conversation_and_skips_internal_records(self):
        tools = [
            record("response_item", {"type": "function_call", "name": "exec_command", "arguments": json.dumps({"cmd": "ls -la\nsecond line"}), "call_id": "call-1"}),
            record("response_item", {"type": "function_call_output", "call_id": "call-1", "output": {"content": "synthetic", "success": False}}),
            record("response_item", {"type": "custom_tool_call", "name": "apply_patch", "input": "*** Begin Patch\n*** Update File: src/app.py\n@@\n", "call_id": "call-2"}),
            record("response_item", {"type": "custom_tool_call_output", "call_id": "call-2", "output": [{"type": "input_text", "text": "done"}]}),
            record("response_item", {"type": "function_call", "name": "spawn_agent", "namespace": "collaboration", "arguments": "{}", "call_id": "call-3"}),
        ]
        thread, path = self.rollout(self.turn_rows("first question", "final answer", tools) + self.turn_rows("second question", "second answer")[:4]
                                    + [record("event_msg", {"type": "turn_aborted", "reason": "interrupted"})])
        turns, cwd, pending, truncated = sessions.transcript(path, thread)
        self.assertEqual(cwd, str(self.project))
        self.assertFalse(pending)
        self.assertFalse(truncated)
        self.assertEqual([turn["prompt"] for turn in turns], ["first question", "second question"])
        self.assertEqual(turns[0]["text"], "checking\n\nfinal answer")
        self.assertEqual(turns[0]["status"], "completed")
        self.assertEqual(turns[1]["status"], "stopped")
        self.assertEqual([(tool["name"], tool.get("detail"), tool["status"]) for tool in turns[0]["tools"]],
                         [("exec_command", "ls -la", "failed"), ("apply_patch", "src/app.py", "completed"), ("collaboration.spawn_agent", None, "called")])
        serialized = json.dumps([{key: value for key, value in turn.items() if not key.startswith("_")} for turn in turns])
        for hidden in ("synthetic developer policy", "environment_context", "synthetic-encrypted", "synthetic inter-agent note", "synthetic base instructions"):
            self.assertNotIn(hidden, serialized)
        self.assertEqual(turns[0]["id"], sessions.turn_id(thread, turns[0]["_offset"]))
        self.assertEqual(uuid.UUID(turns[0]["id"]).version, 4)

    def test_recovery_requires_a_native_rollout_for_the_exact_id(self):
        thread, path = self.rollout(self.turn_rows("old prompt", "old reply"))
        self.assertTrue(sessions.view({"thread": thread})["history_found"])
        other = str(uuid.uuid4())
        path.rename(path.with_name(path.name.replace(thread, other)))
        self.assertFalse(sessions.view({"thread": thread})["history_found"])
        self.assertFalse(sessions.view({"thread": other})["history_found"])

    def test_tail_is_bounded_and_history_is_trimmed(self):
        rows = []
        for number in range(40):
            rows += self.turn_rows("question %d" % number, "answer %d " % number + "x" * 200)
        thread, path = self.rollout(rows)
        with patch.object(sessions, "TAIL_LIMIT", 6000), patch.object(sessions, "HISTORY_LIMIT", 2000):
            turns, _, _, truncated = sessions.transcript(path, thread)
            self.assertTrue(truncated)
            self.assertLess(len(turns), 40)
            self.assertEqual(turns[-1]["prompt"], "question 39")
            # Output before the first prompt in the window is never attributed to it.
            self.assertTrue(all(turn["text"].startswith("checking\n\nanswer") for turn in turns))
            result = sessions.view({"thread": thread, "account": "default"})
        self.assertTrue(result["history_truncated"])
        self.assertLessEqual(len(json.dumps(result["turns"], ensure_ascii=False).encode()), 2000)
        self.assertEqual(result["turns"][-1]["prompt"], "question 39")
        self.assertEqual(result["title"], "question 0")

    def test_discovery_lists_project_sessions_without_subagents(self):
        thread, _ = self.rollout(self.turn_rows("list me\nplease", "ok"))
        sub, subpath = self.rollout(self.turn_rows("worker task", "ok"))
        rows = subpath.read_text().splitlines()
        meta = json.loads(rows[0])
        meta["payload"]["thread_source"] = "subagent"
        meta["payload"]["source"] = {"subagent": {"thread_spawn": {"parent_thread_id": thread, "depth": 1}}}
        subpath.write_text("\n".join([json.dumps(meta), *rows[1:]]) + "\n")
        other = self.home / "other"
        other.mkdir()
        outside, outside_path = self.rollout(self.turn_rows("elsewhere", "ok"))
        outside_path.write_text(outside_path.read_text().replace(json.dumps(str(self.project)), json.dumps(str(other)), 1))
        account = "0123456789abcdef01234567"
        managed, _ = self.rollout(self.turn_rows("managed account", "ok"), home=self.home / ".local/share/cage/codex-accounts" / account / "home")
        found = sessions.discover({"accounts": ["default", account], "workspaces": [str(self.project)]})
        self.assertEqual(sorted((item["thread_id"], item["account"], item["title"]) for item in found),
                         sorted([(thread, "default", "list me please"), (managed, account, "managed account")]))
        self.assertTrue(all(not item["running"] and not item["active"] and item["connection"] == "" for item in found))
        # The cached head answers later listings without re-reading rollouts.
        with patch.object(sessions, "head", side_effect=AssertionError("re-read")):
            self.assertEqual(len(sessions.discover({"accounts": ["default", account], "workspaces": [str(self.project)]})), 2)

    def test_send_runs_detached_exec_and_keeps_request_ids(self):
        connection, first, second = (str(uuid.uuid4()) for _ in range(3))
        request = {"connection": connection, "request_id": first, "text": "hello codex", "account": "default", "cwd": str(self.project)}
        self.assertEqual(sessions.send(request), {"ok": True})
        self.assertEqual(sessions.send(request), {"ok": True})
        with self.assertRaisesRegex(ValueError, "同一发送编号"):
            sessions.send({**request, "text": "different"})
        query = {"connection": connection, "account": "default", "thread": ""}
        result = self.wait(query, lambda result: result["turns"] and result["turns"][-1]["status"] == "completed" and not result["running"])
        thread = result["thread_id"]
        self.assertEqual(uuid.UUID(thread).hex, uuid.UUID(thread).hex)
        self.assertEqual([(turn["id"], turn["prompt"], turn["text"]) for turn in result["turns"]], [(first, "hello codex", "reply: hello codex")])
        self.assertTrue(result["can_send"])
        self.assertEqual(result["workspace"], str(self.project))
        self.assertEqual(sessions.send({**request, "request_id": second, "text": "next step", "thread": thread}), {"ok": True})
        result = self.wait({**query, "thread": thread}, lambda result: len(result["turns"]) == 2 and result["turns"][-1]["status"] == "completed" and not result["running"])
        self.assertEqual([turn["id"] for turn in result["turns"]], [first, second])
        calls = [json.loads(line) for line in self.calls.read_text().splitlines()]
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0]["argv"], ["exec", "--json", "--skip-git-repo-check", "-C", str(self.project), "-"])
        self.assertEqual(calls[1]["argv"], ["exec", "--json", "--skip-git-repo-check", "-C", str(self.project), "resume", thread, "-"])
        self.assertEqual(calls[1]["cwd"], str(self.project))
        state = sessions.read_json(sessions.ROOT / connection / "state.json")
        self.assertEqual(state["thread_id"], thread)
        self.assertNotIn("text", json.dumps(state["sent"]))

    def test_running_turn_blocks_sending_and_stops(self):
        os.environ["FAKE_CODEX_MODE"] = "sleep"
        connection, request_id = str(uuid.uuid4()), str(uuid.uuid4())
        sessions.send({"connection": connection, "request_id": request_id, "text": "long task", "account": "default", "cwd": str(self.project)})
        query = {"connection": connection, "account": "default"}
        result = self.wait(query, lambda result: result["thread_id"] and result["turns"] and result["turns"][-1]["id"] == request_id
                           and result["turns"][-1]["prompt"] == "long task")
        self.assertEqual(result["turns"][-1]["status"], "running")
        self.assertTrue(result["running"] and result["active"])
        self.assertFalse(result["can_send"])
        listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
        self.assertEqual([(item["thread_id"], item["connection"], item["running"], item["active"]) for item in listed],
                         [(result["thread_id"], connection, True, True)])
        with self.assertRaisesRegex(ValueError, "正在处理"):
            sessions.send({"connection": connection, "request_id": str(uuid.uuid4()), "text": "again", "account": "default", "cwd": str(self.project)})
        with self.assertRaisesRegex(ValueError, "已结束或不是从手机发起"):
            sessions.stop({"connection": connection, "request_id": str(uuid.uuid4())})
        self.assertEqual(sessions.stop({"connection": connection, "request_id": request_id}), {"ok": True})
        result = sessions.view(query)
        self.assertEqual(result["turns"][-1]["status"], "stopped")
        self.assertFalse(result["running"])
        self.assertTrue(result["can_send"])

    def test_new_session_is_listed_before_its_rollout_exists(self):
        os.environ["FAKE_CODEX_MODE"] = "wait"
        connection, request_id = str(uuid.uuid4()), str(uuid.uuid4())
        sessions.send({"connection": connection, "request_id": request_id, "text": "start", "account": "default", "cwd": str(self.project)})
        listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
        self.assertEqual([(item["thread_id"], item["connection"], item["running"]) for item in listed], [("", connection, True)])
        result = sessions.view({"connection": connection, "account": "default"})
        self.assertEqual([(turn["id"], turn["prompt"], turn["status"]) for turn in result["turns"]], [(request_id, "start", "running")])

    def test_failed_turn_reports_reason(self):
        os.environ["FAKE_CODEX_MODE"] = "fail"
        connection, request_id = str(uuid.uuid4()), str(uuid.uuid4())
        sessions.send({"connection": connection, "request_id": request_id, "text": "will fail", "account": "default", "cwd": str(self.project)})
        result = self.wait({"connection": connection, "account": "default"}, lambda result: result["turns"] and result["turns"][-1]["status"] == "failed")
        self.assertEqual(result["turns"][-1]["error"], "synthetic usage limit")
        self.assertTrue(result["can_send"])

    def test_account_home_runs_through_account_helper(self):
        account, subject = "0123456789abcdef01234567", "a" * 64
        helper = self.home / "cage-codex-account"
        helper.write_text("#!/bin/sh\nprintf '%s\\n' \"$*\" \"$CAGE_CODEX_SUBJECT\" > " + str(self.home / "helper-call") + "\nexit 78\n")
        helper.chmod(0o755)
        connection, request_id = str(uuid.uuid4()), str(uuid.uuid4())
        request = {"connection": connection, "request_id": request_id, "text": "use account", "account": account, "subject": subject, "cwd": str(self.project)}
        with patch.object(sessions, "ACCOUNT_HELPER", helper):
            with self.assertRaisesRegex(ValueError, "账号状态"):
                sessions.send({**request, "subject": ""})
            sessions.send(request)
            result = self.wait({"connection": connection, "account": account}, lambda result: result["turns"] and result["turns"][-1]["status"] == "failed")
        self.assertTrue(result["account_refused"])
        self.assertIn("Codex 账号", result["turns"][-1]["error"])
        self.assertEqual((self.home / "helper-call").read_text().splitlines(),
                         ["run " + account + " exec --json --skip-git-repo-check -C " + str(self.project) + " -", subject])
        with self.assertRaisesRegex(ValueError, "账号已变化"):
            sessions.send({**request, "request_id": str(uuid.uuid4()), "account": "default"})

    def test_interactive_terminal_blocks_sending(self):
        thread, path = self.rollout(self.turn_rows("in the terminal", "ok")[:5])
        proc = self.home / "proc"
        (proc / "4000001/fd").mkdir(parents=True)
        (proc / "4000001/cmdline").write_bytes(b"node\0/usr/lib/codex.js\0")
        os.symlink(path, proc / "4000001/fd/7")
        (proc / "4000002/fd").mkdir(parents=True)
        (proc / "4000002/cmdline").write_bytes(b"/home/cage/.local/bin/codex\0resume\0" + thread.encode() + b"\0")
        (proc / "4000003/fd").mkdir(parents=True)
        (proc / "4000003/cmdline").write_bytes(b"bash\0")
        other = str(uuid.uuid4())
        lock = self.home / ".codex/thread-writer-locks" / (other + ".lock")
        lock.parent.mkdir(parents=True)
        lock.touch()
        (proc / "4000004/fd").mkdir(parents=True)
        (proc / "4000004/cmdline").write_bytes(b"node\0")
        os.symlink(lock, proc / "4000004/fd/9")
        with patch.object(sessions, "PROC", proc):
            files, threads = sessions.rollout_users()
            self.assertEqual(files, {str(path): {4000001}})
            self.assertEqual(threads, {thread: {4000002}, other: {4000004}})
            result = sessions.view({"thread": thread, "account": "default"})
            self.assertFalse(result["can_send"])
            self.assertTrue(result["running"] and result["active"])
            self.assertEqual(result["connection_notice"], sessions.TUI_OPEN)
            self.assertEqual(result["turns"][-1]["status"], "running")
            with self.assertRaisesRegex(ValueError, "Codex 终端"):
                sessions.send({"connection": str(uuid.uuid4()), "request_id": str(uuid.uuid4()), "text": "x", "account": "default",
                               "thread": thread, "cwd": str(self.project)})
            listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
            self.assertEqual([(item["running"], item["active"]) for item in listed], [(True, True)])
            # Processes of our own exec run are not an interactive terminal.
            with patch.object(sessions.os, "getpgid", return_value=4242):
                self.assertEqual(sessions.foreign(path, thread, (files, threads), {4242}), set())
        self.assertEqual(sessions.view({"thread": thread, "account": "default"})["turns"][-1]["status"], "interrupted")

    def test_purge_removes_only_the_authorized_rollout(self):
        thread, path = self.rollout(self.turn_rows("remove me", "ok"))
        keep, kept = self.rollout(self.turn_rows("keep me", "ok"))
        with self.assertRaisesRegex(ValueError, "不一致"):
            sessions.purge({"thread": thread, "account": "default", "workspace": str(self.home / "other")})
        self.assertTrue(path.exists())
        self.assertEqual(sessions.purge({"thread": thread, "account": "default", "workspace": str(self.project)}), {"ok": True})
        self.assertFalse(path.exists())
        self.assertTrue(kept.exists())
        self.assertEqual(sessions.purge({"thread": thread, "account": "default", "workspace": str(self.project)}), {"ok": True})

    def calls_of(self, kind=None):
        if not self.calls.exists():
            return []
        rows = [json.loads(line) for line in self.calls.read_text().splitlines()]
        return [row for row in rows if kind is None or (row["argv"][:1] == ["exec"]) == (kind == "exec")]

    def launch(self, connection, lease=None, thread="", account="default", subject="", wait=True):
        request = {"connection": connection, "lease": lease or str(uuid.uuid4()), "thread": thread, "account": account,
                   "subject": subject, "cwd": str(self.project), "additional_directories": []}
        process = subprocess.Popen([sys.executable, "-I", str(source), "tui-launch", json.dumps(request)], cwd=str(self.project),
                                   env=self.env_now(), stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.finish, process)
        if wait:
            deadline = time.monotonic() + 15
            while not any(row.get("pid") == process.pid for row in self.calls_of("tui")):
                if process.poll() is not None:
                    self.fail("tui-launch ended: %r" % (process.stderr.read().decode(),))
                if time.monotonic() > deadline:
                    self.fail("the TUI did not start")
                time.sleep(0.02)
        return process

    def env_now(self):
        return dict(os.environ)

    def finish(self, process):
        if process.poll() is None:
            process.kill()
        process.communicate()

    def state(self, connection):
        return sessions.read_json(sessions.ROOT / connection / "state.json", {})

    def send(self, connection, thread="", text="from the phone"):
        return sessions.send({"connection": connection, "request_id": str(uuid.uuid4()), "text": text, "account": "default",
                              "thread": thread, "cwd": str(self.project)})

    def test_probe_reads_the_cli_help(self):
        result = sessions.probe({})
        self.assertEqual({key: result[key] for key in ("installed", "version", "resume", "no_daemon", "resume_no_daemon", "delete")},
                         {"installed": True, "version": "0.171.0", "resume": True, "no_daemon": True, "resume_no_daemon": True, "delete": True})
        asked = self.calls.with_name("calls.jsonl.help").read_text().splitlines()
        self.assertEqual(sorted(asked), ["--help", "delete --help", "resume --help"])
        # Cached for the same binary.
        sessions.probe({})
        self.assertEqual(len(self.calls.with_name("calls.jsonl.help").read_text().splitlines()), 3)
        (sessions.ROOT / "probe.json").unlink()
        os.environ["FAKE_CODEX_FLAGS"] = "none"
        result = sessions.probe({})
        self.assertEqual((result["resume"], result["no_daemon"], result["delete"]), (False, False, False))
        os.environ["PATH"] = str(self.home / "empty")
        self.assertFalse(sessions.probe({})["installed"])
        self.assertEqual(sessions.help_commands("Commands:\n  resume  x\n          wrapped words\n  fork-it  y\n\n  later  z"), {"resume", "fork-it"})

    def test_tui_launch_takes_a_lease_that_blocks_the_phone_until_codex_ends(self):
        thread, _ = self.rollout(self.turn_rows("desk", "ok"))
        connection, lease = str(uuid.uuid4()), str(uuid.uuid4())
        process = self.launch(connection, lease, thread)
        state = self.state(connection)
        # Every step execs, so the lease records the PID that is now Codex.
        self.assertEqual(state["lease"]["pid"], process.pid)
        self.assertEqual((state["lease"]["id"], state["lease"]["thread"], state["thread_id"]), (lease, thread, thread))
        tui = [row for row in self.calls_of("tui") if row["pid"] == process.pid][0]
        self.assertEqual(tui["argv"], ["resume", "-C", str(self.project), "--no-daemon", thread])
        with self.assertRaisesRegex(ValueError, "电脑的 Codex 终端"):
            self.send(connection, thread)
        # Another connection of the same thread is refused as well.
        with self.assertRaisesRegex(ValueError, "电脑的 Codex 终端"):
            self.send(str(uuid.uuid4()), thread)
        result = sessions.view({"connection": connection, "thread": thread, "account": "default"})
        self.assertEqual((result["owner"], result["lease"], result["can_send"], result["running"]), ("desktop", lease, False, True))
        self.assertEqual(result["connection_notice"], sessions.DESKTOP_NOTICE)
        listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
        self.assertEqual([(item["thread_id"], item["owner"], item["lease"]) for item in listed], [(thread, "desktop", lease)])
        self.calls.with_name("calls.jsonl.quit").touch()
        process.wait(10)
        self.calls.with_name("calls.jsonl.quit").unlink()
        self.assertEqual(sessions.view({"connection": connection, "thread": thread, "account": "default"})["owner"], "")
        self.assertNotIn("lease", self.state(connection))
        self.assertEqual(self.send(connection, thread), {"ok": True})

    def test_launch_is_refused_while_another_writer_owns_the_thread(self):
        thread, _ = self.rollout(self.turn_rows("busy", "ok"))
        connection = str(uuid.uuid4())
        with sessions.state_file(connection) as state:
            state.update(thread_id=thread, account="default", run={"request_id": str(uuid.uuid4()), "pid": os.getpid(), "start": sessions.fingerprint(os.getpid())})
        refused = self.launch(connection, thread=thread, wait=False)
        _, error = refused.communicate(timeout=15)
        self.assertEqual(refused.returncode, sessions.LAUNCH_REFUSED)
        self.assertIn(sessions.PHONE_RUNNING, error.decode())
        self.assertEqual(self.calls_of("tui"), [])
        self.assertNotIn("lease", self.state(connection))
        other = str(uuid.uuid4())
        first = self.launch(other, thread=self.rollout(self.turn_rows("second", "ok"))[0])
        for connection_id in (other, str(uuid.uuid4())):
            second = self.launch(connection_id, thread=self.state(other)["thread_id"], wait=False)
            _, error = second.communicate(timeout=15)
            self.assertEqual(second.returncode, sessions.LAUNCH_REFUSED)
            self.assertIn(sessions.OTHER_TERMINAL, error.decode())
        self.assertEqual(len(self.calls_of("tui")), 1)
        self.assertIsNone(first.poll())

    def test_old_corrupt_and_unknown_states(self):
        thread, _ = self.rollout(self.turn_rows("old", "ok"))
        old = str(uuid.uuid4())
        with sessions.state_file(old) as state:
            state.update(thread_id=thread, account="default", cwd=str(self.project))
        self.assertEqual(self.send(old, thread), {"ok": True})
        corrupt = str(uuid.uuid4())
        (sessions.ROOT / corrupt).mkdir(parents=True)
        (sessions.ROOT / corrupt / "state.json").write_text("{not json")
        with self.assertRaisesRegex(ValueError, "状态无法读取"):
            self.send(corrupt)
        self.assertFalse(sessions.view({"connection": corrupt, "account": "default"})["can_send"])
        unknown = str(uuid.uuid4())
        with sessions.state_file(unknown) as state:
            state["lease"] = {"id": str(uuid.uuid4()), "pid": 4242, "start": "x", "thread": ""}
        with patch.object(sessions, "process_start", return_value=None):
            self.assertEqual(sessions.lease_state(self.state(unknown)["lease"]), "unknown")
            with self.assertRaisesRegex(ValueError, "电脑的 Codex 终端"):
                self.send(unknown)
        # A lease of a process that has ended is dropped by the next send.
        ended = str(uuid.uuid4())
        with sessions.state_file(ended) as state:
            state["lease"] = {"id": str(uuid.uuid4()), "pid": 999999, "start": "gone", "thread": ""}
        os.environ["FAKE_CODEX_MODE"] = "wait"
        self.assertEqual(self.send(ended), {"ok": True})
        self.assertNotIn("lease", self.state(ended))

    def test_new_desktop_session_binds_once_and_its_thread_is_withheld_until_then(self):
        connection, lease = str(uuid.uuid4()), str(uuid.uuid4())
        process = self.launch(connection, lease)
        self.assertEqual(self.calls_of("tui")[0]["argv"], ["-C", str(self.project), "--no-daemon"])
        older, _ = self.rollout(self.turn_rows("from yesterday", "ok"))
        thread, path = self.rollout(self.turn_rows("new on the desktop", "ok"), now=True)
        # The rollout appeared after the lease, in its directory: it may be the
        # new session's, so it is neither listed nor writable from the phone.
        listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
        self.assertEqual(sorted((item["thread_id"], item["connection"], item["owner"]) for item in listed), sorted([(older, "", ""), ("", connection, "desktop")]))
        with self.assertRaisesRegex(ValueError, "电脑的 Codex 终端"):
            self.send(str(uuid.uuid4()), thread)
        # The process holding it binds it.
        proc = self.home / "proc"
        (proc / str(process.pid) / "fd").mkdir(parents=True)
        (proc / str(process.pid) / "cmdline").write_bytes(b"node\0/usr/lib/codex.js\0")
        os.symlink(path, proc / str(process.pid) / "fd" / "7")
        with patch.object(sessions, "PROC", proc):
            listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
        self.assertEqual(sorted((item["thread_id"], item["connection"], item["owner"]) for item in listed), sorted([(older, "", ""), (thread, connection, "desktop")]))
        self.assertEqual((self.state(connection)["thread_id"], self.state(connection)["lease"]["thread"]), (thread, thread))
        # A thread the TUI opens later never re-binds the session.
        later, later_path = self.rollout(self.turn_rows("switched", "ok"))
        os.unlink(proc / str(process.pid) / "fd" / "7")
        os.symlink(later_path, proc / str(process.pid) / "fd" / "7")
        with patch.object(sessions, "PROC", proc):
            result = sessions.view({"connection": connection, "account": "default"})
        self.assertEqual((result["thread_id"], result["owner"]), (thread, "desktop"))
        self.assertEqual(self.send(str(uuid.uuid4()), later), {"ok": True})

    def quit_tui(self, process):
        self.calls.with_name("calls.jsonl.quit").touch()
        process.wait(10)
        self.calls.with_name("calls.jsonl.quit").unlink()

    def test_a_new_session_that_ends_before_it_is_bound_takes_its_thread(self):
        connection = str(uuid.uuid4())
        process = self.launch(connection)
        older, _ = self.rollout(self.turn_rows("from yesterday", "ok"))
        thread, _ = self.rollout(self.turn_rows("quick question", "ok"), now=True)
        self.quit_tui(process)
        listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
        self.assertEqual(sorted((item["thread_id"], item["connection"], item["owner"]) for item in listed), sorted([(older, "", ""), (thread, connection, "")]))
        self.assertEqual(self.state(connection).get("thread_id"), thread)
        self.assertNotIn("lease", self.state(connection))
        # The phone continues that thread, not a new one.
        self.assertEqual(self.send(connection), {"ok": True})
        self.assertEqual(self.wait({"connection": connection, "account": "default"}, lambda result: result["owner"] == "")["thread_id"], thread)
        # Two threads it could have made, or one another process holds: none is taken.
        for count, held in ((2, False), (1, True)):
            other = str(uuid.uuid4())
            process = self.launch(other)
            made = [self.rollout(self.turn_rows("ambiguous", "ok"), now=True) for _ in range(count)]
            self.quit_tui(process)
            proc = self.home / "proc"
            if held:
                holder = subprocess.Popen(["sleep", "30"])
                self.addCleanup(holder.kill)
                (proc / str(holder.pid) / "fd").mkdir(parents=True)
                os.symlink(made[0][1], proc / str(holder.pid) / "fd" / "3")
            with patch.object(sessions, "PROC", proc if held else sessions.PROC):
                result = sessions.view({"connection": other, "account": "default"})
            self.assertEqual((result["thread_id"], self.state(other).get("thread_id", "")), ("", ""))
            self.assertNotIn("lease", self.state(other))
            listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
            self.assertTrue({thread for thread, _ in made} <= {item["thread_id"] for item in listed if not item["connection"]})

    def test_a_thread_the_desktop_tui_moves_to_is_not_idle(self):
        thread, _ = self.rollout(self.turn_rows("bound", "ok"))
        other, other_path = self.rollout(self.turn_rows("after /new", "ok"))
        connection = str(uuid.uuid4())
        process = self.launch(connection, thread=thread)
        proc = self.home / "proc"
        (proc / str(process.pid) / "fd").mkdir(parents=True)
        (proc / str(process.pid) / "cmdline").write_bytes(b"codex\0")
        os.symlink(other_path, proc / str(process.pid) / "fd" / "9")
        with patch.object(sessions, "PROC", proc):
            listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
            viewed = sessions.view({"thread": other, "account": "default"})
            with self.assertRaisesRegex(ValueError, "Codex 终端"):
                self.send(str(uuid.uuid4()), other)
        owners = {item["thread_id"]: item["owner"] for item in listed}
        self.assertEqual(owners, {thread: "desktop", other: "external"})
        self.assertEqual((viewed["owner"], viewed["can_send"]), ("external", False))

    def test_archive_stop_and_purge(self):
        os.environ["FAKE_CODEX_MODE"] = "sleep"
        connection, request_id = str(uuid.uuid4()), str(uuid.uuid4())
        sessions.send({"connection": connection, "request_id": request_id, "text": "long", "account": "default", "cwd": str(self.project)})
        result = self.wait({"connection": connection, "account": "default"}, lambda result: result["thread_id"] and result["owner"] == "phone")
        self.assertEqual(sessions.stop({"connection": connection, "reason": "archive"}), {"ok": True})
        result = sessions.view({"connection": connection, "account": "default"})
        self.assertEqual((result["owner"], result["turns"][-1]["status"]), ("", "stopped"))
        self.assertEqual(sessions.stop({"connection": connection, "reason": "archive"}), {"ok": True})
        self.assertEqual(sessions.stop({"connection": str(uuid.uuid4()), "reason": "archive"}), {"ok": True})
        thread = result["thread_id"]
        lease = str(uuid.uuid4())
        process = self.launch(connection, lease, thread)
        with self.assertRaisesRegex(ValueError, "请先关闭该终端"):
            sessions.purge({"thread": thread, "account": "default", "connection": connection, "workspace": str(self.project)})
        process.kill()
        process.wait(10)
        self.assertEqual(sessions.purge({"thread": thread, "account": "default", "connection": connection, "workspace": str(self.project)}), {"ok": True})
        deletes = [row for row in self.calls_of("tui") if row["argv"][:1] == ["delete"]]
        self.assertEqual([row["argv"] for row in deletes], [["delete", "--force", thread]])
        self.assertIsNone(sessions.find_rollout(self.home / ".codex", thread))
        # Without delete support the rollout is removed directly.
        (sessions.ROOT / "probe.json").unlink()
        os.environ["FAKE_CODEX_FLAGS"] = "none"
        other, other_path = self.rollout(self.turn_rows("plain", "ok"))
        self.assertEqual(sessions.purge({"thread": other, "account": "default", "workspace": str(self.project)}), {"ok": True})
        self.assertFalse(other_path.exists())
        self.assertEqual(len([row for row in self.calls_of("tui") if row["argv"][:1] == ["delete"]]), 1)

    def test_tui_flags_come_from_the_probe(self):
        every = dict(sessions.UNPROBED, complete=True, resume=True, cd=True, add_dir=True, no_daemon=True, resume_cd=True,
                     resume_add_dir=True, resume_no_daemon=True)
        thread, extra = str(uuid.uuid4()), str(self.home)
        self.assertEqual(sessions.tui_args(every, thread, self.project, [extra]),
                         ["resume", "-C", str(self.project), "--add-dir", extra, "--no-daemon", thread])
        self.assertEqual(sessions.tui_args(every, "", self.project, [extra]), ["-C", str(self.project), "--add-dir", extra, "--no-daemon"])
        old = dict(sessions.UNPROBED, complete=True, resume=True)
        self.assertEqual(sessions.tui_args(old, thread, self.project, [extra]), ["resume", thread])
        self.assertEqual(sessions.tui_args(sessions.UNPROBED, "", self.project, [extra]), [])
        # An older CLI: no optional flags, and the TUI starts in the project.
        os.environ["FAKE_CODEX_FLAGS"] = "old"
        resumed, _ = self.rollout(self.turn_rows("old cli", "ok"))
        process = self.launch(str(uuid.uuid4()), thread=resumed)
        tui = [row for row in self.calls_of("tui") if row["pid"] == process.pid][0]
        self.assertEqual((tui["argv"], os.path.realpath(tui["cwd"])), (["resume", resumed], os.path.realpath(self.project)))

    def test_launch_waits_for_an_unfinished_probe(self):
        thread, _ = self.rollout(self.turn_rows("slow", "ok"))
        connection = str(uuid.uuid4())
        request = {"connection": connection, "lease": str(uuid.uuid4()), "thread": thread, "account": "default", "subject": "",
                   "cwd": str(self.project), "additional_directories": []}
        with patch.object(sessions, "PROBE_TIMEOUT", 0.001):
            with self.assertRaisesRegex(ValueError, "暂时无法确认"):
                sessions.tui_launch(request)
        self.assertFalse((sessions.ROOT / "probe.json").exists())
        self.assertNotIn("lease", self.state(connection))
        self.assertEqual(self.calls_of("tui"), [])

    def test_unknown_process_state_counts_as_running(self):
        # A phone turn whose process cannot be told from a reused PID, or whose
        # start time cannot be read, is running for every exclusion: a desktop
        # TUI never starts beside it and no second turn is admitted.
        thread, _ = self.rollout(self.turn_rows("unknown", "ok"))
        sleeper = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"], start_new_session=True)
        self.addCleanup(self.finish, sleeper)
        mine = {"request_id": str(uuid.uuid4()), "argv": ["codex"], "pid": sleeper.pid, "start": sessions.fingerprint(sleeper.pid)}
        self.assertTrue(mine["start"] and sessions.alive(mine))
        real_start = sessions.process_start
        with patch.object(sessions, "process_start", return_value=None):
            self.assertTrue(sessions.alive(mine))
        self.assertTrue(sessions.alive({**mine, "start": ""}))
        # Ended: no such process, a recorded exit, nothing launched.
        self.assertFalse(sessions.alive({**mine, "pid": 999999}))
        self.assertFalse(sessions.alive({**mine, "pid": 999999, "start": ""}))
        self.assertFalse(sessions.alive({**mine, "exit_code": 0}))
        self.assertFalse(sessions.alive({"request_id": mine["request_id"]}))
        # Admitted, but the send was cut off before it recorded the PID: held
        # while a supervisor of the connection may run.
        connection = str(uuid.uuid4())
        admitted = {"request_id": mine["request_id"], "argv": ["codex"]}
        self.assertTrue(sessions.alive(admitted))
        with patch.object(sessions, "supervising", return_value=None):
            self.assertTrue(sessions.alive(admitted, connection))
        with patch.object(sessions, "supervising", return_value=True):
            self.assertTrue(sessions.alive(admitted, connection))
        with patch.object(sessions, "supervising", return_value=False):
            self.assertFalse(sessions.alive(admitted, connection))
        with sessions.state_file(connection) as state:
            state.update(thread_id=thread, account="default", cwd=str(self.project), run=mine)
        with patch.object(sessions, "process_start", side_effect=lambda pid: None if pid == sleeper.pid else real_start(pid)):
            with self.assertRaisesRegex(ValueError, "已有消息正在处理"):
                self.send(connection, thread)
            # Stop signals only a process it can identify.
            with self.assertRaisesRegex(ValueError, "无法确认"):
                sessions.stop({"connection": connection, "request_id": mine["request_id"]})

    def test_a_phone_turn_of_another_connection_excludes_the_thread(self):
        # Two records for one thread (two workbenches adopting it): a phone
        # turn admitted on connection A whose Codex has not started yet is
        # seen by a desktop open and by a phone send on connection B.
        thread, _ = self.rollout(self.turn_rows("shared", "ok"))
        sleeper = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"], start_new_session=True)
        self.addCleanup(self.finish, sleeper)
        first = str(uuid.uuid4())
        with sessions.state_file(first) as state:
            state.update(thread_id=thread, account="default", cwd=str(self.project),
                         run={"request_id": str(uuid.uuid4()), "pid": sleeper.pid, "start": sessions.fingerprint(sleeper.pid)})
        second = str(uuid.uuid4())
        refused = self.launch(second, thread=thread, wait=False)
        _, error = refused.communicate(timeout=15)
        self.assertEqual(refused.returncode, sessions.LAUNCH_REFUSED, error)
        self.assertIn(sessions.PHONE_RUNNING, error.decode())
        self.assertEqual(self.calls_of("tui"), [])
        with self.assertRaisesRegex(ValueError, "已有消息正在处理"):
            self.send(second, thread)
        with self.assertRaisesRegex(ValueError, "仍在处理"):
            sessions.purge({"thread": thread, "account": "default", "workspace": str(self.project), "connection": second})

    def test_an_unreadable_state_is_never_overwritten_while_something_may_run(self):
        thread, path = self.rollout(self.turn_rows("corrupt", "ok"))
        connection = str(uuid.uuid4())
        folder = sessions.ROOT / connection
        folder.mkdir(parents=True)
        (folder / "state.json").write_text("{not json")
        proc = self.home / "proc"
        (proc / "4000001/fd").mkdir(parents=True)
        (proc / "4000001/cmdline").write_bytes(b"codex\0exec\0resume\0" + thread.encode() + b"\0")
        os.symlink(path, proc / "4000001/fd/7")
        (proc / "stat").write_text("cpu 1 2 3\nbtime %d\n" % (time.time() - 3600))
        request = {"connection": connection, "lease": str(uuid.uuid4()), "thread": thread, "account": "default", "subject": "",
                   "cwd": str(self.project), "additional_directories": []}
        sessions.probe({})

        def refused(request):
            started = AssertionError("the terminal started")
            with patch.object(sessions, "PROC", proc), patch.object(sessions.os, "execve", side_effect=started):
                with self.assertRaisesRegex(ValueError, "状态无法读取"):
                    sessions.tui_launch(request)
            self.assertEqual((folder / "state.json").read_text(), "{not json")

        # A process holds the thread: refused, the file stays as it is.
        refused(request)
        # A phone turn's supervisor of this connection runs: refused, also for
        # a file from before this boot (the boot time moves when the clock is
        # stepped).
        shutil.rmtree(proc / "4000001")
        (proc / "4000002").mkdir()
        (proc / "4000002/cmdline").write_bytes(b"/usr/bin/python3\0-I\0/home/x/codex.py\0supervise\0" + connection.encode() + b"\0")
        refused(request)
        stamp = time.time() - 7200
        os.utime(folder / "state.json", (stamp, stamp))
        refused(request)
        # A new session (no thread) from this boot: what it recorded is
        # unknown, refused.
        shutil.rmtree(proc / "4000002")
        os.utime(folder / "state.json", None)
        refused({**request, "thread": ""})
        # Nothing runs and the thread is free: set aside, not overwritten, and
        # the terminal starts.
        launched = []
        cwd, umask = os.getcwd(), os.umask(0o077)
        os.umask(umask)
        self.addCleanup(os.chdir, cwd)
        self.addCleanup(os.umask, umask)
        with patch.object(sessions, "PROC", proc), patch.object(sessions.os, "execve", side_effect=lambda *args: launched.append(args) or (_ for _ in ()).throw(SystemExit(0))):
            with self.assertRaises(SystemExit):
                sessions.tui_launch(request)
        self.assertEqual(len(launched), 1)
        self.assertEqual([item.read_text() for item in folder.glob("state.json.unreadable-*")], ["{not json"])
        self.assertEqual(self.state(connection)["lease"]["thread"], thread)

    def test_admission_is_one_at_a_time_across_connections(self):
        # Two records of one thread: a desktop open on one connection and a
        # phone send on another each check the other's run or lease and
        # record their own under the admission lock, never side by side.
        thread, _ = self.rollout(self.turn_rows("shared", "ok"))
        import fcntl
        sessions.ROOT.mkdir(parents=True, exist_ok=True)
        holder = (sessions.ROOT / "admission.lock").open("a")
        self.addCleanup(holder.close)
        fcntl.flock(holder, fcntl.LOCK_EX)
        desktop, phone = str(uuid.uuid4()), str(uuid.uuid4())
        os.environ["FAKE_CODEX_MODE"] = "sleep"
        launch = self.launch(desktop, thread=thread, wait=False)
        sender = subprocess.Popen([sys.executable, "-I", str(source), "send", json.dumps({"connection": phone, "request_id": str(uuid.uuid4()), "text": "x",
                                   "account": "default", "thread": thread, "cwd": str(self.project)})], env=self.env_now(), stdin=subprocess.DEVNULL,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.finish, sender)
        time.sleep(1)
        self.assertEqual((self.calls_of("tui"), self.calls_of("exec")), ([], []))
        self.assertIsNone(sender.poll())
        self.assertEqual((self.state(desktop), self.state(phone)), ({}, {}))
        fcntl.flock(holder, fcntl.LOCK_UN)
        out, _ = sender.communicate(timeout=15)
        deadline = time.monotonic() + 15
        while not self.calls_of("tui") and launch.poll() is None and time.monotonic() < deadline:
            time.sleep(0.02)
        admitted = [bool(self.calls_of("tui")), b'"ok": true' in out]
        self.assertEqual(sum(admitted), 1, (admitted, out))

    def test_help_options_match_whole_tokens(self):
        # A CLI whose help lists look-alike options only: none is taken for
        # the real one (a wrong -C makes the TUI exit at once).
        fake = self.home / "bin" / "codex"
        text = fake.read_text()
        for before, after in (('level in ("all", "old")', 'level in ("all", "old", "lookalike")'),
                              ('if flags else ""', 'if flags else ("      --cdp-port <PORT>\\n      --add-dirs <DIRS>\\n      --no-daemonize\\n      --force-color\\n" if level == "lookalike" else "")')):
            self.assertIn(before, text)
            text = text.replace(before, after, 1)
        fake.write_text(text)
        os.environ["FAKE_CODEX_FLAGS"] = "lookalike"
        result = sessions.probe({})
        self.assertEqual({key: result[key] for key in ("complete", "resume", "cd", "add_dir", "no_daemon", "resume_cd", "resume_add_dir", "resume_no_daemon")},
                         {"complete": True, "resume": True, "cd": False, "add_dir": False, "no_daemon": False, "resume_cd": False,
                          "resume_add_dir": False, "resume_no_daemon": False})
        top = "Codex CLI\n\nCommands:\n  resume  Resume\n\nOptions:\n      --cdp-port <PORT>\n      --add-dirs <DIRS>\n      --no-daemonize\n"
        self.assertFalse(sessions.has_option(top, "--cd"))
        self.assertFalse(sessions.has_option(top, "--add-dir"))
        self.assertFalse(sessions.has_option(top, "--no-daemon"))
        self.assertFalse(sessions.has_option("Usage: codex delete [OPTIONS] <SESSION>\n      --force-color\n", "--force"))
        for text, name in (("  -C, --cd <DIR>  Tell the agent", "--cd"), ("      --add-dir <DIR>", "--add-dir"), ("      --no-daemon\n", "--no-daemon"),
                           ("      --cd=<DIR>", "--cd"), ("Usage: codex delete [--force] <SESSION>", "--force")):
            self.assertTrue(sessions.has_option(text, name), (text, name))

    def test_probe_timeouts_are_not_cached_and_purge_keeps_to_its_budget(self):
        fake = self.home / "bin" / "codex"
        source_text = fake.read_text()
        fake.write_text(source_text.replace('if args[-1:] == ["--help"]:', 'if args[-1:] == ["--help"] and args[:1] != ["--help"]:\n    time.sleep(float(os.environ.get("FAKE_CODEX_SLOW_HELP", "0")))\nif args[-1:] == ["--help"]:', 1))
        os.environ["FAKE_CODEX_SLOW_HELP"] = "2"
        # A sub-command's help that times out is not taken for "unsupported".
        with patch.object(sessions, "PROBE_TIMEOUT", 0.5):
            result = sessions.probe({})
        self.assertFalse(result["complete"])
        self.assertFalse((sessions.ROOT / "probe.json").exists())
        # Purge finishes within its budget although the probe is slow; the
        # rollout goes all the same.
        thread, path = self.rollout(self.turn_rows("remove me", "ok"))
        started = time.monotonic()
        with patch.object(sessions, "PURGE_BUDGET", 1.0, create=True):
            self.assertEqual(sessions.purge({"thread": thread, "account": "default", "workspace": str(self.project)}), {"ok": True})
        self.assertLess(time.monotonic() - started, 3)
        self.assertFalse(path.exists())

    def test_newer_rollout_names(self):
        thread, path = self.rollout(self.turn_rows("suffixed", "ok"))
        suffixed = path.with_name(path.name[:-len(".jsonl")] + "_" + str(uuid.uuid4()) + ".jsonl")
        path.rename(suffixed)
        listed = sessions.discover({"accounts": ["default"], "workspaces": [str(self.project)]})
        self.assertEqual([item["thread_id"] for item in listed], [thread])
        self.assertEqual(sessions.view({"thread": thread, "account": "default"})["turns"][0]["prompt"], "suffixed")
        squeezed, cold = self.rollout(self.turn_rows("compressed", "ok"))
        cold.rename(cold.with_name(cold.name + ".zst"))
        result = sessions.view({"thread": squeezed, "account": "default"})
        self.assertEqual((result["connection_notice"], result["can_send"], result["history_compressed"]), (sessions.COMPRESSED_NOTICE, False, True))
        # The desktop TUI can still resume what this helper cannot read.
        process = self.launch(str(uuid.uuid4()), thread=squeezed)
        tui = [row for row in self.calls_of("tui") if row["pid"] == process.pid][0]
        self.assertEqual(tui["argv"][-1], squeezed)
        self.assertFalse(sessions.view({"thread": thread, "account": "default"})["history_compressed"])


if __name__ == "__main__":
    unittest.main()
