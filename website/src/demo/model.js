import{robotProfile}from'./robots.js';
const v=x=>x.map(n=>Number(n).toFixed(7)).join(' '),rgba=hex=>{const c=parseInt(hex.slice(1),16);return`${((c>>16)&255)/255} ${((c>>8)&255)/255} ${(c&255)/255} 1`};
export function primitiveXML(p){const size=p.shape==='sphere'?[p.size[0]]:p.shape==='cylinder'?[p.size[0],p.size[1]/2]:p.size.map(x=>x/2);return`<geom type="${p.shape==='sphere'?'sphere':p.shape==='cylinder'?'cylinder':'box'}" pos="${v(p.position)}" size="${v(size)}" rgba="${rgba(p.color)}" ${p.rotation?`euler="${v(p.rotation)}"`:''} friction=".9 .02 .001" solref=".012 1"/>`}
export const nameId=(p,type,name)=>p.mj.mj_name2id(p.model,p.mj.mjtObj[type].value,name);
export function createModel(engine,scene){
 const{mj,xml,meshes}=engine,profile=robotProfile(engine.id),vfs=new mj.MjVFS();Object.entries(meshes).forEach(([n,b])=>vfs.addBuffer(n,b));
 let base=xml.replace('meshdir="assets"','meshdir=""');const stripped=base.replace(/<mesh\b[^>]*\/>/g,'').replace(/<geom\b[^>]*type="mesh"[^>]*\/>/g,'').replace(/<geom\b[^>]*mesh="[^"]+"[^>]*\/>/g,'');const kmodel=mj.MjModel.from_xml_string(stripped,vfs),ghost=new mj.MjData(kmodel);
 let combined=base.replace(/<option\b[^>]*\/>/,`<option integrator="${scene.items.some(x=>x.soft)?'discrete':'implicitfast'}" timestep=".004" iterations="30" tolerance="1e-7" jacobian="sparse" cone="elliptic" impratio="10"/>`);
 let world=primitiveXML({shape:'box',size:[1.20,.94,.06],position:[.24,0,-.03],color:'#c1bdb3'})+scene.static.map(primitiveXML).join('');
 world+='<body name="grasp_anchor" mocap="true" pos=".3 0 .25"><geom type="sphere" size=".001" contype="0" conaffinity="0" rgba="0 0 0 0"/></body>';
 let equality='';
 if(scene.id==='laundry'){
  // stiff hinge (0.12 N m of friction, under 1 N at the handle): the lid stays where the gripper leaves it, and a
  // hanging garment brushing past can't swing it shut
  world+='<body name="washer_lid" pos=".34 .29 .132" gravcomp="1"><joint name="lid_hinge" axis="-1 0 0" range="0 1.60" damping=".04" frictionloss=".12"/><inertial pos="0 -.074 .004" mass=".045" diaginertia=".00016 .00016 .00016"/><geom type="box" pos="0 -.077 .002" size=".100 .077 .005" rgba=".88 .92 .94 1"/><geom type="capsule" fromto="-.026 -.140 .016 .026 -.140 .016" size=".005" mass=".008"/></body>';
  equality+='<connect name="grasp_lid" body1="washer_lid" body2="grasp_anchor" anchor="0 -.140 .016" active="false" solref=".012 1"/>';
 }
 if(scene.conveyor){
  // The belt: a long plate on a slide joint, driven by a position actuator. Parts rest on it and are carried
  // by friction; the drawn slats follow the plate's actual travel. Starts at the conveyor's left end.
  const c=scene.conveyor;world+=`<body name="belt" pos="${v([c.left,c.y,0])}"><joint name="belt_slide" type="slide" axis="1 0 0" range="-3 3" damping="30" armature="1"/><geom name="belt_plate" type="box" size="1.45 .072 .003" pos="1.45 0 .041" friction="1.2 .01 .001" mass="4" rgba="0 0 0 0"/></body>`;
  combined=combined.replace('</actuator>','<position name="belt_drive" joint="belt_slide" kp="5000" kv="500" ctrlrange="-3 3"/></actuator>');
 }
 for(const item of scene.items){
  if(item.carrier){
   // a carrier with a square pocket (46 mm) that a 38 mm bearing drops into
   const g=(hx,hy,hz,x,y,z)=>`<geom type="box" size="${v([hx,hy,hz])}" pos="${v([x,y,z])}" rgba="${rgba(item.color)}" friction=".9 .02 .001" mass=".025"/>`;
   world+=`<body name="${item.id}" pos="${v(item.position)}"><freejoint name="free_${item.id}"/>${g(.0375,.0325,.003,0,0,-.007)}${g(.00725,.0325,.007,.03025,0,.003)}${g(.00725,.0325,.007,-.03025,0,.003)}${g(.023,.00475,.007,0,.02775,.003)}${g(.023,.00475,.007,0,-.02775,.003)}</body>`;
   equality+=`<weld name="grasp_${item.id}" body1="${item.id}" body2="grasp_anchor" active="false" solref=".010 1" torquescale=".05"/>`;
   continue;
  }
  if(item.soft){world+=`<flexcomp name="${item.id}" type="direct" dim="${item.kind==='plush'?3:2}" point="${v(item.mesh.points.flat())}" element="${(item.mesh.volumeIndices||item.mesh.indices).join(' ')}" mass="${item.mass}" radius="${item.kind==='plush'?'.0018':'.004'}" rgba="${rgba(item.color)}"><edge equality="${item.kind==='plush'?'false':'true'}" solref="${item.kind==='plush'?'.025':'.012'} 1"/><elasticity young="${item.bend}" poisson=".3" thickness="${item.kind==='plush'?'.003':'.0015'}" ${item.kind==='plush'?'':'elastic2d="bend"'} damping=".003"/><contact selfcollide="none" ${item.kind==='plush'?'':'contype="2" conaffinity="1"'} friction="${item.kind==='plush'?'.7':'.42'} .015 .001" solref=".014 1"/></flexcomp>`;
   // A body per mesh vertex allows a pinch to hold one vertex while the rest of the fabric remains free.
   equality+=`<connect name="grasp_${item.id}" body1="${item.id}_0" body2="grasp_anchor" anchor="${v(item.mesh.points[0])}" active="false" solref=".008 1"/>`;
  }else{
   const shape=item.proxy,sz=shape==='sphere'?[item.size[0]*.56]:shape==='cylinder'?[item.size[0],item.size[2]]:item.size;
   world+=`<body name="${item.id}" pos="${v(item.position)}"><freejoint name="free_${item.id}"/>${primitiveXML({shape,size:sz,position:[0,0,0],color:item.color}).replace('friction="','mass=".035" friction="')}</body>`;
   equality+=`<weld name="grasp_${item.id}" body1="${item.id}" body2="grasp_anchor" active="false" solref=".010 1" torquescale=".05"/>`;
  }
 }
 combined=combined.replace('</worldbody>',world+'</worldbody>');if(combined.includes('</equality>'))combined=combined.replace('</equality>',equality+'</equality>');else combined=combined.replace('</mujoco>','<equality>'+equality+'</equality></mujoco>');
 let model;try{model=mj.MjModel.from_xml_string(combined,vfs)}catch(e){ghost.delete();kmodel.delete();vfs.delete();throw e}vfs.delete();
 const data=new mj.MjData(model),p={mj,model,data,scene,profile,kin:{model:kmodel,ghost,tool:mj.mj_name2id(kmodel,mj.mjtObj.mjOBJ_SITE.value,'tool')},tool:mj.mj_name2id(model,mj.mjtObj.mjOBJ_SITE.value,'tool'),items:new Map(),xml:combined};
 data.qpos.set(profile.home);data.ctrl.set(profile.home.slice(0,profile.dof+1));ghost.qpos.set(profile.home);mj.mj_forward(model,data);mj.mj_forward(kmodel,ghost);
 for(const item of scene.items){const obj={...item,eq:nameId(p,'mjOBJ_EQUALITY','grasp_'+item.id)};if(item.soft){obj.flex=nameId(p,'mjOBJ_FLEX',item.id);obj.vertexAdr=model.flex_vertadr[obj.flex];obj.vertexNum=model.flex_vertnum[obj.flex];obj.bodies=Array.from({length:obj.vertexNum},(_,i)=>nameId(p,'mjOBJ_BODY',item.id+'_'+i));obj.garment=item.kind!=='plush';}else{obj.body=nameId(p,'mjOBJ_BODY',item.id);const j=nameId(p,'mjOBJ_JOINT','free_'+item.id);obj.qadr=model.jnt_qposadr[j];obj.vadr=model.jnt_dofadr[j];}p.items.set(item.id,obj)}
 if(scene.conveyor){p.beltAct=nameId(p,'mjOBJ_ACTUATOR','belt_drive');p.beltAdr=model.jnt_qposadr[nameId(p,'mjOBJ_JOINT','belt_slide')]}p.adhere=['adhere_l','adhere_r'].map(n=>nameId(p,'mjOBJ_ACTUATOR',n)).filter(i=>i>=0);p.touch=['touch_l','touch_r'].map(n=>{const id=nameId(p,'mjOBJ_SENSOR',n);return id<0?-1:model.sensor_adr[id]});p.anchor=nameId(p,'mjOBJ_BODY','grasp_anchor');p.mocap=model.body_mocapid[p.anchor];if(scene.id==='laundry'){p.lid=nameId(p,'mjOBJ_BODY','washer_lid');p.lidEq=nameId(p,'mjOBJ_EQUALITY','grasp_lid');const j=nameId(p,'mjOBJ_JOINT','lid_hinge');p.lidAdr=model.jnt_qposadr[j];}
 p.initial=Array.from(data.qpos);p.ctrlInitial=Array.from(data.ctrl);p.eqInitial=Array.from(data.eq_active);
 p.dispose=()=>{data.delete();ghost.delete();model.delete();kmodel.delete()};return p;
}
// Garment states. 'sleep': nobody is handling it and it lies still, so it is gravity compensated, heavily
// damped and collides only with the world. 'free': falling or settling, so it also collides with the other
// garments (it lands on top of a pile instead of sinking into it). 'held': the one garment the current job
// picks, the only one the fingers and the adhesive grip skin can touch, so a pinch takes exactly one.
// The solver then couples one or two garments at a time: 1.5-3 ms per step instead of 12 ms with every
// garment touching every other. Contact bits: world 1/1, hand 8/1, grip skin 0/16, garments by state below.
const BITS={sleep:[2,1],free:[2,3],held:[18,11]};
export function garmentState(p,item,state){if(!item?.garment||item.state===state)return;const m=p.model,v=p.data.qvel,k=item.flex;item.state=state;item.still=0;item.free=0;
 [m.flex_contype[k],m.flex_conaffinity[k]]=BITS[state];const sleep=state==='sleep';
 for(const b of item.bodies){m.body_gravcomp[b]=sleep?1:0;const a=m.body_dofadr[b];for(let j=0;j<3;j++){m.dof_damping[a+j]=sleep?5:0;if(sleep)v[a+j]=0}}}
// a free garment goes back to sleep once it has lain still for half a second
// (judged by its mean vertex speed: two free garments resting on each other keep a little contact jitter), or
// after 4 s free once it is no longer really moving
export function settleGarments(p,dt){const m=p.model,v=p.data.qvel;for(const item of p.items.values()){if(item.state!=='free')continue;let sum=0;
 for(const b of item.bodies){const a=m.body_dofadr[b];sum+=Math.hypot(v[a],v[a+1],v[a+2])}const mean=sum/item.bodies.length;item.free=(item.free||0)+dt;
 item.still=mean<.02?item.still+dt:0;if(item.still>.5||(item.free>4&&mean<.05))garmentState(p,item,'sleep')}}
export function objectPosition(p,item){const obj=typeof item==='string'?p.items.get(item):item;if(obj.soft){const points=p.data.flexvert_xpos,a=obj.vertexAdr*3;let pos=[0,0,0];for(let i=0;i<obj.vertexNum;i++)for(let j=0;j<3;j++)pos[j]+=points[a+i*3+j]/obj.vertexNum;return pos}return Array.from(p.data.qpos).slice(obj.qadr,obj.qadr+3)}
export function pinchPosition(p,item){const obj=typeof item==='string'?p.items.get(item):item;if(!obj.soft)return objectPosition(p,obj);const a=obj.vertexAdr*3;let best=0;for(let i=1;i<obj.vertexNum;i++)if(p.data.flexvert_xpos[a+i*3+2]>p.data.flexvert_xpos[a+best*3+2])best=i;obj.pinch=best;return Array.from(p.data.flexvert_xpos).slice(a+best*3,a+best*3+3)}
export function moveRigid(p,item,pos){const obj=typeof item==='string'?p.items.get(item):item;p.data.qpos.set([...pos,1,0,0,0],obj.qadr);p.data.qvel.fill(0,obj.vadr,obj.vadr+6)}
export function attach(p,id,tool){const obj=p.items.get(id),eq=obj.eq;const base=eq*p.mj.mjNEQDATA;
 if(obj.soft){p.model.eq_obj1id[eq]=obj.bodies[obj.pinch??0];p.model.eq_data.set([0,0,0,0,0,0],base)}else p.model.eq_data.set([0,0,0,0,0,0,1,0,0,0,.05],base);
 p.data.mocap_pos.set([tool[0],tool[1],tool[2]-.012],p.mocap*3);p.data.mocap_quat.set([1,0,0,0],p.mocap*4);p.data.eq_active[eq]=1;
}
export function detach(p,id){p.data.eq_active[p.items.get(id).eq]=0}
