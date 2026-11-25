import * as THREE from'three';
import{preparePhysics}from'./wasm-client.js';
import{Simulator}from'./Simulator.js';import{robotProfile}from'./robots.js';import{loadRobotAssets}from'./assets.js';import{primitive,loadProps,disposeGroup}from'./view.js';
const slots=['left','right'];
export class CooperativeSimulator extends Simulator{
 lanes(){return slots}offset(){return[0,0,0]}
 resetCamera(){if(!this.camera)return;const far=this.camera.aspect<1?1.3:1;this.camera.position.set(.3+1.8*far,-1.55*far,.08+1.6*far);this.controls?.target.set(.3,0,.08)}
 placeLight(){if(!this.key)return;this.key.position.set(1,-1,2.6);this.key.target.position.set(.35,0,0)}
 setLanguage(lang){super.setLanguage(lang);if(this.fleetBar)this.fleetBar.hidden=true}
 buildBench(c){c.benchTop={visible:true};c.cutoutBench={visible:false};if(c.id==='left')primitive(c.group,{shape:'box',size:[1.35,1.4,.06],position:[.30,0,-.03],color:'#c5c0b4'})}
 async sync(){const version=++this.syncVersion;this.ready=false;this.focus='left';this.coopStats={status:'loading'};this.coopWorker?.terminate();for(const c of [...this.cells.values()])this.dropCell(c);this.trace=[];this.drawTrace();this.publish();
  const worker=this.coopWorker=new Worker(new URL('./cooperative.worker.js',import.meta.url),{type:'module'});
  for(let i=0;i<2;i++){const id=slots[i],c={id,profile:robotProfile(this.pair[i]),stats:{status:'loading'},revision:version,awakeUntil:performance.now()+2000,frame:null,dirty:true,manifest:null,inflight:false,group:new THREE.Group(),worker};this.cells.set(id,c);this.scene.add(c.group);this.buildBench(c)}
  worker.onmessage=({data:m})=>{if(this.disposed||version!==this.syncVersion||m.revision!==version)return;
   if(m.type==='update'){const{trace,...u}=m.update;Object.assign(this.coopStats,u);for(const c of this.cells.values())Object.assign(c.stats,u);if(trace)for(const e of trace){const c=this.cells.get(e.id);if(c)this.addTrace(c,[e])}this.publish();this.drawStatus()}
   if(m.type==='ready'){for(const c of this.cells.values())c.loaded=true;if(this.stopped)this.all({type:'call',method:'stop'});this.checkReady()}
   if(m.type==='manifest'){for(let i=0;i<2;i++){const c=this.cells.get(slots[i]),r=m.manifest.providers[i];c.manifest={...m.manifest,bodies:r.bodies,wristBody:r.wristBody,items:i===0?m.manifest.items:[],scene:{...m.manifest.scene,static:[],items:i===0?m.manifest.items:[]}};c.manifestRevision=version;if(c.robotMeshes)this.buildEnvironment(c,c.manifest)}this.acceptFrame(m.frame);this.checkReady()}
   if(m.type==='frame')this.acceptFrame(m.frame);
   if(m.type==='started'){for(const c of this.cells.values())c.starting=false;this.publish()}
   if(m.type==='error'){for(const c of this.cells.values()){c.stats.status='error';c.inflight=false;c.starting=false}this.coopStats.status='error';this.fail(Error(m.message))}
   if(m.type==='debug'){this.debugCalls.get(m.id)?.resolve(m.value);this.debugCalls.delete(m.id)}
  };
  worker.onerror=e=>this.fail(Error(e.message||'Cooperative physics worker failed'));
  const [module,assets,props]=await Promise.all([preparePhysics(),Promise.all(this.pair.map(loadRobotAssets)),loadProps()]);if(this.disposed||version!==this.syncVersion)return;this.propAssets=props;worker.postMessage({type:'init',revision:version,robots:this.pair,app:this.activeApp.id,module,visible:!document.hidden,speed:this.speed,budget:12});
  for(let i=0;i<2;i++){const c=this.cells.get(slots[i]);this.buildRobot(c,assets[i]);if(c.manifest)this.buildEnvironment(c,c.manifest)}this.resetCamera();this.checkReady();
 }
 acceptFrame(frame){if(!frame)return;for(let i=0;i<2;i++){const c=this.cells.get(slots[i]);if(c){c.frame={...frame,tool:frame.tools[i]};c.dirty=true;c.inflight=false}}}
 post(c,m){if(!this.coopWorker||this.disposed)return;if(m.type==='tick'){for(const c of this.cells.values())c.inflight=true}for(const c of this.cells.values())if(m.type==='call'||m.type==='start')c.awakeUntil=performance.now()+2000;this.coopWorker.postMessage({...m,revision:this.syncVersion})}
 all(m){const c=this.cells.get('left');if(c)this.post(c,m)}
 publish(){this.onUpdate?.({...this.coopStats,ready:this.ready,status:this.stopped?'stopped':!this.ready?'loading':this.coopStats?.status||'idle',cooperative:true,fleet:true,wristVisible:this.wristVisible,loadedCells:[...this.cells.values()].filter(c=>c.loaded&&c.robotMeshes&&c.manifest).length,totalCells:2})}
 setApp(app,force=false){if(!app||this.activeApp?.id===app.id&&!force)return;this.activeApp=app;this.sync().catch(e=>this.fail(e))}
 async setPair(pair){this.pair=pair.map(id=>robotProfile(id).id);this.cancel();await this.sync()}
 async setRobot(id){const c=[...this.cells.values()].find(c=>c.profile.id===id);if(c)this.focus=c.id;this.publish();this.drawStatus();this.onReady?.()}
 async setFleet(){}
 buildEnvironment(c,manifest){super.buildEnvironment(c,manifest);if(c.id!=='left')return;for(const x of this.activeApp.id==='carry'?[.32,.42]:[.37])primitive(c.environment,{shape:'box',size:[.22,this.activeApp.id==='fixture'?.36:.20,.05],position:[x,0,.025],color:'#677880'})}
 drawTrace(){if(!this.traceCols)return;this.traceBox.style.setProperty('--cols',2);const zh=this.lang==='zh';this.traceCols.replaceChildren(...slots.map((id,i)=>{const c=this.cells.get(id),col=document.createElement('div'),h=document.createElement('h4'),ol=document.createElement('ol');col.className='cap-col '+(c?.profile.id||'piper');h.className=c?.stats.status||'loading';h.textContent=`${i?'B':'A'} · ${c?.profile.short||robotProfile(this.pair?.[i]).short} · ${c?.stats.status||'loading'}`;for(const e of this.trace.filter(e=>e.id===id).slice(0,3)){const li=document.createElement('li'),a=document.createElement('span'),b=document.createElement('span'),parts=e.text?[e.cap,e.text]:this.traceText(e);a.className='call';b.className='out';a.textContent=parts[0];b.textContent='→ '+parts[1];li.append(a,b);ol.append(li)}if(!ol.children.length){const li=document.createElement('li');li.textContent=zh?'选择两台机械臂，运行共同任务。':'Choose two arms and run a shared task.';ol.append(li)}col.append(h,ol);return col}))}
 dispose(){super.dispose();this.coopWorker?.terminate()}
}
