// Real Chrome rendering of the built bundle and host CSP with a simulated VS Code bridge.
// Uses a fresh browser context; no user browser profile or provider quota.
import { chromium } from 'playwright-core';
import { buildSync } from 'esbuild';
import { createRequire } from 'node:module';
import { createServer } from 'node:http';
import { readFileSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import assert from 'node:assert/strict';

const require = createRequire(import.meta.url);
function compile(file) {
  const output = buildSync({ entryPoints:[file], bundle:true, platform:'node', format:'cjs', write:false });
  const mod = {exports:{}}; new Function('module','exports','require',output.outputFiles[0].text)(mod,mod.exports,require); return mod.exports;
}
const {webviewHtml} = compile('src/extension/webviewHtml.ts');
const {makeSnapshot, makeRun} = compile('tests/webview/snapshot.ts');
const snap = makeSnapshot();
snap.providers.claude.models = [
  {id:'claude-opus-5-5',name:'Claude Opus 5.5',efforts:['low','medium','high','xhigh','max']},
  {id:'claude-sonnet-5-5',name:'Claude Sonnet 5.5',efforts:['low','medium','high','xhigh','max']},
  {id:'claude-haiku-4-5-20251001',name:'Claude Haiku 4.5',efforts:[]},
];
snap.providers.claude.version='2.1.288';
Object.assign(snap.settings,{claudePlanModel:'claude-opus-5-5',claudeBuildModel:'claude-sonnet-5-5',claudeAuditModel:'claude-opus-5-5'});
const activeSnapshot = makeSnapshot({ runs: [makeRun({ id: 'live-run', title: 'Verify the session handoff', state: 'RUNNING', provider: 'claude', providerSessionId: 'native-session', cwd: '/tmp/browser-test', worktreePath: '/tmp/browser-worktree' })] });
const activity = { observedAt: new Date().toISOString(), executionActive: true, processId: 4321, processAlive: true, lastEventAt: new Date().toISOString(), entries: [
 { at: new Date().toISOString(), kind: 'tool', text: 'Using Edit' },
 { at: new Date().toISOString(), kind: 'message', text: 'The session handoff now targets the correct workspace. Checking the native conversation and process status.' },
] };
const sequencePreview = {
 id:'preview-token', title:'Feature rollout', folder:'/tmp/prompts/feature-rollout', errors:[], ignored:['README.md'], totalBytes:210,
 files:[
  {name:'001-build-foundation.md',order:1,bytes:100,prompt:'# Build foundation\nInspect the current repository and implement the shared components. Verify the changes.'},
  {name:'002-integrate-and-verify.md',order:2,bytes:110,prompt:'# Integrate and verify\nUse the foundation from step 001. Add integration coverage and verify acceptance checks.'},
 ],
};
const bridge = `window.__posted=[];window.acquireVsCodeApi=()=>{let state={};const snapshot=${JSON.stringify(snap)};const send=data=>window.dispatchEvent(new MessageEvent('message',{data}));return {getState:()=>state,setState:s=>state=s,postMessage:m=>{window.__posted.push(m);setTimeout(()=>{if(m.type==='requestSnapshot'){send({type:'snapshot',snapshot});send({type:'daemonHealth',healthy:true,text:'online'});}if(m.type==='hostInfo')send({type:'hostInfo',host:'Local',workspace:'/tmp/browser-test'});if(m.type==='rpc'){if(m.method==='patchSettings'){Object.assign(snapshot.settings,m.params);send({type:'snapshot',snapshot});}send({type:'rpcResult',requestId:m.requestId,result:m.method==='getRunActivity'?${JSON.stringify(activity)}:m.method==='promptSequence.pick'?${JSON.stringify(sequencePreview)}:m.method==='promptSequence.refresh'?${JSON.stringify(sequencePreview)}:m.method==='promptSequence.pickWorkspace'?'/tmp/sequence-project':m.method==='usageSummary'?{runs:0,completed:0,failed:0,activeMs:0,quotaWaitMs:0,continuations:0,byProviderActiveMs:{},recent:[],note:''}:{runId:'preview-run',batchId:'preview-batch'}})}},0)}}};`;
const server = createServer((req,res)=>{
  if(req.url==='/webview.js'){res.setHeader('Content-Type','text/javascript');res.end(readFileSync('dist/webview/webview.js'));return;}
  const mode=req.url==='/planner'?'planner':'sidebar';
  const selectedBridge = req.url === '/activity' ? bridge.replace('const snapshot='+JSON.stringify(snap), 'const snapshot='+JSON.stringify(activeSnapshot)) : bridge;
  const html=webviewHtml({scriptUri:'/webview.js',cspSource:"'self'",mode,nonce:'browser-check'}).replace('</head>', '<style>body{background:#fff;}</style></head>').replace('<script nonce="browser-check">',`<script nonce="browser-check">${selectedBridge}`);
  res.setHeader('Content-Type','text/html');res.end(html);
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));
const url=`http://127.0.0.1:${server.address().port}`;
const browser=await chromium.launch({headless:true,...(process.env.CHROME_PATH?{executablePath:process.env.CHROME_PATH}:{channel:'chrome'})});
const out=process.env.AC_BROWSER_ARTIFACTS || '/tmp/ac-browser-artifacts';mkdirSync(out,{recursive:true});
try{
 const page=await browser.newPage({viewport:{width:420,height:900}});const errors=[];page.on('pageerror',e=>{errors.push(e.message);console.error('Browser error:',e.message)});
 await page.goto(url);
 await page.getByLabel('Model',{exact:true}).waitFor();
 assert.equal(await page.locator('#root').count(),1);
 assert.equal(await page.getByLabel('Model',{exact:true}).inputValue(),'claude-opus-5-5');
 const levels=await page.getByLabel('Effort',{exact:true}).locator('option').evaluateAll(options=>options.map(o=>o.value));
 assert.deepEqual(levels,['','low','medium','high','xhigh','max']);
 await page.getByLabel('Effort',{exact:true}).selectOption('max');
 await page.getByLabel('Prompt',{exact:true}).fill('Verify the overnight job');
 await page.getByRole('button',{name:'Queue run',exact:true}).click();
 await page.waitForFunction(()=>window.__posted.some(m=>m.method==='enqueueRun'));
 const queued=await page.evaluate(()=>window.__posted.find(m=>m.method==='enqueueRun').params);
 assert.equal(queued.model,'claude-opus-5-5');assert.equal(queued.effort,'max');
 for(const width of [280,320,420,800]){
  await page.setViewportSize({width,height:900});
  const overflow=await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth);
  assert(!overflow,`sidebar overflows at ${width}px`);
 }
 await page.setViewportSize({width:420,height:900});
 await page.screenshot({path:join(out,'queue.png'),fullPage:false});
 // Live queue arrivals must append immediately, before any snapshot refresh.
 const queueRuns = ['First task', 'Second task', 'Third task'].map((title, i) => makeRun({ id: `q${i}`, title, sortKey: (i + 1) * 1024 }));
 await page.evaluate(runs => {
   for (const run of runs) window.dispatchEvent(new MessageEvent('message', { data: { type: 'daemonEvent', event: 'runUpdated', payload: run } }));
 }, queueRuns);
 await page.getByRole('button', {name:'Move Third task up', exact:true}).waitFor();
 assert.deepEqual(await page.locator('.ac-qrow-title > .ac-block').allTextContents(), ['First task', 'Second task', 'Third task']);
 assert(await page.getByRole('button', {name:'Move First task up', exact:true}).isDisabled());
 assert(await page.getByRole('button', {name:'Move Third task down', exact:true}).isDisabled());
 for (const width of [280, 320, 420, 800]) {
  await page.setViewportSize({width,height:1000});
  assert(!(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth)), `queue controls overflow at ${width}px`);
 }
 await page.setViewportSize({width:420,height:1100});
 await page.locator('.ac-qlist').scrollIntoViewIfNeeded();
 await page.screenshot({path:join(out,'queue-controls.png'),fullPage:false});
 await page.getByRole('button', {name:'Move Second task down', exact:true}).click();
 await page.waitForFunction(()=>window.__posted.some(m=>m.method==='reorderQueue'));
 const reordered=await page.evaluate(()=>window.__posted.find(m=>m.method==='reorderQueue').params);
 assert.deepEqual(reordered,{runId:'q1',beforeRunId:''});
 await page.getByRole('button',{name:'Import prompts from folder',exact:true}).click();
 await page.getByText('2 steps in execution order',{exact:true}).waitFor();
 await page.getByRole('tab',{name:'CONTROL',exact:true}).click();
 await page.getByRole('tab',{name:'QUEUE',exact:true}).click();
 await page.getByText('2 steps in execution order',{exact:true}).waitFor();
 assert.equal(await page.evaluate(()=>window.__posted.filter(m=>m.method==='promptSequence.pick').length),1,'switching tabs must preserve the reviewed import');
 assert.equal(await page.getByRole('button',{name:'Queue run',exact:true}).count(),0);
 await page.getByText('build foundation',{exact:true}).click();
 assert((await page.getByLabel('Prompt 001-build-foundation.md',{exact:true}).innerText()).includes('Inspect the current repository'));
 for(const width of [280,320,420,800]){
  await page.setViewportSize({width,height:1000});
  assert(!(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth)),`sequence overflows at ${width}px`);
 }
 await page.setViewportSize({width:420,height:1100});
 await page.locator('.ac-sequence').scrollIntoViewIfNeeded();
 await page.screenshot({path:join(out,'prompt-sequence.png'),fullPage:false});
 await page.getByRole('button',{name:'Change working folder',exact:true}).click();
 await page.getByText('/tmp/sequence-project',{exact:true}).waitFor();
 await page.getByRole('button',{name:'Queue 2 prompts in order',exact:true}).click();
 await page.getByText('2 prompts queued in order.',{exact:false}).waitFor();
 const imported=await page.evaluate(()=>window.__posted.find(m=>m.method==='promptSequence.import').params);
 assert.equal(imported.id,'preview-token');assert.equal(imported.cwd,'/tmp/sequence-project');assert.equal(imported.settings.effort,'max');
 await page.getByRole('button',{name:'View sequence',exact:true}).click();
 assert(await page.evaluate(()=>window.__posted.some(m=>m.type==='openDetail'&&m.batchId==='preview-batch')));
 await page.getByRole('tab',{name:'CONTROL',exact:true}).click();
 await page.getByText('Staged intelligence',{exact:true}).click();
 assert.equal(await page.getByLabel('Claude plan model',{exact:true}).inputValue(),'claude-opus-5-5');
 assert.equal(await page.getByLabel('Claude audit effort',{exact:true}).count(),1);
 await page.getByLabel('Claude build effort',{exact:true}).selectOption('xhigh');
 await page.screenshot({path:join(out,'control.png'),fullPage:false});
 await page.goto(url+'/planner');
 await page.getByLabel('Model',{exact:true}).waitFor();
 assert.equal(await page.getByLabel('Model',{exact:true}).inputValue(),'claude-opus-5-5');
 await page.getByLabel('Effort',{exact:true}).selectOption('max');
 await page.getByLabel('Model',{exact:true}).selectOption('claude-haiku-4-5-20251001');
 await page.waitForFunction(()=>document.querySelector('select[id$="-effort"]').value==='');
 await page.setViewportSize({width:1100,height:800});
 await page.screenshot({path:join(out,'planner.png'),fullPage:false});
 await page.goto(url+'/activity');
 await page.getByText('Process alive · PID 4321',{exact:true}).waitFor();
 await page.getByText('Using Edit',{exact:true}).waitFor();
 for(const width of [280,320,420,800]) {
  await page.setViewportSize({width,height:1000});
  assert(!(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth)),`activity overflows at ${width}px`);
 }
 await page.getByRole('button',{name:'Open in Claude Code',exact:true}).click();
 const opened=await page.evaluate(()=>window.__posted.find(m=>m.type==='openClaude'));
 assert.equal(opened.cwd,'/tmp/browser-worktree');
 assert.equal(opened.sessionId,'native-session');
 await page.setViewportSize({width:420,height:1000});
 await page.getByRole('article',{name:'Active run: Verify the session handoff'}).scrollIntoViewIfNeeded();
 await page.screenshot({path:join(out,'live-activity.png'),fullPage:false});
 await page.evaluate(()=>window.dispatchEvent(new MessageEvent('message',{data:{type:'daemonHealth',healthy:false,text:'offline'}})));
 await page.getByText('Live status unavailable',{exact:true}).waitFor();
 assert.equal(await page.getByText('Process alive · PID 4321',{exact:true}).count(),0);
 assert.deepEqual(errors,[]);
 console.log(`BROWSER CHECKS PASSED: sidebar widths 280/320/420/800, explicit model/effort dispatch, staged controls, planner effort reset, folder sequence preview/import at all widths. Screenshots: ${out}`);
}finally{await browser.close();await new Promise(r=>server.close(r));}
