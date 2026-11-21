# Super Cage automatic resource allocation — design

Status: design for branch resource-allocation (base origin/super-cage-redesign f30a5d4).
Scope: three levels — A gateway host <-> workspace VMs (tenant edition), B machine <-> Cage guest (host agent),
C inside the Linux guest. CPU where it is cheap. Status that the UI shows without interpretation.

## 0. What went wrong (evidence, 2026-11-21)

Workspace VM 8 GiB (4 GiB available), guest booted with -m 3072 (old automatic sizing: host.Cap halves the
7 GiB MemTotal to 3 GiB, AutomaticMemoryGiB(3) = 3), 3 vCPUs (6 / 2). One renderer reached 1.7 GB; guest
available 48 MB, zram (MemTotal/4 = 727 MiB) full; every SSH-based probe timed out; the balloon target was
already at the 3 GiB maximum so level B could not help; status said pressure=true forever and the network
recovery waited on it. Half of the machine was idle.

Root causes, one per level:
- B: ceiling fixed at half of the machine, chosen at boot; no way to use the idle half.
- C: one pool, no priorities: the browser starved sshd sessions, the probes, the browser agent.
- B/status: "pressure" was the guest-wide PSI, so a contained browser would still have blocked recovery;
  a failed sample said "自动回收内存已暂停" and jumped the target to the maximum.

## 1. Common definitions

All sizes in bytes, rounded to 256 MiB units (unit) unless stated.

- Working set W = MemTotal - MemAvailable (+ kernel memory outside MemTotal where the outer level pays it).
  MemAvailable counts reclaimable page cache and reclaimable slab as available, so a page-cache-heavy
  workload has a small W: reading files cannot raise demand. (Not gameable by page cache.)
- Headroom h(W) = max(1 GiB, W/4) (unchanged from today at both levels).
- Instantaneous demand d = W + h(W) [+ kernel term], clamped to [guarantee, ceiling].
- Shortage boost: when the consumer is short — PSI memory "some avg10" >= 10 %, or swap-in activity since
  the last sample (pswpin advanced), or available below h/4 — d = max(d, current + max(512 MiB, current/4)).
  That covers file-backed thrashing (refaults) that W does not show. A tenant can fake it only inside its
  own VM, which at most raises its own demand up to its own ceiling; max-min fairness below caps it at the
  water level whenever others are short.
- Held demand D: D = d when d >= D_prev (rises at once); otherwise D_prev is kept for the hold window, then
  decays by one shrink step per round. Constant d => constant D => constant allocation.
- Cold swap is not demand: swap used counts only while pages are being swapped in (pswpin advanced).
  (Today level B adds all swap used to "used", so one burst kept the target high for ever.)

## 2. Level A — gateway host <-> workspace VMs (internal/tenant)

(Sections 2-6 are the design as first written; section 7 records what was built and supersedes them.)

Pure function: tenant.AllocateMemory(AllocationInput) -> []Allocation{ID, From, To, Demand, Fair, Reason}.

Inputs per round (every -memory-interval, default 15 s): host total H, available Ha, PSI stall Hs; reserve
R = -memory-reserve or max(2 GiB, 15 % H); profile ceiling; per instance: status, limit L, guest
MemTotal/MemAvailable, guest PSI and pswpin (new, read in the same incus exec), active (used within 1 min),
hotplugged, silent count, fixed (starting/cooling/foreign), hold-shrink (shrink failed < 1 min ago);
per-instance state: held demand D and the time it was set.

1. Guarantee g = clamp(-memory-min (default 2 GiB), 1 GiB, ceiling). Ceiling c = min(profile ceiling, H - R) >= g.
2. Pinned P = sum of limits of frozen, fixed and silent instances (they keep their size this round).
   Capacity C = H - R - P.
3. Water-filling (max-min fair): f_i = max(g_i, min(D_i, lambda)), lambda = largest level with
   sum f_i <= C (bisection over sorted demands; exact). If sum g > C, f = g (overcommitted guarantees: status
   says so). Properties: sum f <= C when sum g <= C; f_i >= g_i; f_i <= D_i; if some f_i < D_i then
   sum f = C (work-conserving, nothing idle while someone is short); the smallest unmet demand is met first;
   f is monotone in every D_j for its own i and anti-monotone in the others.
4. Grow: L_i < f_i -> to roundUp(f_i) at once, also under host pressure (f is already fair; a guest OOM is
   worse than host reclaim). Reason "guest needs memory" / "fair share of a short host".
5. Shrink — only memory above the floor F_i = max(f_i, W_i + 256 MiB, g_i), never below the working set:
   a. capacity needed: N = sum_j max(L_j, f_j) - C > 0, and some VM grows this round or is short (held below
      its demand); without either nothing is taken, since a limit above the capacity is not use. Take N from
      instances with L > F, inactive first, then active, within a tier proportional to slack (L - F), at most
      step per instance per round.
   b. idle return: an inactive instance with L - max(D, g) >= step returns one step per round toward
      max(D, g). D only decays after the 5-minute hold, so a VM idle for 5 minutes starts to give back;
      an active VM keeps its excess (its page cache is in use) unless (a) or (c).
      Being active only orders the tiers of (a) and (c) and spares (b); unlike the legacy policy's fair-share
      exemption it never keeps memory another VM or the host needs. That matters because the gateway counts
      every proxied chunk, 20 s terminal heartbeats included, as use (review 2, extra item 6): a workspace
      with an open terminal is active for good, gives nothing through (b), and still yields through (a) and
      (c) (TestAllocationActiveVMStillYieldsToAGrowingOne, TestAllocationHostPressureTakesIdleFirst...). The
      heartbeats' other effect, that such a workspace never cools, belongs to idle cooling, not here.
   c. host pressure (Ha < 2R or Hs >= 10): release N_p = max(2R - Ha, step) by tiers:
      T1 inactive above d_i (instantaneous demand), T2 active above d_i (proportional to slack),
      T3 inactive above W_i + 256 MiB, T4 active above W_i + 256 MiB. Never below W + 256 MiB, never
      below g. Step under pressure 1 GiB per instance per round.
   Shrinks smaller than the step (512 MiB) are not issued (hysteresis); hotplugged and hold-shrink
   instances only grow.
6. Silent guest (sample failed): hold. After 2 silent rounds it grows back (to the ceiling, or one step per
   round under host pressure) — unchanged: a guest that stops answering after a shrink may be thrashing.
   A failed sample never shrinks.
7. Frozen (cooled) instances are pinned: their balloon cannot move while vCPUs are paused; idle cooling
   already pushes their memory to swap (memory.reclaim). Idle-first in T1/T3 is the "cooled or idle first".

Constants: unit 256 MiB, step 512 MiB (1 GiB under pressure), hold 5 min, active window 1 min, headroom
max(1 GiB, W/4), interval 15 s.

Why the levels do not feed back: A's demand for a machine is the machine's working set, which contains the
nested guest's resident memory; B keeps the guest near its own demand. A grows the machine only when the
machine's working set rises (the guest really uses more); B grows the guest only on guest demand, never
because the machine grew; B shrinks the guest when the machine shrinks only down to the guest's demand.
The chain "A shrinks idle machine -> B returns guest cache -> A shrinks more" is monotone and stops at the
demands (each step only takes memory above a working set + headroom); it runs only for inactive machines or
under host pressure. No step of either level is triggered by the other level giving memory back.

8. Nested demand (the machine runs a Cage guest): the machine's working set already contains the guest's
   resident memory (QEMU anonymous memory), so guest page cache is demand to A unless B reclaims it; the
   "not gameable by page cache" property of A holds for nested guests through B's reclaim (B keeps the
   guest within d + 512 MiB after 30 s of headroom). B also publishes what the guest wants and the machine
   cannot back now (short_bytes) in <dir of -guest-control-path>/machine-memory.json; A reads it in the
   same incus exec and adds it to the machine's instantaneous demand (fresh < 30 s, bounded by the
   ceiling, tenant-controlled like meminfo, so it can at most claim its own ceiling under max-min). Without
   it a short guest on a parked machine could only grow by the machine's headroom per A round; with it A
   covers the guest's want in one round (<= 15 s + balloon time).
9. Reading the guest: one incus exec of /bin/sh -c 'head meminfo; head pressure 2>/dev/null; grep vmstat;
   head machine-memory.json 2>/dev/null; exit 0': missing files are tolerated (old kernels without PSI,
   machines without a Cage guest), only meminfo is required.
Legacy policy reachable with CAGE_TENANT_MEMORY_POLICY=legacy in the gateway's environment (no new flag:
cmd/cage-tenant is outside this change).

## 3. Level B — machine <-> Cage guest (internal/cage, host agent)

### 3.1 Ceiling (boot-time maximum, -m) follows the machine

Automatic mode (default for new environments and for files without an explicit user choice):
  machine total Mt = boot physical memory of the machine: macOS/Windows: installed RAM; Linux:
  sum of zone "present" pages in /proc/zoneinfo (a ballooned workspace VM reports a smaller MemTotal,
  but the gateway may grow it back to its boot size; the ceiling is about what the machine can become).
  Reserve for the machine's own needs Rm:
    dedicated machine (Linux without a graphical session: the tenant workspace VM, a server): max(1.5 GiB, Mt/8)
    desktop (macOS, Windows, Linux with DISPLAY/WAYLAND_DISPLAY):                         max(4 GiB, Mt/4)
    override: CAGE_MACHINE_ROLE=dedicated|desktop.
  M = clamp(floorGiB(Mt - Rm), 2 GiB, 32 GiB), in whole GiB. An explicit AGENT_VM_HOST_SHARE (an admin
  choice) still caps it at Mt/share. The -m path: allocation.Limits() -> vm.Lima.Start -> Edit ->
  lima.yaml has no host.Cap on it (host.Cap only clamps explicit CLI flags and the base build).
  8 GiB workspace VM -> 6 GiB (was 3). 16 GiB Mac -> 12 (was 8). 32 GiB -> 24 (was 12). 4 GiB machine -> 2.
  vCPUs: dedicated n-1 (>=1); desktop n - max(2, n/4) (>=2, <=n). 6-CPU workspace VM -> 5 (was 3).
  macOS guests keep their fixed-memory cap (8 GiB, no balloon) and get the same vCPU rule; Windows guests
  (Hyper-V) keep their own ceiling code unchanged.
Manual mode: the user's GiB/CPUs exactly (range raised to the automatic maximum); target logic below still
runs under that ceiling. The old fixed behaviour is reachable by choosing a manual value.

### 3.2 Existing environments: explicit choice vs. old default

resource-limits.json written by this version carries "chosen": "user" (set by the API on every save; older
versions ignore unknown fields). A file without it and with automatic=false is classified:
  - legacy default: memory_gib and cpus equal what the old automatic formula gave on this machine
    (memory: old AutomaticMemoryGiB(share(m)) for some m between the machine's current MemTotal and its
    boot size, cpus: old CPU share). Matching the old formula cannot prove the user did not choose it, so
    the classification is a pure function returning the origin, and what to do with it is one switch
    (LegacyDefaultAction = "offer" | "apply", default "offer"): "offer" keeps the saved values and puts a
    one-step offer in the status: allocation_origin="legacy_default", legacy_allocation={memory_gib,cpus},
    notice "当前的 3 GB 内存 / 3 个 CPU 与旧版默认值相同，可能不是手动选择的。改为自动（推荐）后，内存上限
    可到 6 GB、CPU 5 个，需要重启环境。", offer {allocation:{automatic:true}, apply:true}. "apply" would
    treat it as automatic with the same notice in the past tense. Owner decision; default is the safe one.
  - otherwise: an explicit user choice, never changed; status advice suggests automatic when short.
  - no file (the common case, also likely the production one: the old automatic formula also yields 3/3
    on that machine): automatic, the new sizing applies at the next environment start.
The file is never rewritten by the classification (no write in a read path, no race with a save; rollback
leaves the old version reading the same file). One step to move any environment to automatic: POST /api/environment/resources
{"allocation":{"automatic":true},"apply":true} (save and restart in one call; "apply" is additive — without
it the call only saves, as today).

### 3.3 Target (balloon), every 2 s

Pure function: cage.PlanGuestMemory(GuestInput, *GuestState) -> GuestDecision{Target, Wanted, Capacity,
Demand, Reason, Short, ...}.

Inputs: guest sample (MemTotal, MemAvailable, MemFree, swap, pswpin, OOM kills, balloon, PSI global and per
class), machine reading (Linux: /proc/meminfo MemTotal/MemAvailable/PSI; other OSes: pressure flag only),
boot maximum M, previous target P, quiet (guest CPU < 5 %), state (held demand, idle-since, failures).

- floor g = min(M, 2 GiB).
- demand d = W + h(W) + 256 MiB (kernel and balloon bookkeeping outside MemTotal) [+ swap used only while
  swapping in], roundUp, clamp [g, M]. Pressure = available < max(512 MiB, MemTotal/8) or protected PSI >= 1;
  severe = available < max(256 MiB, MemTotal/16) or protected PSI >= 10. "Protected PSI" is the stall of the
  control and session cgroups (section 4); a throttled browser alone is not guest pressure.
- Machine capacity K (Linux machines): K = min(M, Rg + Ma - margin), Rg = MemTotal - MemFree of the guest
  (its resident estimate), Ma = machine MemAvailable, margin = clamp(machine total/64, 256 MiB, 512 MiB) —
  deliberately below A's minimum headroom (1 GiB) so a machine parked by A at W + headroom never pins the
  guest (advisor finding: with total/20 a 32 GiB machine had a 1.6 GiB margin against 1 GiB of headroom,
  K < P, a deadlock). When the guest wants more than K, B publishes short_bytes = wanted - K for A.
  macOS / Windows (no reliable "available"): K = M while the machine is calm; on an elevated pressure
  signal (darwin kern.memorystatus_vm_pressure_level >= 2 "warn", Windows free < 20 %) K = max(d,
  old share = half the machine): growth past the old ceiling stops early and the excess cache is returned.
- Grow (pressure or d > P): severe -> min(M, K) at once; pressure -> min(M, K, max(d, P + max(2 GiB, P/2)))
  (geometric); otherwise d. Under machine pressure: pressure -> max(P, min(d, K)); else hold.
- Machine shrinking: P > K -> target max(d, K) at once (only guest cache/free is taken: never below d).
  This is how the host agent adapts when the gateway shrinks the machine, before the machine swaps; the
  gateway shrinks at most 512 MiB per 15 s and never below the machine's working set + 1 GiB, so B has
  several samples to react.
- Shrink (idle return): after sustained headroom P >= d + 512 MiB for 30 s (10 s quiet, 0 under machine
  pressure), reclaim a quarter of the excess (half when quiet or machine pressure), >= 256 MiB, never below
  d, and only once the balloon reflects the previous reclaim (unchanged rules).
- Sample failure: hold for 2 failures in a row; from the 3rd, grow one 1 GiB step per failure toward
  min(M, K_last) (a stalled guest may be out of memory; K keeps the machine safe). Never shrink on failure.
  Status: "暂时读不到环境的内存用量（第 N 次），保持当前分配 X GB" / "…已逐步放宽到 X GB". The first good
  sample resumes normal operation; nothing pauses for ever.
- Short: pressure while P >= min(M, K) — the guest cannot get more without a human or without the gateway.

## 4. Level C — inside the Linux guest (provisioning, internal/cage)

cgroup v2 tree (all runtime state, recreated on every environment start by the start script):
  -.slice
  ├─ system.slice         control plane: sshd, lima-guestagent, journald, display, editor, governor
  │                       CPUWeight=200, MemoryLow = dynamic (control use + 128 MiB, <= 25 % of guest)
  ├─ user.slice           control plane, part 2: SSH sessions WITHOUT a terminal — the host agent's
  │  │                    resource probe, the shared control session (guest-session.py), the CLI channel,
  │  │                    one-shot scripts. CPUWeight=200, MemoryLow dynamic
  │  ├─ user-<cage>.slice MemoryLow = same value as user.slice (claims the whole parent protection)
  │  └─ user-<cage-browser>.slice   (Ubuntu: snap Chromium lives here) MemoryHigh/Max = browser limits
  ├─ cage-sessions.slice  new: terminals. The guest governor adopts every process of the cage user that
  │                       has a controlling terminal and CAGE_WORKBENCH=1 in its environment, and every
  │                       claude / codex process tree, into a transient scope cage-term-<pid>.scope here
  │                       (PID 1 StartTransientUnit with PIDs, through /run/systemd/private: systemd moves
  │                       and tracks them). CPUWeight=100, MemoryLow = sessions use + 256 MiB (<= 50 % G),
  │                       MemoryHigh = G - control protection - 256 MiB (soft: a runaway build or session
  │                       is reclaimed/throttled before it can starve the probes; never a hard max)
  └─ cage-browser.slice   new. CPUWeight=50, IOWeight=50
     └─ cage-browser.service  Delegate=memory cpu, OOMScoreAdjust=200 (below Chromium's renderer scores,
                              >= 300, which Chromium may raise: a renderer dies before the agent)
        ├─ agent/      node browser agent, IBus, wrappers. memory.low 256 MiB
        └─ chromium/   Debian Chromium (moved before exec by /etc/chromium.d/zz-cage-cgroup;
                       governor sweep as fallback). memory.high / memory.max = browser limits

Browser limits (pure function cage.PlanGuestClasses, computed by the host agent, applied in the guest):
  G = guest attainable size min(M, K) (not the current target, so the browser may grow while B grows the
  guest, and shrinks first when the machine shrinks).
  protected = control use + sessions use + 512 MiB (new sessions, probes).
  Why three tiers need the adoption step: probes and terminals both arrive as SSH sessions of the same
  user; MemoryLow on their common parent does not protect a new tiny probe scope from a 3 GB sibling.
  browser high = clamp(G - protected, 768 MiB, 75 % G); browser max = high + max(256 MiB, G/16).
  Evidence case (G = 3 GiB, control 0.4 + sessions 0.55): high 1.55 GiB, max 1.73 GiB: the 1.7 GB tab
  is reclaimed into zram and throttled, then killed alone at max; control and sessions keep their memory.
  New sizing (G = 6 GiB): high 4.5 GiB: the same tab simply fits.

Channel: the host agent's probe writes the limits into /run/cage-resources/limits (cage-writable runtime
directory) when they change; the guest governor (cage-resource-governor.service, root, system.slice,
fail-open: every cgroup step is best effort, the browser starts regardless,
OOMScoreAdjust=-900, MemoryMin=32M, python3, 2 s loop) applies them: raw writes in the delegated browser
subtree (Debian), systemctl set-property --runtime for slices (Ubuntu browser slice, MemoryLow of
system/user slices), with hysteresis (>= 64 MiB change). Without a fresh limits file (no host agent, stale
> 60 s) it computes the same formula from its own MemTotal. Class usage (memory.current, memory.pressure)
is read by the probe directly from cgroupfs (world-readable) and reported in the status.

OOM policy (cheapest loss first):
  1. reclaim: browser above memory.high is reclaimed into zram and throttled (no kill);
  2. (follow-up, needs browser-agent endpoint) discard hidden background tabs;
  3. browser at memory.max: cgroup-local OOM kills the largest browser process (the runaway renderer —
     the tab shows a crash page, others survive); memory.oom.group stays 0;
  4. global OOM order: renderers (>= 300) -> browser main/agent (200) -> editor (adj 100) -> sessions and
     others by size (0)
     -> guest agent, governor (-900), sshd listener (-1000, set by sshd).
  A claude session is not lowered below the control plane on purpose: tiny control processes with a higher
  score than a big session would be killed one after another without freeing memory.
zram: disksize = min(MemTotal/2, 4 GiB) (was /4, 2 GiB), priority 100; vm.swappiness=100 (runtime sysctl):
anonymous browser memory is compressed before file cache of the sessions is dropped.
CPU: CPUWeight 200 / 100 / 50 as above; the browser cannot starve sshd or the probes of CPU.

Browser-agent endpoint (follow-up, not in this branch; browser-agent.mjs is owned elsewhere):
  POST /memory/reclaim {"level":"moderate"|"critical"} -> {"discarded":[ids],"purged":n}
  moderate: for every non-visible tab: HeapProfiler.collectGarbage, Memory.simulatePressureNotification
  {level:"moderate"}; critical: additionally close hidden background tabs that are not playing audio, not
  login/private, not pinned to a split pane, remembering URL+title and recreating them on activation
  (Chrome's own discard semantics). The governor calls it when the browser is above memory.high for 10 s
  (moderate) or at 90 % of memory.max (critical); 404 -> skipped.

Container mode (containerenv): no VM boundary; the outer cgroup (host user slice MemoryMax, CPUQuota,
TasksMax from the server deployment) is authoritative and unchanged; no zram, no swap; the slice setup is
attempted only when the container's systemd owns a writable cgroup subtree, otherwise skipped; level B does
not run (no balloon). macOS/Windows guests: level B does not run (no Linux probe); level C does not apply;
their sizing code is untouched except the shared vCPU rule for macOS guests.

## 5. Status (additive; ResourceStatus version stays 1)

cage-resources.json (host agent) gains:
  capacity_bytes (K), demand_bytes (d), wanted_bytes, reason (code), summary (Chinese sentence),
  next_action (Chinese), short (bool), possible_bytes (automatic ceiling this machine supports),
  restart_required (bool: possible > maximum), advice {message, memory_bytes, restart_required},
  machine {total_bytes, boot_bytes, available_bytes, reserve_bytes, pressure},
  classes [{name: control|sessions|browser, current_bytes, protected_bytes, high_bytes, max_bytes,
            stall_percent}], browser_pressure, failures.
  "pressure" keeps its name and now means the guest's protected classes are short (falls back to guest-wide
  PSI on guests without the slices). network_recovery.go (owned elsewhere) consumes it: with this change a
  browser contained at its limit no longer postpones the automatic network recheck.
  Size stays well under the 16 KiB read limit (classes: 3 entries, short strings).
/api/environment/resources gains: allocation_origin, legacy_allocation, notice, automatic_memory_gib,
automatic_cpus, advice (from the observation or from sizing when stopped), possible_memory_bytes.
A caller that wants "short of memory, N GB possible": observation.short && observation.advice != null ->
advice.message ("可以把内存调到 N GB，需要重启环境"), advice.memory_bytes, advice.restart_required.

## 6. Rollback to 0.1.15

Gateway host: state left = instance-local limits.memory on running VMs, exact MiB sizes between 1 GiB and
the profile ceiling: exactly what 0.1.15's balancer writes and its elasticMemory check accepts. No new Incus
keys, no profile change. Steps: stop the new cage-tenant; install the 0.1.15 binary; start. (For releases
before 0.1.15 only: incus config unset cage-<id> limits.memory for each instance, VMs stopped first.)
Desktop / workspace: settings file unchanged by classification; the extra "chosen" key is ignored by 0.1.15.
lima.yaml memory/cpus are recomputed by whichever version starts the environment. Guest: the new slice,
governor unit, drop-ins and limits live in /run (gone at the next boot); the persisted changes are the
cage-browser.service unit and cage-memory-start (rewritten by 0.1.15's component update, which it asks for
because the guest manifest revision differs), /usr/local/bin/cage-resource-governor and
/etc/chromium.d/zz-cage-cgroup (inert without the delegated unit: the hook only moves itself when the
chromium leaf exists and is writable). Steps: install 0.1.15; open the environment; accept the component
update ("环境组件需要更新"), which restarts the browser; restart the environment once so /run is cleared and
the old -m applies.


## 7. As built (supersedes the sections above where they differ)

Branch resource-allocation. The sections above are the design as first written; this section records what
the code does. Code: internal/tenant/allocation.go (A), internal/cage/machine*.go, guest_allocation.go,
resources.go, resource_limits.go (B, sizing, status), internal/workbench/resources.go (API),
internal/tenant/replay_test.go (simulator).

### 7.1 Level A (allocateMemory, unexported; planMemory kept for CAGE_TENANT_MEMORY_POLICY=legacy)
- Floor of a shrink is max(base, guarantee, roundUp(W + 512 MiB)) (memoryMinHeadroom = 512 MiB, not 256:
  a shrink into the headroom under pressure must not read as a shortage in the next round).
- Pressure (review 2, R2-5, replaces "available < 2R"): guest page cache fills the limits and is resident on
  the host, so a host whose limits fill its capacity has R - own available: the plan, not a shortage. With
  the old "available < 2R" a VM host sat in pressure for good once its limits passed total - own - 2R, and a
  VM grew only when nearly out of memory. Pressure is now: stall >= 10, or available < keep (keep =
  max(512 MiB, R/4): limits above the capacity being filled), or available < R while the limits leave more
  unallocated than is available (the host's own use is above its reserve). Below 2R ("reclaim") memory
  above max(demand, fair) is taken back early, idle VMs first; under pressure also the headroom above
  max(W + 512 MiB, fair). The 1 GiB step applies below R.
- Every shrink floor is at least the fair allocation: a VM above its fair share is never taken below it, so
  nothing a shrink takes is grown back the next round (the reviewer saw 4.75 -> 3.75 GiB with fair 4.0).
- Growth to the fair allocation is at once within what the host has free above keep, short guests first,
  max-min fair among the rest (waterFill over the gaps); what it cannot back now (result.backlog) follows
  as memory taken back from VMs above their fair shares is freed. While the host is in pressure only a VM
  that is short itself grows; one that merely lacks headroom waits.
- "Capacity needed" reclaim (above fair share, idle first) runs only when some VM grows or is short.
- Shortage boost: stall >= 10, pswpin advanced, or available < 256 MiB: d >= L + max(512 MiB, L/4).
  Nested short_bytes is added to d. machine-memory.json must be fresh within +-45 s of the gateway clock
  (the clocks of gateway and workspace VM are compared; NTP skew beyond that drops the signal).
- Reasons in the log: "guest needs memory", "nested guest needs memory", "fair share of a short host",
  "host memory is low", "others need memory", "idle memory returned", "guest did not answer".

### 7.2 Level B sizing (AutomaticLimits, ManualLimitRange)
- Root cause confirmed in code: host.MemGiB() floors MemTotal (8 GiB VM -> 7), ResourceLimits() halves it
  (3), the old AutomaticMemoryGiB took min(share, ...) = 3, and the old Limits() rejected any manual value
  above the same share: neither the automatic size nor a manual setting could exceed 3 GiB. Every start
  rewrites lima.yaml .memory from Limits(), hence -m 3072.
- New: machine boot size Mt = host.MemGiB() without an outer balloon; with one, bootBytes(MemTotal, balloon,
  memory blocks, deflate-on-oom) rounded down once (MemTotal + balloon on kernels that shrink MemTotal,
  MemTotal alone when the balloon negotiated deflate-on-oom, as /sys/bus/virtio/drivers/virtio_balloon/
  virtio*/features bit 2 reports; without that file the online memory blocks decide). Not /proc/zoneinfo.
- Reserve: dedicated max(1 GiB, Mt/8); desktop max(4 GiB, Mt/4). M = clamp(floorGiB(Mt - reserve), 2, 32).
  vCPUs: dedicated n-1; desktop min(n, max(2, n - max(2, n/4))). AGENT_VM_HOST_SHARE does not cap the
  automatic size (it still bounds CLI flags and is part of the manual range).
- Manual range = max(old share, automatic): on the production machine 2-6 GiB, 1-5 vCPUs (was 2-3, 1-3).
- macOS guests keep the previous sizing (LegacyLimits) - no balloon. Windows guests: code unchanged.
- Origin: automatic | user (file has "chosen":"user", written by every save through settings) |
  legacy_default (no marker, automatic=false, values equal the old formula on this machine).
  LegacyDefaultAction = "offer" (default): kept, offered in the API status.

### 7.3 Level B target (memoryPolicy.plan)
- Never grows past what a measured (Linux) machine backs, K, not even when severe: the machine's OOM
  killer would take QEMU (the whole guest) while the guest's own OOM order takes one browser process. The
  shortfall goes to the gateway (short_bytes). On macOS/Windows (not measured) a guest in trouble still gets
  its demand.
- Machine smaller (P > K, no pressure): target min(P, max(K, floor)), floor = W + 256 MiB rounded up, at
  least min(2 GiB, M) - may be below the demand d; short_bytes = d - K tells the gateway.
- wanted for pressure/severe is the geometric step max(d, P + max(2 GiB, P/2)), so short_bytes can ask
  for more than d; the gateway bounds it (ceiling, max-min water level, 5-minute held demand).
- Failed samples: hold 2, then +1 GiB per failure within K; when the machine has no room the step is asked
  of the gateway (short_bytes, kept through failures - never withdrawn by a failed sample).
- The class limits are computed for size min(M, max(K, d)) and prefixed to the next probe as a shell
  snippet that writes /run/cage-resources/in/limits when that directory is writable (level C), with every
  probe.
- Status keeps short/limited_by/advice of the last good sample through failed samples.

### 7.4 Status fields
cage-resources.json (host agent, ResourceStatus version 1, all additive): capacity_bytes, demand_bytes,
wanted_bytes, possible_bytes, short, limited_by ("maximum" | "machine"), browser_pressure, reason,
summary, next_action, advice {message, memory_bytes, restart_required}, machine {...}, classes [...],
failures. pressure now means: the protected classes stall (guest-wide PSI on guests without the governor).
/api/environment/resources (Linux guest, additive): allocation_origin, legacy_allocation, notice, offer
{automatic, memory_gib, cpus, restart_required}, automatic_memory_gib, automatic_cpus,
possible_memory_bytes, advice; max_memory_gib/max_cpus now the manual range; recommended_* the automatic
size. POST accepts "apply": true (save, then restart a running environment).

### 7.5 Rollback additions
- A manual value saved above the old share (e.g. 6 GiB on the 8 GiB machine) makes 0.1.15's Limits()
  fail and the environment will not start under 0.1.15. Before rolling back a desktop install: set the
  allocation to automatic in settings (or delete <profile>/resource-limits.json), then install 0.1.15.
- The "chosen" key is ignored by 0.1.15.
- The manual range depends on the machine's role, which follows DISPLAY/WAYLAND_DISPLAY: a value saved by
  a headless cage serve (dedicated) can be above the desktop app's range on the same machine. A saved value
  outside the current range is clamped at start, never refused (review 2, R2-9): Limits() clamps,
  LimitsNotice() says so (the API's notice, the Lima start log, cage setup's stderr), and the file is not
  rewritten (the other role still reads it). Choosing a value checks the range (Validate()).

### 7.6 Level C as built (supersedes section 4 where they differ)
Built (declarative or fail-open), in guest-services.sh, guest-memory.sh, guest.go, resource-governor.py:
- cage-browser.slice (CPUWeight=50, IOWeight=50); cage-browser.service: Slice=cage-browser.slice,
  OOMScoreAdjust=200, OOMPolicy=continue (default stop would end the whole browser on one renderer's OOM
  kill); cage-editor.service OOMScoreAdjust=100.
- zram disksize min(MemTotal/2, 4 GiB) (was /4, 2 GiB), vm.swappiness=100 at runtime. cage-memory-start
  exits early when its device exists, so the size applies from the next environment start.
- cage-resource-governor.service (root, OOMScoreAdjust=-900, MemoryMin=32M, CPUWeight=200,
  Restart=on-failure, ExecStopPost --reset), python3 -I /usr/local/lib/cage-resource-governor.py, 2 s loop:
  limits from /run/cage-resources/in/limits (dir owned by cage, the probe's user; file fresh <= 10 s,
  regular file, version 1, four keys), every value clamped (control_low <= MemTotal/4, sessions_low <=
  MemTotal/2, browser_high >= max(768 MiB, MemTotal/4), browser_max >= high + 256 MiB, all <= 2^44);
  without fresh limits the same rule (planGuestClasses, ported) on its own MemTotal. Applied with
  systemctl set-property --runtime (MemoryHigh/MemoryMax on cage-browser.slice; MemoryLow on system.slice
  and user.slice only when the cgroup2 mount has memory_recursiveprot), hysteresis 64 MiB. Reports
  "governor 1 <uptime>" and "class <name> <current> <stall> <low> <high> <max>" to /run/cage-resources/state
  (root-owned dir, 0644). Exits 0 (no restart) without the unified hierarchy. Skipped in containers.
- The host agent sends the limits with every probe (they are ~200 bytes), so a stale file means the host
  agent is gone or older.
Ubuntu guests: snap Chromium runs in a scope under the browser user's slice in user.slice. The governor
counts user.slice/user-<cage-browser uid>.slice as browser (use, stall, MemoryHigh/MemoryMax), subtracts it
from the sessions, takes the sessions' stall from user-<cage uid>.slice and sets that slice's MemoryLow
equal to user.slice's, so no protection is left over for the browser user's slice.
Accounts (review 2, R2-3): new environments have random account names (internal/guestprofile), so the
governor takes both from its unit: Environment=CAGE_SESSION_USER=cage and CAGE_BROWSER_USER=cage-browser,
rewritten by the same rename rules as every other account reference (guestprofile linuxAccountRules has a
prefix for each; nothing else in any template contains them, TestGovernorAccountRulesMatchOnlyItsUnit).
A name is resolved when getpwnam knows it (a browser user without a slice, Debian, is resolved). When
either is missing, malformed, unknown or both are the same uid, the governor does not guess: it limits
only cage-browser.slice, protects only system.slice, sets user.slice MemoryLow=0, reports no class (the
host agent falls back to the whole guest's pressure), writes "accounts unresolved <roles>" to the state
file and says which names in the journal. TestRenderedGovernorFindsTheProfileUsers runs the governor and
unit rendered by Config.SetupScript/StartScript for no profile and two random ones in a fake guest.
Not built (moved to the follow-up with the browser-agent endpoint): the chromium.d hook, Delegate= and
agent/chromium leaves, adoption of sessions into cage-sessions.slice (no single spawn point owned here:
terminals, claude and codex arrive by several SSH paths), memory.high on the sessions.
Consequence, shown by the simulator: the host agent's probes are SSH sessions of the same user as the
sessions and share user.slice; a runaway session at the guest's maximum silences the probes (the guest
really is out of memory; the browser is reclaimed first). The control plane in system.slice is protected.

### 7.7 Replay results (go test ./internal/tenant -run Replay -v)
Model numbers: measured from the evidence: guest 3 GiB (MemTotal 2.84), tab growing to 1.7 GB, two claude
sessions 513 MB, 48 MB available at the end, machine 8 GiB with ~4 GiB available. Fitted: the rest of the
190 processes (system, display, editor, browser agent, SSH) as one 900 MiB "control" class - the evidence's
0.4 GB control figure did not reproduce the outage, 900 MiB is what makes the 2.84 GiB guest end at 48 MB
with zram full; zram 3:1; probes answer while the probing class stalls < 10 % and (without classes) 128 MiB
is available. The model is a model: it shows the rules interact as intended, not kernel timings.

| trace | old | new |
|---|---|---|
| outage, 3.5 h compressed to 30 min | 3 GiB, control silent 1048 s | 6 GiB max, target 4864 MiB, silent 0 s |
| outage, new size, no governor | | silent 0 s |
| outage, old 3 GiB size + governor | | silent 0 s, 1 browser process killed at memory.max |
| runaway tab (8 MiB/s) | | 1 browser kill, sessions untouched, silent 0 s |
| session runaway to 5.9 GiB in a 6 GiB guest | | target = maximum, probes silent from 742 s (sessions 4952 MiB) |
| 4 GiB machine | | maximum 2 GiB, silent 0 s |
| two machines growing on 20 GiB | | 8448/8448 MiB, equal, silent 0 s |
| page cache hog vs quiet neighbour | | guest 2816 vs 2560 MiB: cache buys nothing |
| machine shrinking under an idle guest | | 10 -> 4.75 GiB, neighbour grows to 8.5 GiB, no machine OOM |
| 60 s of failed samples | | target never shrinks, resumes |
| two machines growing, machine page cache filling (R2-5) | | pressure in 102 of 120 rounds before, 3 after (boot at the ceiling) |
| reviewer's 17 GiB host, 3 plain VMs, cache fills limits | | vm0 grows in the round its demand rises, never short, no pressure |
| 8 h steady state, plain VMs, middle band and saturated | | no move after settling, no round in pressure |
| parked machine, +3 GiB over 30 s | | guest has it after 30 s, probes silent 6 s |
| parked machine, +3 GiB in 1 s | | guest has it after 35 s, probes silent 34 s |
Machine out of memory: 0 s in every trace.

### 7.8 Open decisions
- LegacyDefaultAction "offer" (default) vs "apply": a Go variable, not exposed; "apply" would move old
  defaults to automatic silently. Needs the owner's call and, for production, the actual resource-limits.json.
- Parked-machine burst: never growing a guest past what the machine backs costs up to two gateway rounds
  (34 s of silent probes for an instantaneous 3 GiB burst). Alternatives: a short-triggered faster gateway
  round (5 s while a nested guest is short), or a larger headroom for machines that host a guest.
- short_bytes for pressure is the geometric step, not the demand: it can ask more than the guest settles
  at; bounded by the ceiling, the max-min level and the 5-minute hold.
- Session adoption into their own slice (separating probes from sessions) needs a spawn point owned by
  the guest provisioning; see 7.6.
- The governor's python tests are run by go test (TestResourceGovernorPython) when python3 exists; the
  macOS CI workflow lists the other python tests explicitly and does not list this one yet.

### 7.9 Risks
- The kernel model in the simulator is coarse; real zram ratios, reclaim latency and PSI differ.
- Incus balloon behaviour (deflate-on-oom, MemTotal under the balloon) is read from the driver's feature
  bits and memory blocks, not verified on lab hardware here.
- machine-memory.json freshness compares two clocks (gateway, workspace VM) within 45 s.
- The limits file is writable by the agent's user: an agent can tighten the browser down to the clamps
  (MemTotal/4) or relax it; it cannot protect more than 3/4 of the guest. Denial of service of the browser
  by the agent is possible within those bounds; confidentiality is unaffected.
- The governor uses systemctl set-property --runtime every change; drop-ins live in
  /run/systemd/system.control and vanish at reboot.
- 6 GiB guest maximum in an 8 GiB machine leaves ~0.75 GiB to the machine at the ceiling; the balloon
  target (capacity K) keeps the guest below what the machine backs, the ceiling alone does not.

### 7.10 What existing environments and guests see
- Automatic (no settings file, or automatic saved): at the next environment start the new size applies
  (production machine: 3 GiB / 3 vCPUs -> 6 GiB / 5 vCPUs). While running, /api/environment/resources
  says restart_required with the notice "自动分配已按这台机器重新计算为内存 6 GB、CPU 5 核，重启环境后生效。".
- Explicit values equal to the old default, saved by an older version: kept; the API adds
  legacy_allocation, the notice "内存 3 GB、CPU 3 核与旧版本的默认设置相同。改为自动（推荐）后，这台机器可以给到
  6 GB、5 核，需要重启环境；如果这是你特意设的，可以保留。" and offer {automatic, memory_gib, cpus}. One step:
  POST /api/environment/resources {"allocation":{"automatic":true},"apply":true}.
- Explicit values chosen in settings by this version ("chosen":"user"): never changed; the host agent's
  advice says "环境内存已用满手动设置的 N GB。改为自动（推荐）后可以调到 M GB，需要重启环境。" when short.
- Guests: the component revision changes, so every Linux guest asks for a component update; applying it
  restarts the browser (needed to move it into its slice) and starts the governor. zram's new size needs an
  environment restart. macOS and Windows guests and container mode: sizing unchanged (macOS LegacyLimits,
  Windows code untouched); level C is not provisioned on macOS/Windows guests. In containers the governor
  and zram are skipped (systemd-detect-virt), but the browser unit's Slice=, CPU/IO weights,
  OOMScoreAdjust=200 and OOMPolicy=continue apply there too (systemd creates the slice; no limits are set
  on it; raising an OOM score is allowed unprivileged).

### 7.11 Rollback state (complete)
Gateway: only instance-local limits.memory (exact MiB, 1 GiB..ceiling) - accepted by 0.1.15. Nothing else.
Workspace VM / desktop profile: machine-memory.json (ignored by 0.1.15, may be deleted),
resource-limits.json unchanged unless saved in settings ("chosen" is ignored by 0.1.15; a manual value
above the old share - possible now - makes 0.1.15 refuse to start: switch to automatic or delete the file
first). lima.yaml is rewritten at every start by whichever version starts.
Guest, persisted: /etc/systemd/system/cage-browser.slice, cage-resource-governor.service (enabled),
/usr/local/lib/cage-resource-governor.py, changed cage-browser.service, cage-editor.service and
/usr/local/bin/cage-memory-start. 0.1.15's component update rewrites the three changed files; it does not
know the slice or the governor, which keeps running. That is not harmless (review 2, R2-9): without a host
agent sending limits the governor applies its own plan, so it keeps the browser limited (about 3/4 of the
guest) on cage-browser.slice and, on Ubuntu guests, on the browser user's slice user.slice/user-<uid>.slice
where snap Chromium runs, and keeps protecting system.slice, user.slice and the session user's slice.
Remove it in the guest:
  1. sudo systemctl disable --now cage-resource-governor.service
     Its stop (SIGTERM, then ExecStopPost --reset) sets MemoryHigh=infinity, MemoryMax=infinity and
     MemoryLow=0 on cage-browser.slice, system.slice, user.slice, the unit's two accounts' user slices and
     every user-<uid>.slice holding a runtime memory setting, deletes those runtime drop-ins
     (/run/systemd/system.control/<unit>.d/50-MemoryHigh.conf, 50-MemoryMax.conf, 50-MemoryLow.conf) and
     removes /run/cage-resources.
  2. sudo rm -f /etc/systemd/system/cage-resource-governor.service /etc/systemd/system/cage-browser.slice
     /usr/local/lib/cage-resource-governor.py; sudo systemctl daemon-reload
  3. Check: B=$(systemctl show -P User cage-browser.service); S=$(systemctl show -P User cage-editor.service);
     systemctl show cage-browser.slice system.slice user.slice user-$(id -u "$B").slice user-$(id -u "$S").slice
     -p MemoryHigh -p MemoryMax -p MemoryLow (infinity, infinity, 0 everywhere);
     ls /run/systemd/system.control (no 50-Memory*.conf left by the governor); ls -d /run/cage-resources
     (absent).
  A governor installed before this fix (84012c5 up to review 2) resets the slices it finds but leaves the
  runtime drop-ins, which then say infinity/0 (the defaults) until the next boot; to remove them at once:
  sudo rm -f /run/systemd/system.control/{cage-browser,system,user}.slice.d/50-Memory{High,Max,Low}.conf
  /run/systemd/system.control/user-*.slice.d/50-Memory{High,Max,Low}.conf (or reboot the guest).
Guest, runtime (gone at reboot): /run/cage-resources, /run/systemd/system.control/*.d drop-ins, zram size,
vm.swappiness.

### 7.12 Reading the status for "short of memory, N GB possible"
cage-resources.json / ResourceStatus: short == true means the guest is under pressure and has all it can
get; limited_by is "maximum" (raise the maximum) or "machine" (the machine has no more now). advice
{message, memory_bytes, restart_required} is the sentence to show; possible_bytes > maximum_bytes is the
condition for "可以调到 N GB" (N = possible_bytes) - both are set once when the host agent starts, so a
caller can test them without advice. memoryNote in network_recovery.go should read Short/LimitedBy/Advice
and Possible vs Maximum; status.Pressure now means the protected classes stall (a contained browser is
BrowserPressure, not Pressure). Against an older host agent (no such fields) the workbench can compare
cage.AutomaticLimits() with lima.yaml's memory.

### 7.13 Rejected alternatives
- Proportional shares or demand-proportional split at level A: not max-min fair (a large demand starves a
  small one) and not monotone; water-filling over held demand is exact, fair and work-conserving.
- Growing a guest to its demand past what the machine backs (first B version, "a guest OOM is worse"):
  the machine's OOM killer would take QEMU, the whole guest; the guest's own OOM order takes one browser
  process. Replaced by publishing the shortfall to the gateway.
- Counting a nested shortfall as a shortage always: under host pressure, shrink-into-headroom and
  regrowth oscillated; only a nested guest under pressure counts (pressure flag in machine-memory.json).
- Keeping MemTotal/2 as the guest ceiling, or a three-eighths rule: the evidence machine had half its
  memory idle; the balloon target, not the ceiling, protects the machine.
- Rewriting old-default settings files to automatic: a read path that writes races with saves and cannot
  prove nobody chose the values; classify and offer instead.
- Session adoption into cage-sessions.slice through PID 1's private socket, Delegate= and leaf cgroups in
  the browser unit, the chromium.d hook: not checkable without a guest; deferred (7.6).
- memory.high on the sessions: the probes share their cgroup; throttling sessions would throttle the
  probes.
- Applying cgroup limits by writing cgroupfs directly: systemd rewrites them on daemon-reload;
  set-property --runtime goes through systemd and vanishes at reboot.
- A new gateway flag for the legacy policy: cmd/cage-tenant is outside this change; an environment
  variable selects it.
- A faster gateway round while a nested guest is short: would change the silent-round counting; left as
  an open decision (7.8).

### 7.14 Change list (origin/super-cage-redesign..resource-allocation)
internal/tenant: allocation.go (new, level A rule), allocation_test.go (new), memory.go (wiring, legacy
switch, new InstanceMemory fields), incus_memory.go (PSI, pswpin, machine-memory.json in one exec),
incus_memory_test.go, replay_test.go (new, simulator).
internal/cage: machine.go, machine_linux.go, machine_darwin.go, machine_windows.go, machine_other.go (new:
machine reading, roles, automatic sizing, boot size), machine_test.go, machine_linux_test.go (new),
guest_allocation.go (new: target rule, class limits, governor report parsing, simulation API),
guest_allocation_test.go (new), resources.go (probe, status fields, manager loop, machine-memory.json),
resources_test.go, resource_limits.go (origin, chosen marker, manual range, LegacyLimits),
resource-governor.py, resource-governor.test.py, resource_governor_test.go (new), guest-memory.sh (zram,
swappiness), guest-services.sh (browser slice and unit properties, editor OOM score), guest.go (governor
install), guest_memory_container_test.go, resource-allocation.md (this document).
internal/workbench: resources.go (API fields, apply, chosen), resources_test.go, macos_resources.go
(LegacyLimits). internal/vm: macos_resources.go (LegacyLimits), macos_resources_test.go.
Merged: origin/super-cage-redesign at b213406 (merge commit d79716b; the branch is not linear).

### 7.15 Test commands and results (macOS arm64 host, 2026-11-21)
- go vet ./internal/cage ./internal/tenant ./internal/workbench ./internal/vm, and the same with GOOS=linux
  and GOOS=windows: clean (only the linker's duplicate -lobjc warning).
- go test -race -count=1 -p 2 ./internal/cage ./internal/tenant ./internal/workbench ./internal/vm:
  ok cage 220.4 s, tenant 32.7 s, workbench 299.2 s, vm 1.7 s (after level C; before the Ubuntu governor
  change, which was then tested with python3 -I internal/cage/resource-governor.test.py: 8 tests OK, and
  go test ./internal/cage -run 'ResourceGovernor|ContainerMemory': ok).
- go test -race -count=20 -p 2 on the new tests (cage: GuestPlan|GuestDemand|GuestShort|GuestClass|
  Governor|AutomaticSize|Automatic|Legacy|ResourceManagerSends|AgentProfile|StatusStaysWithin|
  ResourceGovernorPython|ContainerMemory; tenant: Replay|Allocation|WaterFill|GuestMemoryReading;
  workbench: the two resource tests): ok cage 169.9 s, tenant 32.0 s, workbench 3.8 s.
- Replay table: go test ./internal/tenant -run Replay -v (7.7).
- Never executed here: machine_linux_test.go (Linux only; vetted with GOOS=linux), and nothing in a real
  guest, workspace VM or Incus host.

### 7.16 Verification on a real Linux guest (not done here)
1. Start the environment with this build; accept "环境组件需要更新" (restarts the browser).
2. In the guest: systemctl status cage-resource-governor; cat /run/cage-resources/state (fresh "governor 1"
   stamp, three class lines); cat /run/cage-resources/in/limits (rewritten every 2 s); ls -ld
   /run/cage-resources /run/cage-resources/in (root 0755; cage 0755); id cage (the probe user is cage).
3. systemctl show cage-browser.slice -p MemoryHigh -p MemoryMax; systemctl show system.slice user.slice
   user-$(id -u cage).slice -p MemoryLow; grep cgroup2 /proc/mounts (memory_recursiveprot present);
   ls /run/systemd/system.control.
4. systemctl show cage-browser.service -p Slice -p OOMPolicy -p OOMScoreAdjust; cat /proc/<chromium
   pid>/cgroup (Debian: /cage-browser.slice/cage-browser.service; Ubuntu: under
   user.slice/user-<cage-browser uid>.slice); the probe's own cgroup: run cat /proc/self/cgroup through
   the same SSH path the host agent uses (expected user.slice/user-<cage uid>.slice/session-*.scope).
5. After an environment restart: cat /proc/swaps; cat /sys/block/zram*/disksize (min(MemTotal/2, 4 GiB));
   sysctl vm.swappiness (100).
6. Containment: sudo systemd-run --slice=cage-browser.slice --uid=cage-browser python3 -c
   "a=bytearray(int(8e9))" - expect throttling then an OOM kill of that process only; the browser
   service stays active; cage-resources.json keeps a fresh updated_at.
7. Host side: <profile>/state/lima/<instance>/cage-resources.json: capacity_bytes, demand_bytes, reason,
   summary, classes; GET /api/environment/resources: allocation_origin, automatic_memory_gib, notice.
   The host agent's profile: tr '\0' '\n' < /proc/<hostagent pid>/environ | grep CLAUDE_CAGE_CONFIG
   (otherwise the <profile>/state/lima/<instance> layout is used).

### 7.17 Verification on an Incus gateway host (not done here)
1. Install the new cage-tenant; journalctl -u <unit> | grep "elastic VM memory" shows "max-min fair over
   held demand" (CAGE_TENANT_MEMORY_POLICY=legacy in the unit's environment selects the old policy).
2. incus config show cage-<id> --expanded | grep limits.memory; incus config get cage-<id> limits.memory
   (instance-local value, exact MiB).
3. In a workspace VM: cat /sys/bus/virtio/drivers/virtio_balloon/virtio*/features (character 3 = bit 2,
   deflate-on-oom); grep MemTotal /proc/meminfo before and after incus config set cage-<id>
   limits.memory=<smaller>; GET /api/environment/resources automatic_memory_gib stays the same.
4. While the nested guest is short: cat /home/cage/.claude-cage/machine-memory.json (fresh updated_at,
   short_bytes, pressure); the gateway log shows "nested guest needs memory".
5. Rollback drill: stop the new cage-tenant, start 0.1.15, check that every VM still starts and the old
   balancer accepts the instance-local limits.memory.

### 7.18 Unverified
- machine_linux.go (meminfo, vmstat balloon, memory blocks, virtio features) has only been compiled.
- Whether Incus negotiates deflate-on-oom and whether MemTotal moves with limits.memory in a workspace VM.
- Everything in the guest: set-property --runtime on the slices, memory_recursiveprot, the browser in its
  slice with OOMPolicy=continue, snap Chromium's cgroup on Ubuntu, zram size after restart, the probe
  user and group being cage, the probe's cgroup being user.slice, the host agent's profile path for
  machine-memory.json.
- The simulator's kernel model and its fitted numbers (7.7).
- On a Linux test machine with an inflated virtio balloon, tests that stub host.MemGiB read the real
  /proc/vmstat through machineBootGiB and are not deterministic.
