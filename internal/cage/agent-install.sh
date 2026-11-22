#!/usr/bin/env bash
# 设置 → 其他客户端: install, update or remove one optional agent client.
#
# Runs as the guest user in the maintenance job, after ToolInstallPrelude
# (cage_download_tool, cage_tool_version) and the AGENT_* assignments that
# internal/agents.ScriptInput builds from the compiled catalog. It travels with
# each job (cage.AgentInstallScript) and is never part of a script the guest
# revision hashes. The contract is docs/agent-install.md.
#
# Every line this script prints goes to stdout, so the host reads them in order:
#   CAGE_AGENT_PROGRESS <phase> <received> <total>  check download digest unpack verify done
#   CAGE_AGENT_DONE <id> <version>                  the version now installed
#   CAGE_AGENT_REMOVED <id>
#   CAGE_AGENT_FAILED <code>                        then one line for the person
# Exit 0: done. 1: refused or failed. 2: malformed input (a host bug).
# shellcheck disable=SC2154 # AGENT_* are the job's input.
set -euo pipefail

# Fixed places. Tests rewrite these lines in a copy of the script; nothing
# here comes from the environment.
agent_lib=/usr/local/lib/agent-clients
agent_state=/var/lib/agent-clients
agent_bin=/usr/local/bin
agent_runtime=/usr/local/bin/client_runtime.py
agent_proc=/proc
agent_python=/usr/bin/python3
agent_etc=/etc
agent_cache_root="$HOME/.cache/cage/downloads"

agent_say() { printf '%s\n' "$*"; }
agent_progress() { printf 'CAGE_AGENT_PROGRESS %s %s %s\n' "$1" "${2:-0}" "${3:-0}"; }
# Only ever called from the main shell: exit must end the job.
agent_fail() {
  printf 'CAGE_AGENT_FAILED %s\n%s\n' "$1" "$2"
  exit 1
}
agent_bad_input() {
  printf 'CAGE_AGENT_FAILED input\n其他客户端的安装参数无效（%s），已停止。\n' "$1"
  exit 2
}
# The last line a helper wrote to stderr: its reason, or a traceback's last line.
agent_reason() { tail -n 1 "$agent_tmp/reason" 2>/dev/null | tr -d '\r'; }

# The input is checked again here, with patterns no looser than the
# catalog's: a value that reaches a path, a launcher or a message is plain.
agent_check_input() {
  local re_action='^(install|update|remove)$' re_id='^[a-z][a-z0-9-]{1,31}$' re_command='^[a-z][a-z0-9-]{0,31}$'
  local re_version='^[0-9]+(\.[0-9]+){1,3}([-+][0-9A-Za-z.-]{1,32})?$' re_status='^(available|hidden|withdrawn)$'
  local re_arch='^(arm64|amd64)$' re_libc='^(glibc|musl|any)$' re_glibc='^(2\.[0-9]{1,2})?$' re_flag='^[01]$'
  local re_url='^https://[a-z0-9.-]+/[A-Za-z0-9._~/%+-]+$' re_sha256='^([0-9a-f]{64})?$' re_sha512='^([0-9a-f]{128})?$'
  local re_bytes='^[1-9][0-9]{0,14}$' re_member='^[A-Za-z0-9_+-][A-Za-z0-9._+-]*(/[A-Za-z0-9_+-][A-Za-z0-9._+-]*)*$'
  local re_package='^[a-z0-9][a-z0-9+.-]{1,63}$' re_env='^[A-Z][A-Z0-9_]{1,63}=[A-Za-z0-9_.,:/@%+=-]*$'
  local word line rest
  [[ ${AGENT_ACTION-} =~ $re_action ]] || agent_bad_input AGENT_ACTION
  [[ ${AGENT_ID-} =~ $re_id ]] || agent_bad_input AGENT_ID
  [[ ${AGENT_COMMAND-} =~ $re_command ]] || agent_bad_input AGENT_COMMAND
  [[ ${AGENT_VERSION-} =~ $re_version ]] || agent_bad_input AGENT_VERSION
  [[ ${AGENT_STATUS-} =~ $re_status ]] || agent_bad_input AGENT_STATUS
  [[ ${AGENT_ARCH-} =~ $re_arch ]] || agent_bad_input AGENT_ARCH
  [[ ${AGENT_LIBC-} =~ $re_libc ]] || agent_bad_input AGENT_LIBC
  [[ ${AGENT_MIN_GLIBC-} =~ $re_glibc ]] || agent_bad_input AGENT_MIN_GLIBC
  [[ ${AGENT_USERNS-} =~ $re_flag && ${AGENT_KEYRING-} =~ $re_flag ]] || agent_bad_input AGENT_USERNS
  [[ ${AGENT_SHA256-} =~ $re_sha256 ]] || agent_bad_input AGENT_SHA256
  [[ ${AGENT_SHA512-} =~ $re_sha512 ]] || agent_bad_input AGENT_SHA512
  for word in "${AGENT_NAME-}" "${AGENT_VENDOR-}"; do
    if [ -z "$word" ] || [ "${#word}" -gt 200 ] || [[ $word == *[[:cntrl:]]* ]]; then agent_bad_input AGENT_NAME; fi
  done
  read -r -a agent_packages <<<"${AGENT_PACKAGES-}"
  for word in ${agent_packages[@]+"${agent_packages[@]}"}; do
    [[ $word =~ $re_package ]] || agent_bad_input AGENT_PACKAGES
  done
  read -r -a agent_env <<<"${AGENT_ENV-}"
  for word in ${agent_env[@]+"${agent_env[@]}"}; do
    rest="${word#*=}"
    # The value's 256-byte limit is a length test: BSD regex stops at {0,255}.
    [[ $word =~ $re_env ]] && [ "${#rest}" -le 256 ] || agent_bad_input AGENT_ENV
    case "${word%%=*}" in PATH | HOME | SHELL | BROWSER | TZ | LANG | NODE_OPTIONS | LD_* | LC_* | PYTHON* | CAGE_* | *_PROXY) agent_bad_input AGENT_ENV ;; esac
  done
  if [ "$AGENT_ACTION" != remove ]; then
    [[ ${AGENT_URL-} =~ $re_url ]] || agent_bad_input AGENT_URL
    [ -n "$AGENT_SHA256$AGENT_SHA512" ] || agent_bad_input AGENT_SHA256
    [[ ${AGENT_SIZE-} =~ $re_bytes && ${AGENT_UNPACKED-} =~ $re_bytes ]] || agent_bad_input AGENT_SIZE
    [[ ${AGENT_ENTRY-} =~ $re_member ]] || agent_bad_input AGENT_ENTRY
    [ -z "${AGENT_REMOVE_DATA-}" ] || agent_bad_input AGENT_REMOVE_DATA
  fi
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    # shellcheck disable=SC2088 # a literal ~/ from the catalog, not an expansion
    case "$line" in '~/'?*) ;; *) agent_bad_input AGENT_REMOVE_DATA ;; esac
    rest="${line:2}"
    case "/$rest/" in *//* | */./* | */../*) agent_bad_input AGENT_REMOVE_DATA ;; esac
    if [[ $rest == *[[:cntrl:]]* ]]; then agent_bad_input AGENT_REMOVE_DATA; fi
  done <<<"${AGENT_REMOVE_DATA-}"
  if ! agent_py settings-check "${AGENT_SETTINGS-}" 2>/dev/null; then agent_bad_input AGENT_SETTINGS; fi
  if [ -n "$AGENT_SHA512" ]; then
    agent_digest="$AGENT_SHA512"
    agent_hash=sha512sum
  else
    agent_digest="$AGENT_SHA256"
    agent_hash=sha256sum
  fi
}

# Archive, receipt and settings work, in one reviewed program. Exit 0 done,
# 1 mismatch, 2 malformed input, 3 refused; the reason is the last stderr line.
agent_py() {
  "$agent_python" -I - "$@" <<'CAGE_AGENT_PY'
import hashlib
import json
import os
import re
import stat
import sys
import tarfile
import time
import zlib

try:
    import tomllib
except ImportError:  # Python before 3.11: only a new TOML file can be written
    tomllib = None

CHUNK = 1 << 20
MAX_MEMBERS = 100000
MAX_SETTINGS = 1 << 20
HEX = {'sha256': re.compile(r'[0-9a-f]{64}'), 'sha512': re.compile(r'[0-9a-f]{128}')}
KEY = re.compile(r'[A-Za-z][A-Za-z0-9_-]{0,63}(\.[A-Za-z][A-Za-z0-9_-]{0,63}){0,3}')
HEADER = re.compile(r'\s*\[\s*([A-Za-z0-9_-]+(?:\s*\.\s*[A-Za-z0-9_-]+)*)\s*\]\s*(?:#.*)?')
ANY_HEADER = re.compile(r'\s*\[')


class Mismatch(Exception):
    """Installed files and receipt do not agree."""


class BadInput(Exception):
    """The job's input is malformed: a host bug."""


class Refuse(Exception):
    """A surprise: the operation stops and changes nothing."""


def plain(text):
    if not isinstance(text, str) or any(ord(c) < 32 or ord(c) == 127 for c in text):
        return False
    try:
        text.encode('utf-8')
    except UnicodeEncodeError:
        return False
    return True


class Members:
    """The rules every archive member passes, in both passes."""

    def __init__(self, entry, limit):
        self.entry, self.limit = entry, int(limit)
        self.kinds, self.listed = {}, set()
        self.total = self.count = 0

    def check(self, m):
        self.count += 1
        if self.count > MAX_MEMBERS:
            raise Refuse('more than %d members' % MAX_MEMBERS)
        name = m.name
        if not plain(name) or '\\' in name:
            raise Refuse('member name %r is not plain' % name)
        if name.startswith('/'):
            raise Refuse('absolute path %s' % name)
        while name.startswith('./'):
            name = name[2:]
        name = name.rstrip('/')
        if m.mode & 0o7000:
            raise Refuse('%s has the setuid, setgid or sticky bit' % (name or '.'))
        if name in ('', '.'):
            if not m.isdir():
                raise Refuse('the archive root is not a directory')
            return ''
        parts = name.split('/')
        if any(part in ('', '.', '..') for part in parts):
            raise Refuse('path %s leaves the archive or is not clean' % name)
        if len(name) > 4096 or any(len(part.encode()) > 255 for part in parts):
            raise Refuse('path %s is too long' % name[:64])
        if name in self.listed:
            raise Refuse('%s is listed twice' % name)
        self.listed.add(name)
        for i in range(1, len(parts)):
            prefix = '/'.join(parts[:i])
            if self.kinds.setdefault(prefix, 'dir') != 'dir':
                raise Refuse('%s lies inside %s, which is not a directory' % (name, prefix))
        if m.issparse():
            raise Refuse('%s is a sparse file' % name)
        if m.isdir():
            self.kinds[name] = 'dir'
        elif name in self.kinds:
            raise Refuse('%s is also a directory' % name)
        elif m.issym():
            target = m.linkname
            if not target or not plain(target) or '\\' in target or target.startswith('/') or '..' in target.split('/'):
                raise Refuse('%s links to %s, outside its own directory' % (name, target))
            self.kinds[name] = 'link'
        elif m.islnk():
            raise Refuse('%s is a hard link to %s' % (name, m.linkname))
        elif m.ischr() or m.isblk():
            raise Refuse('%s is a device file' % name)
        elif m.isfifo():
            raise Refuse('%s is a named pipe' % name)
        elif m.isreg():
            self.total += max(m.size, 0)
            if self.total > self.limit:
                raise Refuse('it unpacks to more than the pinned %d bytes' % self.limit)
            self.kinds[name] = 'file'
        else:
            raise Refuse('%s has an unsupported type' % name)
        return name

    def finish(self):
        if self.kinds.get(self.entry) != 'file':
            raise Refuse('the program %s is not a file in the archive' % self.entry)


def damaged(run):
    try:
        return run()
    except (tarfile.TarError, EOFError, zlib.error) as error:
        raise Refuse('the archive is damaged (%s)' % error)
    except OSError as error:
        raise Refuse('the archive cannot be read or written (%s)' % error)


def archive(path):
    return tarfile.open(path, 'r|gz', encoding='utf-8', errors='surrogateescape')


def archive_check(path, entry, limit):
    def run():
        members = Members(entry, limit)
        with archive(path) as tf:
            for m in tf:
                members.check(m)
        members.finish()
    damaged(run)


def progress(done, total):
    print('CAGE_AGENT_PROGRESS unpack %d %d' % (done, total), flush=True)


def archive_extract(path, dest, entry, limit):
    def run():
        members = Members(entry, limit)
        os.mkdir(dest, 0o700)
        written = reported = 0
        step = max(members.limit // 20, 1)
        with archive(path) as tf:
            for m in tf:
                name = members.check(m)
                if not name:
                    continue
                target = os.path.join(dest, *name.split('/'))
                os.makedirs(os.path.dirname(target), mode=0o700, exist_ok=True)
                if m.isdir():
                    if not os.path.isdir(target):
                        os.mkdir(target, 0o700)
                elif m.issym():
                    os.symlink(m.linkname, target)
                else:
                    source = tf.extractfile(m)
                    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
                    with os.fdopen(fd, 'wb') as out:
                        while True:
                            chunk = source.read(CHUNK)
                            if not chunk:
                                break
                            written += len(chunk)
                            if written > members.limit:
                                raise Refuse('it unpacks to more than the pinned %d bytes' % members.limit)
                            out.write(chunk)
                            if written - reported >= step:
                                progress(written, members.limit)
                                reported = written
                    # Owners, setuid bits and archive modes are never restored.
                    os.chmod(target, 0o755 if name == entry or m.mode & 0o111 else 0o644)
            members.finish()
        for root, dirs, _ in os.walk(dest):
            for name in dirs:
                directory = os.path.join(root, name)
                if not os.path.islink(directory):
                    os.chmod(directory, 0o755)
        os.chmod(dest, 0o755)
        if written != reported:
            progress(written, members.limit)
    damaged(run)


def file_sha256(path):
    digest = hashlib.sha256()
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as f:
        for chunk in iter(lambda: f.read(CHUNK), b''):
            digest.update(chunk)
    return digest.hexdigest()


def tree(root, entry, limit):
    """Digest of every name, type, mode, size and content under root; refuses surprises."""
    limit = int(limit)
    digest = hashlib.sha256()
    total, entry_sha, stack = 0, None, ['']
    while stack:
        rel = stack.pop()
        base = os.path.join(root, rel) if rel else root
        subdirs = []
        for name in sorted(os.listdir(base)):
            path_rel = rel + '/' + name if rel else name
            full = os.path.join(base, name)
            st = os.lstat(full)
            mode = stat.S_IMODE(st.st_mode)
            if mode & 0o7000:
                raise Refuse('%s has the setuid, setgid or sticky bit' % path_rel)
            if stat.S_ISDIR(st.st_mode):
                record = ['d', path_rel, mode]
                subdirs.append(path_rel)
            elif stat.S_ISREG(st.st_mode):
                total += st.st_size
                if total > limit:
                    raise Refuse('the files are larger than the pinned %d bytes' % limit)
                sha = file_sha256(full)
                if path_rel == entry:
                    entry_sha = sha
                record = ['f', path_rel, mode, st.st_size, sha]
            elif stat.S_ISLNK(st.st_mode):
                target = os.readlink(full)
                if target.startswith('/') or '..' in target.split('/'):
                    raise Refuse('%s links outside its own directory' % path_rel)
                record = ['l', path_rel, target]
            else:
                raise Refuse('%s is not a file, directory or link' % path_rel)
            digest.update(json.dumps(record).encode() + b'\n')
        stack.extend(reversed(subdirs))
    if entry_sha is None:
        raise Refuse('the program %s is missing' % entry)
    st = os.stat(os.path.join(root, entry))
    # The launcher's integrity check compares this with GNU stat -c %i:%s:%Y.
    return entry_sha, digest.hexdigest(), '%d:%d:%d' % (st.st_ino, st.st_size, st.st_mtime_ns // 1000000000)


def tree_command(root, entry, limit):
    print(*tree(root, entry, limit))


def digests(path, sha256, sha512):
    want = {name: value for name, value in (('sha256', sha256), ('sha512', sha512)) if value}
    if not want or any(not HEX[name].fullmatch(value) for name, value in want.items()):
        raise BadInput('no digest')
    hashes = {name: hashlib.new(name) for name in want}
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(CHUNK), b''):
            for h in hashes.values():
                h.update(chunk)
    for name, value in want.items():
        if hashes[name].hexdigest() != value:
            raise Refuse('the %s of the download differs from the pin' % name)


def artifact(sha256, sha512):
    return {name: value for name, value in (('sha256', sha256), ('sha512', sha512)) if value}


def read_receipt(path):
    try:
        st = os.lstat(path)
        if not stat.S_ISREG(st.st_mode) or st.st_size > 65536:
            return None
        with open(path, 'rb') as f:
            data = json.loads(f.read().decode('utf-8'))
    except (OSError, ValueError):
        return None
    return data if isinstance(data, dict) and data.get('schema') == 1 else None


def receipt_check(path, root, id, version, command, entry, limit, sha256, sha512):
    rec = read_receipt(path)
    if rec is None:
        raise Mismatch('no readable receipt')
    for key, value in (('id', id), ('version', version), ('command', command), ('entry', entry),
                       ('artifact', artifact(sha256, sha512))):
        if rec.get(key) != value:
            raise Mismatch('the receipt records another %s' % key)
    try:
        entry_sha, tree_sha, stat_key = tree(root, entry, limit)
    except (Refuse, OSError) as error:
        raise Mismatch(str(error))
    if rec.get('entry_sha256') != entry_sha or rec.get('tree_sha256') != tree_sha:
        raise Mismatch('the program files differ from the receipt')
    print(entry_sha, tree_sha, stat_key)


def receipt_make(old_path, out, launcher, settings_json, id, version, arch, command, entry,
                 sha256, sha512, entry_sha, tree_sha, stat_key):
    if not (HEX['sha256'].fullmatch(entry_sha) and HEX['sha256'].fullmatch(tree_sha)
            and re.fullmatch(r'[0-9]+:[0-9]+:[0-9]+', stat_key)):
        raise BadInput('digests')
    old = read_receipt(old_path) or {}
    previous = {}
    if isinstance(old.get('settings'), list):
        previous = {s.get('file'): s for s in old['settings'] if isinstance(s, dict)}
    with open(settings_json, encoding='utf-8') as f:
        written = json.load(f)
    settings = []
    for s in written:
        before = previous.get(s['file'], {})
        settings.append({'file': s['file'], 'sha256': s['sha256'],
                         'created': s['created'] or before.get('created') is True,
                         'dir_created': s['dir_created'] or before.get('dir_created') is True})
    rec = {'schema': 1, 'id': id, 'version': version, 'arch': arch, 'command': command,
           'artifact': artifact(sha256, sha512), 'entry': entry, 'entry_sha256': entry_sha,
           'tree_sha256': tree_sha, 'stat_key': stat_key, 'launcher_sha256': file_sha256(launcher),
           'settings': settings}
    stamp = old.get('installed_at')
    same = isinstance(stamp, str) and old == dict(rec, installed_at=stamp)
    if not same:
        rec['installed_at'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
        with open(out, 'w', encoding='utf-8') as f:
            json.dump(rec, f, ensure_ascii=False, indent=1)
            f.write('\n')
    print('same' if same else 'changed', stat_key)


def parse_settings(spec):
    try:
        settings = json.loads(spec) if spec else []
    except ValueError:
        raise BadInput('settings are not JSON')
    if not isinstance(settings, list):
        raise BadInput('settings are not a list')
    for s in settings:
        if not isinstance(s, dict) or set(s) != {'file', 'format', 'set'}:
            raise BadInput('a setting has other fields')
        file, fmt, values = s['file'], s['format'], s['set']
        if fmt not in ('toml', 'json') or not isinstance(values, dict) or not values:
            raise BadInput('a setting has no format or values')
        if not plain(file) or not file.endswith('.' + fmt) or '\\' in file:
            raise BadInput('a setting names a bad file')
        if not (file.startswith('/etc/') or file.startswith('~/')):
            raise BadInput('a setting file is neither under /etc nor the home')
        rest = file[5:] if file.startswith('/etc/') else file[2:]
        if any(part in ('', '.', '..') for part in rest.split('/')):
            raise BadInput('a setting file is not a clean path')
        for key, value in values.items():
            ok = isinstance(value, (bool, int)) or isinstance(value, str) and plain(value) and len(value.encode()) <= 256
            if not KEY.fullmatch(key) or not ok:
                raise BadInput('a setting key or value is not plain')
    return settings


def place(file, home, etc):
    if file.startswith('/etc/'):
        return 'system', os.path.join(etc, file[5:])
    return 'home', os.path.join(home, file[2:])


def toml_value(value):
    if isinstance(value, bool):
        return 'true' if value else 'false'
    if isinstance(value, int):
        return str(value)
    return json.dumps(value, ensure_ascii=False)


def merge_toml(text, values, file):
    lines = text.splitlines()
    for key in sorted(values, key=lambda k: ('.' in k, k)):
        table, _, leaf = key.rpartition('.')
        line = '%s = %s' % (leaf, toml_value(values[key]))
        assign = re.compile(r'\s*' + re.escape(leaf) + r'\s*=')
        start = 0
        if table:
            start = next((i + 1 for i, l in enumerate(lines)
                          if HEADER.fullmatch(l) and re.sub(r'\s', '', HEADER.fullmatch(l).group(1)) == table), None)
            if start is None:
                while lines and not lines[-1].strip():
                    lines.pop()
                lines += ([''] if lines else []) + ['[%s]' % table, line]
                continue
        end = next((i for i in range(start, len(lines)) if ANY_HEADER.match(lines[i])), len(lines))
        found = next((i for i in range(start, end) if assign.match(lines[i])), None)
        if found is not None:
            lines[found] = line
        else:
            at = end
            while at > start and not lines[at - 1].strip():
                at -= 1
            lines.insert(at, line)
            if at + 1 < len(lines) and ANY_HEADER.match(lines[at + 1]):
                lines.insert(at + 1, '')
    new = '\n'.join(lines) + '\n'
    if tomllib is None:
        if text.strip():
            raise Refuse('%s exists and this Python cannot read TOML to merge it safely' % file)
        return new
    try:
        data = tomllib.loads(new)
    except ValueError as error:
        raise Refuse('%s cannot be merged (%s)' % (file, error))
    for key, want in values.items():
        node = data
        for part in key.split('.'):
            if not isinstance(node, dict) or part not in node:
                raise Refuse('%s cannot be merged (%s)' % (file, key))
            node = node[part]
        if type(node) is not type(want) or node != want:
            raise Refuse('%s cannot be merged (%s)' % (file, key))
    return new


def merge_json(text, values, file):
    try:
        data = json.loads(text) if text.strip() else {}
    except ValueError:
        raise Refuse('%s is not valid JSON' % file)
    if not isinstance(data, dict):
        raise Refuse('%s does not hold a JSON object' % file)
    before = json.dumps(data, sort_keys=True)
    for key, value in values.items():
        *path, leaf = key.split('.')
        node = data
        for part in path:
            node = node.setdefault(part, {})
            if not isinstance(node, dict):
                raise Refuse('%s: %s is not an object' % (file, part))
        node[leaf] = value
    if text.strip() and json.dumps(data, sort_keys=True) == before:
        return text
    return json.dumps(data, indent=2, ensure_ascii=False) + '\n'


def settings_merge(spec, home, etc, outdir):
    """Merged copies in outdir; list.txt for the shell, settings.json for the receipt."""
    lines, written = [], []
    for n, s in enumerate(parse_settings(spec)):
        kind, path = place(s['file'], home, etc)
        exists = os.path.lexists(path)
        mode = 0o644 if kind == 'system' else 0o600
        text = ''
        if exists:
            st = os.lstat(path)
            if not stat.S_ISREG(st.st_mode):
                raise Refuse('%s is not a regular file' % s['file'])
            if st.st_size > MAX_SETTINGS:
                raise Refuse('%s is too large' % s['file'])
            mode = stat.S_IMODE(st.st_mode) & 0o777
            try:
                with open(path, 'rb') as f:
                    text = f.read().decode('utf-8')
            except (OSError, UnicodeDecodeError) as error:
                raise Refuse('%s cannot be read (%s)' % (s['file'], error))
        merge = merge_toml if s['format'] == 'toml' else merge_json
        new = merge(text, s['set'], s['file'])
        with open(os.path.join(outdir, str(n)), 'w', encoding='utf-8') as f:
            f.write(new)
        sha = hashlib.sha256(new.encode('utf-8')).hexdigest()
        created = 'existing' if exists else 'created'
        folder = 'dir-existing' if os.path.isdir(os.path.dirname(path)) else 'dir-created'
        lines.append('%d %s %s %s %s %o %s' % (n, kind, 'same' if exists and new == text else 'changed',
                                              created, folder, mode, path))
        written.append({'file': s['file'], 'sha256': sha, 'created': not exists, 'dir_created': folder == 'dir-created'})
    with open(os.path.join(outdir, 'list.txt'), 'w', encoding='utf-8') as f:
        f.write(''.join(line + '\n' for line in lines))
    with open(os.path.join(outdir, 'settings.json'), 'w', encoding='utf-8') as f:
        json.dump(written, f)


def settings_removable(receipt, spec, etc):
    """System files Cage created and nobody changed since: remove; others: keep."""
    rec = read_receipt(receipt) or {}
    recorded = {}
    if isinstance(rec.get('settings'), list):
        recorded = {s.get('file'): s for s in rec['settings'] if isinstance(s, dict)}
    for s in parse_settings(spec):
        kind, path = place(s['file'], '/nonexistent', etc)
        if kind != 'system' or not os.path.lexists(path):
            continue
        r = recorded.get(s['file'], {})
        sha = r.get('sha256')
        st = os.lstat(path)
        if (r.get('created') is not True or not isinstance(sha, str) or not stat.S_ISREG(st.st_mode)
                or file_sha256(path) != sha):
            print('keep 0', path)
            continue
        print('remove', 1 if r.get('dir_created') is True else 0, path)


COMMANDS = {
    'settings-check': parse_settings, 'archive-check': archive_check, 'archive-extract': archive_extract, 'tree': tree_command,
    'digests': digests, 'receipt-check': receipt_check, 'receipt-make': receipt_make,
    'settings-merge': settings_merge, 'settings-removable': settings_removable,
}

try:
    COMMANDS[sys.argv[1]](*sys.argv[2:])
except Mismatch as error:
    print(error, file=sys.stderr)
    sys.exit(1)
except BadInput as error:
    print(error, file=sys.stderr)
    sys.exit(2)
except Refuse as error:
    print(error, file=sys.stderr)
    sys.exit(3)
CAGE_AGENT_PY
}

agent_cleanup() {
  if [ -n "$agent_stage" ]; then
    case "$agent_stage" in "$agent_lib"/.stage-*) sudo rm -rf -- "$agent_stage" || true ;; esac
  fi
  rm -rf -- "$agent_tmp" || true
  if [ "$agent_locked" = 1 ]; then agent_prune || true; fi
}

agent_canceled() {
  printf 'CAGE_AGENT_FAILED canceled\n已取消，已下载的部分会在下次继续使用。\n' || true
  exit 143
}

# Before the first install and after the last remove nothing of this feature
# is left in the guest.
agent_prune() {
  local item
  for item in "$agent_state"/*.json "$agent_lib"/*; do
    if [ -e "$item" ] || [ -L "$item" ]; then return 0; fi
  done
  sudo rm -rf -- "$agent_lib" "$agent_state"
}

agent_lock() {
  local stale
  sudo mkdir -p -- "$agent_state" "$agent_lib"
  sudo chmod 0755 "$agent_state" "$agent_lib"
  if [ ! -f "$agent_state/.lock" ]; then
    sudo touch -- "$agent_state/.lock"
    sudo chmod 0644 "$agent_state/.lock"
  fi
  # flock needs only a readable descriptor; the file stays root's.
  exec 9<"$agent_state/.lock"
  flock -n 9 || agent_fail busy '另一个客户端的添加、更新或移除仍在进行，请等它完成后再试。'
  agent_locked=1
  # A canceled or stopped job leaves its stage behind; nothing else uses it.
  for stale in "$agent_lib"/.stage-*; do
    if [ -e "$stale" ]; then sudo rm -rf -- "$stale"; fi
  done
}

# Cage's own launcher: a regular file whose second line names this entry.
agent_is_launcher() {
  local line=''
  [ -f "$1" ] && [ ! -L "$1" ] || return 1
  { IFS= read -r line && IFS= read -r line; } <"$1" || return 1
  [ "$line" = "# super-cage-agent-launcher $AGENT_ID" ]
}

# Prints the first program of this name that is not Cage's launcher: in the
# launcher's place, in ~/.local/bin (where a hand-run npm install lands) or on PATH.
agent_own_command() {
  local own="$agent_bin/$AGENT_COMMAND" search dir candidate
  if { [ -e "$own" ] || [ -L "$own" ]; } && ! agent_is_launcher "$own"; then
    printf '%s\n' "$own"
    return 0
  fi
  search="$HOME/.local/bin:$PATH"
  while [ -n "$search" ]; do
    dir="${search%%:*}"
    if [ "$dir" = "$search" ]; then search=''; else search="${search#*:}"; fi
    case "$dir" in /*) ;; *) continue ;; esac
    candidate="$dir/$AGENT_COMMAND"
    if [ -f "$candidate" ] && [ -x "$candidate" ]; then
      if [ "$candidate" -ef "$own" ] && agent_is_launcher "$own"; then continue; fi
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

# The version current points at, when it is a real version directory.
agent_installed_version() {
  local re_version='^[0-9]+(\.[0-9]+){1,3}([-+][0-9A-Za-z.-]{1,32})?$' link
  link="$(readlink "$agent_lib/$AGENT_ID/current" 2>/dev/null)" || return 0
  if [[ $link =~ $re_version ]] && [ -d "$agent_lib/$AGENT_ID/$link" ] && [ ! -L "$agent_lib/$AGENT_ID/$link" ]; then
    printf '%s\n' "$link"
  fi
}

agent_in_use() {
  local exe target
  for exe in "$agent_proc"/[0-9]*/exe; do
    target="$(readlink "$exe" 2>/dev/null)" || continue
    case "$target" in "$agent_lib/$AGENT_ID/"*) return 0 ;; esac
  done
  return 1
}

agent_check_platform() {
  local machine arch glibc
  if [ "$AGENT_STATUS" = withdrawn ]; then
    agent_fail withdrawn "${AGENT_NAME} 已不再受 Super Cage 支持，无法添加或更新。"
  fi
  machine="$(uname -m)"
  case "$machine" in aarch64 | arm64) arch=arm64 ;; x86_64 | amd64) arch=amd64 ;; *) arch="$machine" ;; esac
  if [ "$arch" != "$AGENT_ARCH" ]; then
    agent_fail arch "${AGENT_NAME} 的固定版本适用于 ${AGENT_ARCH}，这个客机的架构是 ${machine}，无法安装。"
  fi
  case "$AGENT_LIBC" in
    glibc)
      glibc="$(getconf GNU_LIBC_VERSION 2>/dev/null)" || glibc=''
      glibc="${glibc#glibc }"
      if [ -z "$glibc" ]; then agent_fail libc "${AGENT_NAME} 需要 glibc，这个客机没有 glibc，无法安装。"; fi
      if [ -n "$AGENT_MIN_GLIBC" ] && ! dpkg --compare-versions "$glibc" ge "$AGENT_MIN_GLIBC"; then
        agent_fail libc "${AGENT_NAME} 需要 glibc ${AGENT_MIN_GLIBC} 或更高版本，这个客机是 ${glibc}，无法安装。"
      fi
      ;;
    musl)
      set -- /lib/ld-musl-*.so.1
      if [ ! -e "$1" ]; then agent_fail libc "${AGENT_NAME} 需要 musl，这个客机没有 musl，无法安装。"; fi
      ;;
  esac
  # The per-entry keyring is design step 2.3; until then such an entry cannot sign in.
  if [ "$AGENT_KEYRING" = 1 ]; then
    agent_fail keyring "${AGENT_NAME} 需要的密钥环支持尚未完成，暂时无法安装。"
  fi
  # The client's sandbox needs an unprivileged user namespace with its own mounts.
  if [ "$AGENT_USERNS" = 1 ] && ! unshare --user --map-root-user --mount /bin/true </dev/null >/dev/null 2>&1; then
    agent_fail userns "这个环境不允许普通用户创建命名空间，${AGENT_NAME} 的沙箱无法运行，因此没有安装。"
  fi
}

agent_need_mb() {
  local bytes="$1" free_kb="$2" need_mb free_mb
  case "$free_kb" in '' | *[!0-9]*) free_kb=0 ;; esac
  need_mb=$(((bytes + bytes / 10 + 1048575) / 1048576))
  free_mb=$((free_kb / 1024))
  if [ "$free_mb" -lt "$need_mb" ]; then
    agent_fail disk "客机磁盘空间不足：需要 ${need_mb} MB，当前只有 ${free_mb} MB。"
  fi
}

# The download goes to the home, the files to /usr/local/lib; on one
# filesystem both count against the same free space. 10 % headroom.
agent_check_disk() {
  local archive="$1" have=0 part need_download probe usage home_fs='' home_kb='' lib_fs='' lib_kb=''
  for part in "$archive" "$archive.part"; do
    if [ -f "$part" ]; then have=$((have + $(wc -c <"$part"))); fi
  done
  need_download=$((AGENT_SIZE > have ? AGENT_SIZE - have : 0))
  probe="$agent_lib"
  while [ -n "$probe" ] && [ ! -d "$probe" ]; do probe="${probe%/*}"; done
  usage="$(df -Pk "$HOME" "${probe:-/}" </dev/null | awk 'NR > 1 { print $1, $4 }')" || usage=''
  { read -r home_fs home_kb && read -r lib_fs lib_kb; } <<<"$usage" || true
  if [ "$home_fs" = "$lib_fs" ]; then
    agent_need_mb $((need_download + AGENT_UNPACKED)) "$home_kb"
  else
    agent_need_mb "$need_download" "$home_kb"
    agent_need_mb "$AGENT_UNPACKED" "$lib_kb"
  fi
}

agent_install_packages() {
  local missing=() package
  for package in ${agent_packages[@]+"${agent_packages[@]}"}; do
    # shellcheck disable=SC2016 # dpkg-query's own field syntax
    if [ "$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null)" != 'install ok installed' ]; then missing+=("$package"); fi
  done
  if [ "${#missing[@]}" = 0 ]; then return 0; fi
  agent_say "正在安装 ${AGENT_NAME} 所需的系统组件：${missing[*]}…"
  if ! sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get -o DPkg::Lock::Timeout=60 update </dev/null 8>&- 9<&- ||
    ! sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get -o DPkg::Lock::Timeout=60 install -y --no-install-recommends "${missing[@]}" </dev/null 8>&- 9<&-; then
    agent_fail packages "无法安装 ${AGENT_NAME} 所需的系统组件（${missing[*]}），已停止安装，原有版本保持不变。"
  fi
}

# Turns the download tool's byte counts into progress lines (at most one per
# percent) and notes what a failure was. Runs in a pipeline: it never exits.
agent_download_lines() {
  local line count last=-1 step=$((AGENT_SIZE / 100 + 1))
  while IFS= read -r line; do
    case "$line" in
      'CAGE_TOOL_BYTES '*)
        count="${line##* }"
        case "$count" in '' | *[!0-9]*) continue ;; esac
        if [ "$count" -gt "$AGENT_SIZE" ]; then count="$AGENT_SIZE"; fi
        if [ "$count" -ge $((last + step)) ] || { [ "$count" = "$AGENT_SIZE" ] && [ "$last" != "$count" ]; }; then
          agent_progress download "$count" "$AGENT_SIZE"
          last="$count"
        fi
        continue
        ;;
      *'returned error: 404'* | *'returned error: 410'*) : >"$agent_tmp/gone" ;;
      'Tool checksum mismatch'*) : >"$agent_tmp/digest" ;;
    esac
    printf '%s\n' "$line"
  done
}

agent_download() {
  local archive="$1" cache status=0
  cache="${archive%/*}"
  mkdir -p -- "$cache"
  exec 8>"$cache/lock"
  flock -w 60 8 || agent_fail busy '另一个下载仍在使用这个安装文件，请稍后再试。'
  agent_say "正在下载 ${AGENT_NAME} ${AGENT_VERSION}（约 $(((AGENT_SIZE + 1048575) / 1048576)) MB）…"
  agent_progress download 0 "$AGENT_SIZE"
  CAGE_SETUP_PROGRESS=1 cage_download_tool "$AGENT_URL" "$agent_digest" "$archive" "$agent_hash" '' 4 </dev/null 2>&1 | agent_download_lines || status=$?
  if [ "$status" = 0 ]; then return 0; fi
  if [ -e "$agent_tmp/gone" ]; then
    agent_fail gone "${AGENT_VENDOR} 已不再提供 ${AGENT_NAME} ${AGENT_VERSION} 的下载。请更新 Super Cage 以获取新的固定版本。"
  fi
  if [ -e "$agent_tmp/digest" ]; then agent_fail digest '安装文件校验失败，已停止安装，原有版本保持不变。'; fi
  agent_fail download '下载中断，已保存进度。重试会继续下载。'
}

# Every pinned digest, of the exact file about to be unpacked.
agent_check_digests() {
  local archive="$1"
  agent_progress digest 0 0
  if ! agent_py digests "$archive" "$AGENT_SHA256" "$AGENT_SHA512" 2>"$agent_tmp/reason"; then
    rm -f -- "$archive"
    agent_fail digest '安装文件校验失败，已停止安装，原有版本保持不变。'
  fi
}

# Every member is checked before anything is unpacked; then the same rules
# guard the extraction, as the guest user, into a fresh directory under the
# root-owned tree. Only then does root take the files over.
agent_unpack() {
  local archive="$1" status=0
  agent_progress unpack 0 "$AGENT_UNPACKED"
  agent_py archive-check "$archive" "$AGENT_ENTRY" "$AGENT_UNPACKED" 2>"$agent_tmp/reason" || status=$?
  if [ "$status" != 0 ]; then
    agent_fail archive "安装文件的内容不符合要求（$(agent_reason)），已停止安装，原有版本保持不变。"
  fi
  agent_stage="$(sudo mktemp -d "$agent_lib/.stage-XXXXXX")"
  sudo chown "$(id -u):$(id -g)" -- "$agent_stage"
  agent_py archive-extract "$archive" "$agent_stage/tree" "$AGENT_ENTRY" "$AGENT_UNPACKED" 2>"$agent_tmp/reason" || status=$?
  if [ "$status" != 0 ]; then
    agent_fail archive "安装文件的内容不符合要求（$(agent_reason)），已停止安装，原有版本保持不变。"
  fi
  sudo chown -hR 0:0 -- "$agent_stage/tree"
}

# The staged copy runs with an empty environment: the job's proxy variables
# carry the install channel's credential and never reach a vendor program.
agent_verify() {
  local staged="$agent_stage/tree/$AGENT_ENTRY" output version
  agent_progress verify 0 0
  # The job's lock descriptors are closed: a client left running must not hold them.
  output="$(timeout -k 2 8 env -i HOME="$HOME" PATH=/usr/bin:/bin ${agent_env[@]+"${agent_env[@]}"} "$staged" --version </dev/null 2>/dev/null 8>&- 9<&-)" || output=''
  version="$(printf '%s\n' "$output" | tr -s ' \t' '\n' | cage_tool_version)" || version=''
  version="${version#v}"
  if [ -z "$version" ]; then
    agent_fail verify '安装文件无法在这个客机运行，已停止安装，原有文件保持不变。'
  fi
  if [ "$version" != "$AGENT_VERSION" ]; then
    agent_fail verify "安装文件报告的版本（${version}）与固定版本 ${AGENT_VERSION} 不一致，已停止安装，原有文件保持不变。"
  fi
}

# Digests of a tree into $agent_tmp/digests: entry sha256, tree sha256, stat key.
agent_tree() {
  if ! agent_py tree "$1" "$AGENT_ENTRY" "$AGENT_UNPACKED" >"$agent_tmp/digests" 2>"$agent_tmp/reason"; then
    agent_fail archive "安装后的文件不符合要求（$(agent_reason)），已停止安装，原有版本保持不变。"
  fi
}

# Merged settings into $agent_tmp/settings: checked before the download, and
# again right before they are written.
agent_prepare_settings() {
  local status=0
  rm -rf -- "$agent_tmp/settings"
  mkdir -- "$agent_tmp/settings"
  agent_py settings-merge "${AGENT_SETTINGS-}" "$HOME" "$agent_etc" "$agent_tmp/settings" 2>"$agent_tmp/reason" || status=$?
  case "$status" in
    0) ;;
    2) agent_bad_input AGENT_SETTINGS ;;
    *) agent_fail settings "无法合并 ${AGENT_NAME} 的设置文件（$(agent_reason)），已停止安装，原有版本保持不变。" ;;
  esac
}

agent_write_settings() {
  local n kind change folder mode file dir
  while IFS=' ' read -r n kind change _ folder mode file; do
    [ -n "$n" ] && [ "$change" = changed ] || continue
    dir="${file%/*}"
    if [ "$kind" = system ]; then
      sudo mkdir -p -- "$dir"
      if [ "$folder" = dir-created ]; then sudo chmod 0755 "$dir"; fi
      sudo cp -- "$agent_tmp/settings/$n" "$dir/.agent-settings-new"
      sudo chmod "$mode" "$dir/.agent-settings-new"
      sudo mv -f -- "$dir/.agent-settings-new" "$file"
    else
      (umask 077 && mkdir -p -- "$dir")
      cp -- "$agent_tmp/settings/$n" "$dir/.agent-settings-new"
      chmod "$mode" "$dir/.agent-settings-new"
      mv -f -- "$dir/.agent-settings-new" "$file"
    fi
  done <"$agent_tmp/settings/list.txt"
}

agent_launcher_text() {
  local kv
  printf '#!/bin/sh\n# super-cage-agent-launcher %s\n' "$AGENT_ID"
  printf '# Written by Super Cage (设置 → 其他客户端); updating or removing the client there replaces it.\n'
  for kv in ${agent_env[@]+"${agent_env[@]}"}; do
    printf "export %s='%s'\n" "${kv%%=*}" "${kv#*=}"
  done
  # client_runtime.py removes every CAGE_* variable before the client starts.
  printf "exec '%s' -I '%s' '%s' \"\$@\"\n" "$agent_python" "$agent_runtime" "$agent_lib/$AGENT_ID/current/$AGENT_ENTRY"
}

agent_write_launcher() {
  local target="$agent_bin/$AGENT_COMMAND" new="$agent_bin/.$AGENT_COMMAND.agent-new"
  agent_launcher_text >"$agent_tmp/launcher"
  if [ -f "$target" ] && [ ! -L "$target" ] && cmp -s "$agent_tmp/launcher" "$target"; then return 0; fi
  sudo cp -- "$agent_tmp/launcher" "$new"
  sudo chmod 0755 "$new"
  sudo mv -f -- "$new" "$target"
}

# Receipt (the full record) and the stat sidecar (one line the launcher reads).
agent_write_receipt() {
  local entry_sha tree_sha stat_key change current=''
  read -r entry_sha tree_sha stat_key <"$agent_tmp/digests"
  if ! agent_py receipt-make "$agent_state/$AGENT_ID.json" "$agent_tmp/receipt" "$agent_bin/$AGENT_COMMAND" \
    "$agent_tmp/settings/settings.json" "$AGENT_ID" "$AGENT_VERSION" "$AGENT_ARCH" "$AGENT_COMMAND" "$AGENT_ENTRY" \
    "$AGENT_SHA256" "$AGENT_SHA512" "$entry_sha" "$tree_sha" "$stat_key" >"$agent_tmp/receipt.state" 2>"$agent_tmp/reason"; then
    agent_fail archive "无法写入安装记录（$(agent_reason)）。"
  fi
  read -r change stat_key <"$agent_tmp/receipt.state"
  if [ "$change" = changed ]; then
    sudo cp -- "$agent_tmp/receipt" "$agent_state/.$AGENT_ID.json.new"
    sudo chmod 0644 "$agent_state/.$AGENT_ID.json.new"
    sudo mv -f -- "$agent_state/.$AGENT_ID.json.new" "$agent_state/$AGENT_ID.json"
  fi
  if [ -f "$agent_state/$AGENT_ID.stat" ]; then IFS= read -r current <"$agent_state/$AGENT_ID.stat" || current=''; fi
  if [ "$current" != "$stat_key" ] || [ -L "$agent_state/$AGENT_ID.stat" ]; then
    printf '%s\n' "$stat_key" >"$agent_tmp/stat"
    sudo cp -- "$agent_tmp/stat" "$agent_state/.$AGENT_ID.stat.new"
    sudo chmod 0644 "$agent_state/.$AGENT_ID.stat.new"
    sudo mv -f -- "$agent_state/.$AGENT_ID.stat.new" "$agent_state/$AGENT_ID.stat"
  fi
}

agent_commit() {
  local id_dir="$agent_lib/$AGENT_ID" dest old item
  dest="$id_dir/$AGENT_VERSION"
  sudo mkdir -p -- "$id_dir"
  sudo chmod 0755 "$id_dir"
  if [ -e "$dest" ] || [ -L "$dest" ]; then
    old="$id_dir/.old-$AGENT_VERSION"
    sudo rm -rf -- "$old"
    sudo mv -- "$dest" "$old"
  fi
  sudo mv -- "$agent_stage/tree" "$dest"
  sudo ln -sfn -- "$AGENT_VERSION" "$id_dir/current"
  agent_write_launcher
  agent_write_settings
  agent_write_receipt
  # The previous version goes only once the new one is in place.
  for item in "$id_dir"/* "$id_dir"/.old-*; do
    if [ ! -e "$item" ] && [ ! -L "$item" ]; then continue; fi
    case "${item##*/}" in current | "$AGENT_VERSION") continue ;; esac
    sudo rm -rf -- "$item"
  done
  sudo rm -rf -- "$agent_stage"
  agent_stage=''
}

agent_done() {
  agent_progress 'done' 0 0
  printf 'CAGE_AGENT_DONE %s %s\n' "$AGENT_ID" "$1"
}

agent_install() {
  local own installed archive
  agent_progress check 0 0
  agent_check_platform
  if own="$(agent_own_command)"; then
    agent_fail own-install "已经安装了 ${AGENT_NAME}（自行安装，${own}），Super Cage 不会覆盖它。"
  fi
  agent_lock
  installed="$(agent_installed_version)"
  # Never go back: a newer version (from a newer Super Cage) stays.
  if [ -n "$installed" ] && dpkg --compare-versions "$installed" gt "$AGENT_VERSION"; then
    agent_say "${AGENT_NAME} ${installed} 比 Super Cage 固定的 ${AGENT_VERSION} 更新，保留现有版本。"
    agent_done "$installed"
    return 0
  fi
  if [ "$installed" = "$AGENT_VERSION" ]; then
    if agent_py receipt-check "$agent_state/$AGENT_ID.json" "$agent_lib/$AGENT_ID/$AGENT_VERSION" "$AGENT_ID" "$AGENT_VERSION" \
      "$AGENT_COMMAND" "$AGENT_ENTRY" "$AGENT_UNPACKED" "$AGENT_SHA256" "$AGENT_SHA512" >"$agent_tmp/digests" 2>"$agent_tmp/reason"; then
      agent_prepare_settings
      agent_write_launcher
      agent_write_settings
      agent_write_receipt
      rm -rf -- "${agent_cache_root:?}/$agent_digest"
      agent_say "${AGENT_NAME} ${AGENT_VERSION} 已是最新。"
      agent_done "$AGENT_VERSION"
      return 0
    fi
    agent_say "${AGENT_NAME} ${AGENT_VERSION} 的程序文件与安装记录不一致（$(agent_reason)），重新下载并修复。"
  fi
  if [ -d "$agent_lib/$AGENT_ID" ] && agent_in_use; then
    agent_fail in-use "请先结束正在运行的 ${AGENT_NAME} 终端，再更新或修复。"
  fi
  agent_prepare_settings
  archive="$agent_cache_root/$agent_digest/$AGENT_ID.tar.gz"
  agent_check_disk "$archive"
  agent_install_packages
  agent_download "$archive"
  agent_check_digests "$archive"
  agent_unpack "$archive"
  agent_verify
  agent_tree "$agent_stage/tree"
  agent_prepare_settings
  agent_commit
  rm -rf -- "${agent_cache_root:?}/$agent_digest"
  agent_say "${AGENT_NAME} ${AGENT_VERSION} 已安装。"
  agent_done "$AGENT_VERSION"
}

# Deletes only the listed paths under the home, never through a link that
# leaves it, never a shared root or a protected tree.
agent_remove_data() {
  local line rest target parent home_real
  home_real="$(cd -P -- "$HOME" && pwd)"
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    rest="${line:2}"
    case "$rest" in
      .ssh | .ssh/* | .gnupg | .gnupg/* | .pki | .pki/* | .claude | .claude/* | .config | .cache | .local | .local/bin | .local/share | .local/state | .local/share/cage)
        agent_say "${line} 是共用或受保护的目录，没有删除。"
        continue
        ;;
    esac
    target="$HOME/$rest"
    if [ ! -e "$target" ] && [ ! -L "$target" ]; then continue; fi
    parent="$(cd -P -- "${target%/*}" 2>/dev/null && pwd)" || continue
    case "$parent/" in
      "$home_real"/*) ;;
      *)
        agent_say "${line} 位于用户目录之外（经由链接），没有删除。"
        continue
        ;;
    esac
    rm -rf -- "$target"
    agent_say "已删除 ${line}。"
  done <<<"${AGENT_REMOVE_DATA-}"
}

agent_remove() {
  local launcher="$agent_bin/$AGENT_COMMAND" action flag file
  agent_progress check 0 0
  if [ -d "$agent_state" ] || [ -d "$agent_lib" ]; then
    agent_lock
    if [ -d "$agent_lib/$AGENT_ID" ] && agent_in_use; then
      agent_fail in-use "请先结束正在运行的 ${AGENT_NAME} 终端，再移除。"
    fi
    # Paths come from the catalog; the receipt only says which file Cage created and its hash.
    if ! agent_py settings-removable "$agent_state/$AGENT_ID.json" "${AGENT_SETTINGS-}" "$agent_etc" >"$agent_tmp/removable" 2>"$agent_tmp/reason"; then
      agent_bad_input AGENT_SETTINGS
    fi
    if agent_is_launcher "$launcher"; then sudo rm -f -- "$launcher"; fi
    sudo rm -rf -- "${agent_lib:?}/$AGENT_ID"
    sudo rm -f -- "$agent_state/$AGENT_ID.json" "$agent_state/$AGENT_ID.stat"
    while IFS=' ' read -r action flag file; do
      case "$action" in
        remove)
          sudo rm -f -- "$file"
          if [ "$flag" = 1 ]; then sudo rmdir -- "${file%/*}" 2>/dev/null || true; fi
          ;;
        keep) agent_say "设置文件 ${file} 不是 Super Cage 创建的，或安装后被修改过，已保留。" ;;
      esac
    done <"$agent_tmp/removable"
  fi
  agent_remove_data
  if [ -n "$agent_digest" ]; then rm -rf -- "${agent_cache_root:?}/$agent_digest"; fi
  agent_say "${AGENT_NAME} 已移除。"
  agent_progress 'done' 0 0
  printf 'CAGE_AGENT_REMOVED %s\n' "$AGENT_ID"
}

agent_check_input
agent_tmp="$(mktemp -d "${TMPDIR:-/tmp}/agent-clients.XXXXXX")"
agent_stage=''
agent_locked=0
trap agent_cleanup EXIT
trap agent_canceled TERM
trap 'exit 130' INT
trap 'exit 129' HUP
case "$AGENT_ACTION" in
  install | update) agent_install ;;
  remove) agent_remove ;;
esac
