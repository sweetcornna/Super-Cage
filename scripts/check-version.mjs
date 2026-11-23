import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { iosProjectPath, marketingVersions } from './release-version.mjs';
const root=fileURLToPath(new URL('../',import.meta.url));
const version=fs.readFileSync(path.join(root,'VERSION'),'utf8').trim();
assert.match(version,/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/);
for(const directory of ['ui']){
 const pkg=JSON.parse(fs.readFileSync(path.join(root,directory,'package.json'),'utf8'));
 const lock=JSON.parse(fs.readFileSync(path.join(root,directory,'package-lock.json'),'utf8'));
 for(const actual of [pkg.version,lock.version,lock.packages[''].version]) assert.equal(actual,version,`${directory}: version differs from VERSION`);
}
// The iPhone app carries its version in the Xcode project; a stale one ships to TestFlight as the wrong build.
const iosVersions = marketingVersions(fs.readFileSync(path.join(root,iosProjectPath),'utf8'));
assert.ok(iosVersions.length>0,`${iosProjectPath}: no MARKETING_VERSION found`);
for(const actual of iosVersions) assert.equal(actual,version,`${iosProjectPath}: MARKETING_VERSION differs from VERSION`);
console.log(`Application package and iOS project versions match VERSION: ${version}`);
