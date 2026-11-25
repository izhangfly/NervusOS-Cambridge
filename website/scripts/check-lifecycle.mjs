// Development-only worker/lifecycle regression checks. Production exposes no inspection API.
import assert from 'node:assert/strict';
import {chromium} from 'playwright';
const browser=await chromium.launch({headless:true});
try{
 const page=await browser.newPage({viewport:{width:1440,height:1000}}),errors=[];
 page.on('pageerror',e=>errors.push(e.message));
 await page.goto(process.argv[2]||'http://127.0.0.1:5180/demo?app=laundry');
 await page.waitForFunction(()=>window.__nervusDemo?.ready,null,{timeout:60000});
 const result=await page.evaluate(async()=>{
  const s=__nervusDemo,wait=async predicate=>{for(let i=0;i<600;i++){if(predicate())return;await new Promise(r=>setTimeout(r,100))}throw Error('Lifecycle wait timed out')};
  const report={};
  await Promise.all([s.setFleet(false),s.setFleet(true),s.setFleet(false)]);
  await wait(()=>s.ready);report.race={fleet:s.fleet,cells:[...s.cells.keys()]};
  s.stop();await s.setFleet(true);await wait(()=>s.ready&&[...s.cells.values()].every(c=>c.stats.status==='stopped'));
  report.stop=[...s.cells.values()].map(c=>c.stats.status);s.rearm();await wait(()=>[...s.cells.values()].every(c=>c.stats.status==='idle'));
  const {apps}=await import('/src/demo/apps/index.js');
  s.setApp(apps[2]);s.setApp(apps[3]);s.setApp(apps[0]);await wait(()=>s.ready);
  report.apps=[...s.cells.values()].map(c=>c.manifest.scene.id);
  s.start(apps[0].actions[0]);await wait(()=>[...s.cells.values()].every(c=>c.stats.status==='running'));
  s.pause();await wait(()=>[...s.cells.values()].every(c=>c.stats.status==='paused'));
  const before=await s.debug('piper',c=>c.p.data.time);await new Promise(r=>setTimeout(r,500));const after=await s.debug('piper',c=>c.p.data.time);report.pause={before,after};
  s.cancel();await wait(()=>[...s.cells.values()].every(c=>c.stats.status==='idle'));
  report.cancel=await s.debug('piper',c=>({action:!!c.action,adhesion:c.p.adhere.map(i=>c.p.data.ctrl[i])}));
  s.reset();await new Promise(r=>setTimeout(r,4500));
  // Manually settling rigid scenes is cheaper than laundry for an idle-render check.
  s.setApp(apps[2]);await wait(()=>s.ready);await new Promise(r=>setTimeout(r,4000));
  const frames=s.renderer.info.render.frame;await new Promise(r=>setTimeout(r,1000));report.idleRedraws=s.renderer.info.render.frame-frames;
  let updates=0;const original=s.onUpdate;s.onUpdate=v=>{updates++;original(v)};
  report.ready=s.ready;return report;
 });
 assert.deepEqual(result.race,{fleet:false,cells:['piper']});assert.deepEqual(result.stop,Array(4).fill('stopped'));assert.deepEqual(result.apps,Array(4).fill('laundry'));
 assert.equal(result.pause.before,result.pause.after);assert.equal(result.cancel.action,false);assert.deepEqual(result.cancel.adhesion,[0,0]);assert.ok(result.idleRedraws<=1);
 const manual=await page.evaluate(async()=>{
  const s=__nervusDemo,sleep=ms=>new Promise(r=>setTimeout(r,ms));
  await s.setFleet(false);for(let i=0;i<600&&!s.ready;i++)await sleep(100);
  if(!s.ready)throw Error('Single workcell did not become ready');
  await sleep(2500);
  const before=await s.debug('piper',c=>({q:c.p.data.qpos[0],grip:c.p.data.qpos[c.p.profile.gripper]}));
  s.setJoint(0,before.q+.2);s.setGripper(true);await sleep(1000);
  const moved=await s.debug('piper',c=>({q:c.p.data.qpos[0],grip:c.p.data.qpos[c.p.profile.gripper]}));
  s.reset();await sleep(2500);
  const reset=await s.debug('piper',c=>({q:c.p.data.qpos[0],initial:c.p.initial[0],grip:c.p.data.qpos[c.p.profile.gripper]}));
  // Wake a settled arm before hiding the page: the guard must stop active stepping.
  s.setJoint(0,reset.q+.2);await sleep(100);
  return {before,moved,reset};
 });
 assert.ok(manual.moved.q-manual.before.q>.1,'idle joint command must move the arm');
 assert.ok(manual.before.grip-manual.moved.grip>.01,'idle gripper command must close the fingers');
 assert.ok(Math.abs(manual.reset.q-manual.reset.initial)<.02,'reset must publish and restore the pose');
 const firstTime=await page.evaluate(()=>__nervusDemo.debug('piper',c=>c.p.data.time));
 // Exercise the explicit visibility guard without moving a person's existing browser tab.
 await page.evaluate(()=>Object.defineProperty(document,'hidden',{configurable:true,value:true}));
 await page.waitForTimeout(1000);
 const hiddenTime=await page.evaluate(()=>__nervusDemo.debug('piper',c=>c.p.data.time));assert.ok(hiddenTime-firstTime<=.11);
 await page.evaluate(()=>{delete document.hidden;document.dispatchEvent(new Event('visibilitychange'))});
 await page.waitForTimeout(500);
 const visibleTime=await page.evaluate(()=>__nervusDemo.debug('piper',c=>c.p.data.time));assert.ok(visibleTime-hiddenTime>.1,'visible arm must resume its pending motion');
 await page.evaluate(()=>{window.__disposedSim=__nervusDemo;__nervusDemo.dispose()});
 assert.equal(await page.evaluate(()=>__disposedSim.cells.size),0);assert.equal(await page.evaluate(()=>__disposedSim.debugCalls.size),0);
 assert.deepEqual(errors,[]);console.log(JSON.stringify({...result,manual,hiddenDelta:hiddenTime-firstTime,visibleDelta:visibleTime-hiddenTime,errors},null,2));
}finally{await browser.close()}
