// Release bookkeeping for scripts/release/release.sh: asset names, checksum
// files, release links, build metadata and the uploaded digests. No network and
// no signing here; the shell driver runs the Apple, Android and GitHub tools.
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { createReadStream, existsSync, lstatSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

// The in-place updater of every published App rejects a larger Windows installer.
export const maxInstallerBytes = 512 * 1024 * 1024;
export const sumsFiles = { main: 'SHA256SUMS', linux: 'SHA256SUMS-linux-desktop.txt', windows: 'SHA256SUMS-windows-desktop.txt' };
const versionSyntax = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
const literal = text => text.replace(/[.*+?^$()|[\]\\{}]/g, '\\$&');

export function checkVersion(version) {
  if (typeof version !== 'string' || !versionSyntax.test(version)) throw new Error(`Invalid version: ${version}`);
  return version;
}

// Corresponding-source names pinned at the release commit. The macOS, Linux
// and Windows builds ship the same Mihomo, so one source archive serves all three.
export function dependencyNames({ linux, windows, proxyCore }) {
  const problems = [];
  const mihomo = /^version=(v[0-9.]+)$/m.exec(proxyCore)?.[1];
  const source = linux?.mihomoSource ?? {};
  const pinned = String(source.sha256 ?? '').toLowerCase();
  if (!mihomo) problems.push('scripts/fetch-proxy-core.sh names no Mihomo version');
  else if (source.file !== `mihomo-${mihomo}-source.tar.gz`) problems.push(`macOS ships Mihomo ${mihomo} but packaging/linux/dependencies.json pins ${source.file}`);
  if (!/^[0-9a-f]{64}$/.test(pinned)) problems.push('packaging/linux/dependencies.json pins no Mihomo source SHA-256');
  if (windows?.mihomoSource?.file !== source.file || String(windows?.mihomoSource?.hash ?? '').toLowerCase() !== pinned) {
    problems.push('packaging/windows/dependencies.json and packaging/linux/dependencies.json pin different Mihomo sources');
  }
  const qemu = windows?.qemu?.version;
  return {
    mihomoSource: source.file,
    mihomoSha256: pinned,
    qemu: [
      `qemu-${qemu}-cage-changes-source.tar.gz`,
      `qemu-${qemu}-windows-full-source.tar.gz`,
      `${windows?.qemuSource?.file}.gz`,
      `qemu-keycodemapdb-${windows?.qemuKeycodemap?.version}-source.tar.gz`,
    ].sort(),
    problems,
  };
}

// Every asset by checksum file, in the order of the published files (0.1.17).
export function assetGroups(version, names) {
  const v = checkVersion(version);
  return {
    main: [
      `Super-Cage-${v}-macos-arm64.dmg`, `Super-Cage-${v}-android-arm64.apk`,
      `Super-Cage-NoVirt-${v}-linux-amd64.tar.gz`, `Super-Cage-NoVirt-${v}-linux-arm64.tar.gz`,
      `Super-Cage-Server-${v}-linux-amd64.tar.gz`, `Super-Cage-Server-${v}-linux-arm64.tar.gz`,
      `cage-macos-guest-${v}-source.tar.gz`, names.mihomoSource, 'release-metadata.json',
    ],
    linux: [`Super-Cage-${v}-linux-amd64.deb`, 'linux-desktop-build-metadata.json'],
    windows: [`Super-Cage-${v}-windows-amd64-setup-unsigned.exe`, names.mihomoSource, ...names.qemu, 'windows-desktop-build-metadata.json'],
  };
}

// A checksum file is written once its group has a binary to describe.
export function groupPrimaries(version) {
  const v = checkVersion(version);
  return {
    main: [`Super-Cage-${v}-macos-arm64.dmg`, `Super-Cage-${v}-android-arm64.apk`,
      ...['NoVirt', 'Server'].flatMap(edition => ['amd64', 'arm64'].map(arch => `Super-Cage-${edition}-${v}-linux-${arch}.tar.gz`))],
    linux: [`Super-Cage-${v}-linux-amd64.deb`],
    windows: [`Super-Cage-${v}-windows-amd64-setup-unsigned.exe`],
  };
}

export function renderSums(order, hashes) {
  return order.filter(name => hashes.has(name)).map(name => `${hashes.get(name)}  ${name}\n`).join('');
}

export function parseSums(text) {
  const entries = new Map();
  for (const line of text.split(/\r?\n/)) {
    if (!line) continue;
    const match = /^([0-9a-f]{64}) [ *]([A-Za-z0-9][A-Za-z0-9._-]*)$/i.exec(line);
    if (!match) throw new Error(`Invalid checksum line: ${line}`);
    const known = entries.get(match[2]);
    if (known && known !== match[1].toLowerCase()) throw new Error(`Two checksums for ${match[2]}`);
    entries.set(match[2], match[1].toLowerCase());
  }
  return entries;
}

// The platforms whose binaries SHA256SUMS describes (the Linux server editions
// count as linux-amd64 and linux-arm64).
export function distributionPlatforms(present, version) {
  const v = checkVersion(version);
  const has = name => present.includes(name);
  const linux = arch => ['NoVirt', 'Server'].some(edition => has(`Super-Cage-${edition}-${v}-linux-${arch}.tar.gz`));
  return [
    ...(has(`Super-Cage-${v}-macos-arm64.dmg`) ? ['macos-arm64'] : []),
    ...(linux('amd64') ? ['linux-amd64'] : []),
    ...(linux('arm64') ? ['linux-arm64'] : []),
    ...(has(`Super-Cage-${v}-android-arm64.apk`) ? ['android-arm64'] : []),
  ];
}

export function releaseMetadata(base, { platforms, signed, notarized }) {
  const document = { ...base, distribution: { platforms, macosDeveloperIDSigned: signed, macosNotarized: notarized } };
  return `${JSON.stringify(document, null, 2)}\n`;
}

// What scripts/release-version.mjs metadata writes: {schema, platform, version, commitSha}.
export function checkBuildMetadata(text, { platform, version, sha }) {
  let document;
  try { document = JSON.parse(text); } catch { return ['not JSON']; }
  const problems = [];
  if (document?.schema !== 1) problems.push(`schema ${document?.schema}`);
  if (document?.platform !== platform) problems.push(`platform ${document?.platform}, expected ${platform}`);
  if (document?.version !== version) problems.push(`version ${document?.version}, expected ${version}`);
  if (document?.commitSha !== sha) problems.push(`commitSha ${document?.commitSha}, expected ${sha}`);
  return problems;
}

// File names a Markdown text downloads from the release of version: release
// links, "$base/<name>" after base=.../releases/download/v<version>, and
// versioned asset names written anywhere.
export function releaseReferences(text, version) {
  const v = literal(checkVersion(version));
  const found = new Set();
  const add = name => found.add(name.replace(/\.+$/, ''));
  for (const match of text.matchAll(new RegExp(`releases/download/v${v}/([A-Za-z0-9._-]+)`, 'g'))) add(match[1]);
  const bases = new RegExp(`\\b([A-Za-z_][A-Za-z0-9_]*)=["']?[^\\s"']*releases/download/v${v}(?![/A-Za-z0-9.-])`, 'g');
  for (const base of text.matchAll(bases)) {
    const variable = literal(base[1]);
    for (const match of text.matchAll(new RegExp(`\\$(?:\\{${variable}\\}|${variable}(?![A-Za-z0-9_]))/([A-Za-z0-9._-]+)`, 'g'))) add(match[1]);
  }
  const assetName = `(?<![A-Za-z0-9._-])((?:Super-Cage(?:-Server|-NoVirt)?|cage-macos-guest)-${v}-[A-Za-z0-9._-]*?\\.(?:dmg|deb|exe|apk|tar\\.gz|zip|txt|json))(?![A-Za-z0-9_-])`;
  for (const match of text.matchAll(new RegExp(assetName, 'g'))) add(match[1]);
  return [...found].sort();
}

// Move the release links and asset names of the README and docs from one
// version to the next. Other mentions of the old version are left for a person.
export function bumpReleaseReferences(text, from, to) {
  const f = literal(checkVersion(from));
  checkVersion(to);
  let count = 0;
  const end = '(?![0-9A-Za-z-]|\\.[0-9A-Za-z])';
  const swap = (_, before) => { count++; return before + to; };
  const releaseUrl = new RegExp(`/releases/(?:download|tag)/v${f}${end}`);
  const labelVersion = new RegExp(`(^|[^0-9A-Za-z.-])(v)${f}${end}`, 'g');
  let result = text.replace(/\[([^\]\n]*)\]\(([^)\s]*\/releases\/(?:download|tag)\/v[^)\s]*)\)/g, (link, label, url) => {
    if (!releaseUrl.test(url)) return link;
    return `[${label.replace(labelVersion, (_, before, v) => { count++; return before + v + to; })}](${url})`;
  });
  result = result.replace(new RegExp(`(releases/(?:download|tag)/v)${f}${end}`, 'g'), swap);
  const assetName = `((?<![A-Za-z0-9._-])(?:Super-Cage(?:-Server|-NoVirt)?|cage-macos-guest)-)${f}(?=-(?:macos|linux|windows|android|source)(?![A-Za-z0-9]))`;
  result = result.replace(new RegExp(assetName, 'g'), swap);
  return { text: result, count };
}

export function versionMentions(text, version) {
  const mention = new RegExp(`(?<![0-9A-Za-z.])v?${literal(checkVersion(version))}(?![0-9A-Za-z]|\\.[0-9])`);
  return text.split('\n').flatMap((line, index) => (mention.test(line) ? [{ line: index + 1, text: line.trim() }] : []));
}

// GitHub's asset digests against the local checksum files, both ways.
export function compareDigests(local, assets) {
  const problems = [];
  const remote = new Map();
  for (const asset of assets) {
    if (remote.has(asset.name)) problems.push(`uploaded twice: ${asset.name}`);
    remote.set(asset.name, asset);
  }
  for (const [name, asset] of remote) {
    const expected = local.get(name);
    if (!expected) { problems.push(`on GitHub but not in the local assets: ${name}`); continue; }
    if (asset.state && asset.state !== 'uploaded') problems.push(`upload state ${asset.state}: ${name}`);
    if (!asset.digest) problems.push(`GitHub reports no digest: ${name}`);
    else if (asset.digest.toLowerCase() !== `sha256:${expected.sha256}`) problems.push(`digest differs: ${name} (GitHub ${asset.digest}, local sha256:${expected.sha256})`);
    if (expected.size !== undefined && asset.size !== expected.size) problems.push(`size differs: ${name} (GitHub ${asset.size}, local ${expected.size})`);
  }
  for (const name of local.keys()) if (!remote.has(name)) problems.push(`not uploaded: ${name}`);
  return problems;
}

export async function sha256File(path) {
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  return hash.digest('hex');
}

function regularFiles(directory) {
  return readdirSync(directory).filter(name => !name.startsWith('.') && lstatSync(join(directory, name)).isFile()).sort();
}

// The local side of the digest check: what the checksum files list, plus the
// checksum files themselves. The caller has just run shasum -c on them.
export async function localDigests(directory) {
  const local = new Map();
  for (const file of Object.values(sumsFiles)) {
    const path = join(directory, file);
    if (!existsSync(path)) continue;
    for (const [name, sha256] of parseSums(readFileSync(path, 'utf8'))) {
      local.set(name, { sha256, size: lstatSync(join(directory, name)).size });
    }
    local.set(file, { sha256: await sha256File(path), size: lstatSync(path).size });
  }
  return local;
}

function atCommit(git, sha, path) {
  return execFileSync('git', ['-C', git, 'show', `${sha}:${path}`], { encoding: 'utf8', maxBuffer: 64 << 20 });
}

function documentsAtCommit(git, sha) {
  return execFileSync('git', ['-C', git, 'ls-tree', '-r', '--name-only', sha, '--', 'README.md', 'docs'], { encoding: 'utf8' })
    .split('\n').filter(path => path.endsWith('.md'));
}

// Write the checksum files and run the bookkeeping checks on an assets directory.
export async function assemble({ directory, version, sha, git, android, mihomoCopies = [], macosMetadata, signed, notarized }) {
  const report = [];
  const ok = text => report.push(['ok', text]);
  const fail = text => report.push(['FAIL', text]);
  const names = dependencyNames({
    linux: JSON.parse(atCommit(git, sha, 'packaging/linux/dependencies.json')),
    windows: JSON.parse(atCommit(git, sha, 'packaging/windows/dependencies.json')),
    proxyCore: atCommit(git, sha, 'scripts/fetch-proxy-core.sh'),
  });
  names.problems.forEach(fail);
  const groups = assetGroups(version, names);
  const primaries = groupPrimaries(version);
  for (const file of Object.values(sumsFiles)) if (existsSync(join(directory, file))) throw new Error(`${file} already exists in ${directory}`);

  if (macosMetadata) {
    const base = JSON.parse(readFileSync(macosMetadata, 'utf8'));
    const platforms = distributionPlatforms(regularFiles(directory), version);
    writeFileSync(join(directory, 'release-metadata.json'), releaseMetadata(base, { platforms, signed, notarized }));
  }
  const present = regularFiles(directory);
  const known = new Set(Object.values(groups).flat());
  for (const name of present) if (!known.has(name)) fail(`unexpected file in the assets: ${name}`);
  const required = new Set(known);
  if (!android) required.delete(`Super-Cage-${version}-android-arm64.apk`);
  for (const name of required) if (!present.includes(name)) report.push(['MISSING', name]);

  const metadata = [['release-metadata.json', 'macos'], ['linux-desktop-build-metadata.json', 'linux'], ['windows-desktop-build-metadata.json', 'windows']];
  for (const [file, platform] of metadata) {
    if (!present.includes(file)) continue;
    const problems = checkBuildMetadata(readFileSync(join(directory, file), 'utf8'), { platform, version, sha });
    if (problems.length) fail(`${file}: ${problems.join('; ')}`);
    else ok(`${file}: ${platform} ${version} at ${sha.slice(0, 12)}`);
  }

  const hashes = new Map();
  for (const name of present) if (known.has(name)) hashes.set(name, await sha256File(join(directory, name)));
  if (hashes.has(names.mihomoSource)) {
    const copies = [join(directory, names.mihomoSource), ...mihomoCopies];
    const differing = [];
    for (const copy of copies) if ((await sha256File(copy)) !== names.mihomoSha256) differing.push(copy);
    if (differing.length) fail(`Mihomo source differs from the pinned SHA-256: ${differing.join(', ')}`);
    else ok(`${names.mihomoSource}: ${copies.length} build copies are byte-identical to the pinned source`);
  }
  const installer = `Super-Cage-${version}-windows-amd64-setup-unsigned.exe`;
  if (present.includes(installer)) {
    const size = lstatSync(join(directory, installer)).size;
    if (size < maxInstallerBytes) ok(`${installer}: ${(size / 1048576).toFixed(0)} MiB (limit 512 MiB)`);
    else fail(`${installer}: ${size} bytes; the updater limit is ${maxInstallerBytes}`);
  }

  const written = [];
  for (const [group, file] of Object.entries(sumsFiles)) {
    if (!primaries[group].some(name => hashes.has(name))) continue;
    writeFileSync(join(directory, file), renderSums(groups[group], hashes));
    written.push(file);
  }
  const shipped = new Set([...hashes.keys(), ...written]);
  const unknown = [];
  for (const path of documentsAtCommit(git, sha)) {
    for (const name of releaseReferences(atCommit(git, sha, path), version)) if (!shipped.has(name)) unknown.push(`${path}: ${name}`);
  }
  if (unknown.length) unknown.forEach(link => fail(`README/docs name an asset this release does not have: ${link}`));
  else ok('README.md and docs/ name only assets of this release');
  return { report, written, complete: !report.some(([kind]) => kind === 'MISSING'), failed: report.some(([kind]) => kind === 'FAIL') };
}

// prepare: the version files (scripts/release-version.mjs) and the release links.
export async function bump(root, version) {
  const { versionFiles } = await import(pathToFileURL(join(root, 'scripts/release-version.mjs')).href);
  const from = readFileSync(join(root, 'VERSION'), 'utf8').trim();
  const changed = [];
  for (const [path, text] of versionFiles(root, version)) {
    if (readFileSync(path, 'utf8') !== text) { writeFileSync(path, text); changed.push(path); }
  }
  const tracked = execFileSync('git', ['-C', root, 'ls-files', 'docs'], { encoding: 'utf8' }).split('\n').filter(path => path.endsWith('.md'));
  const leftover = [];
  for (const relative of ['README.md', ...tracked]) {
    const path = join(root, relative);
    const { text, count } = bumpReleaseReferences(readFileSync(path, 'utf8'), from, version);
    if (count) { writeFileSync(path, text); changed.push(path); }
    for (const { line, text: content } of versionMentions(text, from)) leftover.push(`${relative}:${line}: ${content}`);
  }
  return { from, changed, leftover };
}

function options(args) {
  const result = { _: [] };
  for (let index = 0; index < args.length; index++) {
    const arg = args[index];
    if (!arg.startsWith('--')) { result._.push(arg); continue; }
    const key = arg.slice(2);
    if (['android', 'signed', 'notarized'].includes(key)) { result[key] = true; continue; }
    const value = args[++index];
    if (value === undefined) throw new Error(`${arg} needs a value`);
    if (key === 'mihomo') (result.mihomo ??= []).push(value);
    else result[key] = value;
  }
  return result;
}

const usage = `Usage:
  release-tool.mjs assemble <assets> --version V --sha SHA --git <repo> [--android] [--mihomo <copy>]...
                   [--macos-metadata <base.json> [--signed] [--notarized]]
  release-tool.mjs digests <assets> <gh release view --json assets output>
  release-tool.mjs upload-list <assets>
  release-tool.mjs bump <checkout> <version>
  release-tool.mjs references <markdown file> <version>`;

async function main([command, ...rest]) {
  const args = options(rest);
  if (command === 'assemble') {
    const result = await assemble({
      directory: resolve(args._[0]), version: checkVersion(args.version), sha: args.sha, git: args.git,
      android: Boolean(args.android), mihomoCopies: args.mihomo ?? [], macosMetadata: args['macos-metadata'],
      signed: Boolean(args.signed), notarized: Boolean(args.notarized),
    });
    for (const [kind, text] of result.report) console.log(`${kind.padEnd(8)}${text}`);
    console.log(`wrote   ${result.written.join(', ') || 'no checksum file'}`);
    return result.failed ? 1 : result.complete ? 0 : 3;
  }
  if (command === 'digests') {
    const release = JSON.parse(readFileSync(args._[1], 'utf8'));
    const problems = compareDigests(await localDigests(resolve(args._[0])), release.assets ?? []);
    for (const problem of problems) console.log(`FAIL    ${problem}`);
    if (!problems.length) console.log(`ok      all ${release.assets.length} uploaded assets match the local checksum files`);
    return problems.length ? 1 : 0;
  }
  if (command === 'upload-list') {
    const directory = resolve(args._[0]);
    const local = await localDigests(directory);
    const unlisted = regularFiles(directory).filter(name => !local.has(name));
    if (!local.size) throw new Error(`No checksum files in ${directory}`);
    if (unlisted.length) throw new Error(`Files no checksum file lists: ${unlisted.join(', ')}`);
    for (const name of local.keys()) console.log(join(directory, name));
    return 0;
  }
  if (command === 'bump') {
    const result = await bump(resolve(args._[0]), checkVersion(args._[1]));
    for (const path of result.changed) console.log(`changed ${path}`);
    for (const line of result.leftover) console.log(`check   ${line}`);
    return 0;
  }
  if (command === 'references') {
    for (const name of releaseReferences(readFileSync(args._[0], 'utf8'), args._[1])) console.log(name);
    return 0;
  }
  console.error(usage);
  return 2;
}

if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  main(process.argv.slice(2)).then(code => { process.exitCode = code; }, error => { console.error(error.message); process.exitCode = 1; });
}
