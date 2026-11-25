import test from 'node:test';
import assert from 'node:assert/strict';
import {Coordinator} from '../src/demo/coordinator.js';
import {CooperativeCell} from '../src/demo/cooperative-cell.js';
import {createCooperativeModel} from '../src/demo/cooperative-model.js';
import {engine} from '../scripts/physics-fixture.mjs';
import carry from '../src/demo/apps/carry.js';

test('shared motion requires physical grips and exclusive resource ownership',()=>{
 const c=new Coordinator();
 assert.throws(()=>c.barrier('load',[0,1]));
 c.confirmGrip(0,true);assert.throws(()=>c.barrier('load',[0,1]));
 c.confirmGrip(1,true);c.barrier('load',[0,1]);c.require('load');
 c.hold(0,[.3,-.15,.1]);
 assert.throws(()=>c.insertion(1,0,[.3,-.14,.1],true));
 assert.throws(()=>c.insertion(1,0,[.3,-.15,.1],false));
 c.insertion(1,0,[.3,-.15,.1],true);
 assert.throws(()=>c.reserve('insertion',0));
 assert.throws(()=>c.release('insertion',0));
 c.release('insertion',1);c.clear();assert.equal(c.leases.size,0);assert.throws(()=>c.require('load'));
});

for(const id of ['piper','panda','ur5e','gen3'])test(`${id}: matching arms have distinct joints and share one physical tray`,()=>{
 const e=engine(id),p=createCooperativeModel([e,e],carry);
 try{
  assert.equal(p.providers.length,2);assert.equal(p.items.length,1);
  assert.notEqual(p.providers[0].tool,p.providers[1].tool);
  const a=new Set(p.providers[0].qadr);
  assert.ok(p.providers[1].qadr.every(j=>!a.has(j)));
  for(const r of p.providers){assert.equal(r.act.length,r.profile.dof+1);assert.ok(r.touch.every(j=>j>=0));}
  for(let i=0;i<50;i++)p.mj.mj_step(p.model,p.data);
  assert.ok(p.data.qpos.every(Number.isFinite));
 }finally{p.dispose()}
});

test('cooperative pause, cancellation and E-STOP preserve lifecycle and clear leases',()=>{
 const e=engine('piper'),events=[],c=new CooperativeCell([e,e],v=>events.push(v));
 try{
  c.setApp('carry');c.coordinator.reserve('load',0);c.stop();
  assert.equal(c.coordinator.leases.size,0);
  const time=c.p.data.time;c.advance(.1);assert.equal(c.p.data.time,time);
  assert.equal(c.start('carry'),null);c.reset();assert.equal(events.at(-1).status,'stopped');
  c.rearm();c.action={};c.pause(true);const paused=c.p.data.time;c.advance(.1);assert.equal(c.p.data.time,paused);
  c.cancel();assert.equal(c.action,null);assert.equal(c.paused,false);
 }finally{c.p?.dispose()}
});

test('physics-only verification retains the actual UR5e inertias and contact geometry',()=>{
 const e=engine('ur5e'),full=createCooperativeModel([e,e],carry),fast=createCooperativeModel([engine('ur5e',{physicsOnly:true}),engine('ur5e',{physicsOnly:true})],carry);
 try{
  assert.equal(fast.model.nq,full.model.nq);assert.equal(fast.model.nbody,full.model.nbody);
  assert.deepEqual(Array.from(fast.model.body_mass),Array.from(full.model.body_mass));
  assert.deepEqual(Array.from(fast.model.body_inertia),Array.from(full.model.body_inertia));
  const count=p=>Array.from(p.model.geom_contype).filter(Boolean).length;
  assert.equal(count(fast),count(full));
 }finally{full.dispose();fast.dispose()}
});
