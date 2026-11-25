import{loadPhysics}from'./physics.js';import{CooperativeCell}from'./cooperative-cell.js';
import{WorkerClock}from'./worker-clock.js';
let cell,revision=0;const send=m=>postMessage({...m,revision});
const clock=new WorkerClock({advance:(dt,budget)=>{try{cell.advance(dt,budget)}catch(e){cell.cancel();clock.stop();send({type:'error',message:e.message})}},frame:()=>{cell.telemetry();send({type:'frame',frame:cell.frame()})},blocked:()=>!cell?.p||cell.paused||cell.stopped,awake:()=>!!cell?.action||!!cell?.p&&cell.p.data.qvel.some(v=>Math.abs(v)>.04)});
self.onmessage=async({data:m})=>{try{
 if(m.type==='init'){revision=m.revision;const engines=await Promise.all(m.robots.map(id=>loadPhysics(id,m.module)));cell=new CooperativeCell(engines,update=>send({type:'update',update}));cell.speed=m.speed||1;const manifest=cell.setApp(m.app,true);send({type:'manifest',manifest,frame:cell.frame()});clock.configure(m);send({type:'ready'});return}
 if(!cell||m.revision!==revision)return;
 if(m.type==='configure'){if(m.speed!==undefined)cell.speed=m.speed;clock.configure(m);return}
 if(m.type==='start'){const r=cell.start(m.action);if(r)send({type:'manifest',manifest:r.manifest,frame:cell.frame()});send({type:'started',ok:!!r});clock.wake();return}
 if(m.type==='tick'){cell.speed=m.speed;cell.advance(m.dt,m.budget);cell.telemetry();send({type:'frame',frame:cell.frame()});return}
 if(m.type==='call'&&['pause','stop','rearm','reset','cancel','setJoint','setGripper'].includes(m.method)){cell[m.method](...(m.args||[]));if(cell.p){cell.telemetry();send({type:'frame',frame:cell.frame()})}clock.wake(['reset','setJoint','setGripper'].includes(m.method));return}
 if(m.type==='debug'&&import.meta.env.DEV){const value=new Function('cell','return ('+m.src+')(cell)')(cell);send({type:'debug',id:m.id,value:JSON.parse(JSON.stringify(value??null))})}
 }catch(e){cell?.cancel();send({type:'error',message:e.message||String(e)})}
};
