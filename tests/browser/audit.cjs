/**
 * 审计中心黑盒验收：独立 Chromium 经本地 TLS 代理访问隔离 pam-verify-api。
 * 仅使用 tests/fixtures/audit 的合成审计；不读取生产账本或凭据。
 * 输出 PAM_BROWSER_OUTPUT_DIR 的桌面/手机截图与摘要，不保存 Cookie、HAR 或登录操作日志。
 */
const fs=require('node:fs/promises');
const http=require('node:http');
const https=require('node:https');
const {execFileSync}=require('node:child_process');
const assert=require('node:assert/strict');
const {chromium}=require(require.resolve('playwright',{paths:[process.cwd(),'/runtime','/runtime/browser']}));
let phase='setup';
const backend=process.env.PAM_BROWSER_BACKEND_URL || 'http://127.0.0.1:8080';

/** triggered 在 action 触发的审计读取渲染完成后返回响应；page 为独立浏览器，check 为可选请求参数条件。 */
async function triggered(page,action,check=()=>true){
 const response=page.waitForResponse(value=>new URL(value.url()).pathname==='/api/v1/audit'&&check(new URL(value.url())));
 await action();const result=await response;await page.waitForFunction(()=>document.getElementById('audit-view').getAttribute('aria-busy')==='false');return result;
}

/** main 使用真实 UI 验证联动、原文、历史空值、XSS、分页、失败重试和响应式；finally 关闭代理与浏览器。 */
async function main(){
 const output=process.env.PAM_BROWSER_OUTPUT_DIR || '/tmp/pam-browser-audit';await fs.mkdir(output,{recursive:true,mode:0o700});const temp=await fs.mkdtemp('/tmp/pam-audit-tls-');
 execFileSync('openssl',['req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj','/CN=localhost','-keyout',temp+'/key.pem','-out',temp+'/cert.pem'],{stdio:'ignore'});
 const proxy=https.createServer({key:await fs.readFile(temp+'/key.pem'),cert:await fs.readFile(temp+'/cert.pem')},(incoming,outgoing)=>{
  const upstream=http.request(backend+incoming.url,{method:incoming.method,headers:incoming.headers},response=>{outgoing.writeHead(response.statusCode,response.headers);response.pipe(outgoing);});upstream.on('error',()=>{outgoing.writeHead(502);outgoing.end();});incoming.pipe(upstream);
 });
 await new Promise(resolve=>proxy.listen(0,'127.0.0.1',resolve));const origin=`https://127.0.0.1:${proxy.address().port}`;const browser=await chromium.launch({headless:true});
 try{
  const context=await browser.newContext({ignoreHTTPSErrors:true,viewport:{width:1440,height:1050}});const page=await context.newPage();const errors=[];page.on('pageerror',error=>errors.push(error.message));
  phase='login';await page.goto(origin);await page.locator('#token-details').evaluate(node=>{node.open=true;});await page.locator('#access-token').fill(process.env.PAM_BROWSER_TEST_TOKEN || 'pam-isolated-acceptance-token-20261125');await page.locator('#token-form button').click();await page.locator('#app').waitFor({state:'visible'});
  phase='dashboard';await triggered(page,()=>page.locator('[data-view="audit"]').click());await page.locator('#audit-search').fill('audit-visual-fixture');
  const fixture=await triggered(page,()=>page.locator('#audit-filters button[type="submit"]').click());const data=await fixture.json();assert.equal(data.summary.total,52);assert.equal(await page.locator('.audit-record').count(),20);
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);assert.equal(await page.locator('.audit-time-bar').count(),data.timeline.length);
  await page.screenshot({path:output+'/desktop.png',fullPage:true});await page.screenshot({path:output+'/desktop-viewport.png'});
  phase='legacy-record';await page.locator('.audit-record').nth(2).click();assert.match(await page.locator('#audit-detail-body').textContent(),/历史 SQL 模式/);await page.locator('#audit-detail-close').click();
  phase='status-drill';const drill=await triggered(page,()=>page.locator('[data-audit-status="error"]').click());assert.equal((await drill.json()).summary.total,6);await page.locator('.audit-record').first().click();assert.match(await page.locator('#audit-detail-body').textContent(),/查询超时或被取消/);await page.locator('#audit-detail-close').click();
  phase='pattern-drill';const pattern=await triggered(page,()=>page.locator('.audit-pattern').first().click());assert.ok((await pattern.json()).summary.total<6);assert.match(await page.locator('#audit-chips').textContent(),/模式/);
  phase='reason-and-time-drill';await triggered(page,()=>page.locator('#audit-reset').click());await page.locator('#audit-search').fill('audit-visual-fixture');await triggered(page,()=>page.locator('#audit-filters button[type="submit"]').click());const reason=await triggered(page,()=>page.locator('.audit-error-item').first().click());assert.ok((await reason.json()).summary.total>0);assert.match(await page.locator('#audit-chips').textContent(),/原因/);const time=await triggered(page,()=>page.locator('.audit-time-bar').last().click(),url=>url.searchParams.has('to'));const selectedTime=await time.json();const requestedTime=new URL(time.url());assert.equal(Date.parse(selectedTime.from),Date.parse(requestedTime.searchParams.get('from')));assert.equal(Date.parse(selectedTime.to),Date.parse(requestedTime.searchParams.get('to')));
  phase='remove-search';await triggered(page,()=>page.locator('#audit-chips button').filter({hasText:'SQL：'}).click());assert.equal(await page.locator('#audit-search').inputValue(),'');
  phase='pagination';await triggered(page,()=>page.locator('#audit-reset').click());await page.locator('#audit-search').fill('audit-visual-fixture');await triggered(page,()=>page.locator('#audit-filters button[type="submit"]').click());const next=await triggered(page,()=>page.locator('#audit-next').click());assert.equal((await next.json()).records.offset,20);
  phase='empty';await page.locator('#audit-search').fill('no-such-audit-fixture');const empty=await triggered(page,()=>page.locator('#audit-filters button[type="submit"]').click());assert.equal((await empty.json()).summary.total,0);assert.match(await page.locator('#audit-records').textContent(),/没有符合条件/);
  phase='original-and-parameters';await page.locator('#audit-search').fill('<img');await triggered(page,()=>page.locator('#audit-filters button[type="submit"]').click());await page.locator('.audit-record').first().click();assert.match(await page.locator('.audit-parameters').textContent(),/9007199254740993.0001/);assert.equal(await page.locator('#audit-detail img').count(),0);assert.equal(await page.evaluate(()=>window.PAM_AUDIT_XSS),undefined);await page.screenshot({path:output+'/detail.png'});await page.locator('#audit-detail-close').click();
  phase='retry';await page.route('**/api/v1/audit?**',route=>route.fulfill({status:503,contentType:'application/json',body:JSON.stringify({error:{message:'Synthetic audit outage'}})}));await triggered(page,()=>page.locator('#audit-refresh').click());assert.equal(await page.locator('#audit-content').isVisible(),false);await page.unroute('**/api/v1/audit?**');await triggered(page,()=>page.locator('#audit-message button').click());assert.equal(await page.locator('#audit-content').isVisible(),true);
  phase='mobile';await page.locator('#audit-search').fill('audit-visual-fixture');await triggered(page,()=>page.locator('#audit-filters button[type="submit"]').click());await page.setViewportSize({width:390,height:844});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);await page.screenshot({path:output+'/mobile.png',fullPage:true});
  phase='logout';await page.locator('#logout').click();await page.locator('#auth-view').waitFor({state:'visible'});assert.equal(await page.locator('#audit-records').textContent(),'');assert.equal(await page.locator('#audit-detail-body').textContent(),'');assert.deepEqual(errors,[]);
  await fs.writeFile(output+'/summary.json',JSON.stringify({passed:true,synthetic:true,calls:52,desktop:[1440,1050],mobile:[390,844],checks:['timeline counts','status drill-down','pattern drill-down','error-code drill-down','time drill-down','remove-search control sync','pagination','legacy record','exact raw parameters','XSS safety','empty state','retry','responsive overflow','logout clears private data'],browserErrors:errors},null,2),{mode:0o600});
 }catch(error){await fs.writeFile(output+'/failure.json',JSON.stringify({phase,message:phase==='login'?'Login failed':error.message},null,2),{mode:0o600});throw new Error('Audit browser test failed at '+phase);}finally{await browser.close();await new Promise(resolve=>proxy.close(resolve));}
}
main().catch(error=>{process.stderr.write(error.message+'\n');process.exitCode=1;});
