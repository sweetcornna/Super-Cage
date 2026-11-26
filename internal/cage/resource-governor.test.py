import importlib.util
import os
from pathlib import Path
import stat
import sys
import tempfile
import time
import unittest

spec = importlib.util.spec_from_file_location('resource_governor', Path(__file__).with_name('resource-governor.py'))
governor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(governor)

MIB = 1 << 20
GIB = 1 << 30


REAL_GETPWNAM = governor.pwd.getpwnam
PROC_MOUNTS = governor.MOUNTS


class Guest:
    """A fake guest: cgroup root, /run directory, proc files, accounts (cage is
    uid 1000, cage-browser 1001, named by the unit's environment) and a
    systemctl that records its calls and writes runtime drop-ins as systemd
    does."""

    def __init__(self, root, total=3 * GIB, recursiveprot=True):
        self.root = Path(root)
        self.calls = self.root / 'calls'
        for name in ('cgroup', 'run/in', 'proc', 'control'):
            (self.root / name).mkdir(parents=True, exist_ok=True)
        (self.root / 'cgroup' / 'cgroup.controllers').write_text('cpu memory\n')
        for unit, current, pressure in (('system.slice', 400 * MIB, '0.50'), ('user.slice', 513 * MIB, '0.00'), ('cage.slice/cage-browser.slice', 1700 * MIB, '31.00')):
            directory = self.root / 'cgroup' / unit
            directory.mkdir(parents=True)
            (directory / 'memory.current').write_text('%d\n' % current)
            (directory / 'memory.low').write_text('0\n')
            (directory / 'memory.high').write_text('max\n')
            (directory / 'memory.max').write_text('max\n')
            (directory / 'memory.pressure').write_text('some avg10=%s avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n' % pressure)
        (self.root / 'proc' / 'meminfo').write_text('MemTotal:       %d kB\nMemFree: 1 kB\n' % (total // 1024))
        (self.root / 'proc' / 'uptime').write_text('123.45 100.00\n')
        options = 'rw,nosuid,nodev,noexec,relatime,nsdelegate' + (',memory_recursiveprot' if recursiveprot else '')
        (self.root / 'proc' / 'self').mkdir()
        (self.root / 'proc' / 'self' / 'mounts').write_text('proc /proc proc rw 0 0\ncgroup2 %s cgroup2 %s 0 0\n' % (self.root / 'cgroup', options))
        (self.root / 'proc' / 'mounts').symlink_to('self/mounts')
        recorder = self.root / 'systemctl'
        recorder.write_text('#!/bin/sh\nprintf "%%s\\n" "$*" >> %s\nmkdir -p "%s/$3.d" && printf "[Slice]\\n%%s\\n" "$4" > "%s/$3.d/50-${4%%%%=*}.conf"\n' % (
            self.calls, self.root / 'control', self.root / 'control'))
        recorder.chmod(0o755)
        governor.CGROUP = str(self.root / 'cgroup')
        governor.RUN = str(self.root / 'run')
        governor.MEMINFO = str(self.root / 'proc' / 'meminfo')
        governor.UPTIME = str(self.root / 'proc' / 'uptime')
        governor.MOUNTS = str(self.root / PROC_MOUNTS.lstrip('/'))
        governor.SYSTEMCTL = [str(recorder)]
        governor.CONTROL = str(self.root / 'control')
        self.accounts({'cage': 1000, 'cage-browser': 1001}, 'cage', 'cage-browser')

    def accounts(self, uids, session, browser):
        """The guest's accounts (name to uid) and the names the unit gives."""
        def getpwnam(name):
            if name in uids:
                return type('Entry', (), {'pw_uid': uids[name]})()
            raise KeyError(name)
        governor.pwd.getpwnam = getpwnam
        for variable, name in (('CAGE_SESSION_USER', session), ('CAGE_BROWSER_USER', browser)):
            if name is None:
                os.environ.pop(variable, None)
            else:
                os.environ[variable] = name

    def dropins(self):
        return sorted(str(path.relative_to(self.root / 'control')) for path in (self.root / 'control').glob('*/*'))

    def user_slice(self, uid, current, pressure):
        directory = self.root / 'cgroup' / 'user.slice' / ('user-%d.slice' % uid)
        directory.mkdir()
        (directory / 'memory.current').write_text('%d\n' % current)
        (directory / 'memory.pressure').write_text('some avg10=%s avg60=0.00 avg300=0.00 total=1\n' % pressure)

    def limits(self, text, age=0):
        path = self.root / 'run' / 'in' / 'limits'
        path.write_text(text)
        stamp = time.time() - age
        os.utime(path, (stamp, stamp))

    def called(self):
        if not self.calls.exists():
            return []
        lines = self.calls.read_text().splitlines()
        self.calls.unlink()
        return lines


class GovernorTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.guest = Guest(self.directory.name)

    def tearDown(self):
        governor.pwd.getpwnam = REAL_GETPWNAM
        for variable in ('CAGE_SESSION_USER', 'CAGE_BROWSER_USER'):
            os.environ.pop(variable, None)
        self.directory.cleanup()

    def test_plan_matches_the_host_agent(self):
        # TestGuestClassLimits in guest_allocation_test.go: the evidence guest.
        self.assertEqual(governor.plan(3 * GIB, 400 * MIB, 513 * MIB),
                         {'control_low': 576 * MIB, 'sessions_low': 832 * MIB, 'browser_high': 1600 * MIB, 'browser_max': 1856 * MIB})
        self.assertGreaterEqual(governor.plan(6 * GIB, 400 * MIB, 513 * MIB)['browser_high'], 4 * GIB)

    def test_host_limits_are_clamped(self):
        self.guest.limits('version 1\ncontrol_low %d\nsessions_low %d\nsessions_high 1\nbrowser_high 1\nbrowser_max 2\n' % (8 * GIB, 8 * GIB))
        limits = governor.received(time.time(), 3 * GIB)
        self.assertEqual(limits, {'control_low': 768 * MIB, 'sessions_low': 1536 * MIB, 'browser_high': 768 * MIB, 'browser_max': 1024 * MIB})
        # Stale, unversioned, incomplete or not a regular file: not used.
        self.guest.limits('version 1\ncontrol_low 1\nsessions_low 1\nbrowser_high 1\nbrowser_max 1\n', age=30)
        self.assertIsNone(governor.received(time.time(), 3 * GIB))
        self.guest.limits('control_low 1\nsessions_low 1\nbrowser_high 1\nbrowser_max 1\n')
        self.assertIsNone(governor.received(time.time(), 3 * GIB))
        self.guest.limits('version 1\ncontrol_low 1\n')
        self.assertIsNone(governor.received(time.time(), 3 * GIB))
        path = Path(governor.RUN) / 'in' / 'limits'
        path.unlink()
        target = Path(self.directory.name) / 'elsewhere'
        target.write_text('version 1\ncontrol_low 1\nsessions_low 1\nbrowser_high 1\nbrowser_max 1\n')
        path.symlink_to(target)
        self.assertIsNone(governor.received(time.time(), 3 * GIB))

    def test_applies_host_limits_with_hysteresis_and_reports(self):
        self.guest.limits('version 1\ncontrol_low %d\nsessions_low %d\nbrowser_high %d\nbrowser_max %d\n' % (576 * MIB, 832 * MIB, 4 * GIB, 4 * GIB + 384 * MIB))
        g = governor.Governor()
        g.once()
        self.assertEqual(sorted(self.guest.called()), sorted([
            'set-property --runtime cage-browser.slice MemoryHigh=%d' % (4 * GIB),
            'set-property --runtime cage-browser.slice MemoryMax=%d' % (4 * GIB + 384 * MIB),
            'set-property --runtime system.slice MemoryLow=%d' % (576 * MIB),
            'set-property --runtime user.slice MemoryLow=%d' % (832 * MIB)]))
        state = (Path(governor.RUN) / 'state').read_text().splitlines()
        self.assertEqual(state[0], 'governor 1 123.5')
        self.assertIn('class control %d 0.50 0 0 0' % (400 * MIB), state)
        self.assertIn('class browser %d 31.00 0 0 0' % (1700 * MIB), state)
        self.assertEqual(stat.S_IMODE(os.stat(Path(governor.RUN) / 'state').st_mode), 0o644)
        # Less than 64 MiB of change: nothing is reapplied.
        self.guest.limits('version 1\ncontrol_low %d\nsessions_low %d\nbrowser_high %d\nbrowser_max %d\n' % (600 * MIB, 832 * MIB, 4 * GIB + 32 * MIB, 4 * GIB + 384 * MIB))
        g.once()
        self.assertEqual(self.guest.called(), [])

    def test_without_host_limits_the_guest_size_decides(self):
        g = governor.Governor()
        g.once()
        calls = self.guest.called()
        self.assertIn('set-property --runtime cage-browser.slice MemoryHigh=%d' % (1600 * MIB), calls)
        self.assertIn('set-property --runtime cage-browser.slice MemoryMax=%d' % (1856 * MIB), calls)

    def test_no_protection_without_recursive_protection(self):
        self.directory.cleanup()
        self.directory = tempfile.TemporaryDirectory()
        self.guest = Guest(self.directory.name, recursiveprot=False)
        governor.Governor().once()
        calls = self.guest.called()
        self.assertEqual(len(calls), 2)
        self.assertFalse(any('MemoryLow' in call for call in calls))

    def test_reset_and_unsupported_hierarchy(self):
        governor.Governor().once()
        self.guest.called()
        governor.reset()
        self.assertEqual(sorted(self.guest.called()), sorted([
            'set-property --runtime cage-browser.slice MemoryHigh=infinity',
            'set-property --runtime cage-browser.slice MemoryMax=infinity',
            'set-property --runtime system.slice MemoryLow=0',
            'set-property --runtime user.slice MemoryLow=0']))
        self.assertFalse((Path(governor.RUN) / 'state').exists())
        # Nothing it set is left behind: no runtime drop-in, no runtime directory.
        self.assertEqual(self.guest.dropins(), [])
        self.assertFalse(Path(governor.RUN).exists())
        (Path(governor.RUN) / 'in').mkdir(parents=True)
        (Path(governor.CGROUP) / 'cgroup.controllers').unlink()
        self.assertEqual(governor.main(['governor']), 0)
        self.assertEqual(self.guest.called(), [])

    def test_ubuntu_snap_browser_counts_as_browser(self):
        # snap Chromium runs under the browser user's slice in user.slice.
        self.guest.user_slice(1000, 400 * MIB, '12.00')
        self.guest.user_slice(1001, 1200 * MIB, '40.00')
        (Path(governor.CGROUP) / 'user.slice' / 'memory.current').write_text('%d\n' % (1600 * MIB))
        found = governor.classes('user.slice/user-1000.slice', 'user.slice/user-1001.slice')
        self.assertEqual(found['sessions']['current'], 400 * MIB)
        self.assertEqual(found['sessions']['stall'], 12.0)
        self.assertEqual(found['browser']['current'], 2900 * MIB)
        self.assertEqual(found['browser']['stall'], 40.0)
        governor.Governor().once()
        calls = self.guest.called()
        for unit in ('cage-browser.slice', 'user-1001.slice'):
            self.assertTrue(any(call.startswith('set-property --runtime %s MemoryHigh=' % unit) for call in calls), calls)
            self.assertTrue(any(call.startswith('set-property --runtime %s MemoryMax=' % unit) for call in calls), calls)
        for unit in ('user.slice', 'user-1000.slice'):
            self.assertTrue(any(call.startswith('set-property --runtime %s MemoryLow=' % unit) for call in calls), calls)
        self.assertFalse(any(call.startswith('set-property --runtime user-1001.slice MemoryLow') for call in calls), calls)
        governor.reset()
        self.assertIn('set-property --runtime user-1001.slice MemoryMax=infinity', self.guest.called())

    def test_unresolved_accounts_protect_and_count_nothing_they_cannot_name(self):
        # A guest whose accounts are not the ones the unit names (or a unit
        # without them): the browser user's slice cannot be told from the
        # sessions', so user.slice is not protected (a protection set before
        # is taken back), no user slice is limited and no class is reported.
        self.guest.user_slice(1000, 400 * MIB, '12.00')
        self.guest.user_slice(1001, 1200 * MIB, '40.00')
        for uids, session, browser in (({'alex': 1000, 'jordan': 1001}, 'cage', 'cage-browser'), ({'cage': 1000, 'cage-browser': 1001}, None, None),
                                       ({'cage': 1000, 'cage-browser': 1001}, 'cage', 'cage'), ({'cage': 1000, 'cage-browser': 1001}, 'cage; rm', 'cage-browser')):
            self.guest.accounts(uids, session, browser)
            governor.Governor().once()
            calls = self.guest.called()
            self.assertIn('set-property --runtime user.slice MemoryLow=0', calls)
            self.assertTrue(any(call.startswith('set-property --runtime system.slice MemoryLow=') and not call.endswith('=0') for call in calls), calls)
            self.assertTrue(any(call.startswith('set-property --runtime cage-browser.slice MemoryHigh=') for call in calls), calls)
            self.assertFalse(any('user-' in call for call in calls), calls)
            state = (Path(governor.RUN) / 'state').read_text().splitlines()
            self.assertEqual(state[0], 'governor 1 123.5')
            self.assertEqual(state[1].split()[:2], ['accounts', 'unresolved'])
            self.assertFalse(any(line.startswith('class ') for line in state), state)

    def test_a_browser_user_without_a_slice_is_resolved(self):
        # Debian: the browser runs in cage-browser.slice and its user has no
        # slice of its own; the sessions are protected and reported.
        self.guest.user_slice(1000, 400 * MIB, '12.00')
        governor.Governor().once()
        calls = self.guest.called()
        for unit in ('user.slice', 'user-1000.slice'):
            self.assertTrue(any(call.startswith('set-property --runtime %s MemoryLow=' % unit) and not call.endswith('=0') for call in calls), calls)
        state = (Path(governor.RUN) / 'state').read_text()
        self.assertIn('class sessions ', state)
        self.assertNotIn('accounts', state)

    def test_reset_undoes_slices_set_under_other_accounts(self):
        # A browser user's slice limited while the accounts were other ones
        # (an older governor, a renamed account) is found by its drop-in.
        self.guest.user_slice(1000, 400 * MIB, '0.00')
        self.guest.user_slice(1001, 1200 * MIB, '0.00')
        governor.Governor().once()
        self.guest.called()
        self.assertIn('user-1001.slice.d/50-MemoryMax.conf', self.guest.dropins())
        self.guest.accounts({'alex': 1000, 'jordan': 1001}, 'alex', 'jordan-new')
        governor.reset()
        calls = self.guest.called()
        self.assertIn('set-property --runtime user-1001.slice MemoryHigh=infinity', calls)
        self.assertIn('set-property --runtime user-1001.slice MemoryMax=infinity', calls)
        self.assertIn('set-property --runtime user-1000.slice MemoryLow=0', calls)
        self.assertEqual(self.guest.dropins(), [])

    def test_failed_steps_do_not_stop_the_governor(self):
        governor.SYSTEMCTL = ['/nonexistent/systemctl']
        g = governor.Governor()
        g.once()
        self.assertTrue((Path(governor.RUN) / 'state').exists())


if __name__ == '__main__':
    unittest.main()
