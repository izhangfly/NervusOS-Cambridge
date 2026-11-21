// Production smoke test. Use one browser at a time so the verification does not compete for CPU.
import assert from 'node:assert/strict';
import {chromium,webkit,devices} from 'playwright';
import {apps} from '../src/demo/apps/index.js';
import {demoReceipt} from '../src/demo/marketplace.js';
const [url='http://127.0.0.1:8777/projects/nervusos/demo/',engine='chromium',size='desktop',task='load']=process.argv.slice(2);
const browser=await ({chromium,webkit}[engine]).launch({headless:true});
const errors=[],failures=[],report={engine,size,task,url};
try{
 const ctx=await browser.newContext(size==='phone'?devices['iPhone 15']:{viewport:{width:1440,height:1000}});
 await ctx.addInitScript(({ids,receipts})=>{
  localStorage.setItem('nervus-demo-installed',JSON.stringify(ids));
  localStorage.setItem('nervus-demo-purchases',JSON.stringify(receipts));
  localStorage.setItem('nervus-demo-fleet','1');localStorage.setItem('nervus-demo-robot','piper');
 },{ids:apps.map(a=>a.id),receipts:Object.fromEntries(apps.filter(a=>a.price).map(a=>[a.id,demoReceipt(a)]))});
 const page=await ctx.newPage();
 page.on('pageerror',e=>errors.push(e.message));
 page.on('console',m=>{if(m.type()==='error')errors.push(m.text())});
 page.on('requestfailed',r=>failures.push({url:r.url(),error:r.failure()?.errorText}));
 await page.goto(url,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>document.querySelectorAll('.cap-col h4').length===2&&[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('Ready')),null,{timeout:60000});
 assert.equal(await page.evaluate(()=>typeof window.__nervusDemo),'undefined','production inspection hook must be absent');
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth+1),false,'phone overflow');
 const target=task==='inspect'?'LineSight QC':task==='assemble'?'BearingFit':task==='sort'?'Toy tidy':task==='teach'?'Teach & repeat':'Laundry transfer';
 const label=task==='inspect'?'Inspect a batch':task==='assemble'?'Assemble six modules':task==='sort'?'Sort every toy':task==='all'?'Clear the table & load all':task==='teach'?'Replay: tray → jig':'Load the laundry basket';
 await page.getByRole('button',{name:'Settings',exact:true}).click();
 await page.getByRole('button',{name:/Franka Emika Panda/}).click();
 await page.waitForFunction(()=>document.querySelector('.robot-option.selected')?.textContent.includes('Panda'));
 assert.match(await page.locator('.robot-switch-intro').textContent(),/fleet view/);
 await page.getByRole('button',{name:'Desktop home',exact:true}).click();
 await page.getByRole('button',{name:target,exact:true}).click();
 const start=page.getByRole('button',{name:new RegExp(label.replace(/[.*+?^${}()|[\]\\]/g,'\\$&'))});
 await start.waitFor();
 await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('Ready')));
 await page.locator('.speed-select select').selectOption('1.5');
 await start.click();
 await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].some(e=>e.textContent.includes('Running')));
 await page.getByRole('button',{name:'Pause',exact:true}).click();
 await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('Paused')),null,{timeout:15000});
 await page.getByRole('button',{name:'Resume',exact:true}).click();
 console.log(JSON.stringify({phase:'running',engine,size,task}));
 const progressTimer=setInterval(async()=>{try{console.log(JSON.stringify({phase:'progress',states:await page.locator('.cap-col h4').allTextContents(),text:await page.locator('.task-status').textContent(),progress:await page.locator('.task-status progress').getAttribute('value')}))}catch{}},15000);
 try{await page.waitForFunction(()=>!!document.querySelector('.demo-error')||[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('Finished')),null,{timeout:360000})}
 catch(e){await page.locator('.simulation-panel').screenshot({path:'/tmp/nervus-timeout.png'}).catch(()=>{});throw e}
 finally{clearInterval(progressTimer)}
 assert.equal(await page.locator('.demo-error').count(),0,await page.locator('.demo-error').textContent().catch(()=>''));
 report.result=await page.locator('.task-status').textContent();
 if(task==='load'||task==='all'){const n=task==='all'?6:3;assert.equal((report.result.match(new RegExp(n+' garments in the washer','g'))||[]).length,2,report.result)}
 if(task==='inspect'){const batches=[...report.result.matchAll(/Batch inspected: (\d+) pass, (\d+) rework/g)];assert.equal(batches.length,2,report.result);for(const b of batches)assert.equal(+b[1]+ +b[2],6,report.result)}
 if(task==='sort')assert.equal((report.result.match(/All 13 toys sorted/g)||[]).length,2,report.result);
 if(task==='assemble')assert.equal((report.result.match(/6 wheel modules/g)||[]).length,2,report.result);report.states=await page.locator('.cap-col h4').allTextContents();
 report.layout=await page.evaluate(()=>({width:innerWidth,content:document.documentElement.scrollWidth,trace:!!document.querySelector('.cap-trace'),canvas:document.querySelector('.simulation-canvas')?.getBoundingClientRect().toJSON()}));
 const overlays=await page.evaluate(()=>['.fleet-bar','.wrist-camera'].map(sel=>document.querySelector(sel).getBoundingClientRect().toJSON()));
 const [bar,cam]=overlays;assert.ok(bar.right<=cam.left||bar.bottom<=cam.top||cam.bottom<=bar.top,'fleet controls must not cover the wrist camera');
 report.overlays=overlays;
 const audit=await page.locator('.os-device, .cap-trace, .vision-readout').evaluateAll(es=>es.map(e=>({class:e.className,background:getComputedStyle(e).backgroundColor,color:getComputedStyle(e).color})));
 report.colors=audit;
 await page.locator('.simulation-panel').screenshot({path:`/tmp/nervus-${engine}-${size}-${task}.png`,fullPage:true});
 await page.getByRole('button',{name:'Stop simulation',exact:true}).click();
 await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('Stop latched')));
 await page.getByRole('button',{name:'One arm',exact:true}).click();
 await page.waitForFunction(()=>document.querySelectorAll('.cap-col h4').length===1&&document.querySelector('.cap-col h4').textContent.includes('Stop latched'));
 await page.getByRole('button',{name:'Re-arm simulation',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('.cap-col h4').textContent.includes('Ready'));
 await page.getByRole('button',{name:'Fleet · PiPER + Panda',exact:true}).click();
 await page.waitForFunction(()=>document.querySelectorAll('.cap-col h4').length===2&&[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('Ready')));
 // Leaving the demo disposes the two physics workers and the WebGL renderers.
 await page.getByRole('link',{name:'Platform',exact:true}).first().click();
 await page.locator('.simulation-canvas').waitFor({state:'detached',timeout:15000});
 assert.equal(await page.locator('.simulation-canvas').count(),0);
 report.errors=errors;report.failures=failures;
 assert.deepEqual(errors,[]);assert.deepEqual(failures,[]);
 console.log(JSON.stringify(report,null,2));
}finally{await browser.close()}
