// Physics worker: one robot workcell (cell.js) per worker. The page sends a tick per animation frame with
// the elapsed time; the worker steps MuJoCo, runs the app's jobs and answers with one frame of poses.
import{loadPhysics}from'./physics.js';import{Cell}from'./cell.js';
let cell=null;
const send=(m,transfer=[])=>postMessage(m,transfer);
self.onmessage=async({data:m})=>{
 try{
  if(m.type==='init'){const engine=await loadPhysics(m.robot);cell=new Cell(engine,update=>send({type:'update',update}));send({type:'ready'});return}
  if(!cell)return;
  if(m.type==='setApp'){const manifest=cell.setApp(m.app,m.force,m.seed);if(manifest)send({type:'manifest',manifest,frame:cell.frame()});return}
  if(m.type==='start'){const r=cell.start(m.action,m.seed);if(r?.manifest)send({type:'manifest',manifest:r.manifest,frame:cell.frame()});send({type:'started',ok:!!r});return}
  if(m.type==='tick'){if(!cell.p){send({type:'frame',frame:null});return}cell.speed=m.speed;try{cell.advance(m.dt)}finally{cell.telemetry();const f=cell.frame();send({type:'frame',frame:f},[f.xpos.buffer,f.xquat.buffer,f.flex.buffer,f.qpos.buffer,f.tool.buffer])}return}
  if(m.type==='call'){cell[m.method]?.(...(m.args||[]));return}
  if(m.type==='debug'&&import.meta.env.DEV){let value;try{value=await new Function('cell','return ('+m.src+')(cell)')(cell);value=JSON.parse(JSON.stringify(value??null))}catch(e){value={error:e.message}}send({type:'debug',id:m.id,value});return}
 }catch(e){send({type:'error',message:e?.message||String(e)})}
};
