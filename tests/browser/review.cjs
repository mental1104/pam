/**
 * 用途：通过独立 Chromium 和本机测试 TLS 代理验收待核表单；仅访问 pam-verify-api，不访问生产。
 * 输入：现有 /runtime Playwright；固定隔离测试 token。输出：PAM_BROWSER_OUTPUT_DIR 下摘要和合成页面截图。
 * 副作用：隔离库创建合成交易和待核，并通过页面明确确认；本机代理和临时证书在进程结束时释放。
 */
const fs = require('node:fs/promises');
const http = require('node:http');
const https = require('node:https');
const {execFileSync} = require('node:child_process');
const assert = require('node:assert/strict');
const {chromium} = require(require.resolve('playwright', {paths: [process.cwd(), '/runtime', '/runtime/browser']}));
const token = process.env.PAM_BROWSER_TEST_TOKEN || 'pam-isolated-acceptance-token-20261125';
const backend = process.env.PAM_BROWSER_BACKEND_URL || 'http://127.0.0.1:8080';

/** 向隔离服务提交合成 JSON。@param {string} path API 路径。@param {object} body 明确夹具。@returns {Promise<object>} 成功响应，否则抛出不含凭据的错误。 */
async function submit(path, body) {
  const response = await fetch(backend + path, {method: 'POST', headers: {Authorization: `Bearer ${token}`, 'Content-Type': 'application/json'}, body: JSON.stringify(body)});
  assert.equal(response.status, 200); return response.json();
}

/** 执行真实浏览器编辑及待核恢复，finally 关闭浏览器和本地代理。@returns {Promise<void>} 失败以非零退出码报告，不修改生产服务。 */
async function main() {
  const output = process.env.PAM_BROWSER_OUTPUT_DIR || '/tmp/pam-browser-review'; await fs.mkdir(output, {recursive: true, mode: 0o700});
  const temp = await fs.mkdtemp('/tmp/pam-p0-browser-');
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=localhost', '-keyout', temp + '/key.pem', '-out', temp + '/cert.pem'], {stdio: 'ignore'});
  // 测试代理只指向隔离服务；保留原 Host 使真实同源/CSRF 检查照常执行。
  const proxy = https.createServer({key: await fs.readFile(temp + '/key.pem'), cert: await fs.readFile(temp + '/cert.pem')}, (incoming, outgoing) => {
    const upstream = http.request(backend + incoming.url, {method: incoming.method, headers: incoming.headers}, response => { outgoing.writeHead(response.statusCode, response.headers); response.pipe(outgoing); });
    upstream.on('error', () => { outgoing.writeHead(502); outgoing.end(); }); incoming.pipe(upstream);
  });
  await new Promise(resolve => proxy.listen(0, '127.0.0.1', resolve));
  const origin = `https://127.0.0.1:${proxy.address().port}`;
  const browser = await chromium.launch({headless: true});
  try {
    const marker = `p0-browser-${Date.now()}`;
    const row = {row_key: 'one', occurred_at: '2026-10-22T10:01:00+08:00', direction: 'expense', amount: '7.00', currency: 'CNY', merchant: marker, description: 'Synthetic review fixture', original: {pam_human_reviewed: true}};
    await submit('/api/v1/imports/transactions', {source: marker, idempotency_key: 'initial', records: [row]});
    const proposal = await submit('/api/v1/imports/proposals', {source: marker + '-second', idempotency_key: 'overlap', records: [row]});
    const imported = await submit(`/api/v1/imports/proposals/${proposal.id}/confirm`, {}); assert.equal(imported.duplicated, 1);
    const context = await browser.newContext({ignoreHTTPSErrors: true}); const page = await context.newPage();
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.goto(origin); await page.locator('#token-details').evaluate(node => { node.open = true; });
    await page.locator('#access-token').fill(token); await page.locator('#token-form button').click(); await page.locator('#app').waitFor({state: 'visible'});
    await page.locator('#import-review-panel > summary').click();
    await page.waitForFunction(() => /待核 [0-9]+ 项/.test(document.getElementById('review-notice').textContent));
    const item = page.locator('#review-items details').filter({hasText: marker});
    // 隔离库也有回归测试产生的旧待核；通过真实分页寻找本轮项，不能假定目标永远在首页。
    for (let offset = 20; await item.count() === 0 && await page.locator('#review-next').isEnabled(); offset += 20) {
      await page.locator('#review-next').click();
      await page.waitForFunction(start => document.getElementById('review-notice').textContent.includes(`当前 ${start}–`), offset + 1);
    }
    await item.waitFor(); await item.locator('summary').click();
    await item.locator('select').selectOption('create'); await item.locator('input[name="reason"]').fill('Distinct payment independently checked against synthetic evidence');
    await item.locator('input[type="checkbox"]').check();
    const response = page.waitForResponse(value => value.url().endsWith('/resolve') && value.request().method() === 'POST');
    await item.locator('button[type="submit"]').click(); assert.equal((await response).status(), 200); await item.waitFor({state: 'detached'});
    await page.reload(); await page.locator('#app').waitFor({state: 'visible'}); await page.locator('#import-review-panel > summary').click();
    await page.locator('#source-ranges').getByText(new RegExp(marker)).first().waitFor();
    assert.equal(await page.locator('#review-items details').filter({hasText: marker}).count(), 0);
    assert.deepEqual(errors, []);
    await page.screenshot({path: output + '/review.png', fullPage: true});
    await page.locator('#logout').click(); await page.locator('#auth-view').waitFor({state: 'visible'}); assert.equal(await page.locator('#review-items').textContent(), '');
    await fs.writeFile(output + '/summary.json', JSON.stringify({passed: true, synthetic: true, browserErrors: errors, checks: ['session login', 'pending recovery', 'explicit separate transaction', 'source ranges', 'reload persistence', 'logout clears private data']}, null, 2), {mode: 0o600});
  } finally { await browser.close(); await new Promise(resolve => proxy.close(resolve)); }
}
main().catch(error => { process.stderr.write(error.message + '\n'); process.exitCode = 1; });
