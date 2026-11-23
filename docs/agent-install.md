# Agent client installer (其他客户端, steps 1.2 and 1.3)

`internal/cage/agent-install.sh` installs, updates and removes one optional entry of the agent catalog
(`docs/agent-catalog.md`) inside a Linux guest. It runs as the guest user in the maintenance job and uses sudo
only for the root-owned tree. It is **not** part of any script the guest revision hashes: it travels with each
job, so adding a client never closes the network gate of every guest.

## How a job is built (host side, step 1.3)

```go
input, err := agents.ScriptInput(entry, agents.Job{Action: agents.Install, Arch: arch}) // agents.Script
script := cage.AgentInstallScript(input) // prelude + input + installer
// internal/workbench/agents.go runs it with withToolsNetworkMonitor and the maintenance channel
```

The routes (`GET /api/agents`, `POST /api/agents/{id}/install|update|remove`, `POST /api/agents/cancel`), the job
(kind `agent-<action>:<id>`, window only, single slot, lease renewal), its progress and failure lines and the
status codes are documented at the top of `internal/workbench/agents.go`. The job's channel is the shared
maintenance list plus, for that job only, `agents.ExtraInstallHosts(entry)` (CONNECT 443; empty for every entry
today).

- `entry` comes from `agents.Lookup`; `ScriptInput` re-validates it against every catalog rule and refuses
  an entry that comes with the environment, an unknown action or architecture, a non-terminal client, and for
  install/update a withdrawn entry, a missing `tar.gz` artifact for the architecture, or a **hidden** entry
  unless `Job.Hidden` (an explicit developer flag; release builds never set it). Remove needs none of that,
  so every entry stays removable.
- `Job.RemoveData` (remove only) deletes the data directories `agents.RemovableData` computes from the
  catalog: the entry's own, minus every directory it marks shared and every one that is, holds or lies
  inside a directory another catalog entry declares. The caller never names a path.
- `agents.Script` has an unexported field: only `ScriptInput` makes one, so no caller splices text into a job.
- No request field reaches the script: every value is from the validated entry or a closed set.

## Input

One assignment per line, each value quoted (`shellescape.Quote`), in this order (`agents.ScriptInputNames`):

| Variable | Value |
|---|---|
| `AGENT_ACTION` | `install`, `update` or `remove` |
| `AGENT_ID`, `AGENT_NAME`, `AGENT_VENDOR`, `AGENT_STATUS`, `AGENT_VERSION`, `AGENT_COMMAND` | from the entry |
| `AGENT_ARCH` | `arm64` or `amd64` (the script compares it with `uname -m`) |
| `AGENT_LIBC`, `AGENT_MIN_GLIBC` | `glibc`/`musl`/`any`, `2.N` or empty |
| `AGENT_URL`, `AGENT_SIZE`, `AGENT_UNPACKED`, `AGENT_ENTRY` | the pinned artifact (empty for remove) |
| `AGENT_SHA256`, `AGENT_SHA512` | the pinned digests; at least one for install/update |
| `AGENT_PACKAGES` | allow-listed Debian packages, space-separated |
| `AGENT_USERNS`, `AGENT_KEYRING` | `0` or `1` |
| `AGENT_ENV` | `KEY=value` pairs, space-separated (values have no spaces by catalog rule) |
| `AGENT_SETTINGS` | the entry's `settings` as JSON |
| `AGENT_REMOVE_DATA` | `~/…` paths, one per line (remove only) |

Every variable must be declared (an empty value is fine); a missing one is exit 2. The script checks every
value again, at least as strictly as the validator (exit 2 on a mismatch), with the validator's lists held
in fixed variables at its top (`agent_reviewed_commands`, `agent_reviewed_env`, `agent_reviewed_data`,
`agent_system_settings`, `agent_packages_allowed`, `agent_reserved_commands`, the denied variable names,
prefixes and suffixes, `agent_home_roots`, `agent_protected`, `agent_cage_own`);
`TestInstallerListsMatchTheValidator` keeps them equal. So: the launcher name is the entry's reviewed one;
packages are on the allow-list; variables carry the entry's vendor prefix and no denied name; system
settings files are the entry's fixed ones; home settings files lie in a dot folder outside the protected
trees and Cage's folders; data folders to delete are reviewed for the entry or two levels deep outside them;
the URL has no `%`, empty, `.` or `..` segment. `ScriptInput` adds two rules of its own: the artifact
`entry` is `[A-Za-z0-9._+-]` path components not starting with a dot, and the URL is
`https://host/[A-Za-z0-9._~/+-]+`; every shipped entry passes them (tested).

## Output

Everything the script prints goes to **stdout**, so the host reads it in order:

| Line | Meaning |
|---|---|
| `CAGE_AGENT_PROGRESS <phase> <received> <total>` | phases `check` 检查环境, `download` 下载 (bytes, at most one line per percent), `digest` 校验, `unpack` 解包 (bytes), `verify` 验证, `done` 完成; `0 0` when a phase has no byte count |
| `CAGE_AGENT_DONE <id> <version>` | installed, already up to date, or a newer installed version kept; `<version>` is what is installed now |
| `CAGE_AGENT_REMOVED <id>` | removed (also when nothing was installed) |
| `CAGE_AGENT_FAILED <code>` | followed by exactly one line for the person (Chinese), then exit |

Exit status: `0` done, `1` refused or failed (after `CAGE_AGENT_FAILED`), `2` malformed input (a host bug;
code `input`), `143` canceled (code `canceled`, best effort). Anything else is an unexpected failure of a
command; the host shows the last log line.

| Code | When | Line (Codex example) |
|---|---|---|
| `input` | a value fails its pattern | 其他客户端的安装参数无效（AGENT_…），已停止。 |
| `busy` | another agent job holds the lock | 另一个客户端的添加、更新或移除仍在进行，请等它完成后再试。 |
| `withdrawn` | install/update of a withdrawn entry | Codex 已不再受 Super Cage 支持，无法添加或更新。 |
| `arch`, `libc` | wrong architecture, no glibc/musl, glibc older than `min_glibc` | …无法安装。 |
| `keyring` | `requires.keyring` (placeholder until step 2.3) | …需要的密钥环支持尚未完成，暂时无法安装。 |
| `userns` | `requires.userns` and `unshare --user --map-root-user --mount` fails | 这个环境不允许普通用户创建命名空间，… |
| `own-install` | a `<command>` that is not Cage's launcher, in `/usr/local/bin`, `~/.local/bin` or on PATH | 已经安装了 Codex（自行安装，<path>），Super Cage 不会覆盖它。 |
| `in-use` | a process's `/proc/<pid>/exe` is under the entry's tree (update, repair, remove) | 请先结束正在运行的 Codex 终端，再… |
| `settings` | a settings file cannot be merged safely | 无法合并 Codex 的设置文件（…），已停止安装，原有版本保持不变。 |
| `disk` | free space below download + unpacked + 10 % | 客机磁盘空间不足：需要 X MB，当前只有 Y MB。 |
| `packages` | apt failed for `requires.packages` | 无法安装 Codex 所需的系统组件（…）… |
| `gone` | the pinned URL answers 404 or 410 | OpenAI 已不再提供 Codex 0.171.0 的下载。请更新 Super Cage 以获取新的固定版本。 |
| `digest` | a pinned digest does not match, before unpacking or over the bytes unpacked | 安装文件校验失败，已停止安装，原有版本保持不变。 |
| `download` | the download failed after its retries | 下载中断，已保存进度。重试会继续下载。 |
| `archive` | an archive member breaks a rule (below) or the archive is damaged | 安装文件的内容不符合要求（<reason>），… |
| `verify` | the staged program does not run, or does not name the pinned version as a word in the first 4 KB it prints | 安装文件无法在这个客机运行，… |
| `changed` | a file root wrote does not hash as the text this job computed (something raced the job) | 安装文件在写入时被改动（…），已停止安装。请重试。 |
| `canceled` | SIGTERM (stop environment, 取消) | 已取消，已下载的部分会在下次继续使用。 |

Every refusal before the download changes nothing in the guest: `/usr/local/lib/agent-clients` and
`/var/lib/agent-clients` are removed again when no entry is installed.

## Install and update (one path)

1. Input checks; withdrawn, architecture, libc, keyring, user namespaces; own install.
2. `flock` on `/var/lib/agent-clients/.lock` (root-owned, opened read-only); leftover `.stage-*` removed, and
   the temporary names a stopped job can leave (`/usr/local/bin/.<command>.agent-new`,
   `.<id>.json.new`, `.<id>.stat.new`, `.agent-settings-new` beside the entry's system settings files).
3. The installed version is what `<id>/current` points at. Above the pin (semver order: a pre-release is
   older than its release; dpkg is not used): kept (never go back), done. Equal to
   the pin: the receipt is checked against a full re-hash of the tree (entry sha256, tree digest, artifact
   digests, id, version, command, entry); if it matches, the launcher, settings and receipt are refreshed only
   where they differ and the job ends with 已是最新 (no download). Otherwise it is repaired from the pin.
4. In use, settings dry-run, disk, packages (apt through the job's `CAGE_APT_CONFIG`).
5. `cage_download_tool` (resumable, checksum before use; sha512 when pinned, else sha256) into
   `~/.cache/cage/downloads/<digest>/<id>.tar.gz`; then every pinned digest of that file (all files are opened
   with `O_NOFOLLOW`).
6. Archive check pass, then extraction pass (Python `tarfile`, the same rules in both), as the guest user, into
   `sudo mktemp -d /usr/local/lib/agent-clients/.stage-XXXXXX` handed to the user. The extraction reads the
   archive once, hashes every byte it reads (to the end of the file) and refuses with `digest` unless that is
   the pin, so what is unpacked is what was checked; it records the digests of the tree as it writes it. Then
   `chown -hR 0:0` of the **whole stage**, so no same-user process can rename the tree away, and `chmod 0755`
   of it (`mktemp -d` made it 0700; the version check and the digest run as the guest user).
7. Verify the staged, root-owned copy: `(cd / && ulimit -f 64 && env -i HOME=… PATH=/usr/bin:/bin <entry env>
   timeout -k 2 8 <entry> --version)` with its output in a file (never a pipe a child left behind could hold
   open); the pinned version must be a word in the first 4 KB. `env -i` comes before `timeout`, so neither
   the client nor its parent sees the job's proxy variables (the channel credential).
8. The tree digest of the root-owned stage (every file root's, no hard link, no linked root) must equal the
   digests the extraction recorded, or `archive`. Settings planned again, the own-install check again (a
   program of that name may have appeared during the download), commit: move the stage to `<id>/<version>`
   (a same-version tree is moved aside first), `ln -sfn <version> current`, launcher, settings, receipt and
   sidecar, then other versions and the download cache are deleted.

**What root writes** (launcher, receipt, sidecar, system settings) is computed by this job and piped to
`sudo tee` into a temporary name; the written copy must hash as the text did, then it is renamed into place.
No destination, mode or content comes from a file in the job's `/tmp` folder, which the guest user (and a
sandboxed client of that user) can write; destinations are the fixed places plus paths the script derives
from its checked input. The launcher's temporary copy is renamed only after the own-install check passes once
more.

Archive rules (refuse on the first surprise, nothing left behind): only regular files, directories and symlinks
whose target is relative with no `..`; no member below a symlink; no absolute path, `..`, empty or `.`
component, backslash, control character or non-UTF-8 name; no hard link, device, fifo or sparse file; no
setuid, setgid or sticky bit; no name listed twice; the sum of file sizes at most the pinned `unpacked`;
`entry` must be a regular file; at most 100000 members. Files are created with `O_EXCL|O_NOFOLLOW`; owners and
archive modes are never restored: files get 0755 (entry, or any execute bit) or 0644, directories 0755.

## Remove

In-use check; settings files to delete are computed first (only system files the catalog names, that the
receipt says Cage created, whose sha256 still matches the receipt; each path checked again against the
entry's fixed list); then the launcher (only if it is Cage's: marker line **and** the hash the receipt
records or the text this job would write), the tree, the receipt and sidecar, those settings files (and their
directory when Cage created it), and leftover temporary names. With `AGENT_REMOVE_DATA` each `~/…` path is
deleted only if its parent resolves inside the home and neither the path as named nor as resolved through
links is a shared root, one of Cage's folders or a protected tree (a data directory that is itself a link
loses the link, not the target). The partial download for the pin is deleted. When no entry is left, both
feature directories go.

## What the guest holds

| Path | Owner, mode | Content |
|---|---|---|
| `/usr/local/lib/agent-clients/<id>/<version>/` | root, 0755 | the unpacked files |
| `/usr/local/lib/agent-clients/<id>/current` | root | relative symlink to `<version>` |
| `/usr/local/bin/<command>` | root, 0755 | launcher (below) |
| `/var/lib/agent-clients/<id>.json` | root, 0644 | receipt (below) |
| `/var/lib/agent-clients/<id>.stat` | root, 0644 | one line: the stat key |
| `/var/lib/agent-clients/.lock` | root, 0644 | the job lock |
| `/etc/codex/config.toml` (Codex) | root, 0644 | merged settings; other keys kept |

Presence (step 1.4) should count receipts as `/var/lib/agent-clients/*.json`.

**Launcher** (`agent_launcher_text`): line 2 is `# super-cage-agent-launcher <id>`; the script takes a file
for Cage's launcher only when that line is there and its sha256 is the receipt's `launcher_sha256` (or the
text this job writes, after an install stopped before its receipt); then `export KEY='value'` for the entry's `env`; then
`exec '/usr/bin/python3' -I '/usr/local/bin/client_runtime.py' '/usr/local/lib/agent-clients/<id>/current/<entry>' "$@"`,
which removes every `CAGE_*` variable before the client starts (design 5.1). It does **not** check the stat
key yet: that is step 2.1, which can compare `stat -c %i:%s:%Y <entry>` (one fork) with the sidecar's
single line (one `read`) and exit 3 on a mismatch.

**Receipt** (JSON, `schema` 1): `id`, `version`, `arch`, `command`, `artifact` (`{"sha256": …}` and/or
`{"sha512": …}` as pinned), `entry`, `entry_sha256`, `tree_sha256` (every name, type, mode, size and content
of the tree), `stat_key`, `launcher_sha256`, `settings` (`[{file, sha256, created, dir_created}]`),
`installed_at`. The stat key is `<inode>:<size>:<mtime seconds>` of the entry binary, exactly what GNU
`stat -c %i:%s:%Y` prints (design 4.6's format; 3.5's nanoseconds were dropped so the launcher needs no
second command). Nothing the script deletes or writes is taken from a receipt: paths come from the catalog,
the receipt only says which settings file Cage created and its hash. A receipt that is missing, malformed, a
symlink, or disagrees with the files only leads to a repair from the pin.

## Settings

`json` files: parsed, keys set under dotted paths, other keys kept; unchanged files are not rewritten. `toml`
files: line-based edit (top-level keys before the first table, `[table]` sections appended when missing), then
the result is parsed with `tomllib` and every key must read back with its value and type, or the job refuses
before any change. Without `tomllib` (Python before 3.11, e.g. Ubuntu 22.04) only a new file is written; an
existing one is refused. System files (`/etc/…`, allow-listed per entry in the catalog and in the script) are
written by root, home files by the user (new directories 0700, existing file modes kept). The plan
(`settings-plan`: change, existed, mode, sha256 before and after, path) and each merged text
(`settings-text`) come from the script's Python on stdout, never through a file.

A settings file Cage created stays Cage's (`created` in the receipt) only while it is what Cage last wrote: a
later install, update or repair records `created: false` once the person has edited it, so remove keeps it.

Known edges of the merge (review F13; none lets a value inject anything: keys are pattern-checked and TOML
values are read back with `tomllib`):
- TOML is edited line by line: a line inside a multi-line string that looks like the key can be rewritten
  when the real key already has the wanted value.
- JSON is re-serialised when a key changes: `NaN` and `1e400` come back as `NaN` and `Infinity` (not valid
  JSON), duplicate keys collapse, indentation becomes two spaces.
- CRLF line endings become LF; an existing system file's group becomes root.
- Every install, update or repair sets Cage's keys again, undoing a deliberate change to one of them.
- After remove, Cage's keys stay in a settings file the person already had and in home files. They are
  privacy defaults (no update check, no analytics); a Codex installed later by hand inherits them. The
  remove dialog should say so.

## Tests (on a Mac, no VM)

`internal/agents/script_test.go`: every shipped entry × architecture × action, refusals, the input's shape, and
hostile-looking values through bash and sh (nothing runs, values arrive intact).

`internal/cage/agent_install_test.go` (+ `agent_install_unix_test.go`): the script runs as shipped after the real
prelude, inside the maintenance job's subshell-on-stdin shape. Only the fixed paths at its top are rewritten
to a test root (a copy of the script; nothing in the shipped script reads a test variable). Guest commands are
stand-ins on PATH: `sudo` runs the command as the test user, makes `chown` a logged no-op and refuses any
absolute path outside the test root; `uname`, `flock`, `timeout`, `unshare`, `getconf`, `dpkg`,
`dpkg-query`, `apt-get`, `df`, `sleep`, and `sha256sum`/`sha512sum` on macOS. The download is real
`curl` over TLS: test URLs name `*.invalid` hosts (never resolvable), a self-signed certificate for them, and
the test home's `.curlrc` connects them to a local server; the environment is built from scratch.

The review's fixes have their own tests in `internal/cage/agent_install_review_test.go` (racers rewriting the
job's `/tmp` folder at every sudo call, a marked program of the person's, a program appearing during the
download, leftovers, an archive swapped after the digest check, a stage swapped during the version check, a
wrong owner and a hard link, an edited settings file, the version check's environment, directory, waiting and
output bound, pre-release order, a data folder reached through a link into `~/.ssh`) and
`internal/agents/validate_review_test.go` / `installer_lists_test.go`. The rig's `agent_root_uid` is the
test user's: it cannot make root's files.

## Only a real Linux guest can prove (step 1.8)

- `sudo` for real: root ownership after `chown -hR 0:0`, `mktemp -d` as root then handed to the user, modes
  under the job's `umask`, `/etc/codex` created 0755.
- GNU tools: `ln -sfn` replacing `current` atomically, `mv` of the stage on the same filesystem, `flock` on a
  read-only descriptor of a root-owned file, `timeout -k 2 8`, `df -Pk` columns, `readlink` of `/proc/<pid>/exe`
  for a running client (and its ` (deleted)` form), `getconf GNU_LIBC_VERSION`, `dpkg --compare-versions`.
- `stat -c %i:%s:%Y` of the installed entry equals the receipt's `stat_key` and the sidecar (proved here only
  with the BSD equivalent).
- `unshare --user --map-root-user --mount /bin/true` as the guest user: succeeds on Debian 13, and what it does in
  the cloud container (Docker seccomp) decides whether Codex is refused there.
- `systemd-run` exit status and stdout/stderr ordering as the host sees them; SIGTERM on stop with
  `KillMode=control-group` leaving no stage (here: a process-group SIGTERM).
- The real Codex tarball (124 MB, one member) unpacked within the install-time budget; Codex prints
  `codex-cli 0.171.0` for `--version` under `env -i` and starts through `client_runtime.py`.
- Codex reads `/etc/codex/config.toml`; `bubblewrap` from apt through the job's proxy.
- `/usr/local/bin` is on the PATH the Codex helpers see over SSH (design 15).
- The script under the guest's bash 5.2 and Python 3.13 (the Mac ran bash 3.2 and Python 3.15).
- Real `sha256sum`/`sha512sum --check --status` inside `cage_download_tool` (wrapped `shasum` here) and real
  `flock` contention between two jobs (the busy case used a stand-in).
- A download of 8 MB or more, which takes `tool-download.py`'s multi-connection path, through the job's proxy,
  with the progress filter (the test files are smaller, so only the single-connection path ran).
- The `gone` code depends on curl printing `returned error: 404` (or 410) for `--fail`; checked here with
  macOS curl only.

## First real-guest run: the list (keep current)

0. Codex is still hidden, so the run needs a development build started with the switch on. Only a version
   with `-dev` lists or installs hidden entries, and only when the process serving the window has
   `CAGE_AGENTS_HIDDEN=1` in its environment; a release build refuses them at the route and in `ScriptInput`.
   Take a number above the newest release, for example `make cage VERSION=0.1.18-dev.agents` while 0.1.17
   is the newest, and start that `cage` with `CAGE_AGENTS_HIDDEN=1` (`make` needs `npm ci` for the UI;
   this command has not been run). A `-dev` version of the current number is a prerelease older than the
   release, so it would be offered the release as an update, silently from a writable `.app` bundle on macOS,
   and the update would drop the switch. The guest manifest's `app_version` changes but does not mark the
   guest stale (`Current` compares protocol and revision only). The switch took when `GET /api/agents`
   lists `codex`. A hidden client installed from such a build gets 404 on remove from a release build;
   remove it before switching back.
1. Paused at the version check: owner and mode of the stage folder and its tree (all root's). After install:
   `find /usr/local/lib/agent-clients /var/lib/agent-clients -not -user root`, `-perm /7000`, `-type f -links +1`
   all print nothing.
2. `stat -c %i:%s:%Y` of the entry equals the receipt's `stat_key` and the `.stat` line.
3. Inside a Codex workspace-write sandbox: does `touch /tmp/x` succeed, does `sudo -n true` fail, are
   `/tmp/agent-clients.*` folders visible during a job (decides how much F1 mattered; it is fixed either way).
4. `findmnt /tmp`; peak memory and tmpfs use during the 124 MB download; the install time.
5. Real `timeout`: a client that sleeps 30 s fails at about 8-10 s; a client whose child calls `setsid` and
   holds stdout does not hold the job (output goes to a file). `ulimit -f 64` cuts a loud client;
   real `codex --version` with a fresh HOME passes under `ulimit -f 64` and the 8 s limit.
6. From a fake client: `/proc/$PPID/environ` (the `timeout` process) has no proxy entry; a launched
   client's environment has no `CAGE_*` variable and no credential.
7. Two real jobs at once: the second gets `busy` (409 at the route; `busy` from the lock if one came another way).
8. Cancel during download, unpack, the version check and commit: afterwards no stage, no `.codex.agent-new`,
   no `.agent-settings-new`, `current` resolves, no process left in the unit; the next job clears leftovers.
9. After remove: none of `/usr/local/lib/agent-clients`, `/var/lib/agent-clients`, `/usr/local/bin/codex`, or
   `/etc/codex` (when Cage created it) remains.
10. sudo or sudo-rs; `sudo tee` and `sudo mv` under the job's umask; apt goes through the job's proxy;
    `/etc/codex` 0755 and `config.toml` 0644, root's.
11. `getconf GNU_LIBC_VERSION` and `unshare --user --map-root-user --mount /bin/true` in the Lima guest and the
    cloud container.
12. The real GitHub redirect through the proxy, no `~/.curlrc` in the guest home, the 404 wording.
13. Codex reads `/etc/codex/config.toml` and uses the system `bwrap`; `/usr/local/bin` is on the PATH the
    Codex helpers see over SSH.
14. The full install, already-up-to-date, update and remove sequence under bash 5.2 and Python 3.13, with the
    log kept; the job's progress in the window (`setup_progress`, phase `agent`) during the job and none
    once it ends.
15. `POST /api/agents/codex/install` from `cage agent` is refused (`native_transport_required`).
