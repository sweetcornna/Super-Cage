# Agent client installer (其他客户端, step 1.2)

`internal/cage/agent-install.sh` installs, updates and removes one optional entry of the agent catalog
(`docs/agent-catalog.md`) inside a Linux guest. It runs as the guest user in the maintenance job and uses sudo
only for the root-owned tree. It is **not** part of any script the guest revision hashes: it travels with each
job, so adding a client never closes the network gate of every guest.

## How a job is built (host side, step 1.3)

```go
input, err := agents.ScriptInput(entry, arch, agents.Install, nil) // or Update, or Remove with data dirs
script := cage.AgentInstallScript(input)                          // prelude + input + installer
// run with withToolsNetworkMonitor(func() error { return runMaintenanceScript(ctx, …, script, …) })
```

- `entry` comes from `agents.Lookup`; `ScriptInput` re-validates it against every catalog rule and refuses
  an entry that comes with the environment, an unknown action or architecture, a non-terminal client, and for
  install/update a withdrawn entry or a missing `tar.gz` artifact for the architecture. Remove needs neither,
  so a withdrawn entry, or one that dropped this architecture, stays removable. Visibility (`hidden`) is the
  caller's decision.
- `removeData` (remove only) lists data directories to delete. Each must be one of the entry's `data_dirs`
  paths; the host leaves out shared ones still used by another installed entry.
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

The script checks every value again with patterns no looser than the catalog's (exit 2 on a mismatch).
`ScriptInput` adds two rules of its own: the artifact `entry` is `[A-Za-z0-9._+-]` path components not starting
with a dot, and the URL is `https://host/[A-Za-z0-9._~/%+-]+`; every shipped entry passes them (tested).

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
| `digest` | a pinned digest does not match | 安装文件校验失败，已停止安装，原有版本保持不变。 |
| `download` | the download failed after its retries | 下载中断，已保存进度。重试会继续下载。 |
| `archive` | an archive member breaks a rule (below) or the archive is damaged | 安装文件的内容不符合要求（<reason>），… |
| `verify` | the staged program does not run or reports another version | 安装文件无法在这个客机运行，… |
| `canceled` | SIGTERM (stop environment, 取消) | 已取消，已下载的部分会在下次继续使用。 |

Every refusal before the download changes nothing in the guest: `/usr/local/lib/agent-clients` and
`/var/lib/agent-clients` are removed again when no entry is installed.

## Install and update (one path)

1. Input checks; withdrawn, architecture, libc, keyring, user namespaces; own install.
2. `flock` on `/var/lib/agent-clients/.lock` (root-owned, opened read-only); leftover `.stage-*` removed.
3. The installed version is what `<id>/current` points at. Above the pin: kept (never go back), done. Equal to
   the pin: the receipt is checked against a full re-hash of the tree (entry sha256, tree digest, artifact
   digests, id, version, command, entry); if it matches, the launcher, settings and receipt are refreshed only
   where they differ and the job ends with 已是最新 (no download). Otherwise it is repaired from the pin.
4. In use, settings dry-run, disk, packages (apt through the job's `CAGE_APT_CONFIG`).
5. `cage_download_tool` (resumable, checksum before use; sha512 when pinned, else sha256) into
   `~/.cache/cage/downloads/<digest>/<id>.tar.gz`; then every pinned digest of that exact file.
6. Archive check pass, then extraction pass (Python `tarfile`, the same rules in both), as the guest user, into
   `sudo mktemp -d /usr/local/lib/agent-clients/.stage-XXXXXX` handed to the user; then `chown -hR 0:0`.
7. Verify the staged, root-owned copy: `timeout -k 2 8 env -i HOME=… PATH=/usr/bin:/bin <entry env> <entry> --version`
   must print the pinned version. The job's proxy variables (with the channel credential) never reach it.
8. Tree digest of the stage, settings merged again, commit: move the stage to `<id>/<version>` (a same-version
   tree is moved aside first), `ln -sfn <version> current`, launcher, settings, receipt and sidecar, then other
   versions and the download cache are deleted.

Archive rules (refuse on the first surprise, nothing left behind): only regular files, directories and symlinks
whose target is relative with no `..`; no member below a symlink; no absolute path, `..`, empty or `.`
component, backslash, control character or non-UTF-8 name; no hard link, device, fifo or sparse file; no
setuid, setgid or sticky bit; no name listed twice; the sum of file sizes at most the pinned `unpacked`;
`entry` must be a regular file; at most 100000 members. Files are created with `O_EXCL|O_NOFOLLOW`; owners and
archive modes are never restored: files get 0755 (entry, or any execute bit) or 0644, directories 0755.

## Remove

In-use check; settings files to delete are computed first (only system files the catalog names, that the
receipt says Cage created, whose sha256 still matches the receipt); then the launcher (only if it is Cage's),
the tree, the receipt and sidecar, those settings files (and their directory when Cage created it). With
`AGENT_REMOVE_DATA` each `~/…` path is deleted only if its parent resolves inside the home and it is not a
shared root or protected tree (a data directory that is itself a link loses the link, not the target). The
partial download for the pin is deleted. When no entry is left, both feature directories go.

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

**Launcher** (`agent_launcher_text`): line 2 is `# super-cage-agent-launcher <id>` (how the script tells
Cage's launcher from a person's own program); then `export KEY='value'` for the entry's `env`; then
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
existing one is refused. System files (`/etc/…`, allow-listed per entry in the catalog) are written by root,
home files by the user (new directories 0700, existing file modes kept).

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
