import{robotProfile}from'./robots.js';
const section=(s,tag)=>{const a=s.indexOf('<'+tag),b=s.lastIndexOf('</'+tag+'>');return a<0?'':s.slice(s.indexOf('>',a)+1,b)};
const ns=(s,prefix)=>s.replace(/\b(name|class|childclass|mesh|material|joint|body|site|joint1|joint2|body1|body2|objname|tendon|target)="([^"]+)"/g,(_,key,value)=>`${key}="${prefix}_${value}"`).replace(/\bfile="([^"]+)"/g,(_,value)=>`file="${prefix}_${value}"`);
const v=a=>a.join(' '),box=(name,size,pos,color='.4 .5 .6 1',mass='.003')=>`<geom name="${name}" type="box" size="${v(size)}" pos="${v(pos)}" rgba="${color}" mass="${mass}" friction="1.4 .02 .002" solref=".008 1"/>`;
const support=(x,depth=.10)=>box('support_'+x,[.11,depth,.025],[x,0,.025],'.4 .47 .5 1','1');
export function createCooperativeModel(engines,app,reverse=false){
 const mj=engines[0].mj,vfs=new mj.MjVFS(),slots=['left','right'],bases=[[0,-.28,0],[0,.28,0]],parts={default:[],asset:[],worldbody:[],actuator:[],equality:[],contact:[],sensor:[]},providers=[];
 for(let i=0;i<2;i++){
  const e=engines[i],id=slots[i],xml=e.xml;
  Object.entries(e.meshes).forEach(([name,bytes])=>vfs.addBuffer(id+'_'+name,bytes));
  parts.default.push(`<default class="${id}_root">${ns(section(xml,'default'),id)}</default>`);
  for(const tag of['asset','actuator','equality','contact','sensor'])parts[tag].push(ns(section(xml,tag),id));
  parts.worldbody.push(`<body name="mount_${id}" pos="${v(bases[i])}" childclass="${id}_root">${ns(section(xml,'worldbody'),id)}</body>`);
 }
 const start=app.id==='carry'?(reverse?.42:.32):.37,end=reverse?.32:.42;
 const items=[];
 if(app.id==='carry'){
  parts.worldbody.push(support(.32),support(.42));
  const shape=box('tray_floor',[.10,.14,.007],[0,0,0],'.52 .65 .69 1','.07')+[-1,1].map(sign=>`<geom name="handle_${sign}" type="capsule" fromto="-.040 ${sign*.15} .021 .040 ${sign*.15} .021" size=".008" mass=".012" friction="2 .03 .002" solref=".004 1" rgba=".85 .65 .29 1"/>`).join('');
  parts.worldbody.push(`<body name="tray" pos="${start} 0 .060"><freejoint name="free_tray"/>${shape}</body>`);items.push({id:'tray',kind:'tray',size:[.20,.28,.014],color:'#85a7b0'});
 }else{
  const sign=reverse?1:-1;
  parts.worldbody.push(support(.37,.18));
  const shell=box('housing_floor',[.10,.18,.006],[0,0,0],'.45 .55 .7 1','.12')+[-1,1].map(sign=>box('wallx_'+sign,[.012,.062,.024],[sign*.054,0,.030])+box('wally_'+sign,[.042,.010,.024],[0,sign*.052,.030])).join('');
  const handles=[-1,1].map(sign=>box('handle_neck_'+sign,[.023,.067,.006],[-.040,sign*.093,.012],'.45 .55 .7 1','.002')+[-.10,.02].map(x=>box('handle_post_'+sign+'_'+x,[.004,.004,.026],[x,sign*.15,.040],'.45 .55 .7 1','.001')).join('')+`<geom name="fixture_handle_${sign}" type="capsule" fromto="-.10 ${sign*.15} .065 .02 ${sign*.15} .065" size=".008" mass=".002" condim="6" friction="2 .02 .002" rgba=".88 .65 .27 1"/>`).join('');
  parts.worldbody.push(`<body name="housing" pos=".37 0 .060"><freejoint name="free_housing"/>${shell+handles}</body>`,`<body name="insert" pos=".37 ${-sign*.30} .023"><freejoint name="free_insert"/>${box('insert_geom',[.019,.018,.023],[0,0,0],'.82 .49 .31 1','.025')}</body>`);
  items.push({id:'housing',kind:'fixture',size:[.114,.104,.060],color:'#748dae'},{id:'insert',kind:'cube',size:[.038,.036,.046],color:'#d17e50'});
 }
 parts.worldbody.unshift(box('bench',[.675,.7,.03],[.30,0,-.03],'.76 .74 .70 1','10'));
 const xml=`<mujoco model="cooperative"><compiler angle="radian" autolimits="true"/><option timestep=".004" iterations="40" integrator="implicitfast" cone="elliptic" impratio="10"/><default>${parts.default.join('')}</default><asset>${parts.asset.join('')}</asset><worldbody>${parts.worldbody.join('')}</worldbody><actuator>${parts.actuator.join('')}</actuator><equality>${parts.equality.join('')}</equality><contact>${parts.contact.join('')}</contact><sensor>${parts.sensor.join('')}</sensor></mujoco>`;
 let model;try{model=mj.MjModel.from_xml_string(xml,vfs)}finally{vfs.delete()}
 const data=new mj.MjData(model),named=(tag,name)=>mj.mj_name2id(model,mj.mjtObj['mjOBJ_'+tag].value,name);
 for(let i=0;i<2;i++){
  const e=engines[i],id=slots[i],profile=robotProfile(e.id),stripped=e.xml.replace(/<mesh\b[^>]*\/>/g,'').replace(/<geom\b[^>]*type="mesh"[^>]*\/>/g,'').replace(/<geom\b[^>]*mesh="[^"]+"[^>]*\/>/g,''),km=mj.MjModel.from_xml_string(stripped),ghost=new mj.MjData(km);
  const qadr=[],act=[];for(let j=0;j<km.njnt;j++){const name=mj.mj_id2name(km,mj.mjtObj.mjOBJ_JOINT.value,j);qadr.push(model.jnt_qposadr[named('JOINT',id+'_'+name)])}
  for(let j=0;j<profile.dof+1;j++){const name=mj.mj_id2name(km,mj.mjtObj.mjOBJ_ACTUATOR.value,j);act.push(named('ACTUATOR',id+'_'+name))}
  const bodies={};for(const [,name] of e.xml.matchAll(/<body[^>]*name="([^"]+)"/g))bodies[name]=named('BODY',id+'_'+name);
  const provider={id,profile,base:bases[i],qadr,act,bodies,mj,kin:{model:km,ghost,tool:mj.mj_name2id(km,mj.mjtObj.mjOBJ_SITE.value,'tool')},tool:named('SITE',id+'_tool'),touch:['l','r'].map(s=>model.sensor_adr[named('SENSOR',id+'_touch_'+s)]),wristBody:named('BODY',id+'_'+profile.wristBody)};
  profile.home.forEach((q,j)=>data.qpos[qadr[j]]=q);act.forEach((a,j)=>data.ctrl[a]=profile.home[j]);providers.push(provider);
 }
 for(const item of items){const j=named('JOINT','free_'+item.id);item.qadr=model.jnt_qposadr[j];item.body=named('BODY',item.id)}
 mj.mj_forward(model,data);
 return{mj,model,data,providers,items,app,start,end,reverse,initial:Float64Array.from(data.qpos),controls:Float64Array.from(data.ctrl),xml,dispose(){data.delete();model.delete();for(const p of providers){p.kin.ghost.delete();p.kin.model.delete()}}};
}
