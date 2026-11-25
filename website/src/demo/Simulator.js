import * as THREE from'three';import{OrbitControls}from'three/examples/jsm/controls/OrbitControls.js';import{STLLoader}from'three/examples/jsm/loaders/STLLoader.js';import{OBJLoader}from'three/examples/jsm/loaders/OBJLoader.js';
import{fleetState,fleetPauseIntent}from'./runtime.js';
import{robotProfile}from'./robots';import{loadRobotAssets}from'./assets';import{GarmentSurface}from'./cloth';import{washerLid,primitive,softMesh,toyMesh,plushDetail,loadProps,propsView,disposeGroup}from'./view';
// The page side of the demo. Each robot's workcell (physics, planning, sensing) runs in its own worker
// (cell.worker.js); this class only draws what the workers report. One arm, or the fleet: PiPER and Panda
// side by side running the same installed app, with a trace of the capability calls each robot's provider
// turns into its own joint commands.
const LANE={piper:[0,0,0],panda:[0,1.1,0],ur5e:[1.45,0,0],gen3:[1.45,1.1,0]},FLEET=['piper','panda','ur5e','gen3'],TRACE_ROWS=3;
const newSeed=()=>Math.floor(Math.random()*0xffffffff);
const STATUS={loading:['Loading','加载中'],idle:['Ready','就绪'],running:['Running','运行中'],paused:['Paused','已暂停'],stopped:['Stop latched','停止已锁存'],complete:['Finished','已完成'],error:['Error','错误']};
export class Simulator{
 constructor(container,{traceContainer,onUpdate,onReady,onError,lang='en',robot='piper',app,pair=['piper','panda']}){Object.assign(this,{container,traceContainer:traceContainer||container.parentElement,onUpdate,onReady,onError,lang,focus:robotProfile(robot).id,activeApp:app,pair});if(import.meta.env.DEV)window.__nervusSim=this;
  this.disposed=false;this.ready=false;this.stopped=false;this.speed=1;this.cells=new Map();this.seed=newSeed();this.fleet=true;this.wristVisible=false;this.trace=[];this.debugCalls=new Map();this.syncVersion=0;this.dirty=true;this.buildView();this.sync().catch(e=>this.fail(e))}
 get robotId(){return this.focus}
 fail(e){if(this.disposed)return;this.onError(e);this.onUpdate({status:'error'})}
 // ------------------------------------------------------------------ view
 buildView(){this.scene=new THREE.Scene();this.scene.background=new THREE.Color('#eeeee9');this.scene.fog=new THREE.Fog('#eeeee9',3.6,9);this.camera=new THREE.PerspectiveCamera(39,1,.01,15);this.camera.up.set(0,0,1);
  this.renderer=new THREE.WebGLRenderer({antialias:true});this.renderer.setPixelRatio(Math.min(devicePixelRatio,1.6));this.renderer.shadowMap.enabled=true;this.renderer.shadowMap.type=THREE.PCFSoftShadowMap;this.renderer.toneMapping=THREE.ACESFilmicToneMapping;this.container.appendChild(this.renderer.domElement);this.renderer.domElement.setAttribute('aria-label','Interactive robot task scene');
  this.controls=new OrbitControls(this.camera,this.renderer.domElement);this.controls.addEventListener('start',()=>{this.interacting=true;this.dirty=true});this.controls.addEventListener('end',()=>{this.interacting=false});this.controls.enableDamping=true;this.controls.minDistance=.50;this.controls.maxDistance=6.5;this.controls.maxPolarAngle=Math.PI*.85;this.resetCamera();
  this.scene.add(new THREE.HemisphereLight('#ffffff','#949c92',2.7));const key=this.key=new THREE.DirectionalLight('#fffdf7',3.1);key.castShadow=true;key.shadow.bias=-.00025;this.scene.add(key,key.target);const fill=new THREE.DirectionalLight('#c2d8ff',.9);fill.position.set(-1,1,1.2);this.scene.add(fill);this.placeLight();
  primitive(this.scene,{shape:'box',size:[9,9,.02],position:[0,.6,-.79],color:'#e3e2dc'});const grid=new THREE.GridHelper(9,90,'#c9c9bf','#d6d6ce');grid.rotation.x=Math.PI/2;grid.position.set(0,.6,-.778);this.scene.add(grid);
  this.wristCamera=new THREE.PerspectiveCamera(78,1.5,.008,3);this.wristCamera.up.set(0,1,0);this.wristRenderer=new THREE.WebGLRenderer({antialias:false});this.wristRenderer.setPixelRatio(1);this.wristRenderer.setSize(216,144);this.wristRenderer.toneMapping=THREE.ACESFilmicToneMapping;this.wristBox=document.createElement('div');this.wristBox.className='wrist-camera';this.wristBox.hidden=!this.wristVisible;this.wristTitle=document.createElement('span');this.wristBox.append(this.wristTitle,this.wristRenderer.domElement);this.wristRenderer.domElement.setAttribute('aria-label','Simulated wrist camera view');this.container.append(this.wristBox);
  // View controls stay over the scene; capability calls have their own panel below it.
  this.fleetBar=document.createElement('div');this.fleetBar.className='fleet-bar';this.fleetBar.setAttribute('role','group');this.fleetButtons=['one','fleet'].map(mode=>{const b=document.createElement('button');b.type='button';b.onclick=()=>this.setFleet(mode==='fleet');this.fleetBar.append(b);return b});
  this.traceBox=document.createElement('div');this.traceBox.className='cap-trace';this.traceHead=document.createElement('header');this.traceCols=document.createElement('div');this.traceCols.className='cap-cols';this.traceBox.append(this.traceHead,this.traceCols);this.container.append(this.fleetBar);this.traceContainer.append(this.traceBox);
  this.setLanguage(this.lang);this.resizeObserver=new ResizeObserver(()=>this.resize());this.resizeObserver.observe(this.container);this.resize();this.last=performance.now();this.tick=this.tick.bind(this);this.raf=requestAnimationFrame(this.tick)}
 lanes(){return this.fleet?FLEET:[this.focus]}
 offset(id){return this.fleet?LANE[id]:[0,0,0]}
 placeLight(){const c=this.fleet?[.96,.55,0]:[.24,0,0],r=this.fleet?2.5:1.4;this.key.position.set(c[0]+.5,c[1]-.9,2.5);this.key.target.position.set(...c);const s=this.key.shadow;s.mapSize.set(this.fleet?2048:1024,this.fleet?2048:1024);Object.assign(s.camera,{left:-r,right:r,top:r,bottom:-r});s.camera.updateProjectionMatrix();s.map?.dispose();s.map=null}
 resetCamera(){if(!this.camera)return;if(this.fleet){const far=this.camera.aspect<1?1.55:1;this.camera.position.set(.96+3.05*far,.55-1.95*far,.1+2.4*far);this.controls?.target.set(.96,.55,.1)}else{const panda=this.focus==='panda';this.camera.position.set(...(panda?[1.45,-1.68,1.22]:[1.32,-1.52,1.08]));this.controls?.target.set(.25,0,panda?.16:.04)}}
 resize(){const oldAspect=this.camera.aspect,r=this.container.getBoundingClientRect();if(r.width<1||r.height<1)return;this.renderer.setSize(r.width,r.height);this.camera.aspect=r.width/r.height;this.camera.updateProjectionMatrix();if((oldAspect<1)!==(this.camera.aspect<1))this.resetCamera();this.dirty=true}
 setLanguage(lang){this.lang=lang;const zh=lang==='zh';if(this.wristTitle)this.wristTitle.textContent=zh?'腕部摄像头 · 模拟':'WRIST CAMERA · SIMULATED';
  if(this.fleetButtons){this.fleetButtons[0].textContent=zh?'单臂':'One arm';this.fleetButtons[1].textContent=zh?'机群 · 四种机械臂':'Fleet · 4 arms';this.fleetBar.setAttribute('aria-label',zh?'显示单臂或机群':'Show one arm or the fleet');this.fleetButtons.forEach((b,i)=>b.setAttribute('aria-pressed',String(this.fleet===(i===1))))}
  if(this.traceHead){this.traceHead.innerHTML=`<b>${zh?'能力调用追踪':'CAPABILITY TRACE'}</b><small>${zh?'同一应用调用 → 各机器人自己的关节指令':'same app call → each robot’s own joint commands'}</small>`;this.drawTrace()}
  for(const c of this.cells.values())c.environment?.traverse(o=>{if(o.userData.labels)this.drawLabel(o)});this.dirty=true;this.wristDirty=true}
 // ------------------------------------------------------------------ cells (one worker per robot)
 async sync(){const version=++this.syncVersion,want=this.lanes();this.ready=false;this.publish();
  for(const[id,c]of this.cells)if(!want.includes(id))this.dropCell(c);
  for(const id of want){if(this.disposed||version!==this.syncVersion)return;const c=this.cells.get(id);if(c){c.group.position.set(...this.offset(id))}else await this.addCell(id)}
  if(this.disposed||version!==this.syncVersion)return;this.placeLight();this.resetCamera();this.checkReady()}
 async addCell(id){const c={id,profile:robotProfile(id),stats:{status:'loading'},revision:0,awakeUntil:0,frame:null,dirty:true,manifest:null,inflight:false,acc:0,group:new THREE.Group(),worker:new Worker(new URL('./cell.worker.js',import.meta.url),{type:'module'})};this.cells.set(id,c);this.ready=false;
  c.group.position.set(...this.offset(id));this.scene.add(c.group);this.buildBench(c);
  c.worker.onmessage=e=>this.message(c,e.data);c.worker.onerror=e=>{c.stats.status='error';this.fail(Error(`${c.profile.short}: ${e.message||'physics worker failed'}`))};c.worker.postMessage({type:'init',robot:id});
  const[assets,props]=await Promise.all([loadRobotAssets(id),loadProps()]);if(this.disposed||this.cells.get(id)!==c)return;this.propAssets=props;this.buildRobot(c,assets);if(c.manifest)this.buildEnvironment(c,c.manifest)}
 dropCell(c){c.worker.terminate();this.scene.remove(c.group);disposeGroup(c.group);this.cells.delete(c.id);for(const[id,d]of this.debugCalls)if(d.robot===c.id){d.resolve(null);this.debugCalls.delete(id)}}
 post(c,m){if(m.type==='call'||m.type==='setApp'||m.type==='start')c.awakeUntil=performance.now()+2000;if(!this.disposed&&this.cells.get(c.id)===c)c.worker.postMessage({...m,revision:c.revision})}
 all(m){for(const c of this.cells.values())this.post(c,m)}
 message(c,m){if(this.disposed||this.cells.get(c.id)!==c)return;if(m.type!=='ready'&&m.type!=='debug'&&m.revision!==c.revision)return;
  if(m.type==='ready'){c.loaded=true;if(this.activeApp)this.post(c,{type:'setApp',app:this.activeApp.id,force:true,seed:this.seed});if(this.stopped)this.post(c,{type:'call',method:'stop'});if(!this.activeApp)this.checkReady();return}
  if(m.type==='manifest'){c.manifest=m.manifest;c.manifestRevision=m.revision;c.frame=m.frame;c.dirty=true;c.inflight=false;c.acc=0;if(c.robotMeshes)this.buildEnvironment(c,m.manifest);this.checkReady();return}
  if(m.type==='started'){c.starting=false;if(!m.ok)c.stats.status='idle';this.publish();return}
  if(m.type==='frame'){if(m.frame){c.frame=m.frame;c.dirty=true}c.inflight=false;return}
  if(m.type==='update'){const{trace,...u}=m.update;Object.assign(c.stats,u);if(trace)this.addTrace(c,trace);if(Object.keys(u).length)this.publish();if(u.status)this.drawStatus();return}
  if(m.type==='error'){c.starting=false;c.stats.status='error';c.inflight=false;this.fail(Error(this.fleet?`${c.profile.short}: ${m.message}`:m.message));return}
  if(m.type==='debug'){this.debugCalls.get(m.id)?.resolve(m.value);this.debugCalls.delete(m.id)}}
 publish(){this.onUpdate({...fleetState(this.lanes().map(id=>this.cells.get(id)).filter(Boolean).map(c=>c.starting?{...c,stats:{...c.stats,status:'running'}}:c),this.focus,{ready:this.ready,stopped:this.stopped}),robot:this.focus,fleet:this.fleet,wristVisible:this.wristVisible})}
 checkReady(){const cells=this.lanes().map(id=>this.cells.get(id));const ok=cells.every(c=>c&&c.loaded&&c.robotMeshes&&((c.manifest&&c.manifestRevision===c.revision)||!this.activeApp));if(ok&&!this.ready){this.ready=true;this.publish();this.dirty=true;this.drawStatus();this.onReady?.()}}
 // ------------------------------------------------------------------ public controls (the desktop app calls these)
 setApp(app,force=false){if(!app)return;const same=this.activeApp?.id===app.id;this.activeApp=app;if(same&&!force)return;this.ready=false;this.seed=newSeed();this.trace=[];this.drawTrace();for(const c of this.cells.values()){c.revision++;c.starting=false;c.stats={status:'loading'};if(c.loaded)this.post(c,{type:'setApp',app:app.id,force,seed:this.seed})}this.publish()}
 start(action){if(!this.activeApp?.actions.some(a=>a.id===action?.id)||!this.ready||this.stopped||[...this.cells.values()].some(c=>['running','paused'].includes(c.stats.status)))return false;this.trace=[];this.drawTrace();for(const c of this.cells.values()){c.starting=true;c.stats.status='running'}this.publish();this.all({type:'start',action:action.id,seed:newSeed()});return true}
 pause(){this.all({type:'call',method:'pause',args:[fleetPauseIntent([...this.cells.values()])]})}
 stop(){this.stopped=true;this.publish();this.all({type:'call',method:'stop'})}
 rearm(){this.stopped=false;this.all({type:'call',method:'rearm'})}
 reset(){this.all({type:'call',method:'reset'})}
 cancel(){this.all({type:'call',method:'cancel'})}
 setJoint(i,angle){const c=this.cells.get(this.focus);if(c)this.post(c,{type:'call',method:'setJoint',args:[i,angle]})}
 setGripper(closed){const c=this.cells.get(this.focus);if(c)this.post(c,{type:'call',method:'setGripper',args:[closed]})}
 // Settings -> robot: in the fleet both arms are already running, so this only moves the focus
 // (wrist camera, arm control, telemetry); with one arm it swaps the workcell.
 async setRobot(id){this.wristDirty=true;id=robotProfile(id).id;if(id===this.focus&&this.ready){this.onReady?.();return}this.focus=id;
  if(this.fleet&&this.cells.has(id)){this.resetCamera();this.publish();this.drawStatus();if(this.ready)this.onReady?.();return}
  this.ready=false;try{await this.sync()}catch(e){this.fail(e)}}
 setWristVisible(on){this.wristVisible=!!on;this.wristBox.hidden=!this.wristVisible;this.wristDirty=true;this.publish()}
 async setFleet(on){if(on===this.fleet)return;this.fleet=on;this.setWristVisible(!on);this.setLanguage(this.lang);this.cancel();this.trace=[];this.drawTrace();this.ready=false;
  for(const[id,c]of this.cells)if(this.lanes().includes(id)&&this.activeApp){c.revision++;c.starting=false;c.stats={status:'loading'};if(c.loaded)this.post(c,{type:'setApp',app:this.activeApp.id,force:true,seed:this.seed})}
  try{await this.sync()}catch(e){this.fail(e)}}
 debug(robot,fn){if(!import.meta.env.DEV)return Promise.resolve(null);const c=this.cells.get(robot||this.focus);if(!c)return Promise.resolve(null);const id=Math.random();return new Promise(r=>{this.debugCalls.set(id,{robot:c.id,resolve:r});this.post(c,{type:'debug',id,src:String(fn)})})}
 // ------------------------------------------------------------------ capability trace
 addTrace(c,entries){for(const e of entries)this.trace.unshift({robot:c.profile.short,id:c.id,...e});this.trace.length=Math.min(this.trace.length,40);if(!this.traceTimer)this.traceTimer=setTimeout(()=>{this.traceTimer=null;this.drawTrace()},120)}
 traceText(e){const n=v=>v.toFixed(2).replace('-','−');switch(e.cap){
  case'Manipulator.MoveToPose':return[`MoveToPose(${e.target.join(', ')} mm${e.yaw===null?'':`, ${e.yaw}°`})`,`q[${e.q.length}] = ${e.q.map(n).join(' ')}`];
  case'Gripper.Grip':return[`Gripper.Grip(${e.stop?`stop ${e.stop} mm`:'force-limited'})`,`${e.robot==='PiPER'?'joint7/8':'parallel-finger joints'} → ${e.stop?e.stop/1000:0} m`];
  case'Gripper.Open':return[`Gripper.Open(${e.width} mm)`,`${e.robot==='PiPER'?'joint7/8':'parallel-finger joints'} → ${e.width/1000} m`];
  case'Touch.Read':return[`Touch.Read${e.slip?' (lift)':''}`,`${e.touch.join(' / ')} N ${e.ok?'✓':'✗ re-grasp'}`];
  case'Conveyor.Index':return[`Conveyor.Index(${Math.round(e.target[0]*1000)} mm)`,'belt_drive'];default:return[e.cap,'']}}
 // one column per robot: its status, then its latest calls (the same app call, side by side)
 drawTrace(){if(!this.traceCols)return;const zh=this.lang==='zh'?1:0;this.traceBox.style.setProperty('--cols',Math.min(this.lanes().length,2));
  this.traceCols.replaceChildren(...this.lanes().map(id=>{const c=this.cells.get(id),col=document.createElement('div'),h=document.createElement('h4'),ol=document.createElement('ol');col.className=`cap-col ${id}`;
   const st=c?.stats.status||'loading';h.className=st+(id===this.focus&&this.fleet?' focus':'');h.textContent=`${robotProfile(id).short} · ${robotProfile(id).dof} joints · ${(STATUS[st]||STATUS.idle)[zh]}`;
   const rows=this.trace.filter(e=>e.id===id).slice(0,TRACE_ROWS);
   for(const e of rows){const li=document.createElement('li'),[call,out]=this.traceText(e),a=document.createElement('span'),b=document.createElement('span');a.className='call';b.className='out';a.textContent=call;b.textContent='→ '+out;li.append(a,b);ol.append(li)}
   if(!rows.length){const li=document.createElement('li');li.className='empty';li.textContent=zh?'运行任务后显示能力调用。':'Run a task to see its capability calls.';ol.append(li)}
   col.append(h,ol);return col}))}
 drawStatus(){this.drawTrace()}
 // ------------------------------------------------------------------ building the visuals of a cell
 buildBench(c){const bench=new THREE.Group();c.group.add(bench);c.benchTop=primitive(bench,{shape:'box',size:[1.20,.94,.06],position:[.24,0,-.03],color:'#c5c0b4'});c.cutoutBench=new THREE.Group();bench.add(c.cutoutBench);
  for(const[size,position]of[[[1.20,.545,.06],[.24,-.1975,-.03]],[[1.20,.145,.06],[.24,.3975,-.03]],[[.565,.25,.06],[-.0775,.20,-.03]],[[.365,.25,.06],[.6575,.20,-.03]]])primitive(c.cutoutBench,{shape:'box',size,position,color:'#c5c0b4'});c.cutoutBench.visible=false;
  for(const x of[-.22,.65])for(const y of[-.35,.35])primitive(bench,{shape:'box',size:[.035,.035,.72],position:[x,y,-.42],color:'#626b6b'})}
 buildRobot(c,assets){c.robot?.removeFromParent();disposeGroup(c.robot);c.robot=new THREE.Group();c.group.add(c.robot);c.robotMeshes=[];const loader=new STLLoader(),doc=new DOMParser().parseFromString(assets.xml,'text/xml');
  const files=new Map([...doc.querySelectorAll('asset mesh')].map(m=>[m.getAttribute('name')||m.getAttribute('file').replace(/\.(stl|obj)$/i,''),m.getAttribute('file')])),materials=new Map([...doc.querySelectorAll('asset material')].map(m=>[m.getAttribute('name'),m.getAttribute('rgba')]));
  for(const g of doc.querySelectorAll('worldbody geom')){
   if(g.getAttribute('class')==='collision'||(!g.hasAttribute('mesh')&&g.getAttribute('group')!=='2'))continue;
   let b=g.parentElement;while(b&&b.tagName!=='body')b=b.parentElement;if(!b)continue;
   const color=(g.getAttribute('rgba')||materials.get(g.getAttribute('material'))||'.29 .33 .32 1').split(/\s+/).map(Number),mat=new THREE.MeshStandardMaterial({color:assets.id==='piper'?new THREE.Color('#424b50'):new THREE.Color(...color.slice(0,3)),roughness:.42,metalness:.28});
   let visual;
   if(g.hasAttribute('mesh')){
    const file=files.get(g.getAttribute('mesh')),buffer=assets.meshes[file];if(!buffer)continue;
    if(file.endsWith('.obj')){visual=new OBJLoader().parse(new TextDecoder().decode(buffer));visual.traverse(o=>{if(o.isMesh){o.material.dispose();o.material=mat.clone()}});mat.dispose()}
    else visual=new THREE.Mesh(loader.parse(buffer.buffer.slice(buffer.byteOffset,buffer.byteOffset+buffer.byteLength)),mat);
   }else{const size=g.getAttribute('size').split(/\s+/).map(Number);visual=new THREE.Mesh(new THREE.BoxGeometry(...size.map(v=>v*2)),mat)}
   if(g.hasAttribute('quat')){const q=g.getAttribute('quat').split(/\s+/).map(Number);visual.quaternion.set(q[1],q[2],q[3],q[0]).normalize()}
   if(g.hasAttribute('pos'))visual.position.set(...g.getAttribute('pos').split(/\s+/).map(Number));
   visual.traverse(o=>{if(o.isMesh){o.castShadow=true;o.receiveShadow=true}});const group=new THREE.Group();group.add(visual);c.robot.add(group);c.robotMeshes.push({name:b.getAttribute('name'),group});
  }
  // a visible camera housing follows the manufacturer's wrist body
  c.cameraHousing=new THREE.Group();primitive(c.cameraHousing,{shape:'box',size:[.028,.030,.020],position:[.034,0,.045],color:'#223841'});primitive(c.cameraHousing,{shape:'cylinder',size:[.009,.007],position:[.034,0,.059],color:'#64aabd'});c.robot.add(c.cameraHousing);this.checkReady()}
 buildEnvironment(c,man){const scene=man.scene;c.benchTop.visible=scene.id!=='laundry';c.cutoutBench.visible=scene.id==='laundry';
  if(c.environment){c.group.remove(c.environment);disposeGroup(c.environment)}c.environment=new THREE.Group();c.group.add(c.environment);
  for(const p of scene.static){if(scene.id==='laundry'&&(p.id.startsWith('laundry-')||p.id.startsWith('drum-')))continue;primitive(c.environment,p)}c.belts=propsView(scene,c.environment,this.propAssets);c.objectViews=new Map();
  for(const item of man.items){const surface=item.mesh?.grid?new GarmentSurface(item):null;const obj=surface?surface.mesh:item.soft?softMesh(item):toyMesh(item);c.environment.add(obj);const detail=item.kind==='plush'?plushDetail(item):null;if(detail)c.environment.add(detail);c.objectViews.set(item.id,{obj,detail,item,surface})}
  c.lidView=scene.id==='laundry'?washerLid():null;if(c.lidView)c.environment.add(c.lidView);
  for(const l of scene.labels)this.label(c,l.text,l.position);
  // which drawn mesh follows which simulated body
  for(const m of c.robotMeshes)m.id=man.bodies[m.name]??-1;if(c.frame)this.syncCell(c)}
 label(c,labels,pos){const canvas=document.createElement('canvas');canvas.width=512;canvas.height=96;const sprite=new THREE.Sprite(new THREE.SpriteMaterial({map:new THREE.CanvasTexture(canvas),depthTest:false,toneMapped:false}));sprite.position.set(...pos);sprite.scale.set(.22,.042,1);sprite.userData={labels,canvas};c.environment.add(sprite);this.drawLabel(sprite)}
 drawLabel(o){const ctx=o.userData.canvas.getContext('2d');ctx.clearRect(0,0,512,96);ctx.fillStyle='#fafbf7';ctx.fillRect(0,0,512,96);ctx.fillStyle='#354642';ctx.textAlign='center';ctx.font='500 29px sans-serif';ctx.fillText(o.userData.labels[this.lang],256,57);o.material.map.needsUpdate=true}
 // ------------------------------------------------------------------ drawing a frame
 syncCell(c){const d=c.frame,man=c.manifest;if(!d||!man||!c.objectViews)return;c.dirty=false;
  for(const m of c.robotMeshes){const id=m.id;if(id<0)continue;m.group.position.set(d.xpos[id*3],d.xpos[id*3+1],d.xpos[id*3+2]);m.group.quaternion.set(d.xquat[id*4+1],d.xquat[id*4+2],d.xquat[id*4+3],d.xquat[id*4])}
  const w=man.wristBody;c.cameraHousing.position.set(d.xpos[w*3],d.xpos[w*3+1],d.xpos[w*3+2]);c.cameraHousing.quaternion.set(d.xquat[w*4+1],d.xquat[w*4+2],d.xquat[w*4+3],d.xquat[w*4]);
  const left=(man.scene.conveyor?man.scene.conveyor.left:-9)-.03;
  for(const{obj,detail,item,surface}of c.objectViews.values()){
   if(item.soft){if(surface)surface.update(d.flex,item.vertexAdr);else{const attr=obj.geometry.attributes.position;for(let i=0;i<item.vertexNum;i++)attr.setXYZ(i,d.flex[(item.vertexAdr+i)*3],d.flex[(item.vertexAdr+i)*3+1],d.flex[(item.vertexAdr+i)*3+2]);attr.needsUpdate=true;obj.geometry.computeVertexNormals();obj.geometry.computeBoundingSphere()}
    if(detail){const p=[0,0,0];for(let i=0;i<item.vertexNum;i++)for(let j=0;j<3;j++)p[j]+=d.flex[(item.vertexAdr+i)*3+j]/item.vertexNum;detail.position.set(...p)}}
   else{obj.visible=d.qpos[item.qadr]>left;obj.position.set(d.qpos[item.qadr],d.qpos[item.qadr+1],d.qpos[item.qadr+2]);obj.quaternion.set(d.qpos[item.qadr+4],d.qpos[item.qadr+5],d.qpos[item.qadr+6],d.qpos[item.qadr+3])}}
  if(c.lidView&&man.lid!==null){const l=man.lid;c.lidView.position.set(d.xpos[l*3],d.xpos[l*3+1],d.xpos[l*3+2]);c.lidView.quaternion.set(d.xquat[l*4+1],d.xquat[l*4+2],d.xquat[l*4+3],d.xquat[l*4])}
  const travel=d.belt/1.45;for(const b of c.belts||[]){if(b.userData.tex)b.userData.tex.offset.x=travel/(.896/8);for(const s of b.children){if(s.userData.x0===undefined)s.userData.x0=s.position.x;s.position.x=((((s.userData.x0+.445-travel)%.896)+.896)%.896)-.445}}}
 aimWristCamera(c){const t=c.frame.tool,o=c.group.position,pos=new THREE.Vector3(t[0],t[1],t[2]).add(o),dir=new THREE.Vector3(t[5],t[8],t[11]);this.wristCamera.position.copy(pos).addScaledVector(dir,-.055).add(new THREE.Vector3(t[3],t[6],t[9]).multiplyScalar(.055));this.wristCamera.up.set(t[4],t[7],t[10]);this.wristCamera.lookAt(pos.clone().addScaledVector(dir,.35))}
 tick(now){if(this.disposed)return;this.raf=requestAnimationFrame(this.tick);this.last=now;
  if(document.hidden)return;
  // Cap scene redraws at 30 Hz (12 Hz while idle); only update garment geometry on a new worker frame.
  const active=[...this.cells.values()].some(c=>c.stats.status==='running');
  const interval=active||this.interacting?1000/30:1000/12;if(now-(this.lastDraw||0)<interval)return;this.lastDraw=now;
  try{for(const c of this.cells.values()){if(!c.manifest||c.inflight||!this.ready||(!active&&!c.frame?.awake&&now>c.awakeUntil)||['paused','stopped','error'].includes(c.stats.status))continue;
    const elapsed=Math.min((now-(c.lastTick||now-interval))/1000,.1);c.lastTick=now;c.inflight=true;this.post(c,{type:'tick',dt:elapsed,speed:this.speed,budget:this.cells.size>2?6:12})}
   for(const c of this.cells.values())if(c.dirty){this.syncCell(c);this.dirty=true;if(c.id===this.focus)this.wristDirty=true}
  }catch(e){this.fail(e)}
  const changed=this.controls.update();if(this.dirty||changed||active){this.renderer.render(this.scene,this.camera);this.dirty=false}
  const f=this.cells.get(this.focus);if(this.wristVisible&&f?.frame&&f.robotMeshes&&this.wristDirty&&now-(this.lastWrist||0)>180){this.aimWristCamera(f);f.cameraHousing.visible=false;const hide=f.robotMeshes.filter(g=>/finger|link7|link8/.test(g.name));hide.forEach(g=>g.group.visible=false);this.wristRenderer.render(this.scene,this.wristCamera);f.cameraHousing.visible=true;hide.forEach(g=>g.group.visible=true);this.lastWrist=now;this.wristDirty=false}
 }
 dispose(){this.disposed=true;if(import.meta.env.DEV&&window.__nervusSim===this)delete window.__nervusSim;cancelAnimationFrame(this.raf);clearTimeout(this.traceTimer);this.resizeObserver.disconnect();for(const c of this.cells.values())c.worker.terminate();this.cells.clear();for(const d of this.debugCalls.values())d.resolve(null);this.debugCalls.clear();this.controls.dispose();disposeGroup(this.scene);this.renderer.dispose();this.wristRenderer.dispose();this.renderer.domElement.remove();this.wristBox.remove();this.fleetBar.remove();this.traceBox.remove()}
}
