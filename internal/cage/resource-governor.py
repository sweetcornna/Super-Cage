#!/usr/bin/python3
"""Claude Cage memory priorities inside the guest.

resource-allocation.md, level C. Three classes:
the control plane (system.slice: sshd, the guest agent, the display, the
editor, this governor), the sessions (user.slice: terminals, claude, codex,
and also the host agent's SSH probes, which arrive as sessions of the same
user) and the browser (cage.slice/cage-browser.slice). The control plane and the
sessions are protected (memory.low); the browser has a soft limit
(memory.high: reclaimed into zram and throttled above it) and a hard one a
little higher (memory.max: one browser process, the largest, is killed
there, alone).

Every two seconds the governor applies the limits the host agent sends with
its probe (/run/cage-resources/in/limits). The probe's user is the user the
agent sessions run as, so every value read there is clamped: nothing it says
protects more than a share of the guest or squeezes the browser below a
floor. Without fresh limits (no host agent, or an older one) it applies the
same rule to this guest's own MemTotal. It reports each class's use, stall
and limits for the probe (/run/cage-resources/state). Everything is best
effort: when a step fails the guest runs on without it. On exit, and from
ExecStopPost after a crash, the limits are reset and the runtime settings it
made are removed.

The two accounts (the sessions' user, the browser's) come from the unit's
environment, CAGE_SESSION_USER and CAGE_BROWSER_USER, which the host renders
with the environment's own account names. The governor never guesses them:
when either is missing or unknown it cannot tell the browser user's slice
from the sessions', so it protects and limits only the slices it can name
(system.slice, cage-browser.slice), leaves user.slice unprotected, reports no
classes (the host agent then reads the whole guest's pressure, as for a guest
without the governor) and says why in the state file ("accounts unresolved").
"""
import os
import pwd
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time

CGROUP = '/sys/fs/cgroup'
RUN = '/run/cage-resources'
MEMINFO = '/proc/meminfo'
UPTIME = '/proc/uptime'
# /proc/mounts is a symlink. read() rejects a symlink at the final component.
MOUNTS = '/proc/self/mounts'
SYSTEMCTL = ['/usr/bin/systemctl']
# Where systemctl set-property --runtime keeps what it sets.
CONTROL = '/run/systemd/system.control'
# systemd nests dash-separated slices under their parent in cgroupfs.
CLASSES = (('control', 'system.slice'), ('sessions', 'user.slice'), ('browser', 'cage.slice/cage-browser.slice'))
# The unit's variables naming the sessions' user and the browser's: on Ubuntu
# guests snap Chromium runs in a scope of the browser user's own slice under
# user.slice, so that slice belongs to the browser class, not to the sessions.
ACCOUNTS = (('session', 'CAGE_SESSION_USER'), ('browser', 'CAGE_BROWSER_USER'))
ACCOUNT_NAME = re.compile(r'[a-z_][a-z0-9_-]{0,31}')
MIB = 1 << 20
FRESH = 10.0
STEP = 64 * MIB
LIMIT = 1 << 44
INTERVAL = 2.0


def read(path, size=4096):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        return os.read(fd, size).decode('ascii', 'replace')
    finally:
        os.close(fd)


def down(size):
    return size // (64 * MIB) * (64 * MIB)


def up(size):
    return (size + 64 * MIB - 1) // (64 * MIB) * (64 * MIB)


def memtotal():
    for line in read(MEMINFO, 16384).splitlines():
        fields = line.split()
        if len(fields) == 3 and fields[0] == 'MemTotal:' and fields[2] == 'kB' and fields[1].isdigit():
            return int(fields[1]) * 1024
    raise ValueError('MemTotal missing')


def uptime():
    return float(read(UPTIME, 128).split()[0])


def value(text):
    text = text.strip()
    if text == 'max':
        return 0
    if not text.isdigit():
        raise ValueError(text)
    return min(int(text), LIMIT)


def some_avg10(text):
    for line in text.splitlines():
        fields = line.split()
        if fields and fields[0] == 'some':
            for item in fields[1:]:
                key, _, number = item.partition('=')
                if key == 'avg10':
                    return min(max(float(number), 0.0), 100.0)
    return 0.0


def accounts():
    """The user slices of the sessions' and the browser's accounts, as paths
    below the cgroup root, and the roles that could not be resolved. A slice
    of a resolved account need not exist (no session of that user yet)."""
    uids, unresolved = {}, []
    for role, variable in ACCOUNTS:
        name = os.environ.get(variable, '')
        try:
            if not ACCOUNT_NAME.fullmatch(name):
                raise KeyError(name)
            uids[role] = pwd.getpwnam(name).pw_uid
        except KeyError:
            unresolved.append(role)
    if not unresolved and uids['session'] == uids['browser']:
        unresolved = ['session', 'browser']
    if unresolved:
        return None, None, unresolved
    return tuple('user.slice/user-%d.slice' % uids[role] for role, _ in ACCOUNTS) + ([],)


def cgroup(path):
    if path is None:
        return None
    base = os.path.join(CGROUP, path)
    try:
        entry = {'current': value(read(os.path.join(base, 'memory.current'))), 'stall': 0.0, 'low': 0, 'high': 0, 'max': 0}
    except (OSError, ValueError):
        return None
    for key, name in (('low', 'memory.low'), ('high', 'memory.high'), ('max', 'memory.max')):
        try:
            entry[key] = value(read(os.path.join(base, name)))
        except (OSError, ValueError):
            pass
    try:
        entry['stall'] = some_avg10(read(os.path.join(base, 'memory.pressure')))
    except (OSError, ValueError):
        pass
    return entry


def classes(session_slice, browser_slice):
    found = {}
    for name, unit in CLASSES:
        entry = cgroup(unit)
        if entry:
            found[name] = entry
    browser_user = cgroup(browser_slice)
    if browser_user:
        if 'sessions' in found:
            found['sessions']['current'] = max(found['sessions']['current'] - browser_user['current'], 0)
        if 'browser' in found:
            found['browser']['current'] += browser_user['current']
            found['browser']['stall'] = max(found['browser']['stall'], browser_user['stall'])
        else:
            found['browser'] = browser_user
    # The sessions' own slice carries the probes and the terminals: its stall,
    # not the browser user's, is the sessions' stall.
    session_user = cgroup(session_slice)
    if session_user and 'sessions' in found:
        found['sessions']['stall'] = session_user['stall']
    return found

def plan(size, control, sessions):
    """planGuestClasses in guest_allocation.go, for a guest of size bytes."""
    control_low = min(up(control + 128 * MIB), down(size // 4))
    sessions_low = min(up(sessions + 256 * MIB), down(size // 2))
    protected = control + sessions + 512 * MIB
    high = 768 * MIB
    if size > protected:
        high = max(high, size - protected)
    high = down(min(high, max(768 * MIB, size * 3 // 4)))
    return {'control_low': control_low, 'sessions_low': sessions_low, 'browser_high': high,
            'browser_max': up(high + max(256 * MIB, size // 16))}


def clamp(limits, total):
    """Bounds for limits of any origin on a guest of total bytes."""
    high = min(max(limits['browser_high'], down(max(768 * MIB, total // 4))), LIMIT)
    return {'control_low': min(limits['control_low'], down(total // 4)),
            'sessions_low': min(limits['sessions_low'], down(total // 2)),
            'browser_high': high,
            'browser_max': min(max(limits['browser_max'], high + 256 * MIB), LIMIT)}


def received(now, total):
    """The host agent's limits when fresh, clamped; None otherwise."""
    path = os.path.join(RUN, 'in', 'limits')
    try:
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode) or abs(now - info.st_mtime) > FRESH:
            return None
        lines = read(path).splitlines()
    except OSError:
        return None
    if not lines or lines[0].strip() != 'version 1':
        return None
    limits = {}
    for line in lines[1:]:
        fields = line.split()
        if len(fields) == 2 and fields[0] in ('control_low', 'sessions_low', 'browser_high', 'browser_max') and fields[1].isdigit():
            limits[fields[0]] = min(int(fields[1]), LIMIT)
    if len(limits) != 4:
        return None
    return clamp(limits, total)


def recursive_protection():
    """memory.low on a slice protects its children only with memory_recursiveprot."""
    try:
        mounts = read(MOUNTS, 65536)
    except OSError:
        return False
    for line in mounts.splitlines():
        fields = line.split()
        if len(fields) >= 4 and fields[1] == CGROUP and fields[2] == 'cgroup2':
            return 'memory_recursiveprot' in fields[3].split(',')
    return False


class Governor:
    def __init__(self):
        self.applied = {}
        self.source = ''
        self.errors = set()

    def set(self, unit, prop, size, force=False):
        last = self.applied.get((unit, prop))
        if not force and last is not None and abs(last - size) < STEP:
            return
        text = str(size) if size else ('0' if prop == 'MemoryLow' else 'infinity')
        subprocess.run(SYSTEMCTL + ['set-property', '--runtime', unit, prop + '=' + text], check=True, timeout=15,
                       stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.applied[(unit, prop)] = size

    def apply(self, limits, session_slice, browser_slice, resolved):
        browsers = ['cage-browser.slice']
        sessions = ['user.slice']
        for slices, path in ((browsers, browser_slice), (sessions, session_slice)):
            if path and os.path.isdir(os.path.join(CGROUP, path)):
                slices.append(os.path.basename(path))
        steps = []
        for unit in browsers:
            steps += [(unit, 'MemoryHigh', limits['browser_high']), (unit, 'MemoryMax', limits['browser_max'])]
        if recursive_protection():
            # The sessions' user slice claims all of user.slice's protection,
            # so none is left over for the browser user's slice. Without the
            # accounts user.slice may hold the browser: it is not protected,
            # and a protection set before is taken back.
            steps.append(('system.slice', 'MemoryLow', limits['control_low']))
            if resolved:
                steps += [(unit, 'MemoryLow', limits['sessions_low']) for unit in sessions]
            else:
                steps.append(('user.slice', 'MemoryLow', 0))
        for unit, prop, size in steps:
            try:
                self.set(unit, prop, size)
            except (OSError, subprocess.SubprocessError) as error:
                self.warn('%s %s: %s' % (unit, prop, error))

    def once(self):
        total = memtotal()
        session_slice, browser_slice, unresolved = accounts()
        if unresolved:
            self.warn('accounts unresolved (%s): user.slice is neither protected nor counted' %
                      ', '.join('%s=%r' % (variable, os.environ.get(variable, '')) for role, variable in ACCOUNTS if role in unresolved))
        found = classes(session_slice, browser_slice)
        limits = received(time.time(), total)
        source = 'host'
        if limits is None:
            source = 'guest'
            limits = clamp(plan(total, found.get('control', {}).get('current', 0), found.get('sessions', {}).get('current', 0)), total)
        if source != self.source:
            print('cage-resource-governor: limits from the %s' % ('host agent' if source == 'host' else 'guest size'), file=sys.stderr)
            self.source = source
        self.apply(limits, session_slice, browser_slice, not unresolved)
        report(uptime(), {} if unresolved else classes(session_slice, browser_slice), unresolved)

    def warn(self, message):
        if message not in self.errors:
            self.errors.add(message)
            print('cage-resource-governor: ' + message, file=sys.stderr)


def report(stamp, found, unresolved=()):
    # The probe parses this file: a line's first word must not be one of its
    # keys (resources.go, parseResources).
    lines = ['governor 1 %.1f' % stamp]
    if unresolved:
        lines.append('accounts unresolved ' + ' '.join(unresolved))
    for name, _ in CLASSES:
        entry = found.get(name)
        if entry:
            lines.append('class %s %d %.2f %d %d %d' % (name, entry['current'], entry['stall'], entry['low'], entry['high'], entry['max']))
    fd, temporary = tempfile.mkstemp(prefix='.state-', dir=RUN)
    try:
        os.write(fd, ('\n'.join(lines) + '\n').encode('ascii'))
        os.fchmod(fd, 0o644)
    finally:
        os.close(fd)
    os.replace(temporary, os.path.join(RUN, 'state'))


def reset():
    """Undo everything the governor set: every limit and protection goes back
    to systemd's default, and its runtime drop-ins and runtime directory go.
    The user slices are those of the unit's accounts, and any user slice that
    holds a runtime memory setting (made while the accounts were other ones)."""
    governor = Governor()
    steps = {('cage-browser.slice', 'MemoryHigh'), ('cage-browser.slice', 'MemoryMax'), ('system.slice', 'MemoryLow'), ('user.slice', 'MemoryLow')}
    session_slice, browser_slice, _ = accounts()
    for path, props in ((browser_slice, ('MemoryHigh', 'MemoryMax')), (session_slice, ('MemoryLow',))):
        if path and os.path.isdir(os.path.join(CGROUP, path)):
            steps.update((os.path.basename(path), prop) for prop in props)
    try:
        entries = os.listdir(CONTROL)
    except OSError:
        entries = []
    for entry in entries:
        if re.fullmatch(r'user-\d+\.slice\.d', entry):
            steps.update((entry[:-2], prop) for prop in ('MemoryHigh', 'MemoryMax', 'MemoryLow')
                         if os.path.exists(os.path.join(CONTROL, entry, '50-%s.conf' % prop)))
    for unit, prop in sorted(steps):
        try:
            governor.set(unit, prop, 0, force=True)
        except (OSError, subprocess.SubprocessError):
            continue
        # The default is in force: the runtime drop-in that says so goes too.
        try:
            os.unlink(os.path.join(CONTROL, unit + '.d', '50-%s.conf' % prop))
        except OSError:
            pass
    for unit in {unit for unit, _ in steps}:
        try:
            os.rmdir(os.path.join(CONTROL, unit + '.d'))
        except OSError:
            pass
    inbox = os.path.join(RUN, 'in')
    try:
        leftovers = [os.path.join(inbox, name) for name in os.listdir(inbox) if name == 'limits' or name.startswith('.limits.')]
    except OSError:
        leftovers = []
    for path in [os.path.join(RUN, 'state')] + leftovers:
        try:
            os.unlink(path)
        except OSError:
            pass
    for path in (inbox, RUN):
        try:
            os.rmdir(path)
        except OSError:
            pass


def main(argv):
    if argv[1:] == ['--reset']:
        reset()
        return 0
    if not os.path.exists(os.path.join(CGROUP, 'cgroup.controllers')):
        print('cage-resource-governor: no unified cgroup hierarchy; memory priorities are off', file=sys.stderr)
        return 0
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    governor = Governor()
    try:
        while True:
            try:
                governor.once()
            except (OSError, ValueError) as error:
                governor.warn(str(error))
            if argv[1:] == ['--once']:
                return 0
            time.sleep(INTERVAL)
    finally:
        if argv[1:] != ['--once']:
            reset()


if __name__ == '__main__':
    sys.exit(main(sys.argv))
