import test from 'node:test';import assert from 'node:assert/strict';
import {engine} from '../scripts/physics-fixture.mjs';
import {Cell} from '../src/demo/cell.js';import {garmentState,objectPosition} from '../src/demo/model.js';
test('idle cloth layers retain contact, ordering and finite dynamics when a layer wakes',()=>{
 const c=new Cell(engine('piper'),()=>{});try{
  c.setApp('laundry',true,42);const p=c.p,items=[0,1,2].map(i=>p.items.get('garment_'+i));
  for(const item of items){assert.ok(p.model.flex_contype[item.flex]&p.model.flex_conaffinity[item.flex]);assert.equal(item.state,'sleep')}
  garmentState(p,items[2],'free');c.substeps(400);
  const zs=items.map(it=>objectPosition(p,it)[2]);assert.ok(zs[2]>zs[1]+.004);assert.ok(zs[1]>zs[0]+.004);
  assert.ok(p.data.qpos.every(Number.isFinite));
  for(const state of ['held','free','sleep']){garmentState(p,items[2],state);assert.ok((p.model.flex_contype[items[2].flex]&p.model.flex_conaffinity[items[1].flex])||(p.model.flex_contype[items[1].flex]&p.model.flex_conaffinity[items[2].flex]),'contact must survive every state transition')}
 }finally{c.p?.dispose()}
});
