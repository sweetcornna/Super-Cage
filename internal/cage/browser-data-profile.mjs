// Start-up side of browser data management, imported by the controller
// before it launches Chromium: the disk-cache bound and a requested reset.
// Synchronous and small; the rest of browser data loads on first use.
import { existsSync, mkdirSync, renameSync, statfsSync, writeFileSync, rmSync } from "node:fs";
import { rm } from "node:fs/promises";
import { join } from "node:path";

const MiB = 1 << 20;
export const cacheCeiling = 256 * MiB, cacheFloor = 32 * MiB;

// Chromium sizes its HTTP cache from free space (up to 1 % of it on a large
// disk, with no fixed ceiling). Bound it: 5 % of the free space now, between
// 32 and 256 MiB, so the profile's cache cannot grow with the disk.
export function diskCacheBytes(path, statfs = statfsSync) {
  try {
    const info = statfs(path);
    const free = Number(info.bavail) * Number(info.bsize);
    if (!Number.isFinite(free) || free <= 0) return cacheCeiling;
    return Math.max(cacheFloor, Math.min(cacheCeiling, Math.floor(free * 0.05 / MiB) * MiB));
  } catch {
    return cacheCeiling;
  }
}

export const resetMarker = home => join(home, "browser-reset.json");

export function requestReset(home) {
  writeFileSync(resetMarker(home), JSON.stringify({ requested: new Date().toISOString() }), { mode: 0o600 });
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
// removed in the background; favourites, the vault and Cage settings live on
// the host, imported extensions and the download list outside the profile.
export function applyPendingReset(home, language, removeLater = path => rm(path, { recursive: true, force: true })) {
  const marker = resetMarker(home);
  if (!existsSync(marker)) return false;
  const profile = join(home, "profile");
  try {
    if (existsSync(profile)) {
      const aside = join(home, "profile.reset-" + Date.now());
      renameSync(profile, aside);
      void Promise.resolve(removeLater(aside)).catch(() => {});
    }
    mkdirSync(join(profile, "Default"), { recursive: true, mode: 0o700 });
    writeFileSync(join(profile, "Default", "Preferences"), JSON.stringify(seedPreferences(language)), { mode: 0o600 });
  } finally {
    rmSync(marker, { force: true });
  }
  return true;
}
