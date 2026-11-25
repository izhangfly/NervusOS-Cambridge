import test from 'node:test';import assert from 'node:assert/strict';import fs from 'node:fs';
import {engine} from '../scripts/physics-fixture.mjs';import{createModel}from'../src/demo/model.js';import{makeScene}from'../src/demo/scene.js';import{apps}from'../src/demo/apps/index.js';import{decodeVisualBundle}from'../src/demo/model-assets.js';import{fleetSceneSeeds}from'../src/demo/fleet-seeds.js';
const bundles=JSON.parse(fs.readFileSync(new URL('../src/demo/model-bundles.json',import.meta.url),'utf8')),ids=['piper','panda','ur5e','gen3'];
for(const id of ids)test(`${id}: delivered physics preserves mass, inertia, joints and touch sensors`,()=>{
 const bytes=fs.readFileSync(new URL('../public/'+bundles[id].visual,import.meta.url)),visual=decodeVisualBundle(bytes.buffer.slice(bytes.byteOffset,bytes.byteOffset+bytes.byteLength));
 assert.equal(visual.id,id);assert.ok(Object.keys(visual.meshes).length>0);
 const original=engine(id),xml=fs.readFileSync(new URL('../public/'+bundles[id].physics,import.meta.url),'utf8');assert.ok(!/<mesh\b/.test(xml));
 const scene=makeScene(apps.find(a=>a.id==='quality'),42),full=createModel(original,scene),lean=createModel({...original,xml,meshes:{}},scene);
 try{for(const key of ['body_mass','body_inertia','body_ipos','body_iquat','jnt_qposadr','actuator_trnid','sensor_adr'])assert.deepEqual(Array.from(lean.model[key]),Array.from(full.model[key]),key);assert.equal(lean.model.nq,full.model.nq);assert.deepEqual(lean.touch,full.touch);for(let i=0;i<30;i++){lean.mj.mj_step(lean.model,lean.data);full.mj.mj_step(full.model,full.data)}assert.deepEqual(Array.from(lean.data.qpos),Array.from(full.data.qpos));}finally{lean.dispose();full.dispose()}
});
test('each LineSight arm has a different balanced batch, including new batches',()=>{
 for(let seed=0;seed<100;seed++){
  const plans=fleetSceneSeeds('quality',seed,ids),signatures=ids.map(id=>{const items=makeScene({id:'quality'},plans[id]).items,good=items.filter(i=>i.finish==='pass').length;assert.equal(items.length,6);assert.ok(good>=2&&good<=4);return items.map(i=>i.finish).join(',')});
  assert.equal(new Set(signatures).size,ids.length);
 }
 assert.deepEqual(fleetSceneSeeds('kitting',42,ids),Object.fromEntries(ids.map(id=>[id,42])));
});
