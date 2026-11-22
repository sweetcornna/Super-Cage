// Start-up side of browser data management, imported by the controller
// before it launches Chromium: the disk-cache bound and a requested reset.
// Synchronous and small; the rest of browser data loads on first use.
import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, statfsSync, writeFileSync, rmSync } from "node:fs";
import { rm } from "node:fs/promises";
import { basename, join } from "node:path";

const MiB = 1 << 20;
export const cacheCeiling = 256 * MiB;

// Chromium's own HTTP cache size from free space (net/disk_cache/cache_util.cc,
// PreferredCacheSizeInternal, 80 MiB default): 80 % of a nearly full disk,
// 80 MiB, 10 %, 200 MiB, then 1 % of free space. A probe with Chrome 157 on a
// large disk saw the cache pass 568 MB.
export function chromiumCacheBytes(free) {
  const base = 80 * MiB;
  if (free < base * 10 / 8) return Math.floor(free * 0.8);
  if (free < base * 10) return base;
  if (free < base * 25) return Math.floor(free / 10);
  if (free < base * 250) return base * 5 / 2;
  return Math.floor(free / 100);
}

// The bound only ever lowers Chromium's choice: at most 256 MiB and 5 % of
// the free space when the controller starts.
export function diskCacheBytes(path, statfs = statfsSync) {
  try {
    const info = statfs(path);
    const free = Number(info.bavail) * Number(info.bsize);
    if (!Number.isFinite(free) || free <= 0) return cacheCeiling;
    return Math.max(MiB, Math.floor(Math.min(cacheCeiling, chromiumCacheBytes(free), free * 0.05) / MiB) * MiB);
  } catch {
    return cacheCeiling;
  }
}

export const resetMarker = home => join(home, "browser-reset.json");
const resetResult = home => join(home, "browser-reset-result.json");
// A marker the controller did not act on soon after the request (Chromium
// never exited, the host gave up) must not wipe the profile days later.
const markerLifetime = 10 * 60_000;

// The host names each request, so it can ask how that one ended.
export function requestReset(home, id = "", requested = Date.now()) {
  writeFileSync(resetMarker(home), JSON.stringify({ id, requested }), { mode: 0o600 });
}

const readJSON = path => { try { return JSON.parse(readFileSync(path, "utf8")); } catch { return null; } };

// The host gave up waiting: the reset it reported as not done must not happen
// at a later start.
export function cancelReset(home, id) {
  const marker = readJSON(resetMarker(home));
  if (marker && (!id || marker.id === id)) rmSync(resetMarker(home), { force: true });
}

// How the request with this id ended, as written at the controller's start:
// done, failed (the old profile is still in use), incomplete (the old profile
// could not be put back; it is kept aside, not deleted), expired or unknown.
export function resetStatus(home, id) {
  const result = readJSON(resetResult(home));
  return result && result.id === id && ["done", "failed", "incomplete", "expired"].includes(result.state) ? { state: result.state } : { state: "unknown" };
}

function writeResult(home, id, state, aside = "") {
  try { writeFileSync(resetResult(home), JSON.stringify({ id, state, at: Date.now(), ...(aside ? { aside } : {}) }), { mode: 0o600 }); } catch {}
}

function seedProfile(profile, language) {
  mkdirSync(join(profile, "Default"), { recursive: true, mode: 0o700 });
  writeFileSync(join(profile, "Default", "Preferences"), JSON.stringify(seedPreferences(language)), { mode: 0o600 });
}

// The preferences guest-start.sh seeds into a profile; a fresh profile gets
// them again so language and the password policy read the same as before.
export function seedPreferences(language) {
  return {
    intl: { accept_languages: language, selected_languages: language },
    translate: { enabled: false },
    credentials_enable_service: false,
    profile: { password_manager_enabled: false },
  };
}

// Runs before Chromium starts. The old profile is moved aside at once and
// removed in the background while Chromium starts; favourites, the vault and
// Cage settings live on the host, imported extensions and the download list
// outside the profile. A rename or seeding that fails leaves the old profile
// in place and says so in the result the host reads. Old profiles a previous
// start did not finish removing are removed now.
export function applyPendingReset(home, language, { removeLater = path => rm(path, { recursive: true, force: true }), rename = renameSync, seed = seedProfile, now = Date.now } = {}) {
  let names = [];
  try { names = readdirSync(home); } catch {}
  // An old profile a failed reset could not put back stays until a later
  // reset succeeds.
  const last = readJSON(resetResult(home));
  const stranded = last?.state === "incomplete" ? last.aside : "";
  for (const name of names) if (/^profile\.reset-\d+$/.test(name) && name !== stranded) void Promise.resolve(removeLater(join(home, name))).catch(() => {});
  const marker = resetMarker(home);
  if (!existsSync(marker)) return false;
  const request = readJSON(marker) || {};
  const id = typeof request.id === "string" ? request.id.slice(0, 64) : "";
  const requested = Number(request.requested) || Date.parse(request.requested) || 0;
  const profile = join(home, "profile");
  let aside = "";
  try {
    if (!(now() - requested <= markerLifetime && requested <= now() + 60_000)) {
      writeResult(home, id, "expired");
      return false;
    }
    try {
      if (existsSync(profile)) {
        const target = join(home, "profile.reset-" + now());
        rename(profile, target);
        aside = target;
      }
      seed(profile, language);
    } catch {
      if (aside) {
        try { rmSync(profile, { recursive: true, force: true }); rename(aside, profile); aside = ""; } catch {}
      }
      // Still on the old profile unless it could not be put back; then it
      // stays aside, never removed, and the reset is not reported done.
      if (!aside) { writeResult(home, id, "failed"); return false; }
      writeResult(home, id, "incomplete", basename(aside));
      return false;
    }
  } finally {
    rmSync(marker, { force: true });
  }
  if (aside) void Promise.resolve(removeLater(aside)).catch(() => {});
  writeResult(home, id, "done");
  return true;
}
