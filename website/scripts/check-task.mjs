import assert from 'node:assert/strict';
import {engine} from './physics-fixture.mjs';
import {Cell} from '../src/demo/cell.js';
import {selfContacts} from '../src/demo/kinematics.js';
const [robot='panda',app='kitting',action='sort',seed='42']=process.argv.slice(2);
const events=[],cell=new Cell(engine(robot,{physicsOnly:process.env.PHYSICS_ONLY==='1'}),e=>events.push(e));
try {
 cell.setApp(app,true,+seed);cell.start(action,+seed);
 let steps=0,last=-1,worst=0;
 while(cell.action&&steps<140000){
  cell.substeps(25);steps+=25;
  if(robot==='panda')worst=Math.max(worst,selfContacts(cell.p,Array.from(cell.p.data.qpos).slice(0,cell.p.profile.home.length),-.001));
  if(cell.action?.jobIndex!==last){last=cell.action?.jobIndex;console.log(JSON.stringify({robot,app,job:last,counts:cell.counts,sim:cell.p.data.time}));}
 }
 assert.equal(cell.action,null,'task did not complete');
 if(app==='laundry')assert.equal(cell.counts.washer,action==='all'?6:3);
 if(app==='kitting')assert.equal(cell.counts.blocks+cell.counts.cars+cell.counts.plush,13);
 if(app==='quality')assert.equal(cell.counts.pass+cell.counts.rework,6);
 if(app==='assembly')assert.equal(cell.counts.assembled,6);
 if(robot==='panda')assert.equal(worst,0,'Panda self-intersection during the physical replay');
 console.log(JSON.stringify({robot,app,counts:cell.counts,steps,worstSelfContacts:worst,status:'PASS'}));
} finally {cell.p?.dispose()}
