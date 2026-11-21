import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import load from '@mujoco/mujoco';
import {Cell} from '../src/demo/cell.js';
import {garmentState} from '../src/demo/model.js';
const mj=await load();
function engine(id){const root=new URL('../public/models/'+id+'/',import.meta.url);return {mj,id,xml:fs.readFileSync(new URL(id+'.xml',root),'utf8'),meshes:Object.fromEntries(fs.readdirSync(new URL('assets/',root)).map(n=>[n,new Uint8Array(fs.readFileSync(new URL('assets/'+n,root)))]))}}
for(const id of ['piper','panda'])test(`${id}: cancellation releases cloth forces; reset and stop remain consistent`,()=>{
 const events=[],c=new Cell(engine(id),v=>events.push(v));
 try{
  c.setApp('laundry',true,42);const p=c.p,item=p.items.get('garment_0');
  garmentState(p,item,'held');for(const i of p.adhere)p.data.ctrl[i]=1;
  c.action={};c.paused=true;c.cancel();
  assert.equal(c.action,null);assert.equal(c.paused,false);assert.equal(item.state,'free');
  for(const i of p.adhere)assert.equal(p.data.ctrl[i],0);
  assert.equal(events.at(-1).status,'idle');
  c.stop();const time=p.data.time;c.advance(.1);assert.equal(p.data.time,time);
  c.reset();assert.equal(events.at(-1).status,'stopped');assert.equal(c.start('load',42),null);
  c.rearm();assert.equal(events.at(-1).status,'idle');
  c.setJoint(0,Infinity);c.setJoint(99,0);c.setJoint(0,100);assert.equal(p.data.ctrl[0],p.profile.limits[0][1]);
  c.action={};c.pause(true);c.pause(true);assert.equal(c.paused,true);c.pause(false);assert.equal(c.paused,false);
  garmentState(p,item,'held');for(const i of p.adhere)p.data.ctrl[i]=1;c.action={jobIndex:0,jobs:[],definition:{}};c.nextJob();assert.equal(item.state,'free');for(const i of p.adhere)assert.equal(p.data.ctrl[i],0);
 }finally{c.p?.dispose()}
});
test('a second start cannot reset an already running task',()=>{
 const c=new Cell(engine('piper'),()=>{});try{c.setApp('teach',true,42);const result=c.start('tray-to-jig',42);assert.ok(result);const action=c.action;assert.equal(c.start('tray-to-jig',43),null);assert.equal(c.action,action)}finally{c.p?.dispose()}
});

test('laundry audits the drum and requeues a requested garment after a skipped grasp',()=>{
 const c=new Cell(engine('piper'),()=>{});try{
  c.setApp('laundry',true,42);
  c.action={jobIndex:0,jobs:[],wanted:['garment_2'],definition:{},total:0};
  c.nextJob();assert.ok(c.action,'a missing garment must not finish the task');
  assert.equal(c.action.job.id,'garment_2');assert.equal(c.action.sweeps,1);
  assert.equal([...c.p.items.values()].filter(i=>i.state==='held').length,1);
 }finally{c.p?.dispose()}
});

test('cloth settling is independent of render-frame batching',()=>{
 const a=new Cell(engine('piper'),()=>{}),b=new Cell(engine('piper'),()=>{});
 try{
  for(const c of[a,b]){c.setApp('laundry',true,42);garmentState(c.p,c.p.items.get('garment_2'),'free')}
  a.substeps(160);for(let i=0;i<10;i++)b.substeps(16);
  assert.deepEqual(Array.from(a.p.data.qpos),Array.from(b.p.data.qpos));
  assert.equal(a.p.items.get('garment_2').state,b.p.items.get('garment_2').state);
 }finally{a.p?.dispose();b.p?.dispose()}
});

test('sorting completion counts received toys, excluding old tallies and hovering items',()=>{
 const c=new Cell(engine('panda'),()=>{});
 try{
  c.setApp('kitting',true,42);
  const item=[...c.p.items.values()].find(i=>!i.soft&&i.category==='blocks');
  const audit=()=>{c.counts={blocks:99,cars:99,plush:99};c.action={jobIndex:0,jobs:[],definition:{}};c.nextJob();return c.counts};
  assert.deepEqual(audit(),{blocks:0,cars:0,plush:0});
  c.p.data.qpos.set([.18,.27,.04],item.qadr);
  assert.deepEqual(audit(),{blocks:1,cars:0,plush:0});
  c.p.data.qpos[item.qadr+2]=.4;
  assert.deepEqual(audit(),{blocks:0,cars:0,plush:0});
 }finally{c.p?.dispose()}
});
