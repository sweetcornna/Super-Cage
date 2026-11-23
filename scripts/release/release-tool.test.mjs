import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import {
  assemble, assetGroups, bumpReleaseReferences, checkBuildMetadata, checkVersion, compareDigests, dependencyNames,
  distributionPlatforms, localDigests, parseSums, releaseMetadata, releaseReferences, renderSums, versionMentions,
} from './release-tool.mjs';

const root = fileURLToPath(new URL('../../', import.meta.url));
const fixture = fileURLToPath(new URL('./testdata/v0.1.17/', import.meta.url));
const read = path => readFileSync(join(fixture, path), 'utf8');
const sha256 = text => createHash('sha256').update(text).digest('hex');
const published = JSON.parse(read('release.json'));
const digest = name => published.assets.find(asset => asset.name === name).digest;
// The 0.1.17 names: Mihomo v1.19.33 and QEMU 11.1.0 as pinned at that release.
const names = dependencyNames({
  linux: { mihomoSource: { file: 'mihomo-v1.19.33-source.tar.gz', sha256: 'e7c596b37ff27ec487d19b1920f73e0c97a78067ea35766530ee9659ade72711' } },
  windows: {
    mihomoSource: { file: 'mihomo-v1.19.33-source.tar.gz', hash: 'E7C596B37FF27EC487D19B1920F73E0C97A78067EA35766530EE9659ADE72711' },
    qemu: { version: '11.1.0' },
    qemuSource: { file: 'qemu-e470268ff446ec56728215a987ed166d5ed8fb64-source.tar' },
    qemuKeycodemap: { version: 'f5772a62ec52591ff6870b7e8ef32482371f22c6' },
  },
  proxyCore: 'set -e\nversion=v1.19.33\n',
});

test('the checksum files render byte for byte as published for 0.1.17', () => {
  assert.deepEqual(names.problems, []);
  const hashes = new Map();
  for (const file of ['SHA256SUMS', 'SHA256SUMS-linux-desktop.txt', 'SHA256SUMS-windows-desktop.txt']) {
    for (const [name, hash] of parseSums(read(file))) hashes.set(name, hash);
  }
  const groups = assetGroups('0.1.17', names);
  for (const [group, file] of [['main', 'SHA256SUMS'], ['linux', 'SHA256SUMS-linux-desktop.txt'], ['windows', 'SHA256SUMS-windows-desktop.txt']]) {
    const text = renderSums(groups[group], hashes);
    assert.equal(text, read(file));
    assert.equal(`sha256:${sha256(text)}`, digest(file));
  }
  // Every published asset is in exactly the groups the release files list.
  const listed = new Set([...hashes.keys(), 'SHA256SUMS', 'SHA256SUMS-linux-desktop.txt', 'SHA256SUMS-windows-desktop.txt']);
  assert.deepEqual([...listed].sort(), published.assets.map(asset => asset.name).sort());
});

test('a missing asset leaves its line out instead of leaving a gap', () => {
  const hashes = new Map([['release-metadata.json', 'a'.repeat(64)], ['Super-Cage-0.1.18-macos-arm64.dmg', 'b'.repeat(64)]]);
  assert.equal(renderSums(assetGroups('0.1.18', names).main, hashes),
    `${'b'.repeat(64)}  Super-Cage-0.1.18-macos-arm64.dmg\n${'a'.repeat(64)}  release-metadata.json\n`);
});

test('release-metadata.json renders as published for 0.1.17', () => {
  const present = assetGroups('0.1.17', names).main.filter(name => !name.endsWith('.apk') && name !== 'release-metadata.json');
  const platforms = distributionPlatforms(present, '0.1.17');
  assert.deepEqual(platforms, ['macos-arm64', 'linux-amd64', 'linux-arm64']);
  const base = { schema: 1, platform: 'macos', version: '0.1.17', commitSha: published.target_commitish };
  const text = releaseMetadata(base, { platforms, signed: true, notarized: true });
  assert.equal(text, read('release-metadata.json'));
  assert.equal(`sha256:${sha256(text)}`, digest('release-metadata.json'));
  assert.deepEqual(distributionPlatforms(['Super-Cage-0.1.18-android-arm64.apk'], '0.1.18'), ['android-arm64']);
});

test('build metadata must name the release commit and version', () => {
  const sha = published.target_commitish;
  const good = `{"schema":1,"platform":"linux","version":"0.1.17","commitSha":"${sha}"}`;
  assert.deepEqual(checkBuildMetadata(good, { platform: 'linux', version: '0.1.17', sha }), []);
  assert.equal(checkBuildMetadata(good, { platform: 'windows', version: '0.1.18', sha: 'f'.repeat(40) }).length, 3);
  assert.deepEqual(checkBuildMetadata('{', { platform: 'linux', version: '0.1.17', sha }), ['not JSON']);
});

test('pinned sources that disagree are reported', () => {
  const bad = dependencyNames({
    linux: { mihomoSource: { file: 'mihomo-v1.19.34-source.tar.gz', sha256: 'a'.repeat(64) } },
    windows: { mihomoSource: { file: 'mihomo-v1.19.34-source.tar.gz', hash: 'b'.repeat(64) }, qemu: { version: '11.1.0' }, qemuSource: { file: 'q.tar' }, qemuKeycodemap: { version: 'k' } },
    proxyCore: 'version=v1.19.33\n',
  });
  assert.equal(bad.problems.length, 2);
  const current = dependencyNames({
    linux: JSON.parse(readFileSync(join(root, 'packaging/linux/dependencies.json'), 'utf8')),
    windows: JSON.parse(readFileSync(join(root, 'packaging/windows/dependencies.json'), 'utf8')),
    proxyCore: readFileSync(join(root, 'scripts/fetch-proxy-core.sh'), 'utf8'),
  });
  assert.deepEqual(current.problems, []);
});

test('GitHub digests are compared with the local files both ways', async () => {
  const directory = mkdtempSync(join(tmpdir(), 'release-tool-'));
  try {
    const local = new Map();
    for (const file of ['SHA256SUMS', 'SHA256SUMS-linux-desktop.txt', 'SHA256SUMS-windows-desktop.txt']) {
      for (const [name, hash] of parseSums(read(file))) local.set(name, { sha256: hash, size: published.assets.find(asset => asset.name === name).size });
      local.set(file, { sha256: sha256(read(file)), size: Buffer.byteLength(read(file)) });
    }
    assert.deepEqual(compareDigests(local, published.assets), []);
    const changed = published.assets.map(asset => (asset.name === 'SHA256SUMS' ? { ...asset, digest: `sha256:${'0'.repeat(64)}` } : asset));
    assert.match(compareDigests(local, changed).join('\n'), /^digest differs: SHA256SUMS /);
    const partial = published.assets.filter(asset => !asset.name.endsWith('.apk'));
    assert.deepEqual(compareDigests(local, partial), ['not uploaded: Super-Cage-0.1.17-android-arm64.apk']);
    const extra = [...published.assets, { name: 'notes.md', size: 1, digest: `sha256:${'1'.repeat(64)}`, state: 'uploaded' }];
    assert.deepEqual(compareDigests(local, extra), ['on GitHub but not in the local assets: notes.md']);
    const unfinished = published.assets.map(asset => (asset.name.endsWith('.deb') ? { ...asset, state: 'starter', digest: null } : asset));
    assert.equal(compareDigests(local, unfinished).length, 2);
    // localDigests reads the same map from an assets directory.
    for (const file of ['SHA256SUMS-linux-desktop.txt']) writeFileSync(join(directory, file), read(file));
    writeFileSync(join(directory, 'Super-Cage-0.1.17-linux-amd64.deb'), 'x');
    writeFileSync(join(directory, 'linux-desktop-build-metadata.json'), 'y');
    const fromDisk = await localDigests(directory);
    assert.equal(fromDisk.get('SHA256SUMS-linux-desktop.txt').sha256, digest('SHA256SUMS-linux-desktop.txt').slice(7));
    assert.equal(fromDisk.get('Super-Cage-0.1.17-linux-amd64.deb').size, 1);
  } finally { rmSync(directory, { recursive: true, force: true }); }
});

const readme = `从 [v0.1.17 发布页](https://github.com/o/r/releases/tag/v0.1.17) 下载对应平台安装包：
| **macOS** | [` + '`' + `Super-Cage-0.1.17-macos-arm64.dmg` + '`' + `](https://github.com/o/r/releases/download/v0.1.17/Super-Cage-0.1.17-macos-arm64.dmg) | Apple Silicon |
   - **Linux**：运行 ` + '`' + `sudo apt install ./Super-Cage-0.1.17-linux-amd64.deb` + '`' + `。
   base=https://github.com/o/r/releases/download/v0.1.17
   curl -fLO "$base/Super-Cage-NoVirt-0.1.17-linux-amd64.tar.gz" -fLO "$base/SHA256SUMS"
   cd Super-Cage-NoVirt-0.1.17-linux-amd64
   tar -xzf Super-Cage-Server-0.1.17-linux-amd64.tar.gz
Take a number above the newest release, for example ` + '`' + `make cage VERSION=0.1.18-dev.agents` + '`' + ` while 0.1.17 is current.
The v0.1.16 page and v0.1.170 are other releases; see releases/download/v0.1.16/Super-Cage-0.1.16-macos-arm64.dmg.
`;

test('release references include links, $base downloads and asset names', () => {
  assert.deepEqual(releaseReferences(readme, '0.1.17'), [
    'SHA256SUMS', 'Super-Cage-0.1.17-linux-amd64.deb', 'Super-Cage-0.1.17-macos-arm64.dmg',
    'Super-Cage-NoVirt-0.1.17-linux-amd64.tar.gz', 'Super-Cage-Server-0.1.17-linux-amd64.tar.gz',
  ]);
  assert.deepEqual(releaseReferences('See releases/download/v0.1.17/SHA256SUMS.', '0.1.17'), ['SHA256SUMS']);
  assert.deepEqual(releaseReferences(readme, '0.1.16'), ['Super-Cage-0.1.16-macos-arm64.dmg']);
});

test('the bump moves release links and names, and leaves other mentions for a person', () => {
  const { text, count } = bumpReleaseReferences(readme, '0.1.17', '0.1.18');
  assert.equal(count, 10);
  assert.deepEqual(releaseReferences(text, '0.1.17'), []);
  assert.deepEqual(releaseReferences(text, '0.1.18'), [
    'SHA256SUMS', 'Super-Cage-0.1.18-linux-amd64.deb', 'Super-Cage-0.1.18-macos-arm64.dmg',
    'Super-Cage-NoVirt-0.1.18-linux-amd64.tar.gz', 'Super-Cage-Server-0.1.18-linux-amd64.tar.gz',
  ]);
  assert.match(text, /从 \[v0\.1\.18 发布页\]\(https:\/\/github\.com\/o\/r\/releases\/tag\/v0\.1\.18\)/);
  assert.match(text, /cd Super-Cage-NoVirt-0\.1\.18-linux-amd64\n/);
  assert.match(text, /v0\.1\.16 page and v0\.1\.170 .*releases\/download\/v0\.1\.16\/Super-Cage-0\.1\.16-macos-arm64\.dmg/);
  assert.deepEqual(versionMentions(text, '0.1.17').map(mention => mention.line), [8]);
  assert.equal(bumpReleaseReferences(text, '0.1.17', '0.1.18').count, 0);
});

test('versions are canonical SemVer without a v', () => {
  assert.equal(checkVersion('0.1.18'), '0.1.18');
  assert.equal(checkVersion('0.2.0-rc.1'), '0.2.0-rc.1');
  for (const bad of ['v0.1.18', '0.1', '01.1.1', '0.1.18 ']) assert.throws(() => checkVersion(bad));
});

test('assemble writes the checksum files and reports what is missing or wrong', async () => {
  const work = mkdtempSync(join(tmpdir(), 'release-assemble-'));
  try {
    const git = join(work, 'repo');
    const mihomo = 'mihomo source';
    mkdirSync(join(git, 'packaging/linux'), { recursive: true });
    mkdirSync(join(git, 'packaging/windows'), { recursive: true });
    mkdirSync(join(git, 'scripts'), { recursive: true });
    mkdirSync(join(git, 'docs'), { recursive: true });
    writeFileSync(join(git, 'packaging/linux/dependencies.json'), JSON.stringify({ mihomoSource: { file: 'mihomo-v1.2.3-source.tar.gz', sha256: sha256(mihomo) } }));
    writeFileSync(join(git, 'packaging/windows/dependencies.json'), JSON.stringify({
      mihomoSource: { file: 'mihomo-v1.2.3-source.tar.gz', hash: sha256(mihomo) }, qemu: { version: '9.0.0' },
      qemuSource: { file: 'qemu-abc-source.tar' }, qemuKeycodemap: { version: 'def' },
    }));
    writeFileSync(join(git, 'scripts/fetch-proxy-core.sh'), 'version=v1.2.3\n');
    writeFileSync(join(git, 'README.md'), '[dmg](https://github.com/o/r/releases/download/v1.0.0/Super-Cage-1.0.0-macos-arm64.dmg)\n');
    writeFileSync(join(git, 'docs/linux.md'), 'apt install ./Super-Cage-1.0.0-linux-amd64.deb\n');
    const run = (...args) => execFileSync('git', ['-C', git, ...args], { encoding: 'utf8' }).trim();
    run('init', '-q');
    run('add', '.');
    run('-c', 'user.name=t', '-c', 'user.email=t@example.invalid', 'commit', '-q', '-m', 'fixture');
    const sha = run('rev-parse', 'HEAD');
    const assets = join(work, 'assets');
    mkdirSync(assets);
    const put = (name, text) => writeFileSync(join(assets, name), text);
    put('Super-Cage-1.0.0-macos-arm64.dmg', 'dmg');
    put('cage-macos-guest-1.0.0-source.tar.gz', 'guest');
    put('mihomo-v1.2.3-source.tar.gz', mihomo);
    for (const edition of ['Server', 'NoVirt']) for (const arch of ['amd64', 'arm64']) put(`Super-Cage-${edition}-1.0.0-linux-${arch}.tar.gz`, edition + arch);
    const base = join(work, 'base.json');
    writeFileSync(base, JSON.stringify({ schema: 1, platform: 'macos', version: '1.0.0', commitSha: sha }));
    writeFileSync(join(work, 'copy'), 'another mihomo');

    const result = await assemble({ directory: assets, version: '1.0.0', sha, git, android: false, mihomoCopies: [join(work, 'copy')], macosMetadata: base, signed: true, notarized: false });
    const lines = result.report.map(([kind, text]) => `${kind} ${text}`);
    assert.equal(result.complete, false);
    assert.equal(result.failed, true);
    assert.deepEqual(result.written, ['SHA256SUMS']);
    assert.ok(lines.includes('MISSING Super-Cage-1.0.0-linux-amd64.deb'));
    assert.ok(lines.includes('MISSING Super-Cage-1.0.0-windows-amd64-setup-unsigned.exe'));
    assert.ok(lines.includes('MISSING qemu-9.0.0-windows-full-source.tar.gz'));
    assert.ok(!lines.some(line => line.includes('android')));
    assert.ok(lines.some(line => line.startsWith('FAIL Mihomo source differs') && line.endsWith(join(work, 'copy'))));
    assert.ok(lines.includes('FAIL README/docs name an asset this release does not have: docs/linux.md: Super-Cage-1.0.0-linux-amd64.deb'));
    assert.ok(lines.includes(`ok release-metadata.json: macos 1.0.0 at ${sha.slice(0, 12)}`));
    const metadata = JSON.parse(readFileSync(join(assets, 'release-metadata.json'), 'utf8'));
    assert.deepEqual(metadata.distribution, { platforms: ['macos-arm64', 'linux-amd64', 'linux-arm64'], macosDeveloperIDSigned: true, macosNotarized: false });
    const sums = parseSums(readFileSync(join(assets, 'SHA256SUMS'), 'utf8'));
    assert.deepEqual([...sums.keys()], [
      'Super-Cage-1.0.0-macos-arm64.dmg', 'Super-Cage-NoVirt-1.0.0-linux-amd64.tar.gz', 'Super-Cage-NoVirt-1.0.0-linux-arm64.tar.gz',
      'Super-Cage-Server-1.0.0-linux-amd64.tar.gz', 'Super-Cage-Server-1.0.0-linux-arm64.tar.gz',
      'cage-macos-guest-1.0.0-source.tar.gz', 'mihomo-v1.2.3-source.tar.gz', 'release-metadata.json',
    ]);
    assert.equal(sums.get('mihomo-v1.2.3-source.tar.gz'), sha256(mihomo));
    assert.equal(existsSync(join(assets, 'SHA256SUMS-linux-desktop.txt')), false);
    // A second run refuses to overwrite: assembly starts from a clean directory.
    await assert.rejects(assemble({ directory: assets, version: '1.0.0', sha, git, android: false }), /already exists/);

    // A stray file is never uploaded silently.
    rmSync(join(assets, 'SHA256SUMS'));
    put('notes.md', 'x');
    const stray = await assemble({ directory: assets, version: '1.0.0', sha, git, android: true });
    assert.ok(stray.report.some(([kind, text]) => kind === 'FAIL' && text === 'unexpected file in the assets: notes.md'));
    assert.ok(stray.report.some(([kind, text]) => kind === 'MISSING' && text === 'Super-Cage-1.0.0-android-arm64.apk'));
  } finally { rmSync(work, { recursive: true, force: true }); }
});
