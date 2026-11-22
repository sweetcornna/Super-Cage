// Origins that hold site storage in the regular profile, read from the
// profile's own databases: Local Storage and the Service Worker and File
// System databases are LevelDB stores (their keys name the storage key),
// IndexedDB keeps one folder per origin and Cache Storage one index per
// storage key. Kept sites (browser-data.mjs) exclude every origin found here
// under a kept domain, so a subdomain without an open tab or a history entry
// keeps its sign-in too. Read only, while Chromium runs: table files never
// change once written, and a log or table that vanishes mid-read (a
// compaction) makes the reader list the folder again.
import { open, readdir, readFile } from "node:fs/promises";
import { join } from "node:path";

// Snappy, the block compression of Chromium's LevelDB.
export function snappyDecompress(input) {
  let position = 0, length = 0, shift = 0;
  for (;;) {
    const byte = input[position++];
    if (byte === undefined) throw new Error("snappy: truncated length");
    length |= (byte & 0x7f) << shift;
    if (byte < 0x80) break;
    shift += 7;
    if (shift > 28) throw new Error("snappy: length too long");
  }
  const output = Buffer.alloc(length);
  let written = 0;
  while (position < input.length) {
    const tag = input[position++];
    let size, offset;
    switch (tag & 3) {
      case 0: {
        size = tag >> 2;
        if (size >= 60) {
          const bytes = size - 59;
          size = 0;
          for (let i = 0; i < bytes; i++) size |= input[position++] << (8 * i);
        }
        size += 1;
        if (position + size > input.length || written + size > length) throw new Error("snappy: literal out of range");
        input.copy(output, written, position, position + size);
        position += size; written += size;
        continue;
      }
      case 1: size = ((tag >> 2) & 7) + 4; offset = ((tag >> 5) << 8) | input[position++]; break;
      case 2: size = (tag >> 2) + 1; offset = input.readUInt16LE(position); position += 2; break;
      default: size = (tag >> 2) + 1; offset = input.readUInt32LE(position); position += 4; break;
    }
    if (offset === 0 || offset > written || written + size > length) throw new Error("snappy: copy out of range");
    for (let i = 0; i < size; i++, written++) output[written] = output[written - offset];
  }
  if (written !== length) throw new Error("snappy: short output");
  return output;
}

const varint = (buffer, state) => {
  let value = 0, shift = 0;
  for (;;) {
    const byte = buffer[state.at++];
    if (byte === undefined) throw new Error("leveldb: truncated varint");
    value += (byte & 0x7f) * 2 ** shift;
    if (byte < 0x80) return value;
    shift += 7;
    if (shift > 63) throw new Error("leveldb: varint too long");
  }
};

// Every user key of a table file within one of the wanted key ranges, with
// its sequence number and whether it is a value (true) or a deletion (false).
// Keys are sorted, so only the data blocks whose range meets a wanted prefix
// are read: the Local Storage values after the META: keys stay on disk.
async function tableEntries(path, ranges, add) {
  const handle = await open(path, "r");
  try {
    const { size } = await handle.stat();
    const read = async (offset, length) => {
      const buffer = Buffer.alloc(length);
      const { bytesRead } = await handle.read(buffer, 0, length, offset);
      if (bytesRead !== length) throw new Error("leveldb: short read");
      return buffer;
    };
    if (size < 48) throw new Error("leveldb: not a table");
    const footer = await read(size - 48, 48);
    if (footer.readUInt32LE(40) !== 0x8b80fb57 || footer.readUInt32LE(44) !== 0xdb477524) throw new Error("leveldb: not a table");
    const state = { at: 0 };
    varint(footer, state); varint(footer, state); // metaindex handle
    const block = async (offset, length) => {
      if (offset + length + 5 > size) throw new Error("leveldb: block out of range");
      const raw = await read(offset, length + 5);
      const contents = raw.subarray(0, length);
      if (raw[length] === 0) return contents;
      if (raw[length] === 1) return snappyDecompress(contents);
      throw new Error("leveldb: unknown block compression " + raw[length]);
    };
    const index = await block(varint(footer, state), varint(footer, state));
    let previous = null;
    for (const separator of entries(index)) {
      // A block holds user keys from the previous separator to its own.
      const high = separator.key.subarray(0, Math.max(0, separator.key.length - 8)), low = previous;
      previous = Buffer.from(high);
      if (!ranges.some(([from, to]) => Buffer.compare(high, from) >= 0 && (!low || Buffer.compare(low, to) < 0))) continue;
      const handleState = { at: 0 };
      const data = await block(varint(separator.value, handleState), varint(separator.value, handleState));
      for (const entry of entries(data)) {
        if (entry.key.length < 8) continue;
        const user = entry.key.subarray(0, entry.key.length - 8);
        if (!ranges.some(([from, to]) => Buffer.compare(user, from) >= 0 && Buffer.compare(user, to) < 0)) continue;
        const trailer = entry.key.readBigUInt64LE(entry.key.length - 8);
        add(user.toString("latin1"), Number(trailer >> 8n), (trailer & 0xffn) === 1n);
      }
    }
  } finally {
    await handle.close();
  }
}

function* entries(data) {
  const restarts = data.readUInt32LE(data.length - 4);
  const end = data.length - 4 * (restarts + 1);
  if (end < 0) throw new Error("leveldb: bad block");
  const state = { at: 0 };
  let key = Buffer.alloc(0);
  while (state.at < end) {
    const shared = varint(data, state), unshared = varint(data, state), length = varint(data, state);
    if (shared > key.length || state.at + unshared + length > end) throw new Error("leveldb: bad entry");
    key = Buffer.concat([key.subarray(0, shared), data.subarray(state.at, state.at + unshared)]);
    state.at += unshared;
    yield { key, value: data.subarray(state.at, state.at + length) };
    state.at += length;
  }
}

// A log file: 32 KiB blocks of fragments (FULL, FIRST, MIDDLE, LAST), each
// record a write batch. A torn tail (Chromium is writing) ends the read.
function logEntries(file, ranges, add) {
  let record = [];
  for (let block = 0; block < file.length; block += 32768) {
    let at = block;
    const end = Math.min(block + 32768, file.length);
    while (at + 7 <= end) {
      const length = file.readUInt16LE(at + 4), type = file[at + 6];
      if (type === 0 && length === 0) break;
      if (at + 7 + length > end) return;
      const fragment = file.subarray(at + 7, at + 7 + length);
      at += 7 + length;
      if (type === 1) batch(fragment, ranges, add);
      else if (type === 2) record = [fragment];
      else if (type === 3) record.push(fragment);
      else if (type === 4) { record.push(fragment); batch(Buffer.concat(record), ranges, add); record = []; }
    }
  }
}

function batch(data, ranges, add) {
  if (data.length < 12) return;
  let sequence = Number(data.readBigUInt64LE(0));
  const count = data.readUInt32LE(8);
  const state = { at: 12 };
  for (let i = 0; i < count && state.at < data.length; i++, sequence++) {
    const tag = data[state.at++];
    const keyLength = varint(data, state);
    const key = data.subarray(state.at, state.at + keyLength);
    state.at += keyLength;
    if (tag === 1) { const valueLength = varint(data, state); state.at += valueLength; }
    if (ranges.some(([from, to]) => Buffer.compare(key, from) >= 0 && Buffer.compare(key, to) < 0)) add(key.toString("latin1"), sequence, tag === 1);
  }
}

// Live user keys with one of the prefixes: the newest entry of each key
// decides. Throws when a file cannot be read as LevelDB.
export async function liveKeys(directory, prefixes) {
  // A prefix as a key range [prefix, prefix with its last byte raised).
  const ranges = prefixes.map(prefix => { const from = Buffer.from(prefix, "latin1"), to = Buffer.from(from); to[to.length - 1]++; return [from, to]; });
  for (let attempt = 0; ; attempt++) {
    let names;
    try { names = await readdir(directory); } catch (error) { if (error.code === "ENOENT") return new Set(); throw error; }
    const newest = new Map();
    const add = (key, sequence, live) => { const seen = newest.get(key); if (!seen || seen.sequence < sequence) newest.set(key, { sequence, live }); };
    try {
      for (const name of names.filter(name => /^\d+\.(ldb|sst)$/.test(name))) await tableEntries(join(directory, name), ranges, add);
      for (const name of names.filter(name => /^\d+\.log$/.test(name)).sort()) logEntries(await readFile(join(directory, name)), ranges, add);
    } catch (error) {
      if (error.code === "ENOENT" && attempt < 3) continue;
      throw error;
    }
    return new Set([...newest].filter(([, entry]) => entry.live).map(([key]) => key));
  }
}

// A storage key's origin: first-party keys are the origin itself, partitioned
// ones carry "^" and the top-level site after it.
export const storageKeyOrigin = key => {
  const origin = key.split("^")[0];
  try {
    const url = new URL(origin);
    return ["http:", "https:"].includes(url.protocol) && (url.origin === origin || url.origin + "/" === origin) ? url.origin : "";
  } catch { return ""; }
};

// Origins with Local Storage, service workers, IndexedDB, Cache Storage or a
// file system in the profile's Default folder.
export async function storageOrigins(profileDefault) {
  const origins = new Set();
  const take = (keys, prefix) => { for (const key of keys) { const origin = storageKeyOrigin(key.slice(prefix.length).split("\x00")[0]); if (origin) origins.add(origin); } };
  take(await liveKeys(join(profileDefault, "Local Storage", "leveldb"), ["META:"]), "META:");
  take(await liveKeys(join(profileDefault, "Service Worker", "Database"), ["INITDATA_UNIQUE_ORIGIN:"]), "INITDATA_UNIQUE_ORIGIN:");
  take(await liveKeys(join(profileDefault, "File System", "Origins"), ["ORIGIN:"]), "ORIGIN:");
  try {
    for (const name of await readdir(join(profileDefault, "IndexedDB"))) {
      const match = /^(https?)_(.+)_(\d+)\.indexeddb\.(leveldb|blob)$/.exec(name);
      // Chromium names a default port 0.
      if (match) origins.add(storageKeyOrigin(`${match[1]}://${match[2]}${match[3] === "0" ? "" : ":" + match[3]}`) || "");
    }
  } catch (error) { if (error.code !== "ENOENT") throw error; }
  try {
    const caches = join(profileDefault, "Service Worker", "CacheStorage");
    for (const name of await readdir(caches)) {
      let index;
      try { index = await readFile(join(caches, name, "index.txt")); } catch { continue; }
      for (const match of index.toString("latin1").matchAll(/https?:\/\/[\x21-\x7e]+/g)) { const origin = storageKeyOrigin(match[0]); if (origin) origins.add(origin); }
    }
  } catch (error) { if (error.code !== "ENOENT") throw error; }
  origins.delete("");
  return origins;
}
