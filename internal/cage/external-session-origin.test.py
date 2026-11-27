#!/usr/bin/python3
"""Exercise the guest's read-only external session origin contract."""
import base64
import errno
import hashlib
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch
import uuid


RUNTIME = Path(__file__).resolve().with_name("agent-runtime.py")
ORIGIN = runpy.run_path(str(RUNTIME))["external_session_origin"]
ACCOUNT = "a" * 24
OTHER_ACCOUNT = "b" * 24
THREAD = "91801bed-cf47-4adc-a520-63839a32860b"
OTHER_THREAD = "056e29c7-5c3e-45e3-9290-6660d907f31a"
BOOT = "e0976dde-f8a8-47ad-94a6-28c0f0cb024d"
TENANT_SUBJECT = hashlib.sha256(b"fixture@example.test\nfixture-tenant").hexdigest()


def as_json(value):
    return json.dumps(value, separators=(",", ":"))


def private_json(path, value):
    path.write_text(as_json(value))
    path.chmod(0o600)


class ExternalSessionOriginTests(unittest.TestCase):
    def setUp(self):
        fixture = tempfile.TemporaryDirectory()
        self.addCleanup(fixture.cleanup)
        self.root = Path(fixture.name).resolve()
        self.workspace = self.root / "original-workspace"
        self.workspace.mkdir()
        self.home = self.root / "home"
        self.accounts = self.home / ".local/share/cage/codex-accounts"
        self.profile = self.accounts / ACCOUNT / "home"
        self.profile.mkdir(parents=True, mode=0o700)
        self.proc = self.root / "proc"
        self.proc.mkdir()
        self.boot_file = self.proc / "sys/kernel/random/boot_id"
        self.boot_file.parent.mkdir(parents=True)
        self.boot_file.write_text(BOOT + "\n")
        self.exe = self.root / "codex (deleted)"
        self.exe.write_text("#!/bin/sh\nexit 99\n")
        self.exe.chmod(0o700)
        self.request = {
            "terminal_handle": "term_" + uuid.uuid4().hex,
            "provider": "codex",
            "working_directory": str(self.workspace),
            "tab_id": "tab-previous",
            "worktree_id": "repo:fixture::" + str(self.workspace),
        }
        self.environment = {
            "ORCA_TERMINAL_HANDLE": self.request["terminal_handle"],
            "ORCA_TAB_ID": self.request["tab_id"],
            "ORCA_WORKTREE_ID": self.request["worktree_id"],
            "CODEX_HOME": str(self.profile),
            "IGNORED_SECRET": "synthetic-environment-secret",
        }
        self.roster = [{"id": ACCOUNT, "subject": TENANT_SUBJECT}]
        self.publish(self.roster)
        self.auth()

    def publish(self, roster):
        private_json(self.accounts / "launch.json", {"history_accounts": roster, "accounts": []})

    def auth(self, *, tenant="fixture-tenant", mode="chatgpt"):
        claims = {"email": "Fixture@Example.test", "https://api.openai.com/auth": {"chatgpt_account_id": tenant}}
        encoded = base64.urlsafe_b64encode(as_json(claims).encode()).decode().rstrip("=")
        private_json(self.profile / "auth.json", {"auth_mode": mode, "tokens": {
            "access_token": "synthetic-private-access-token", "id_token": f"header.{encoded}.signature",
        }})

    def rollout(self, thread=THREAD, *, source="cli", profile=None, filename=None, **meta):
        home = profile or self.profile
        payload = {"id": thread, "source": source, "cwd": str(self.workspace), **meta}
        day = home / "sessions/2026/10/02"
        day.mkdir(parents=True, exist_ok=True)
        path = day / (filename or f"rollout-2026-10-02T22-06-17-{thread}.jsonl")
        # The helper never reads messages, headers, instructions, or a second record.
        path.write_text(as_json({"type": "session_meta", "payload": payload}) + "\n\0synthetic-prompt-secret\n")
        return path

    def process(self, *rollouts, pid=101, executable=None, cwd=None, env=None, state="S"):
        path = self.proc / str(pid)
        (path / "fd").mkdir(parents=True)
        (path / "comm").write_text("codex\n")
        (path / "exe").symlink_to(executable or self.exe)
        (path / "cwd").symlink_to(cwd or self.workspace, target_is_directory=True)
        environ = {**self.environment, **(env or {})}
        (path / "environ").write_bytes(b"".join(os.fsencode(f"{key}={value}") + b"\0" for key, value in environ.items()))
        fields = [state, "1", *(["0"] * 17), str(pid * 10)]
        (path / "stat").write_text(f"{pid} (codex) " + " ".join(fields) + "\n")
        for index, rollout in enumerate(rollouts, 4):
            (path / "fd" / str(index)).symlink_to(rollout)
        return path

    def observe(self, **override):
        return ORIGIN({**self.request, **override}, proc_root=self.proc, home=self.home)

    def test_root_and_three_subagents_with_deleted_executable(self):
        root = self.rollout(base_instructions="synthetic-meta-secret")
        children = [
            self.rollout(thread, source={"subagent": {"thread_spawn": {"parent_thread_id": THREAD, "depth": 1}}})
            for thread in (OTHER_THREAD, str(uuid.uuid4()), str(uuid.uuid4()))
        ]
        self.process(root, root, *children)
        result = self.observe()
        self.assertEqual(result, {"state": "found", "thread_id": THREAD, "account_id": ACCOUNT,
                                  "subject": TENANT_SUBJECT, "boot_id": BOOT})
        for forbidden in ("secret", str(self.workspace), self.request["terminal_handle"]):
            self.assertNotIn(forbidden, as_json(result))

    def test_two_roots_are_ambiguous(self):
        self.process(self.rollout(), self.rollout(OTHER_THREAD))
        self.assertEqual(self.observe(), {"state": "ambiguous"})

    def test_two_live_processes_are_ambiguous(self):
        self.process(self.rollout())
        self.process(self.rollout(OTHER_THREAD), pid=102)
        self.assertEqual(self.observe(), {"state": "ambiguous"})

    def test_no_root_with_only_subagents(self):
        child = self.rollout(OTHER_THREAD, source={"subagent": {"thread_spawn": {"parent_thread_id": THREAD}}})
        self.process(child)
        self.assertEqual(self.observe(), {"state": "unavailable"})

    def test_binding_is_exact_and_rejects_provider_overrides(self):
        self.process(self.rollout(), env={"OPENAI_BASE_URL": "https://proxy.example.test"})
        self.assertEqual(self.observe(), {"state": "unavailable"})
        shutil.rmtree(self.proc / "101")
        self.process(self.rollout())
        for mismatch in ({"terminal_handle": self.request["terminal_handle"] + "x"}, {"terminal_handle": "term_;fake"},
                         {"tab_id": "different-tab"}, {"worktree_id": "different-worktree"},
                         {"working_directory": str(self.root)}, {"working_directory": "relative/path"},
                         {"provider": "bogus"}, {"pid": 101}):
            with self.subTest(mismatch=mismatch):
                self.assertEqual(self.observe(**mismatch), {"state": "unavailable"})
        self.assertEqual(self.observe(tab_id="", worktree_id="")["state"], "found")

    def test_launcher_root_needs_full_orca_binding(self):
        self.process(self.rollout(), cwd=Path("/"))
        self.assertEqual(self.observe()["thread_id"], THREAD)
        for override in ({"tab_id": ""}, {"worktree_id": ""}, {"tab_id": "other-tab"},
                         {"worktree_id": "other-worktree"}):
            self.assertEqual(self.observe(**override), {"state": "unavailable"})

    def test_disconnected_launcher_root_uses_current_workspace_proof(self):
        children = [self.rollout(str(uuid.uuid4()), source={"subagent": {"thread_spawn": {"parent_thread_id": THREAD}}})
                    for _ in range(3)]
        proc = self.process(self.rollout(), *children, cwd=Path("/"))
        real_stat = os.stat

        def disconnected(path, *args, **kwargs):
            if path == "cwd" and kwargs.get("dir_fd") is not None:
                raise OSError(errno.ENOTCONN, "old mount disconnected")
            return real_stat(path, *args, **kwargs)

        with patch.object(os, "stat", side_effect=disconnected):
            self.assertEqual(self.observe()["thread_id"], THREAD)
            self.assertEqual(self.observe(tab_id=""), {"state": "unavailable"})
            self.rollout(cwd=str(self.root))
            self.assertEqual(self.observe(), {"state": "unavailable"})
            self.rollout()
            (proc / "cwd").unlink()
            (proc / "cwd").symlink_to(self.root, target_is_directory=True)
            self.assertEqual(self.observe(), {"state": "unavailable"})

    def test_launcher_root_rejects_other_errors_and_changes_in_cwd_evidence(self):
        self.process(self.rollout(), cwd=Path("/"))
        real_stat = os.stat
        errors = iter([errno.EACCES, errno.ENOTCONN, None])

        def changing(path, *args, **kwargs):
            if path == "cwd" and kwargs.get("dir_fd") is not None:
                code = next(errors)
                if code is not None:
                    raise OSError(code, "cwd observation changed")
            return real_stat(path, *args, **kwargs)

        with patch.object(os, "stat", side_effect=changing):
            self.assertEqual(self.observe(), {"state": "unavailable"})  # Permission error.
            self.assertEqual(self.observe(), {"state": "unavailable"})  # ENOTCONN to attached /.

    def test_launcher_root_refuses_replaced_workspace(self):
        self.process(self.rollout(), cwd=Path("/"))
        read_subject = ORIGIN.__globals__["_origin_subject"]

        def replace_workspace(*args):
            subject = read_subject(*args)
            self.workspace.rename(self.root / "former-workspace")
            self.workspace.mkdir()
            return subject

        with patch.dict(ORIGIN.__globals__, {"_origin_subject": replace_workspace}):
            self.assertEqual(self.observe(), {"state": "unavailable"})

    def test_lookalike_process_and_foreign_rollout_are_refused(self):
        fake = self.root / "unrelated-process"
        fake.write_text("unrelated")
        fake.chmod(0o700)
        self.process(self.rollout(), executable=fake)
        self.assertEqual(self.observe(), {"state": "unavailable"})
        shutil.rmtree(self.proc / "101")
        foreign = self.rollout(OTHER_THREAD, profile=self.accounts / OTHER_ACCOUNT / "home")
        self.process(self.rollout(), foreign)
        self.assertEqual(self.observe(), {"state": "unavailable"})

    def test_bad_rollout_metadata_and_wrong_workspace_are_refused(self):
        for changes in ({"cwd": str(self.root)}, {"parent_thread_id": OTHER_THREAD},
                        {"source": "exec"}, {"model_provider": "proxy"},
                        {"filename": f"rollout-2026-10-02T22-06-17-{OTHER_THREAD}.jsonl"}):
            with self.subTest(changes=changes):
                self.process(self.rollout(**changes))
                self.assertEqual(self.observe(), {"state": "unavailable"})
                shutil.rmtree(self.proc / "101")

    def test_roster_membership_and_live_profile_identity_are_required(self):
        self.process(self.rollout())
        for roster in ([], [*self.roster, *self.roster],
                       [*self.roster, {"id": OTHER_ACCOUNT, "subject": TENANT_SUBJECT}]):
            self.publish(roster)
            self.assertEqual(self.observe(), {"state": "unavailable"})
        self.publish(self.roster)
        self.auth(tenant="other-tenant")
        self.assertEqual(self.observe(), {"state": "unavailable"})
        self.auth(mode="apiKey")
        self.assertEqual(self.observe(), {"state": "unavailable"})
        self.auth()
        (self.profile / "auth.json").chmod(0o644)
        self.assertEqual(self.observe(), {"state": "unavailable"})

    def test_unreadable_disappeared_and_zombie_processes_are_ignored(self):
        self.process(self.rollout(), pid=101)
        self.process(self.rollout(), pid=102, state="Z")
        real_read = ORIGIN.__globals__["_origin_read"]

        def deny_env(path, *args, **kwargs):
            if path == "environ":
                raise PermissionError()
            return real_read(path, *args, **kwargs)

        with patch.dict(ORIGIN.__globals__, {"_origin_read": deny_env}):
            self.assertEqual(self.observe(), {"state": "unavailable"})
        with patch.dict(ORIGIN.__globals__, {"_origin_start_time": Mock(side_effect=(b"1", b"2"))}):
            self.assertEqual(self.observe(), {"state": "unavailable"})
        shutil.rmtree(self.proc / "101")
        self.assertEqual(self.observe(), {"state": "unavailable"})

    def test_claude_has_no_verified_origin_and_cli_returns_only_state(self):
        result = subprocess.run([sys.executable, "-I", str(RUNTIME), "external-session-origin"],
                                input=as_json({**self.request, "provider": "claude"}), capture_output=True, text=True,
                                env={"HOME": str(self.home), "PATH": os.defpath}, check=True, timeout=5)
        self.assertEqual(json.loads(result.stdout), {"state": "unsupported"})
        self.assertEqual(result.stderr, "")

    def test_boot_observation_uses_kernel_identity_without_scanning_processes(self):
        self.process(self.rollout())
        request = {"provider": "codex", "working_directory": str(self.workspace), "boot_only": True}
        fail = Mock(side_effect=AssertionError("boot observation opened a process"))
        with patch.dict(ORIGIN.__globals__, {"_origin_process": fail}):
            observed = ORIGIN(request, home=self.home, proc_root=self.proc)
            self.assertEqual(observed, {"state": "boot", "boot_id": BOOT})
            self.assertEqual(ORIGIN({**request, "working_directory": "/does-not-exist"},
                                    home=self.home, proc_root=self.proc), {"state": "unavailable"})
        self.boot_file.write_text("not-a-boot-uuid\n")
        self.assertEqual(ORIGIN(request, home=self.home, proc_root=self.proc), {"state": "unavailable"})
        self.assertEqual(self.observe(), {"state": "unavailable"})

    @unittest.skipUnless(sys.platform == "linux", "Linux procfs")
    def test_real_proc_keeps_unlinked_exe_identity_with_launcher_root(self):
        binary = self.root / "codex"
        shutil.copy2(shutil.which("cat", path=os.defpath), binary)
        with self.rollout().open("rb") as opened:
            process = subprocess.Popen([binary], cwd="/", env=self.environment, stdin=subprocess.PIPE,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, pass_fds=(opened.fileno(),))
            try:
                binary.unlink()
                observed = subprocess.run([sys.executable, "-I", str(RUNTIME), "external-session-origin"],
                                          input=as_json(self.request), capture_output=True, text=True,
                                          env={"HOME": str(self.home), "PATH": os.defpath}, check=True, timeout=5)
                result = json.loads(observed.stdout)
            finally:
                process.communicate(timeout=5)
        self.assertEqual(result, {"state": "found", "thread_id": THREAD, "account_id": ACCOUNT, "subject": TENANT_SUBJECT,
                                  "boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip()})


if __name__ == "__main__":
    unittest.main()
