// One robot workcell: its MuJoCo model, the running app's jobs, sensing and the capability trace.
// It runs inside a physics worker (cell.worker.js), one per robot, so each arm simulates on its own core
// and the page only draws. Everything the page needs leaves through emit() (status, telemetry, trace)
// and frame() (poses for drawing).
import{createModel,toolPosition,inverseKinematics,objectPosition,attach,detach,moveRigid,garmentState,settleGarments}from'./physics.js';
import{makeScene}from'./scene.js';import{planJobs,jobSteps,resultText,inWasher}from'./planner.js';import{apps}from'./apps/index.js';
const DT=.004,round=(v,n=2)=>+v.toFixed(n);
export class Cell{
 constructor(engine,emit){Object.assign(this,{engine,emit,speed:1,stopped:false,paused:false,counts:{},p:null,action:null,lastNotify:0})}
 setApp(appId,force=false,seed){const app=apps.find(a=>a.id===appId)||apps[0];if(!force&&this.p&&this.app?.id===app.id)return null;this.app=app;this.cancel();
  const previous=this.p;this.p=createModel(this.engine,makeScene(app,seed));previous?.dispose();this.reset();for(let i=0;i<35;i++)this.p.mj.mj_step(this.p.model,this.p.data);
  this.emit({ready:true,robot:this.engine.id,joints:Array.from(this.p.data.qpos).slice(0,this.p.profile.dof)});return this.manifest()}
 // what the page needs to build the scene's visuals once (frames then carry only poses)
 manifest(){const p=this.p,bodies={};for(const[,name]of this.engine.xml.matchAll(/<body[^>]*name="([^"]+)"/g)){const id=p.model.body(name).id;if(id>=0)bodies[name]=id}
  const items=[...p.items.values()].map(({bodies:_,...it})=>it);
  return{robot:this.engine.id,scene:p.scene,items,bodies,wristBody:p.model.body(p.profile.wristBody).id,lid:p.lid??null}}
 frame(){const p=this.p,d=p.data,t=p.tool;return{xpos:Float64Array.from(d.xpos),xquat:Float64Array.from(d.xquat),flex:Float64Array.from(d.flexvert_xpos||[]),qpos:Float64Array.from(d.qpos),
  tool:Float64Array.from([...d.site_xpos.subarray(t*3,t*3+3),...d.site_xmat.subarray(t*9,t*9+9)]),belt:p.belt?p.belt.offset:0}}
 telemetry(){const now=performance.now();if(!this.p||now-this.lastNotify<140)return;this.lastNotify=now;const d=this.p.data,a=this.action;
  this.emit({joints:Array.from(d.qpos).slice(0,this.p.profile.dof),position:toolPosition(this.p),time:d.time,...(a&&a.duration?{status:this.paused?'paused':'running',step:this.currentStep,progress:(a.jobIndex+a.elapsed/a.duration)/a.total}:{}),lidAngle:this.p.lidAdr!==undefined?d.qpos[this.p.lidAdr]:null})}
 reset(){if(!this.p)return;this.cancel();const p=this.p;p.mj.mj_resetData(p.model,p.data);p.data.qpos.set(p.initial);p.data.ctrl.set(p.ctrlInitial);p.data.eq_active.set(p.eqInitial);p.mj.mj_forward(p.model,p.data);
  for(const item of p.items.values()){if(item.garment){item.state=null;garmentState(p,item,'sleep')}item.onBelt=!!p.scene.items.find(x=>x.id===item.id)?.onBelt;delete item.placed;delete item.module;delete item.joined;item.beltX0=item.position[0];item.beltPos=[...item.position]}
  p.belt={offset:0};this.counts={};this.emit({status:this.stopped?'stopped':'idle',progress:0,step:null,result:null,counts:{},vision:null,item:null})}
 cancel(){if(this.p){for(const item of this.p.items.values()){this.p.data.eq_active[item.eq]=0;if(item.state==='held')garmentState(this.p,item,'free')}if(this.p.lidEq!==undefined)this.p.data.eq_active[this.p.lidEq]=0}this.action=null;this.activeItem=null;this.lidHeld=false;this.paused=false}
 // shuffle-type actions rebuild the scene from the shared seed (both robots of a fleet get the same layout)
 start(actionId,seed){if(!this.p||this.stopped)return null;const action=this.app.actions.find(a=>a.id===actionId);if(!action)return null;let manifest=null;
  if(['shuffle','new-batch'].includes(action.id))manifest=this.setApp(this.app.id,true,seed);else this.reset();for(let i=0;i<50;i++)this.p.mj.mj_step(this.p.model,this.p.data);
  if(this.p.scene.id==='teach'&&action.objectStart)moveRigid(this.p,'workpiece',action.objectStart);this.p.mj.mj_forward(this.p.model,this.p.data);
  this.action={definition:action,jobs:planJobs(this.p,action),jobIndex:0,elapsed:0,total:0};this.action.total=this.action.jobs.length;this.counts={};this.paused=false;
  this.emit({status:'running',progress:0,result:null,counts:{},total:this.action.jobs.filter(x=>x.type==='transfer'&&x.category!=='basket').length});
  try{this.nextJob()}catch(e){this.cancel();throw e}return{manifest}}
 nextJob(){const a=this.action;if(!a)return;if(a.jobIndex>=a.jobs.length){this.action=null;this.activeItem=null;this.emit({status:'complete',progress:1,step:null,result:a.definition.result||resultText(this.p,this.counts),counts:{...this.counts},vision:null,item:null});return}
  const job=a.jobs[a.jobIndex];a.job=job;a.elapsed=0;a.si=-1;a.events=new Set();
  if(job.type==='index'||job.type==='outfeed'){const p=this.p,offset=-p.data.ctrl[p.beltAct];a.start=offset;a.target=job.type==='index'?offset+(objectPosition(p,job.id)[0]-job.dest[0]):offset+job.distance;a.duration=Math.max(.8,Math.abs(a.target-a.start)/.16);
   this.emit({step:{en:job.type==='index'?'Index conveyor to the wrist inspection station':'Convey the assembled module out',zh:job.type==='index'?'步进输送到腕部检查工位':'沿输送线送出装配模块'},vision:null,trace:[{cap:'Conveyor.Index',target:[round(a.target-a.start,3)]}]});return}
  let seed=Array.from(this.p.data.qpos).slice(0,this.p.profile.home.length),time=0;const planned=jobSteps(this.p,job);if(!planned.length){a.jobIndex++;this.emit({progress:a.jobIndex/a.total});return this.nextJob()}
  // a pose the arm can't reach with the fingers at the requested angle (a long hand close to its own base)
  // is tried again with the wrist free to turn, before the job gives up
  a.steps=planned.map(s=>{let solved=inverseKinematics(this.p,s.target,seed,s.yaw);if(solved.error>.026&&s.yaw!==undefined){const free=inverseKinematics(this.p,s.target,seed);if(free.error<solved.error){solved=free;s={...s,yaw:undefined,freeYaw:true}}}if(solved.error>.026)throw Error(`Unreachable task point (${this.p.profile.short}, ${(solved.error*1000).toFixed(0)} mm at ${s.target.map(v=>v.toFixed(3)).join(',')})`);const st={...s,from:[...seed],to:solved.q,start:time,end:time+s.duration};time=st.end;seed=[...solved.q];return st});a.duration=time}
 pause(){if(!this.action)return;this.paused=!this.paused;this.emit({status:this.paused?'paused':'running'})}
 stop(){this.cancel();this.stopped=true;if(this.p){this.p.data.ctrl.set(Array.from(this.p.data.qpos).slice(0,this.p.profile.dof+1));this.p.data.qvel.fill(0)}this.emit({status:'stopped',step:null,result:null,progress:0})}
 rearm(){this.stopped=false;this.emit({status:'idle',progress:0,result:null})}
 setJoint(i,angle){if(this.p&&!this.action&&!this.stopped)this.p.data.ctrl[i]=angle}
 setGripper(closed){if(this.p&&!this.action&&!this.stopped)this.p.data.ctrl[this.p.profile.gripper]=closed?0:this.p.profile.open}
 // The capability trace: what the app asked for and what this robot's provider turned it into.
 traceStep(s,prev){const out=[],dof=this.p.profile.dof;
  if(s.grip&&!prev?.grip)out.push({cap:'Gripper.Grip',stop:s.closeTo?round(s.closeTo*1000,1):0,cmd:this.p.profile.gripper});
  if(!s.grip&&prev?.grip)out.push({cap:'Gripper.Open',width:round((s.openTo??this.p.profile.open)*1000,0)});
  const same=prev&&prev.target.every((v,i)=>Math.abs(v-s.target[i])<1e-4)&&prev.yaw===s.yaw;
  if(!same)out.push({cap:'Manipulator.MoveToPose',target:s.target.map(v=>round(v*1000,0)),yaw:s.yaw===undefined?null:round((s.yaw*180/Math.PI+540)%360-180,0),q:s.to.slice(0,dof).map(v=>round(v,2))});
  if(out.length)this.emit({trace:out})}
 event(s){const p=this.p;if(s.event==='grasp'){attach(p,s.item,toolPosition(p));this.activeItem=s.item;p.items.get(s.item).onBelt=false}
  if(s.event==='grip-check'){const f=p.touch.map(a=>a<0?0:p.data.sensordata[a]);const a=this.action,opening=Math.abs(p.data.qpos[p.profile.gripper]),ok=!(Math.min(...f)<(s.soft?.12:.3)||opening<.0012||(s.expect!==undefined&&Math.abs(opening-s.expect)>.007));
   this.emit({grip:f,opening,trace:[{cap:'Touch.Read',touch:f.map(v=>round(v,1)),ok,slip:!!s.slip}]});
   if(!ok){a.retries=(a.retries||0)+1;const n=f.map(v=>v.toFixed(1)).join(' / ');this.emit({step:s.slip?{en:`Slipped (touch ${n} N) · grasp again`,zh:`布料滑脱（触觉 ${n} N）· 重新抓取`}:{en:`No grip (touch ${n} N) · look again`,zh:`未夹住（触觉 ${n} N）· 重新观察`}});a.replan=a.retries<=2?'retry':'skip'}}
  if(s.event==='release'){detach(p,s.item);this.activeItem=null;const it=p.items.get(s.item);it.placed=this.action.job.category;
   // the garment now lies on the drum floor or the pile: the opening fingers draw out of it instead of hooking it
   garmentState(p,it,'free')}
  if(s.event==='lid-grip'){p.model.eq_data.fill(0,p.lidEq*p.mj.mjNEQDATA+3,p.lidEq*p.mj.mjNEQDATA+6);p.data.eq_active[p.lidEq]=1;this.lidHeld=true}
  if(s.event==='lid-release'){this.lidHeld=false;if(p.data.qpos[p.lidAdr]<1.0)throw Error('The gripper did not open the washer lid')}}
 finishJob(){const a=this.action,job=a.job,p=this.p;if(job.type==='transfer'){
   const item=p.items.get(job.id),pos=objectPosition(p,item),distance=job.category==='washer'?(inWasher(p,item)?0:1):job.carrier?(()=>{const cp=objectPosition(p,job.carrier);return Math.hypot(pos[0]-cp[0],pos[1]-cp[1])<.02&&Math.abs(pos[2]-cp[2])<.02?0:1})():Math.hypot(pos[0]-job.dest[0],pos[1]-job.dest[1]);
   if(!Number.isFinite(distance)||distance>.115){this.counts.missed=(this.counts.missed||0)+1;
    // laundry: the wrist camera sees the garment didn't land where it should, so the app queues one more pick (at most three)
    if(job.pick&&(a.extra||0)<3){a.extra=(a.extra||0)+1;a.jobs.splice(a.jobIndex+1,0,{...job,id:undefined,tries:0});a.total++}}
   else this.counts[job.category]=(this.counts[job.category]||0)+1;a.retries=0;garmentState(p,item,'free')}
  a.jobIndex++;this.emit({counts:{...this.counts},progress:a.jobIndex/a.total});this.nextJob()}
 advance(dt){const p=this.p;if(!p||this.stopped||this.paused)return;try{this.substeps(Math.max(1,Math.round(dt*this.speed/DT)))}catch(e){this.cancel();this.emit({status:'error'});throw e}}
 substeps(n){const p=this.p;
  for(let k=0;k<n;k++){
   const a=this.action;if(a){a.elapsed+=DT;const job=a.job;if(job.type==='index'||job.type==='outfeed'){let x=Math.min(1,a.elapsed/a.duration);x=x*x*(3-2*x);p.data.ctrl[p.beltAct]=-(a.start+(a.target-a.start)*x);
   }else{const index=a.steps.findIndex(s=>a.elapsed<=s.end),i=index<0?a.steps.length-1:index,s=a.steps[i];if(i!==a.si){a.si=i;this.traceStep(s,a.steps[i-1])}
    let x=Math.max(0,Math.min(1,(a.elapsed-s.start)/s.duration));x=x*x*(3-2*x);for(let j=0;j<p.profile.dof;j++)p.data.ctrl[j]=s.from[j]+(s.to[j]-s.from[j])*x;const shut=s.closeTo??0;p.data.ctrl[p.profile.gripper]=s.grip?(s.ramp?shut+(p.profile.open-shut)*(1-x):shut):(s.openTo??p.profile.open);for(const id of p.adhere||[])p.data.ctrl[id]=s.adhere?1:0;
    if(a.elapsed>s.start+s.duration*.82&&!a.events.has(s.start)){a.events.add(s.start);if(s.vision)this.emit({vision:s.vision,item:s.item||null});if(s.event)this.event(s);if(a.replan){const r=a.replan;a.replan=null;if(r==='skip'){this.counts.missed=(this.counts.missed||0)+1;a.retries=0;a.jobIndex++}this.nextJob();break}}
    this.currentStep=s.label}}
   if(this.activeItem||this.lidHeld){const pos=toolPosition(p);p.data.mocap_pos.set([pos[0],pos[1],pos[2]-.012],p.mocap*3)}
   if(p.beltAdr!==undefined)p.belt={offset:-p.data.qpos[p.beltAdr]};
   p.mj.mj_step(p.model,p.data);
   if(a&&a.elapsed>=a.duration)this.finishJob()}
  settleGarments(p,n*DT)}
}
