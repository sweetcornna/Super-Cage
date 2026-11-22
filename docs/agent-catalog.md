# Agent client catalog (其他客户端)

`internal/agents/catalog.json` lists the agent clients Super Cage knows. Claude Code comes with every environment and
its entry only describes how (setup, the cloud image, `guest-tools.sh`, 更新环境工具); nothing installs through the
catalog for it. Every other client is optional: the person adds it in 设置 → 其他客户端.

- **Compiled in, never fetched.** New entries, new pins and withdrawals arrive only with a Super Cage release.
- **Parsed on first use** (`sync.OnceValues`); a session that only uses Claude never parses it.
- **Outside the guest revision.** Nothing of the catalog, and nothing an install job sends, goes into a file the guest
  revision hashes (`internal/cage/guest.go`, `guestRevision`).
- **Admins restrict, never add.** A tenant server may allow or deny entries by id; nobody adds recipes at run time.
- **Installed by one reviewed script.** How a job installs, updates and removes an entry, and what it leaves in
  the guest: `docs/agent-install.md`.

## Statuses

| status | meaning |
|---|---|
| `available` | offered in the section. An optional entry needs its `measured` block (real-guest acceptance) and an idle CPU at most 0.5 % of a core. |
| `hidden` | compiled in, never shown in a release build: waiting for its acceptance. May have no artifact yet. |
| `withdrawn` | no install; an installed copy shows 不再受支持 with 移除. |

## What the validator enforces

`go test ./internal/agents` runs `Catalog.Validate` on the shipped file; `validate_test.go` has one case per rule.
Decoding is strict: unknown fields, data after the catalog, and a `measured` block with a missing or null value are
errors (write `"measured": null` until the entry is measured).

- `schema` is 1; `id` matches `^[a-z][a-z0-9-]{1,31}$` and is unique; `name`, `vendor` set; `summary_zh` at most
  40 characters.
- `kind` terminal or app; `session` existing:claude, existing:codex or generic; a built kind belongs to one entry; an
  app uses generic; `person_only` needs generic (the built kinds take phone turns and agent-opened terminals).
- `with_environment` if and only if every artifact is `builtin`; such an entry carries no version, libc, packages, env,
  settings, updater, telemetry, data dirs, launch or measurements, and says where today's mechanism lives.
- Artifacts: `arm64`/`amd64` only; format `tar.gz` (the only download format implemented; `zip`, `binary`,
  `npm-tgz` and `apt` are added with their installer code and field rules, never as data); https on port 443, no
  credentials, query, fragment or address literal; the host is in `hosts.install`; the URL names the pinned version;
  sha256 (64 hex) or sha512 (128 hex), lowercase; size and unpacked size; `entry` a clean relative path without `..`.
- Optional entries: a version, `libc` glibc/musl/any, `min_glibc` only with glibc; `requires.packages` from an
  allow-list in code; `env` names `^[A-Z][A-Z0-9_]{1,63}$` outside the deny-list (`PATH`, `LD_*`, `*_PROXY`, `HOME`,
  `SHELL`, `BROWSER`, `TZ`, `LANG`, `LC_*`, `NODE_OPTIONS`, `PYTHON*`, `CAGE_*`) with plain values up to 256 bytes;
  `settings` json or toml with a matching extension, either a system file allowed for that entry in code
  (`/etc/codex/config.toml` for codex) or a file inside one of the entry's own data dirs, setting bool, string or
  integer values under dotted keys; `updater.off_by` env (needs env), setting (needs settings) or none (a self-updating
  client then needs a note); `telemetry.how`, and a note when telemetry stays on; at least one sign-in method from the
  closed set; at least one data dir; sources.
- Hosts are lowercase hostnames. Every `hosts.install` host is in today's download channel
  (`internal/workbench/maintenance.go`; a test keeps the copy in `validate.go` from drifting) or in
  `extraInstallHosts` for that entry in code, so a new download host is a visible code change with a security review.
  Login-link hosts are sign-in hosts.
- Data dirs: under `~/`, clean, not a shared root (`~/.config`, `~/.local`, `~/.local/share`, `~/.cache`, Cage's
  `~/.local/share/cage`, ...) and not in a protected tree (`~/.ssh`, `~/.gnupg`, `~/.pki`, `~/.claude`); not nested
  within one entry; two entries may claim the same or nested directories only when both mark them `shared`.
  "Depth 2 or more" in the design counts `~` as level 1, so `~/.codex` is allowed and `~/` is not.
- Project files: clean relative paths.
- Launch: `command` matches `^[a-z][a-z0-9-]{0,31}$`, is unique, is not `cage`, `cage-*`, `claude` or another name Cage
  installs in `/usr/local/bin`, and does not shadow a system program (`sudo`, `git`, `ssh`, ...); `argv[0]` is
  `/usr/local/bin/<command>`; `cwd` is `project`; a title.
- Mark: a drawn mark matching the session (`claude`, `codex`) or a monogram of one or two letters or digits.
- Regions are two-letter codes; terms and sources are https; dates are YYYY-MM-DD; an available entry names its
  reviewer and review date.

## Adding an entry or bumping a pin

A pull request carries:

1. **Sources** with read dates in `review.sources`: vendor docs, release page or API, manifest URL.
2. **Digests from the vendor**: its manifest, checksum file or release API. Download each artifact, compute the digest
   yourself and compare; record the URL, the date and both values in `review.notes`. When the vendor publishes no
   checksum, compute it from the official URL through two different networks, record it as trust-on-first-use in
   `review.notes`, and say so in the pull request. Never run or unpack the artifact outside a scratch directory; read
   member names and sizes with `tar -tzvf`.
3. **Switches and hosts**: the updater and telemetry switches and the four host lists, checked on a real guest (the
   entry's acceptance step).
4. **Terms and regions**: what the vendor allows (`person_only`) and where it is not offered.
5. **Measurements**: disk, idle RSS, idle CPU, install time from `scripts/measure-agent.sh` on arm64 and amd64, in
   `measured`. Only then may the status become `available`.
6. **Security review** for a new install format or a new download host (both are code changes); a pin bump of an existing
   format and host needs only this checklist.
7. **Liveness**: the pinned URL answers HEAD 200 at release time (vendors drop old builds).

Never edit `internal/cage/guest-tools.sh`, `tool-versions.env` or a script the start script embeds for an entry: that
changes the guest revision and closes the network gate on every guest until 更新环境组件.

## Shipped entries

| id | status | version | vendor checksum | checked |
|---|---|---|---|---|
| `claude-code` | available (comes with the environment) | resolved by 更新环境工具 | n/a | — |
| `codex` | hidden until step 1.8 | 0.171.0 (`rust-v0.171.0`) | sha256 from the GitHub release API asset digest; `codex-package_SHA256SUMS` does not cover these tarballs | 2026-11-22, both architectures matched |
| `antigravity-cli` | hidden until steps 2.3 and 2.4 | 1.5.14 | sha512 from the vendor's per-platform manifest (no signature) | 2026-11-22, both architectures matched; storage md5 matched too |
