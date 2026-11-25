import assert from'node:assert/strict';import{chromium,webkit,devices}from'playwright';
const[url='https://ianzhyy.com/projects/nervusos/demo/',engine='chromium',size='desktop']=process.argv.slice(2),browser=await({chromium,webkit}[engine]).launch({headless:true});
try{
 const context=await browser.newContext(size==='phone'?devices['iPhone 15']:{viewport:{width:1440,height:1000}}),page=await context.newPage(),errors=[],requests=[];
 page.on('pageerror',e=>errors.push(e.message));page.on('request',r=>requests.push(r.url()));
 const load=async reload=>{requests.length=0;const t=performance.now();if(reload)await page.reload({waitUntil:'domcontentloaded'});else await page.goto(url,{waitUntil:'domcontentloaded'});await page.waitForFunction(()=>document.querySelectorAll('.cap-col h4').length===4&&[...document.querySelectorAll('.cap-col h4')].every(e=>e.textContent.includes('Ready')),null,{timeout:90000});const seconds=(performance.now()-t)/1000;assert.equal(requests.filter(u=>/\.wasm(?:\?|$)/.test(u)).length,1,'one engine request for the entire fleet');assert.equal(requests.filter(u=>/visual-.*\.bin/.test(u)).length,4,'one display bundle per arm');assert.equal(requests.filter(u=>/\.stl(?:\?|$)/.test(u)).length,0,'workers and renderer should not download individual mesh files');return +seconds.toFixed(2)};
 const cold=await load(false),warm=await load(true);assert.deepEqual(errors,[]);console.log(JSON.stringify({engine,size,coldSeconds:cold,warmSeconds:warm,status:'PASS'}));
}finally{await browser.close()}
