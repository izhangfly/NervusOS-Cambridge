import assert from 'node:assert/strict';import{chromium,webkit,devices}from'playwright';import{apps}from'../src/demo/apps/index.js';import{demoReceipt}from'../src/demo/marketplace.js';
const[url='http://127.0.0.1:5180/demo/?app=carry',engine='chromium',size='desktop',app='carry']=process.argv.slice(2),browser=await({chromium,webkit}[engine]).launch({headless:true});
const errors=[],failed=[];
try{
 const ctx=await browser.newContext(size==='phone'?devices['iPhone 15']:{viewport:{width:1440,height:1000}});
 await ctx.addInitScript(({ids,receipts})=>{localStorage.setItem('nervus-demo-installed',JSON.stringify(ids));localStorage.setItem('nervus-demo-purchases',JSON.stringify(receipts))},{ids:apps.map(a=>a.id),receipts:Object.fromEntries(apps.filter(a=>a.price).map(a=>[a.id,demoReceipt(a)]))});
 const page=await ctx.newPage();page.on('pageerror',e=>errors.push(e.message));page.on('console',e=>{if(e.type()==='error')errors.push(e.text())});page.on('requestfailed',r=>failed.push(r.url()));
 await page.goto(url,{waitUntil:'domcontentloaded'});
 // Complete the default fleet load before replacing its workers. Interrupting their asset
 // requests during a view change produces expected cancellation events on slower networks.
 const initialPair=apps.find(a=>a.id===new URL(url).searchParams.get('app'))?.cooperative;
 await page.waitForFunction(({count,state})=>document.querySelectorAll('.cap-col h4').length===count&&[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes(state)),{count:initialPair?2:4,state:initialPair?'idle':'Ready'},{timeout:90000});
 await page.getByRole('button',{name:app==='carry'?'CarryTogether':'FixtureMate',exact:true}).click();
 await page.waitForFunction(()=>document.querySelectorAll('.cap-col h4').length===2&&[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('idle')),null,{timeout:60000});
 assert.equal(await page.locator('.wrist-camera').isVisible(),false);assert.equal(await page.locator('.telemetry').count(),0);
 await page.getByLabel('Arm A model').selectOption('ur5e');await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('idle')),null,{timeout:60000});
 await page.getByLabel('Arm B model').selectOption('gen3');await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('idle')),null,{timeout:60000});
 await page.getByRole('button',{name:new RegExp(app==='carry'?'Carry the tray together':'Hold and assemble together')}).click();
 await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].some(e=>e.textContent.includes('running')));
 await page.getByRole('button',{name:'Pause',exact:true}).click();await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('paused')));
 await page.getByRole('button',{name:'Resume',exact:true}).click();
 await page.waitForFunction(()=>!!document.querySelector('.demo-error')||[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('complete')),null,{timeout:240000});
 assert.equal(await page.locator('.demo-error').count(),0,await page.locator('.demo-error').textContent().catch(()=>''));
 const result=await page.locator('.task-status').textContent();assert.match(result,app==='carry'?/Tray carried together/:/Insert located in the housing/);
 const bounds=await page.evaluate(()=>({scene:document.querySelector('.simulation-canvas').getBoundingClientRect().toJSON(),trace:document.querySelector('.cap-trace').getBoundingClientRect().toJSON(),width:innerWidth,content:document.documentElement.scrollWidth}));assert.ok(bounds.trace.top>=bounds.scene.bottom-1);assert.ok(bounds.content<=bounds.width+1);
 await page.locator('.simulation-panel').screenshot({path:`/tmp/nervus-cooperative-${engine}-${size}-${app}.png`});
 await page.getByRole('button',{name:'E-STOP',exact:true}).click();await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('stopped')));
 await page.getByRole('button',{name:'Re-arm simulation',exact:true}).click();await page.waitForFunction(()=>[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('idle')));
 await page.getByRole('link',{name:'Platform',exact:true}).first().click();await page.locator('.simulation-canvas').waitFor({state:'detached'});
 assert.deepEqual(errors,[]);assert.deepEqual(failed,[]);console.log(JSON.stringify({engine,size,app,result,bounds,status:'PASS'}));
}finally{await browser.close()}
