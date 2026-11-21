// UI actions reach a real isolated production browser controller. The VM status,
// remote display and account are the existing workbench fixture. The first
// part runs a controller whose Chromium has no browser data extension (a
// branded Chrome ignores --load-extension), so it covers the earlier cookie
// manager that older guests keep; the second part covers the 隐私与网站数据
// section that browser data management brings (browser-data-fixture.mjs).
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { mkdir, writeFile } from 'node:fs/promises';
import { createWorkbench } from './workbench-fixture.mjs';
import { createPrivacyController } from '../../scripts/browser-privacy-fixture.mjs';
import { createDataController, startSites } from './browser-data-fixture.mjs';

const output = new URL('../../docs/test-reports/2026-11-07-browser-privacy/', import.meta.url);
const engine = process.env.CAGE_TEST_BROWSER || 'chromium';
await mkdir(output, { recursive: true });
const controller = await createPrivacyController();
let workbench;
let failCookies = false;
const checks = [];
try {
  const normal = (await controller.api('/tabs')).active;
  await controller.cdp(normal, 'Network.setCookies', { cookies: [
    { name: 'fixture-session', value: 'fixture-only-value', domain: 'localhost', path: '/', httpOnly: true },
    { name: 'unrelated', value: 'fixture-other', domain: 'other.test', path: '/' },
  ] });
  workbench = await createWorkbench({ browserRequest: async (route, path, body) => {
    if (path === '/browser/resize') return route.fulfill({ json: { ok: true } });
    if (path === '/browser/cookies' && failCookies) return route.fulfill({ status: 409, contentType: 'text/plain; charset=utf-8', body: '测试：Cookie 服务暂时不可用' });
    try { return await route.fulfill({ json: await controller.api(path.replace('/browser', ''), body ?? undefined) }); }
    catch (error) { return route.fulfill({ status: 400, body: error.message }); }
  } });
  const { page, errors } = workbench;
  page.setDefaultTimeout(10000);
  workbench.state.settings.browser_home_url = controller.origin + '/ui-home';
  await page.getByRole('button', { name: '启动 Super Cage 环境', exact: true }).click();
  await page.getByRole('heading', { name: 'Super Cage 环境已就绪', exact: true }).waitFor();
  await page.getByRole('button', { name: '打开浏览器', exact: true }).click();
  await page.getByRole('tab', { name: 'Privacy fixture', exact: true }).waitFor();
  const menu = page.getByRole('button', { name: '浏览器菜单', exact: true });
  await menu.focus(); await page.keyboard.press('ArrowDown');
  await page.getByRole('menuitem', { name: '新建无痕标签', exact: true }).press('Enter');
  await page.getByRole('tab', { name: /无痕 ·/ }).waitFor();
  await page.locator('.browser-private-badge').waitFor();
  assert.equal((await controller.api('/tabs')).tabs.filter(tab => tab.incognito).length, 1);
  await page.getByRole('button', { name: '新建网页', exact: true }).click();
  await controller.waitFor(async () => (await controller.api('/tabs')).tabs.filter(tab => tab.incognito).length === 2, 'plus inherits private session');
  await page.screenshot({ path: fileURLToPath(new URL(`private-tabs-${engine}.png`, output)) });
  checks.push('keyboard menu creates a real private session; tab title, address badge and notice identify privacy; plus inherits private mode');

  // Private label edits must remain in memory even after the tab closes.
  const privateTab = page.getByRole('tab', { name: /无痕 ·/ }).last();
  await privateTab.click({ button: 'right' });
  await page.getByRole('menuitem', { name: '重命名标签', exact: true }).click();
  await page.getByRole('dialog', { name: '重命名标签' }).locator('input').fill('PRIVATE_TITLE_MUST_NOT_PERSIST');
  await page.getByRole('button', { name: '保存名称', exact: true }).click();
  assert.equal(await page.evaluate(() => JSON.stringify(localStorage).includes('PRIVATE_TITLE_MUST_NOT_PERSIST')), false);
  await page.getByRole('button', { name: '关闭 无痕 · PRIVATE_TITLE_MUST_NOT_PERSIST', exact: true }).click();
  await page.getByRole('tab', { name: 'Privacy fixture', exact: true }).click();
  await page.getByRole('tab', { name: 'Privacy fixture', exact: true }).click({ button: 'right' });
  await page.getByRole('menuitem', { name: '置顶标签', exact: true }).click();
  assert.equal(await page.evaluate(() => JSON.stringify(localStorage).includes('PRIVATE_TITLE_MUST_NOT_PERSIST')), false);
  checks.push('private rename never persists, including after closing the tab and saving normal tab metadata');

  await menu.click();
  await page.getByRole('menuitem', { name: '管理 Cookie', exact: true }).click();
  const panel = page.getByRole('region', { name: 'Cookie 管理' });
  await panel.getByText('fixture-session', { exact: true }).waitFor();
  assert.equal(await panel.getByText('unrelated', { exact: true }).count(), 0);
  await panel.getByRole('button', { name: '查看值', exact: true }).click();
  await panel.getByText('fixture-only-value', { exact: true }).waitFor();
  await panel.getByRole('button', { name: '隐藏值', exact: true }).click();
  const range = panel.getByRole('combobox', { name: 'Cookie 网站范围' });
  await range.click();
  await page.getByRole('option', { name: '所有网站', exact: true }).waitFor();
  await page.screenshot({ path: fileURLToPath(new URL(`cookie-range-inline-${engine}.png`, output)) });
  await range.press('ArrowDown'); await range.press('Enter');
  await panel.getByText('unrelated', { exact: true }).waitFor();
  await panel.getByRole('textbox', { name: '搜索 Cookie' }).fill('fixture-session');
  await panel.getByText('共 1 条', { exact: false }).waitFor();
  await panel.getByRole('button', { name: '删除 Cookie fixture-session', exact: true }).click();
  await panel.getByText('没有匹配的 Cookie。', { exact: true }).waitFor();
  assert.equal((await controller.api('/cookies', { id: normal, mode: 'list', scope: 'all' })).total, 1);
  checks.push('real cookies viewed, values revealed and hidden, range selects with keyboard, search and exact deletion preserve other cookies');

  await panel.getByRole('textbox', { name: '搜索 Cookie' }).fill('');
  await panel.getByText('unrelated', { exact: true }).waitFor();
  await panel.getByRole('button', { name: '清空所有网站', exact: true }).click();
  await panel.getByRole('button', { name: '取消', exact: true }).click();
  assert.equal((await controller.api('/cookies', { id: normal, mode: 'list', scope: 'all' })).total, 1);
  failCookies = true;
  await panel.getByRole('button', { name: '刷新 Cookie', exact: true }).click();
  await panel.getByRole('alert').getByText('测试：Cookie 服务暂时不可用', { exact: false }).waitFor();
  failCookies = false;
  await panel.getByRole('button', { name: '刷新 Cookie', exact: true }).click();
  await panel.getByText('unrelated', { exact: true }).waitFor();
  await page.setViewportSize({ width: 820, height: 558 });
  await page.screenshot({ path: fileURLToPath(new URL(`cookies-minimum-window-${engine}.png`, output)) });
  assert.ok((await panel.boundingBox()).x >= 0);
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1), false);
  await panel.getByRole('button', { name: '清空所有网站', exact: true }).click();
  await panel.getByRole('button', { name: '确认清空', exact: true }).click();
  await panel.getByText('没有匹配的 Cookie。', { exact: true }).waitFor();
  await panel.getByRole('textbox', { name: '搜索 Cookie' }).press('Escape');
  assert.equal(await panel.count(), 0);
  checks.push('clear cancel and confirm, error and refresh recovery, 820×558 layout, Escape closes and restores focus');
  // The settings page's section on a guest without browser data: the earlier
  // cookie manager and Chromium's own pages.
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.evaluate(() => document.dispatchEvent(new CustomEvent('cage-browser-data', { detail: { view: 'privacy' } })));
  const legacySection = page.getByRole('region', { name: '隐私与网站数据' });
  await legacySection.getByRole('button', { name: '清除普通会话浏览数据' }).waitFor();
  await legacySection.getByRole('region', { name: 'Cookie 管理' }).waitFor();
  assert.equal(await legacySection.getByRole('button', { name: /^网站数据/ }).count(), 0);
  checks.push('the settings section on a guest without browser data keeps the cookie manager and Chromium links');
  assert.deepEqual(errors, []);
} catch (error) {
  if (workbench) {
    await workbench.page.screenshot({ path: fileURLToPath(new URL('failure.png', output)) });
    console.error({ tabs: await workbench.page.getByRole('tab').allTextContents(), errors: workbench.errors });
  }
  throw error;
} finally { await workbench?.browser.close(); await controller.close(); }

// The section with browser data management: it loads with the settings page
// (browser-settings.jsx), lists what it manages, offers no loopback page as a
// site to keep, and shows settings it cannot read with 重试 instead of empty
// lists (review 3, findings 2 to 4).
const data = await createDataController();
const sites = await startSites();
let second, unreadable = true;
const settings = { defaults: { range: 'day', types: ['history', 'site', 'cache'] }, keep: [], auto: { enabled: true, trigger: 'stop', days: 7, types: ['history', 'site'], attempted: new Date().toISOString(), result: 'failed' } };
try {
  // A loopback page like the file editor's, and an ordinary site.
  for (const url of [`http://127.0.0.1:${sites.port}/?folder=/w`, `http://s1.localhost:${sites.port}/`]) await data.api('/new', { url });
  await data.waitFor(async () => (await data.api('/tabs')).tabs.filter(tab => tab.url.includes(':' + sites.port)).length === 2, 'two pages');
  second = await createWorkbench({ browserRequest: async (route, path, body) => {
    if (path === '/browser/resize') return route.fulfill({ json: { ok: true } });
    if (path === '/browser/data-settings') {
      if (unreadable) return route.fulfill({ status: 503, contentType: 'text/plain; charset=utf-8', body: '浏览数据设置暂时读不出来，请稍后重试；保持登录的网站列表没有改动' });
      return route.fulfill({ json: settings });
    }
    try { return await route.fulfill({ json: await data.api(path.replace('/browser', ''), body ?? undefined) }); }
    catch (error) { return route.fulfill({ status: 409, contentType: 'text/plain; charset=utf-8', body: error.message }); }
  } });
  const { page, errors } = second;
  page.setDefaultTimeout(15000);
  second.state.status.ready = second.state.status.vm_running = true;
  await page.reload();
  await page.getByRole('button', { name: '打开浏览器', exact: true }).click();
  await page.evaluate(() => document.dispatchEvent(new CustomEvent('cage-browser-data', { detail: { view: 'privacy' } })));
  const section = page.getByRole('region', { name: '隐私与网站数据' });
  for (const entry of [/^清除浏览数据/, /^网站数据/, /^浏览记录/, /^网站权限/]) await section.getByRole('button', { name: entry }).waitFor();
  await section.getByText('设置暂时读不出来', { exact: false }).waitFor();
  assert.equal(await section.getByRole('textbox', { name: '要保留的网站' }).count(), 0, 'no empty kept list while the settings cannot be read');
  await page.screenshot({ path: fileURLToPath(new URL(`privacy-section-unreadable-${engine}.png`, output)) });
  unreadable = false;
  await section.getByRole('button', { name: '重试', exact: true }).click();
  await section.getByRole('textbox', { name: '要保留的网站' }).waitFor();
  await section.locator('.bd-suggest button', { hasText: 's1.localhost' }).waitFor();
  assert.equal(await section.locator('.bd-suggest button', { hasText: '127.0.0.1' }).count(), 0, 'the editor is not offered as a site to keep');
  await section.getByText('自动清理还没有成功过', { exact: false }).waitFor();
  await page.screenshot({ path: fileURLToPath(new URL(`privacy-section-${engine}.png`, output)) });
  checks.push('the browser data section: entries, settings it cannot read shown with 重试 and no empty list, no loopback suggestion, an automatic cleaning failure before any success');
  assert.deepEqual(errors, []);
  await writeFile(new URL(`ui-result-${engine}.json`, output), JSON.stringify({ passed: true, engine, checks }, null, 2) + '\n');
  console.log(JSON.stringify({ passed: true, checks }, null, 2));
} catch (error) {
  if (second) await second.page.screenshot({ path: fileURLToPath(new URL('failure-section.png', output)) });
  throw error;
} finally { await second?.browser.close(); await data.close(); sites.server.close(); }
