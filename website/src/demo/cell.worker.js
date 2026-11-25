// Physics worker: one robot workcell (cell.js) per worker. The page sends a tick per animation frame with
// the elapsed time; the worker steps MuJoCo, runs the app's jobs and answers with one frame of poses.
import{loadPhysics}from'./physics.js';import{Cell}from'./cell.js';
import{WorkerClock}from'./worker-clock.js';
let cell=null,revision=0;
const send=(m,transfer=[])=>postMessage({...m,revision},transfer);
const frame=()=>{cell.telemetry();const f=cell.frame();send({type:'frame',frame:f},[f.xpos.buffer,f.xquat.buffer,f.flex.buffer,f.qpos.buffer,f.tool.buffer])};
const clock=new WorkerClock({advance:(dt,budget)=>{try{cell.advance(dt,budget)}catch(e){clock.stop();send({type:'error',message:e.message})}},frame,blocked:()=>!cell?.p||cell.stopped||cell.paused,awake:()=>!!cell?.action||!!cell?.p&&([...cell.p.items.values()].some(i=>i.state==='free')||cell.p.data.qvel.some(v=>Math.abs(v)>.04))});
self.onmessage=async({data:m})=>{
 try{
  if(m.type==='init'){const engine=await loadPhysics(m.robot,m.module);cell=new Cell(engine,update=>send({type:'update',update}));cell.speed=m.speed||1;clock.configure(m);send({type:'ready'});return}
  if(!cell)return;
  if(m.type==='configure'){if(m.speed!==undefined)cell.speed=m.speed;clock.configure(m);return}
  if(m.type==='setApp'){revision=m.revision;const manifest=cell.setApp(m.app,m.force,m.seed);if(manifest)send({type:'manifest',manifest,frame:cell.frame()});clock.wake();return}
  if(m.type==='start'){const r=cell.start(m.action,m.seed);if(r?.manifest)send({type:'manifest',manifest:r.manifest,frame:cell.frame()});send({type:'started',ok:!!r});clock.wake();return}
  if(m.type==='tick'){if(!cell.p){send({type:'frame',frame:null});return}cell.speed=m.speed;try{cell.advance(m.dt,m.budget)}finally{cell.telemetry();const f=cell.frame();send({type:'frame',frame:f},[f.xpos.buffer,f.xquat.buffer,f.flex.buffer,f.qpos.buffer,f.tool.buffer])}return}
  if(m.type==='call'&&['pause','stop','rearm','reset','cancel','setJoint','setGripper'].includes(m.method)){cell[m.method](...(m.args||[]));if(cell.p)frame();clock.wake(['reset','setJoint','setGripper'].includes(m.method));return}
  if(m.type==='debug'&&import.meta.env.DEV){let value;try{value=await new Function('cell','return ('+m.src+')(cell)')(cell);value=JSON.parse(JSON.stringify(value??null))}catch(e){value={error:e.message}}send({type:'debug',id:m.id,value});return}
 }catch(e){send({type:'error',message:e?.message||String(e)})}
};
