# Super Cage: how the app updates itself (design)

Branch `app-update-policy` (stacked on `super-cage-redesign`). Owner's words: "做一个点击升级按钮，不会自动更新，只能用户自己手动更新，或者用户可以开启自动静默更新（也就是在机器没有任何任务闲置的情况下自动更新）"; and "当然要保证整个性能优秀，占用小".

## 0. Problem, in one paragraph

Today "upgrade" is several unrelated steps (download+install in the shell, then the user restarts the environment, then on native guests clicks 更新环境组件 before the gate opens), and on macOS and Windows the in-app updater does not even see this project's releases (section 2.1). We want: **one button, one click, the whole chain**, nothing installed without that click, and an opt-in **silent mode** that runs the same chain only when the machine is provably idle, decided by the workbench under its operation lock, fail-closed. Everything added must cost nothing for people who never turn silent mode on, and very little for those who do.

Terms used below:

| term | meaning |
|---|---|
| **manual** (默认) | Nothing is downloaded or installed without a click. A *check* that only learns "a newer version exists" is allowed; it is what makes the button appear. |
| **silent** (自动静默更新, off by default) | The whole chain may run with nobody watching, only when the workbench says the machine is idle (section 4) and only for an eligible release (section 8). |
| **chain** | download → verify → decide under lock → stop environment → install → relaunch → restore environment → sync components (and tools when required) → gate check → "已升级到 x.y.z". |
| **shell** | the native app: macOS `desktop/Updates.swift` + `UpdateInstaller.swift`, Windows `desktop/windows/AppUpdater.cs` + `UpdateInstaller.cs`, Linux `desktop/linux/cage-desktop.c`. |
| **workload** | the local controller process (`cage serve`, profile `~/.claude-cage`), owner of `s.op`, jobs, terminals, phone runs, the guest. |
| **gateway** | the desktop-entry controller (profile `~/.claude-cage/desktop-entry`), which serves the window and forwards `/api/*` to the selected connection (local workload or a cloud workspace). |

## 1. Audit: what changes installed software without a click today

"Today" = branch `super-cage-redesign` at 484da37. "Check" rows say what is sent and to whom. Rows marked **unverified** need a real guest; the command to verify is given.

| # | Software | What it does today | Under **manual** it must | Under **silent** it may | Status |
|---|---|---|---|---|---|
| 1 | Desktop app, macOS/Windows: check | UI asks the shell (`native("appUpdate",{operation:"check"})`) at window load, on focus/visibility, and every 6 h while the window is visible (5 min after a failure); the shell runs `cage app-update check`: one `GET https://api.github.com/repos/sweetcornna/Super-Cage/releases?per_page=100` (headers Accept, X-GitHub-Api-Version; Go default User-Agent, no product name — enforced by `about_test.go`/`network_test.go`; no cookies, no identifiers), host network/system proxy first, the selected Super Cage proxy only after a host connection failure. | Unchanged: allowed, it only reveals a version. | Same request; additionally from the workload's existing hourly ticker at most every 6 h while the window is hidden (section 8.1). | verified (code) |
| 2 | Desktop app, macOS/Windows: download + install | Only from the button (`AppUpdateButton` → `install`). On macOS the click also stops the environment without a second confirmation (`prepareEnvironment(stop:false)` → `stop_required` → `prepareEnvironment(stop:true)`). | Only after a click **and** a confirmation that lists what will be interrupted (section 3.1). | Only when the workbench answers "idle, eligible, not failed before" under its lock (section 4.4). | verified (code) |
| 3 | Desktop app, Linux (.deb) | `check` finds the .deb, the shell reports `manual` with the release page. Never installs. | Unchanged (manual-only platform). | Silent is not offered on Linux (system package manager owns installation). | verified (code) |
| 4 | 设置 → 关于 → 检测更新 | Click only: `GET .../releases`, `GET .../commits/main`, `GET .../compare/<a>...<b>` (same transport, same UA rule). | Unchanged. | Unchanged. | verified (code) |
| 5 | Guest components, Linux guest (Lima/container) | Re-applied from the **installed app's own bundle** on every environment start (`cage up` → `StartGuestServices` → `StartScript` → `componentStartScript` + manifest). An app update therefore takes effect at the next start, which the user clicks. Attaching to an already running VM with an older manifest closes the gate (`errGuestUpdateRequired`) and the start job then runs `synchronizeEnvironment` (runtime.go:578). No download from the internet except packages the start script finds missing (`apt-get install` through the scoped maintenance proxy). | Only as part of a start or 更新环境组件 the user clicked, or as a step of an update chain the user clicked. | As a step of the silent chain. | verified (code) |
| 6 | Guest components, native macOS / Windows guests | Only by 更新环境组件 (`maintainNativeComponents`). After an app update a drifted guest keeps the gate closed (`errMacOSComponentUpdate`, `guestUpdateRequired`) until the user clicks it. **This is the "外网未放行 after upgrade" the owner hit.** | Becomes a step of the chain the user clicked. | Step of the silent chain. | verified (code) |
| 7 | Guest tools: gh, code-server, mise | Pinned in `internal/cage/tool-versions.env` with SHA-256; installed by setup and by 更新环境工具 (click). | Unchanged. The chain runs 更新环境工具 only if the new app's pinned version is above the installed one. | Same, inside the silent chain. | verified (code) |
| 8 | Guest tool: **Claude Code CLI** | Installed at **`@latest`** (npm) at setup / 更新环境工具; **not pinned**. Its own **self-updater is not disabled**: `DISABLE_AUTOUPDATER=1` appears only on version probes, and `cage_configure_claude_npm_prefix` exists so that the vendor updater writes to the same prefix. So Claude Code updates itself in the guest whenever the gate is open. | **Owner decision** (section 1.1): disable the vendor updater (managed setting `env.DISABLE_AUTOUPDATER=1` in the guest) and surface "Claude Code 有新版本" next to 更新环境工具; or document it as the vendor's own channel. Not changed in this pass. | Same decision; silent chain may run 更新环境工具 when idle. | verified (code); runtime behaviour **unverified**: `claude doctor` and `ls ~/.local/lib/node_modules/@anthropic-ai/claude-code/package.json` before/after a day with the gate open |
| 9 | Guest tool: Codex CLI | Not installed by Cage (user installs). Codex's `check_for_update_on_startup` is passed through unchanged (`codex-account.py`): a notice, Codex does not install by itself. | Allowed (notice only). | Same. | verified (code); Codex self-install behaviour **unverified** |
| 10 | Workbench browser (Ubuntu guest: snap Chromium `latest/stable`) | Installed only when missing (`guest-services.sh`); a normal start never runs `snap refresh`. But **snapd's own auto-refresh is not held**, so the snap refreshes itself (default 4×/day) whenever the gate is open. Debian guests use apt `chromium` (installed when missing). | **Owner decision**: `snap refresh --hold chromium` (or `snap set system refresh.hold=forever`) and refresh it in 更新环境组件; not changed in this pass. | Refresh inside the silent chain. | **unverified**: in the guest `snap refresh --time`, `snap changes` |
| 11 | Guest OS packages (Ubuntu 22.04) | Nothing in Cage configures `unattended-upgrades` or `apt-daily*.timer`, so the image default applies (Ubuntu cloud images enable security unattended-upgrades). | **Owner's call (security)**: keep security updates on (recommended, they are what protect an internet-facing browser), or disable and fold them into 更新环境组件. Proposal only. | Same. | **unverified**: `systemctl list-timers 'apt-daily*'`, `cat /etc/apt/apt.conf.d/20auto-upgrades`, `/var/log/unattended-upgrades/` |
| 12 | code-server (editor) and its extensions | Started with `--disable-update-check`. `extensions.autoUpdate` is not set, so editor extensions the user installed update themselves (VS Code default) when the gate is open. | Owner decision: write `"extensions.autoUpdate": false` into the editor defaults. Not changed. | — | **unverified**: `~/.local/share/code-server/User/settings.json`, extension versions over a day |
| 13 | Browser extensions (workbench Chromium) | Imported from the host by the user; no `update_url` handling found in `browser-extensions.mjs` (loaded as files) → no self-update. | Unchanged. | — | verified (code), **unverified** runtime: `chrome://extensions` shows no "update" source |
| 14 | Claude Desktop (Linux guest) | Installed/updated only by its install job (click); apt source `downloads.claude.ai/claude-desktop/apt/stable` is added, and unattended-upgrades' default origins (Ubuntu security) do not include it. | Unchanged. | — | verified (code); app-internal updater **unverified** |
| 15 | Claude Science (Linux guest) | Version, build and SHA-256 pinned; `auto_update = false` written into its config. | Unchanged (already manual). | — | verified (code) |
| 16 | Native macOS guest: Chrome (Keystone), Claude.app (Squirrel), macOS Software Update | Nothing disables them. | Owner decision (proposal). | — | **unverified**: `defaults read com.google.Keystone.Agent`, `softwareupdate --schedule` in the guest |
| 17 | Native Windows guest: Windows Update, browser updaters, Claude Desktop | Nothing disables them. | Owner decision (proposal; Windows Update cannot be fully disabled on Home). | — | **unverified** |
| 18 | Android app | Checks GitHub releases at most every 12 h when the app opens or returns to the foreground; downloads only on a tap; Android's installer asks the user. | Already manual. | Not offered. | verified (mobile/README.md, AppUpdate.java) |
| 19 | iOS app | TestFlight / App Store; the OS may auto-update apps per the user's iOS settings. Outside Cage's control. | — | — | — |
| 20 | Cloud gateway and workspace images | Updated by whoever runs the server (deploy/tenant/README.md). | Proposal (section 10). | — | verified (docs) |

### 1.1 What "manual" changes in this pass, and what is left to the owner

This pass pins rows 1–7 (the app and what follows it) with tests. Rows 8, 10, 11, 12, 16, 17 are vendor or OS updaters inside the guest; turning them off changes the guest scripts (and therefore `guestRevision`, which forces every existing guest through a component sync), and for row 11 it removes security updates. They are listed with a recommendation and left to the owner:

- **Recommended**: hold the vendor updaters that change *tools the user works in* (Claude Code: managed setting `env.DISABLE_AUTOUPDATER`; snap Chromium: `refresh.hold`; editor extensions: `extensions.autoUpdate=false`) and move them under 更新环境工具, which already exists and is a click; **keep OS security updates on** (row 11) because they protect the guest browser and do not change what the user works with.

## 2. Findings that change the plan

### 2.1 macOS and Windows never see this project's releases
`catalog.archiveNames` accepts `Super-Cage-<v>-macos-arm64.dmg` and `Super-Cage-<v>-windows-amd64-setup.exe` (plus the pre-rename `Cage-` prefix). The published 0.1.15 assets are `Super-Cage-0.1.15-macos-arm64-signed.dmg` and `Super-Cage-0.1.15-windows-amd64-setup-unsigned.exe` (verified with `gh release view v0.1.15`). So `Select` skips every release on these two platforms and `cage app-update check` reports **`current` ("已是当前更新通道的最新版本") although a newer version exists**: the button never appears, long before `spctl` could reject the DMG. Only Linux sees its `.deb` and correctly reports `manual`.

This pass changes the check (not the trust rules): when the newest eligible release has no package this platform may install in-app, `check` reports `manual` with the version and the exact reason (未公证的 macOS 包 / 未签名的 Windows 安装包 / 没有本平台安装包), so the existing button opens the release page. Making `-signed.dmg` installable is part of the notarization decision (section 9) and is **not** done here.

### 2.2 The "gate stays closed after an upgrade" claim, per guest type
- **Linux guest (Lima, container):** every start re-applies the components of the installed app and writes the manifest, so after an app update the first start brings the components current; the gate opens after the normal network check. The gate stays closed only when the new app **attaches to a VM that is still running** with an old manifest (`attach` → `errGuestUpdateRequired` → `closeProxyGate("环境组件需要更新，外网保持关闭")`). Today's installer stops the VM first, so this happens when the old app was quit without stopping the environment, or with container environments that outlive the controller.
- **Native macOS / Windows guests:** components change only through 更新环境组件; a drifted guest keeps the gate closed (`guestUpdateRequired`) until the user clicks it. This is the case the owner hit.
- Consequence for the chain: "sync components" = *start the environment (Linux)*, plus *run 更新环境组件 when the workbench reports `environment_update_required`* (native guests, or a Linux VM that was still running).

### 2.3 Two processes, and `/api/*` is forwarded
The window talks to the **gateway**, which forwards `/api/*` to the selected connection — possibly a **cloud workspace**. The gateway has no account store (`s.account == nil` ⇒ always locked), and `/api/client/about` is answered *before* the account gate. Therefore:
- The update policy, status and decision live in the **local workload** (it owns `s.op`, jobs, terminals, phone runs and the account lock). The window reaches them through the gateway's normal forwarding when the local connection is selected.
- The gateway **never forwards `/api/app-update*` to a cloud workspace** (answers 409 `local_required`): a cloud server must not learn or decide anything about this machine's app.
- The shell reaches the workload with the launcher token (control record), exactly like `prepare-update` does today.

### 2.4 Where the mode is stored
`Preferences` (`workbench.json`) is rewritten wholesale by settings, projects, project removal and profile restore, and replaced from client input in `proposedSettings`. A mode stored there could be reset by an old client's settings POST or switched on by a restore. The mode therefore lives in **`<profile>/updates/policy.json`**, next to the update state the shell and `cage app-update` already share (`updates/`), and has **exactly one writer**: `POST /api/app-update/policy`, which requires the trusted desktop view and an unlocked account, and which the agent transport refuses. A missing or unreadable file means **manual**.

### 2.5 Today's stop has a gap
`prepare-update --stop` runs `POST /api/stop` (a job that cancels whatever job is running), polls, checks terminals, then `POST /api/handoff`. Between the stop job finishing and the handoff, `s.op` is free and anything can start. The new install decision is **one operation under `s.op` with admission sealed** (section 4.4).

## 3. The chain, step by step

Actors: **S** shell, **C** `cage` CLI child of the shell, **W** local workload (workbench), **G** guest, **H** installer helper (macOS `CageUpdateInstaller`, Windows `Cage.exe --install-update`).

### 3.1 Click path, local environment (macOS and Windows)

| # | Step | Who | Notes / failure exit |
|---|---|---|---|
| 1 | Button visible (`available`) | S via C `app-update check` | as today; `manual` when not installable (2.1) |
| 2 | Click → confirmation: what will be interrupted now | UI asks W `GET /api/app-update/interruptions` | list of named blockers (section 4); "继续升级" / "稍后". Must-wait blockers (a running job, storage recovery) disable "继续升级" and say what to wait for. |
| 3 | Download with progress | S → C `app-update download` | resumable, digest-pinned (as today). Failure: `failed` + retry; nothing changed. |
| 4 | Verify | S (macOS: digest, hdiutil, signature, team, version, architecture, nested code, Gatekeeper; Windows: digest, Authenticode, same publisher) | failure: `failed`, archive removed, nothing changed |
| 5 | Helper ready, backup of the current app | H | failure: helper aborted, nothing changed |
| 6 | **Decide under lock + stop** | C `prepare-update --json --stop --app-update <v> --trigger click --confirmed <ids>` → W `POST /api/desktop-local/app-update-handoff` | W: TryLock `s.op`, seal admission, re-read all blockers (incl. one guest read), refuse with `confirm_required` if an interruptible blocker is not in the confirmed set, `must_wait` if a job runs; else write attempt + chain record (environment snapshot), stop the environment, stop the proxy, hand off. Failure to stop: chain `failed(stopping)`, attempt failed, environment restarted if it was running, old app keeps running. |
| 7 | Gateway released | C (`releaseDesktopEntry`) | as today |
| 8 | COMMIT, quit, replace, verify, relaunch | S, H | as today; rollback restores the old app (H). |
| 9 | Launch acknowledged | new S (`markLaunchReady`) | as today; helper rolls back if the new app does not acknowledge in 45 s |
| 10 | Chain resumes | new W on startup reads `chain.json` | `installing` + own version = target ⇒ `relaunched`; own version = source ⇒ `rolled_back` (old binary after rollback) |
| 11 | Restore environment | W submits the ordinary `start` job if it was running | Linux: this re-applies components. Failure ⇒ `failed(restoring)`, next step 启动环境. |
| 12 | Sync components | W: if `environment_update_required` ⇒ `environment-update` job | native guests, or a still-running Linux VM. Failure ⇒ `failed(components)`, next step 更新环境组件. |
| 13 | Tools (only if required) | W: pinned tool (gh, code-server, mise) below the new app's pin ⇒ `environment-tools` job | one version probe at this point only. Failure ⇒ `failed(tools)`, next step 更新环境工具. |
| 14 | Gate check | W: the start job already runs `checkNetworkIfWanted`; W reads the published gate | gate open, or paused by the user's own saved intent ⇒ ok. Else `failed(network)`, next step 重新检查网络. |
| 15 | "已升级到 x.y.z" | W chain `done`, attempt ok | UI shows the release notes link (`https://github.com/sweetcornna/Super-Cage/releases/tag/v<x.y.z>`) and reopens the recorded terminal tabs (Claude/Codex conversations resume by id). |

### 3.2 Silent path (macOS and Windows, local environment)
Same steps, with: step 1 done by W's hourly tick when the window is hidden; step 2 replaced by the idle decision (section 4) recorded in `updates/silent-request.json`; the shell, watching `updates/` only while silent is on, runs 3–9 with `--trigger silent` and relaunches **without activating** the window; at step 6 W re-decides under the lock and refuses (`busy`, `not_held`, `user_active`, `failed_before`, `not_eligible`, `manual_mode`) unless everything still holds. A refusal before step 6 changes nothing; the verified download is kept for the next idle window.

### 3.3 Linux desktop (.deb): manual-only
Steps 1–2 as above; the button reads "查看新版本" and opens the release page with the exact command (`sudo apt install ./Super-Cage-<v>-linux-amd64.deb` after 停止环境并退出). After the user installs and reopens, W has no chain record (the package manager replaced the app), so step 10 finds nothing; the first start re-applies components (2.2). Silent is not offered: the system package manager owns installation and needs elevation.

### 3.4 Desktop connected to a cloud workspace
- The cloud workspace keeps running during the local update; its agent turns are **not** interrupted and are not local blockers. What the local update interrupts: the window's streams and any transfer through the gateway.
- Click: as 3.1; at step 6 the workload has no running VM (or is not running at all) and the gateway is released as today; after relaunch the window reconnects to the same cloud connection (the gateway keeps the selection in `desktop-entry`). Steps 11–14 apply only to the local environment if it was running.
- Silent: needs the local workload's hourly tick. If the user only ever uses cloud connections the workload may not be running, and silent mode does not run (the button still works). Proposal in 11.

### 3.5 Manual-only platforms and steps

| Platform / case | Why | What the button does |
|---|---|---|
| Linux .deb | system package manager, needs root | opens release page + exact command |
| macOS until the notarization decision | releases are `-signed.dmg`, unnotarized; catalog and Gatekeeper both refuse | `manual`: "此版本的 macOS 安装包未经 Apple 公证，暂不能在应用内安装。请下载 DMG，退出 Super Cage 后替换应用。" |
| Windows unsigned installers (`-setup-unsigned.exe`) | no Authenticode publisher to compare | `manual`: "此版本的 Windows 安装包没有发布者签名，不能在应用内安装。请保存工作、停止环境并退出后运行安装包。" |
| Windows installed to a folder the user cannot write (per-machine install) | NSIS `/S` would need elevation (UAC) | click: allowed, UAC prompt is the user's click; silent: refused (`elevation_required`) and the button stays |
| Intel / trial / client-only macOS builds, Windows client-only | separate packages (already `manual` in the shells) | unchanged |
| Development builds without a team identifier | no publisher to pin | unchanged (shell refuses) and silent never offered |

## 4. The idle registry

### 4.1 Model
A **blocker** is `{id, kind, label, interruptible, unknown}`; `label` is the Chinese text the UI shows ("终端「Claude 2」里还有命令在运行（make）"). A **feeder** is a function the workbench registers per source; it returns blockers from state the workbench already holds. The registry adds:
- **unknown is busy**: a feeder that errors, panics or times out produces `{kind:"unknown", label:"无法确认<source>的状态", unknown:true}`; the decision treats it like any blocker.
- an **activity fingerprint**: a hash of monotonic facts (job id, terminal ids and bytes written, phone-run ids, upload/download ids, viewer count, phone room, an activity counter that admission points bump). Idle has **held** from T0 to T1 only if both snapshots were idle and the fingerprint did not change: anything that started *and ended* between two looks changes the fingerprint, so no polling is needed to prove continuity.
- **external holds**: `Hold(id, label)` returns a release function; later features (the SSH entry into the guest being designed) register a blocker for as long as their session lives.

### 4.2 Blockers and their sources

| id | label (UI) | source (already in memory) | cost | click path | silent |
|---|---|---|---|---|---|
| `job:<kind>` | 「启动环境」等操作正在进行 | `s.jobs.snapshot()` | memory | **must wait** | busy |
| `storage` | 环境存储待恢复 | `s.storageRecoveryError` | memory | **must wait** | busy |
| `turn:phone:<id>` | 手机发起的 Claude 轮次正在运行 | `s.mobileCLI.active` (`run.cancel != nil`) | memory | interrupt | busy |
| `terminal:<id>` | 终端「<title>」仍在运行 / 里还有命令在运行（<cmd>） | `s.terminals` live sessions; foreground command from the decision-time guest read | memory + 1 guest read at decision | interrupt | busy unless the guest read proves the shell is at its prompt |
| `agent:external` | 有不属于工作台终端的 Claude/Codex 进程在运行 | decision-time guest read (`claude`/`codex` processes outside workbench shells) | same read | interrupt | busy |
| `app:claude-desktop`, `app:claude-science` | Claude Desktop / Claude Science 正在运行 | decision-time guest read (`systemctl is-active`) | same read | interrupt | busy |
| `viewer:display`, `viewer:browser` | 正在查看环境画面 / 浏览器画面 | `desktopViews`, `browserViews` shown viewers | memory | interrupt | busy |
| `audio` | 正在播放环境声音 | `s.audioClients` | memory | interrupt | busy |
| `download:<id>` | 浏览器下载「<name>」正在保存到电脑 | `s.nativeDownloads` not done | memory | interrupt | busy |
| `upload:<id>` | 手机正在上传「<name>」 | `s.mobileUploads.active` | memory | interrupt | busy |
| `phone` | 已配对的手机正在连接 | `s.phones` active room | memory | interrupt | busy |
| `input` | 最近 30 分钟内有键盘或鼠标操作 | host idle time read once at decision (macOS `ioreg -c IOHIDSystem` HIDIdleTime; Windows `GetLastInputInfo`); unreadable ⇒ unknown | one read at decision | n/a | busy |
| `external:<id>` | (given by the feature) | `Hold` | memory | interrupt | busy |
| `vm` | 无法确认环境进程状态 | `controllerVMOwners` error (existing probe used by handoff) | existing probe | **must wait** | busy |

Guest read (Linux guests only; one `guestQuick` call, ≤ 4 s, at decision time and when the confirmation list is opened, never on a timer): for each session leader whose environment has `CAGE_WORKBENCH=1`, compare the terminal's foreground process group with the shell's own; list `claude`/`codex` processes not inside those sessions; `systemctl is-active` of the Claude Desktop and Claude Science units. Native macOS/Windows guests have no such read yet: a live terminal there is `unknown` ⇒ busy (silent waits until the terminals are closed or the environment is stopped). Until host terminals carry a tag that maps them to guest sessions (build plan), any non-shell foreground command, including an idle Claude TUI, counts as busy.

### 4.3 What idle means for silent mode
1. mode is silent (read at every decision; switching to manual cancels everything pending);
2. **no blocker at all**, unknown included;
3. idle **held for 60 min**: two consecutive hourly ticks saw idle with the same fingerprint;
4. host input idle **≥ 30 min** at decision time (known, not unknown);
5. the release is eligible (section 8.2) and not failed before;
6. **decided again under `s.op` at the moment of installing**, with admission sealed, and the fingerprint still equal to the one recorded when the silent request was made.

### 4.4 The decision under the lock ("may I install now?")
`POST /api/desktop-local/app-update-handoff` (launcher token; never reachable by the agent transport, phones, or the browser):
1. `s.op.TryLock()`; held ⇒ `busy` ("另一个操作仍在进行").
2. **Seal admission** (`s.appUpdateSealed = true` under `s.mu`): new jobs (`submitJob`), new terminals, new phone turns and new uploads are refused with "正在准备升级，请稍候" while the decision runs. Reads keep working (unlike `s.closing`, which fails every request).
3. Read every blocker (memory) and the guest read (≤ 4 s, counts as unknown on timeout).
4. Apply `Decide` (pure, section 5.3). Refusal ⇒ unseal, unlock, reply 409 with the blockers.
5. Allowed ⇒ write the attempt (`attempts.json`) and the chain record (`chain.json`, step `stopping`, environment snapshot) **before** stopping anything, stop the environment (still holding `s.op`), verify no VM owner remains, step `installing`, stop the proxy, set `s.closing`, reply `{"status":"prepared"}`, shut down.
6. Stop failure ⇒ chain `failed(stopping)`, attempt failed, unseal, restart the environment if it was running (a `start` job), reply 409.

### 4.5 Cost
- **Manual mode**: no new timer, no new file read on any path except the hourly tick checking an in-memory mode; no new requests. The window's existing check cadence is unchanged.
- **Silent mode**: the existing hourly ticker evaluates in-memory feeders (microseconds) and, every 6th tick, the same GitHub request as the window's check. One guest read (≤ 4 s, one python3 process in the guest) and one host idle read happen only at a decision. The shell watches `updates/` with a kernel file watch (no polling). No new process, no new always-on loop.

## 5. State machines

### 5.1 What the button shows (shell phase + workbench status)
Shell phases (existing): `idle current available manual waiting checking downloading downloaded verifying ready installing failed`. The UI module (`ui/src/app-update-state.mjs`) combines them with the workbench status (`GET /api/app-update`): mode, pending version, waiting-for blockers, last attempt, chain step. New *derived* states: `confirm` (list open), `waiting_idle` (silent, pending, blockers listed), `must_wait`, `restoring`/`syncing`/`checking_network` (chain after relaunch), `upgraded` (chain done), `chain_failed` (with step and next action).

### 5.2 Chain record (`updates/chain.json`, schema 1)
Written by the **old** binary at the decision, read by the **new** binary after relaunch, and read again by the **old** binary after a rollback. Fields: `schema, id, from, to, trigger, step, environment{running, workspace, terminals[{title, kind, workspace, conversation}]}, started_at, updated_at, failed_step, error`.

```
(no record) --decision allowed (W, under s.op)--> stopping
stopping   --env stopped, no VM owner--------> installing            (old W, before replying "prepared")
stopping   --stop failed------------------------> failed[stopping]     (+ restart env if it was running)
installing --W starts, version == to-----------> relaunched
installing --W starts, version == from---------> rolled_back -> failed[installing] (+ restart env if it was running)
installing --W starts, other version-----------> attention (show the button; do nothing)
relaunched --env was running-------------------> restoring             (submit "start")
relaunched --env was stopped-------------------> done
restoring  --start ok, components current------> tools
restoring  --start ok, update required---------> components            (submit "environment-update")
restoring  --start failed----------------------> failed[restoring]
components --ok----------------------------------> tools
components --failed------------------------------> failed[components]
tools      --no pinned tool below the new pin-> network
tools      --update needed, ok----------------> network               (submit "environment-tools")
tools      --failed------------------------------> failed[tools]
network    --gate open / paused by the user's saved intent -> done
network    --gate closed with an error---------> failed[network]
any        --unknown schema or step------------> attention
```
- Every step is idempotent: a restart in the middle re-enters the same step (start only if not running, sync only if still required).
- Every `failed[*]` and `done` finishes the attempt in `attempts.json` (result, step, reason). An attempt that **began but never finished** (crash, power loss, helper killed) reads as failed for that version.
- Silent mode never retries a version with a failed or unfinished attempt; the button shows the reason and offers the click path.
- No record (first update from 0.1.15, a manual DMG/installer/.deb replacement): nothing to resume; the first start re-applies components on Linux; native guests show 更新环境组件 as today.

### 5.3 `Decide` (pure function, `internal/appupdate/policy`)
Inputs: trigger (`click`/`silent`), version, current version, mode, attempts, blockers snapshot, held-since, host input idle (known/unknown), release published-at, confirmed ids, now. Outputs: `allowed | manual_mode | not_eligible | failed_before | must_wait | confirm_required | busy | not_held | user_active | user_unknown`, with the blockers to show. Rules:
- any trigger: the version must be canonical semver and **newer** than the running version; a prerelease only when the running version is a prerelease (same channel rule as `catalog.Select`).
- click: must-wait blockers refuse (`must_wait`); every interruptible blocker must be in the confirmed set, else `confirm_required` with the new ones; unknown blockers are interruptible (shown as "无法确认…").
- silent: mode must be silent; no failed/unfinished attempt for the version; release at least 24 h old; no blocker; held ≥ 60 min with an unchanged fingerprint; host input idle ≥ 30 min and known.

## 6. API between shell and workbench

### 6.1 CLI (shell → `cage`)
| command | new? | answer |
|---|---|---|
| `cage app-update check` | changed | adds `{"phase":"manual","latestVersion":v,"message":reason}` when a newer release exists but is not installable here |
| `cage app-update download` | unchanged | |
| `cage prepare-update --json [--stop]` | unchanged | old behaviour for old shells |
| `cage prepare-update --json --stop --app-update <v> --trigger click\|silent [--confirmed <id,...>] [--request <id>]` | new | `{"status":"prepared"}` or `{"status":"blocked","code":…,"message":…,"blockers":[…]}` (exit 0 with status `blocked`; exit 1 only for errors) |

### 6.2 Launcher-token routes on the workload (CLI → W; never forwarded, never via agent transport)
- `POST /api/desktop-local/app-update-handoff` `{token, version, trigger, confirmed[], request}` → 200 `{"status":"prepared"}` then shutdown, or 409 `{"status":"blocked",…}`.
- `POST /api/desktop-local/app-update-blockers` `{token}` → snapshot (for diagnostics and a future gateway).

### 6.3 Window routes on the workload (session cookie, Origin on POST, account unlocked, **trusted desktop view**)
- `GET /api/app-update` → `{mode, pending:{version, requested_at}|null, waiting_for:[blockers], last_attempt, chain:{step, to, from, error, failed_step, next}, silent_available, silent_unavailable_reason}`.
- `POST /api/app-update/policy` `{mode:"manual"|"silent"}` → the same status. Refused for phones, remote views and the agent transport.
- `GET /api/app-update/interruptions` → `{blockers:[…], must_wait:[…], confirm:[ids]}` (runs the one guest read).
- The gateway answers `/api/app-update*` with 409 `{"local_required":true}` when the selected connection is a cloud workspace.

### 6.4 Files in `<profile>/updates/` (private directory, 0700; files 0600)
| file | writer | reader |
|---|---|---|
| `policy.json` `{mode, updated_at}` | W (policy route only) | W, S (to arm the file watch) |
| `silent-request.json` `{id, version, fingerprint, decided_at}` | W hourly tick | S (file watch), W (decision compares fingerprint) |
| `attempts.json` (last 20) | W | W |
| `chain.json` | W (old and new binaries) | W |
| `last-result.json`, `launch-<token>.json`, `download-resume/`, `verified-*/` | H, S, C (existing) | S |

## 7. Settings and wording

设置 → 关于 → **应用更新** (rows defined in `app-update-state.mjs`, wired once the settings restyle lands):

| row | text |
|---|---|
| 更新方式 | ◉ **手动更新**（默认）：有新版本时显示升级按钮，只有点击后才会下载和安装。<br>○ **空闲时自动静默更新**：电脑空闲、没有任何任务时自动完成升级，并恢复原来的环境。 |
| 说明 (manual) | 不会自动更新。Super Cage 只会检查是否有新版本，不会自行下载或安装。 |
| 说明 (silent) | 只在没有运行中的任务、终端命令、下载、手机或远程连接，且电脑 30 分钟无人操作时升级；失败会恢复原版本，并且不会自动重试同一版本。 |
| 不可用 (Linux / unsigned / unnotarized / per-machine Windows) | 此安装方式不支持静默更新：<reason> |
| 等待中 | 新版本 x.y.z 已就绪，等待空闲：终端「dev」里还有命令在运行 |
| 上次结果 | 上次升级：已升级到 0.1.17（11月21日 03:12） / 上次升级未完成：<reason>（不会自动重试） |

Button labels and hints (the full table is the test fixture in `ui/src/app-update-state.test.mjs`): 升级到 x.y.z · 正在下载 42% · 正在验证 · 正在停止环境 · 正在安装，完成后自动重启 · 正在恢复环境 · 正在同步环境组件 · 正在检查网络 · 已升级到 x.y.z · 等待空闲 · 重试升级 · 查看新版本.

Confirmation dialog: title "升级到 x.y.z"; body "升级会停止环境并重启 Super Cage，完成后自动恢复原来的环境。以下内容会被中断：" + the list; buttons "继续升级" / "稍后"; must-wait: "请等待「更新环境组件」完成后再升级" with "继续升级" disabled.

## 8. Silent mode: trigger, eligibility, security

### 8.1 Who wakes up
The window's `useAppUpdates` deliberately does nothing while the window is hidden, which is exactly when silent mode should run. The trigger is the workload's **existing hourly ticker** (`startMonitors`, today only diagnostics cleanup). On each tick, only if the in-memory mode is silent: (a) every 6th tick, the same release check as the window (in-process, same transport); (b) if a newer eligible version is pending, take an in-memory snapshot and update the hold tracker; (c) when idle has held across two ticks with the same fingerprint, the host has been idle 30 min and the guest read (once) finds nothing, write `updates/silent-request.json`. The shell (only while silent is on) watches `updates/` with a kernel file watch (macOS `DispatchSource` vnode on the directory, Windows `FileSystemWatcher`), and on a new request runs the chain with `--trigger silent --request <id>`. The workload re-decides under the lock and compares the fingerprint with the request's.

### 8.2 Eligibility for an unattended install
- Source: only `sweetcornna/Super-Cage` releases (`catalog.Repository`, `allowedURL`), the same asset and digest rules as the click path, digest-pinned download.
- Publisher: the same checks as the click path (macOS: Apple anchor, Developer ID, team OU equal to the running app's team, identifier, version, Gatekeeper; Windows: Authenticode and the same publisher rule). Silent is **not offered** where those checks cannot run: development builds without a team, unsigned Windows builds, client-only/trial/Intel builds, Linux.
- Channel: never a prerelease unless the running version is a prerelease. Never a downgrade, never the running version.
- Age: the release must have been published at least **24 h** before (catalog now parses `published_at`), so the owner can pull a bad release before unattended machines take it. The click path has no age limit.
- No automatic retry of a version with a failed or unfinished attempt.

### 8.3 What silent mode adds to an attacker's reach, and how it is closed

| attacker controls | manual mode | silent mode without mitigation | mitigation (silent) |
|---|---|---|---|
| the release page (GitHub account/repo) **without** the signing key | can publish anything; every install fails the signature/team check | same | same checks; nothing to add |
| the release page **and** the signing key | users who click install the attacker's build | every silent machine installs within hours, with no human seeing the release notes | 24 h minimum age (window for the owner to notice and delete the release); on macOS, notarization (section 9) adds Apple's scan and revocation |
| the network path | TLS to github.com and the asset host; digest from the release metadata; signature check | same; only denial of service (no update) | same |
| replay of an older signed build | refused: version must be newer; macOS checks `CageVersion`, Windows the installer version | same | same, plus the request names the version and W re-checks "newer than running" |
| a process in the guest | cannot start an install | could it fake "idle"? No: idle needs host-side facts (no jobs, no host terminal output, host input idle, no viewers); the guest read can only **add** blockers. It can delay updates (deny service), not trigger one. It cannot reach `updates/`, the launcher token or the host loopback API (gate, project-share rules). | guest read can only add blockers; timeouts count as busy |
| a host process as the same user (e.g. an agent using `cage agent`) | could already run anything as the user | could turn silent on | policy route refused by the agent transport and requires the trusted desktop view; turning silent on writes a diagnostics line |
| a paired phone | cannot install | could turn silent on | policy route requires the trusted desktop view |

### 8.4 What it costs
See 4.5. In numbers: manual mode adds zero requests and zero wakeups; silent mode adds at most 4 GitHub requests a day (the same as a visible window), one host idle read and one ≤4 s guest read per decision, and a file watch in the shell.

## 9. The notarization decision (for the owner)

**Question:** should macOS in-app updates require Apple notarization (A), or trust the pinned Developer ID team signature without Gatekeeper's notarization check (B)?

| | A. Notarize every release | B. Trust the pinned team signature |
|---|---|---|
| Release work | `scripts/package-macos.sh` already notarizes and staples (notarytool profile on the release Mac; ~5–15 min per release); publish as `Super-Cage-<v>-macos-arm64.dmg` | keep `-signed.dmg`; catalog accepts that suffix on darwin; the shell skips `spctl` for update candidates only |
| Code change | none (catalog already accepts the name) | one constant in the catalog, one argument in `verifyApp` calls for candidates (`gatekeeper:`) |
| Protects against a build not signed by team 7WAXRDP7NN | yes | yes (unchanged requirement) |
| Apple's malware scan of each build | yes | no |
| Apple can revoke a bad build after release | yes (ticket revocation) | only by revoking the whole Developer ID certificate |
| First manual install from the DMG | opens normally | Gatekeeper blocks; the user must allow it in System Settings (harder on every macOS release) |
| Silent mode on macOS | offered | **recommend not offering** (no independent check between a compromised publisher and every idle Mac) |

**Recommendation: A.** Silent mode removes the human from the loop, and notarization is the only check that is independent of this project's own keys and repository; it also fixes the first-install friction. Either answer is small: A needs no code; B is the two flagged lines plus keeping `silent_available=false` on macOS. **This pass weakens nothing**: `-signed.dmg` is reported as `manual` with the reason.

## 10. Proposals (no code in this pass)

### 10.1 Cloud gateway
- Check-only: the gateway reads the same releases list (server tarball names) at most daily and shows "服务端有新版本 x.y.z" on the portal's admin page; nothing is downloaded.
- `super-cage-server upgrade` (operator command): verifies the tarball digest and the project's signature (to be introduced for server tarballs), **drains** (refuses while any workspace reports a running turn or job, listing them; `--force` to override), swaps binaries atomically, restarts, keeps the previous version for `upgrade --rollback`. Desktop and phone clients show "云端服务正在升级，稍后自动重连".
- No silent mode for the gateway by default; an operator may enable an "idle window" (e.g. 03:00–05:00) that runs the same drain + upgrade only when no tenant is connected.

### 10.2 Workspace images
- Pin images by digest in the server config; a new image is used at a workspace's **next start**, never by restarting a running workspace.
- Each workspace runs the same workbench, so the same idle registry decides; an operator-enabled policy may restart an **idle, disconnected** workspace onto the new image, using the chain of section 3 (restore running state, sync components, gate check) inside the workspace.
- Keep the previous digest for one-command rollback.

### 10.3 Phone apps
- Android: keep the 12 h check + tap-to-download + system installer (already manual; silent installs are not possible for side-loaded APKs). Add a compatibility check: the desktop advertises its protocol version in pairing; the phone says "请更新手机端到 x.y.z" when it is too old, and vice versa.
- iOS: TestFlight/App Store own updates (the user's iOS setting); show the same compatibility message.

## 11. Build plan

### 11.1 Built in this pass (branch `app-update-policy`)
1. `internal/appupdate/catalog`: `Newest` (newest eligible release regardless of assets), `ManualReason` (`-signed.dmg`, `-setup-unsigned.exe`, missing), `published_at`; fixture test with the real 0.1.15 names.
2. `internal/appupdate`: `check` reports `manual` with the reason (2.1); in-process `Latest` for the silent tick.
3. `internal/appupdate/policy` (pure, no workbench imports): mode file, attempts ledger, eligibility, blocker registry with fingerprint and holds, hold tracker, `Decide`, chain record and transitions, status view. Unit tests.
4. `internal/workbench`: feeders for in-memory state; the decision-time guest read (script + parser); host idle readers (macOS, Windows, other = unknown); admission seal; launcher routes (handoff, blockers); window routes (status, policy, interruptions); gateway refusal for cloud connections; agent-transport refusal; chain resume runner on startup; hourly silent tick writing `silent-request.json`; `PrepareAppUpdate` and the new `prepare-update` flags. Tests listed in 11.3.
5. `ui/src/app-update-state.mjs` + `app-update-state.test.mjs` (node): button label/hint for every state, confirmation list, settings rows. No view-layer edits.
6. macOS shell: see 11.2.

### 11.2 Not built here (wiring and follow-ups)
- **UI wiring** (after the settings/status-bar restyles land): `useAppUpdates` also polls `GET /api/app-update` when visible; `AppUpdateButton` uses `buttonState()`; the confirmation dialog uses `confirmation()`; 设置 → 关于 uses `settingsRows()`; after relaunch reopen `chain.environment.terminals`.
- **macOS shell**: pass `--app-update/--trigger/--confirmed/--request` to `prepare-update` and handle `blocked` (back to `available`/`waiting_idle` with the message); `install` takes `confirmed`; file watch on `updates/` while `policy.json` says silent; silent relaunch with `activates = false` (plan field `silent`); power assertion during the chain. Patch plan in 11.4.
- **Windows shell** (`desktop/windows`): the same four changes in `AppUpdater.cs` (`Backend.Json([... "--app-update", v, "--trigger", t ...])`), `FileSystemWatcher` on `updates/`, refuse silent when `!Directory`-writable(`AppContext.BaseDirectory`), `MarkReady` unchanged. To be written on `app-update-policy-windows` and built on Windows by the lead.
- **Terminal tags**: give each workbench terminal a `CAGE_TERMINAL_TAG` so the guest read maps foreground commands to tab names and lets an idle Claude/Codex TUI bound to a conversation count as restorable (today it is busy).
- **Native guest read** (macOS/Windows guests) for terminals and agents; until then a live terminal there is unknown ⇒ busy.
- **Cloud-only desktops**: a gateway-local status (files only, no account) and the hourly tick in the gateway, so silent mode works when the local workload is not running.
- **Guest vendor updaters** (audit rows 8, 10, 12, 16, 17): after the owner decides.
- **Keep-awake** during the chain (`internal/keepawake` on the Go side, `IOPMAssertion`/`SetThreadExecutionState` in the shells).

### 11.3 Tests that pin "manual"
- policy: `TestManualModeRefusesSilentTrigger`, `TestMissingOrCorruptPolicyIsManual`, `TestUnknownBlockerIsBusy`, `TestFeederPanicIsUnknown`, `TestHoldResetsWhenFingerprintChanges`, `TestFailedOrUnfinishedAttemptIsNotRetried`, `TestNoPrereleaseNoDowngrade`, `TestClickNeedsConfirmationOfNewInterruptions`, `TestReleaseAgeForSilent`, chain transition table incl. crash mid-install and rollback.
- workbench: `TestAppUpdateHandoffManualModeNeverInstallsSilently`, `TestAppUpdateHandoffBlockerAppearingAfterRequestWins` (fingerprint), `TestAppUpdateHandoffSealRefusesNewJob`, `TestAppUpdateHandoffUnknownGuestIsBusy`, `TestAppUpdatePolicyRequiresTrustedDesktopAndUnlockedAccount`, `TestAppUpdateRoutesNotForwardedToCloud`, `TestAgentTransportRefusesAppUpdatePolicy`, `TestSilentTickDoesNothingInManualMode` (no request, no file), chain resume tests (relaunched → start; rolled back → failed + restart; unknown schema → attention).
- appupdate: `TestCheckReportsManualForUnnotarizedAndUnsignedAssets` (real 0.1.15 names), and the existing tests unchanged.
- UI: `node --test ui/src/app-update-state.test.mjs`.

### 11.4 macOS patch plan (Updates.swift / UpdateInstaller.swift / UpdatesTests.swift)
See the "macOS shell" section of the final report; implemented only if `scripts/test-app-updates.sh` builds and passes here without touching the installed app.

## 12. Verifying on real machines (throwaway builds)

Never on the owner's account or `/Applications/Super Cage.app`. Use a separate macOS user (or a VM) and a separate Windows user.

**Prerequisite:** two builds A (x.y.z-test.1) and B (x.y.z-test.2) of this branch with a test-only release source (`-ldflags -X …/catalog.repository=<owner>/<test-repo>`, to be added as a build-time-only override) signed with the same Developer ID (macOS) / code-signing certificate (Windows). Publish B as a prerelease in the test repository with the production asset names (macOS: notarized `…-macos-arm64.dmg`, or option B; Windows: signed `…-windows-amd64-setup.exe`) and `SHA256SUMS`.

1. **Manual, nothing without a click**: install A; leave the window hidden 24 h with mode manual. Expect: no download directory under `updates/`, no `silent-request.json`, the button appears only after the window's own check; `diagnostics` shows no app-update download.
2. **Click chain**: start the environment, open a project, open two terminals (one idle shell, one `sleep 900`), open a Claude conversation, start a browser download. Click 升级 → the confirmation lists the `sleep` terminal, the Claude terminal and the download. Start a new `sleep` in the idle terminal and continue → expect `confirm_required` and the list again. Continue → download, verify, stop, install, relaunch. Expect: same project, environment running, gate open, terminal tabs reopened, "已升级到 B" with the release-notes link; `updates/attempts.json` has B ok; `chain.json` step done.
3. **Failure exits**: (a) corrupt B's asset digest in the test release → `failed` at verify, A intact; (b) kill the helper after READY → A intact, environment restarted; (c) make the post-update start fail (e.g. remove the proxy node) → "已升级到 B，但环境未能启动…" with 启动环境; (d) on a native macOS/Windows guest, confirm components sync runs and the gate opens without clicking 更新环境组件.
4. **Silent**: set mode silent (test build with the 24 h age, 60 min hold and 30 min input reduced via the same build-time-only flags); leave a `sleep` running → `GET /api/app-update` shows 等待空闲：终端…; stop it, keep hands off → within two ticks `silent-request.json` appears, the chain runs without activating the window, environment restored; touch the mouse during download → install refused (`user_active`), retried at the next idle window. Make the install fail once → version marked failed, no retry, button shows the reason.
5. **Cloud connection**: select a cloud workspace with a turn running there; click 升级 → the cloud turn is not listed and keeps running; after relaunch the window reconnects to the same workspace.
6. **Windows specifics**: install per-user (default) and per-machine (Program Files): per-machine silent shows "此安装位置需要管理员权限，不能静默更新"; click shows UAC.

## 13. Adversarial review of this design (one round, recorded)

| # | Attack / failure | Finding | Change made |
|---|---|---|---|
| 1 | Something starts between the decision and the stop | Today's `/api/stop` + `/api/handoff` has a gap. | One operation under `s.op` with admission sealed; blockers re-read after sealing (4.4). |
| 2 | A short job or phone turn starts and ends between two looks | Point-in-time idle checks would miss it. | Activity fingerprint over monotonic ids/counters; hold requires equal fingerprints (4.1). |
| 3 | The user came back during the download | The idle decision is minutes old at install time. | The request carries the fingerprint; the final decision re-reads everything and also requires input idle ≥ 30 min. |
| 4 | Crash or power loss mid-install | A clean-failure-only ledger would retry the bad version. | Attempt written *before* stopping; unfinished = failed; chain record lets either binary resume or roll back (5.2). |
| 5 | Old binary after rollback reads a record written for the new one | Could try to "continue" a chain it does not understand. | Schema + `from/to` check: version == from ⇒ rolled_back; unknown ⇒ attention. |
| 6 | Silent mode turned on by a phone, a remote view or a host agent | Would let a less-trusted surface enable unattended installs. | Trusted desktop view + unlocked account; agent transport refuses the route. |
| 7 | Cloud workspace answers "may I install" | The gateway forwards `/api/*`. | Gateway refuses `/api/app-update*` for cloud connections (2.3). |
| 8 | Mode reset or switched by a settings save or profile restore | `Preferences` is rewritten wholesale. | Separate `policy.json`, single writer (2.4). |
| 9 | Mac in a dark wake (lid closed) passes "idle" | Ticks run during maintenance wakes. | Install chain takes a power assertion (build plan); the helper is transactional and rolls back; the age and hold make it rare. Left as a known risk until the shell change lands. |
| 10 | A compromised publisher pushes to every idle machine | Removes the human. | 24 h release age; notarization recommended; silent not offered on macOS under option B (8.3, 9). |
| 11 | Guest lies "idle" | Guest is untrusted. | Guest read can only add blockers; idle needs host facts (8.3). |
| 12 | Idle Claude TUI blocks forever | Users keep Claude open; silent would never run. | Accepted for this pass (fail-closed); terminal tags + conversation binding in the build plan. |
| 13 | Sealing breaks the UI during a refused decision | Requests during ≤4 s get "正在准备升级". | Seal blocks only admissions of new work, not reads; documented. |
| 14 | Stale `silent-request.json` after silent is turned off or the app restarts | Shell could start a chain. | The workload decides again (mode, fingerprint, request id must match the one it wrote in this process); a stale request is refused before anything stops. |

## 14. Open questions and unverified items (存疑)
- All **unverified** audit rows (8, 9 runtime, 10, 11, 12, 13 runtime, 14 runtime, 16, 17): need a running guest; commands are in the table.
- Whether the workload controller outlives the window when the app quits (affects who consumes a silent request); this design is safe either way because the decision is re-made under the lock.
- `GetLastInputInfo` from the workload process on Windows: correct only if the process runs in the user's interactive session (it is started by the app); unverified here (no Windows build).
- `ioreg` HIDIdleTime after a dark wake: believed to keep counting; unverified.
- The 24 h / 60 min / 30 min values are proposals; the owner may prefer different ones.
