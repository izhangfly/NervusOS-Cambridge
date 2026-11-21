import test from 'node:test';
import assert from 'node:assert/strict';
import {fleetState} from '../src/demo/runtime.js';
const cell=(id,status,progress=0)=>({id,profile:{short:id},stats:{status,progress,joints:[id==='piper'?6:7],result:{en:id+' finished',zh:id+' done'}}});
test('fleet stays active when the focused robot finishes first',()=>{
 const out=fleetState([cell('piper','complete',1),cell('panda','running',.5)],'piper');
 assert.equal(out.status,'running');assert.equal(out.result,null);assert.equal(out.progress,.75);assert.deepEqual(out.joints,[6]);
});
test('pause, stop and errors follow the entire fleet',()=>{
 assert.equal(fleetState([cell('piper','complete'),cell('panda','paused')],'piper').status,'paused');
 assert.equal(fleetState([cell('piper','idle'),cell('panda','error')],'piper').status,'error');
 assert.equal(fleetState([cell('piper','idle')],'piper',{stopped:true}).status,'stopped');
 assert.equal(fleetState([cell('piper','idle')],'piper',{ready:false}).status,'loading');
});
test('fleet completion reports both robot results',()=>{
 const out=fleetState([cell('piper','complete',1),cell('panda','complete',1)],'panda');
 assert.equal(out.status,'complete');assert.match(out.result.en,/piper.*panda/);assert.deepEqual(out.joints,[7]);
});
