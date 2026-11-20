import{robotProfile}from'./robots.js';
const v=x=>x.map(n=>Number(n).toFixed(7)).join(' '),rgba=hex=>{const c=parseInt(hex.slice(1),16);return`${((c>>16)&255)/255} ${((c>>8)&255)/255} ${(c&255)/255} 1`};
export function primitiveXML(p){const size=p.shape==='sphere'?[p.size[0]]:p.shape==='cylinder'?[p.size[0],p.size[1]/2]:p.size.map(x=>x/2);return`<geom type="${p.shape==='sphere'?'sphere':p.shape==='cylinder'?'cylinder':'box'}" pos="${v(p.position)}" size="${v(size)}" rgba="${rgba(p.color)}" ${p.rotation?`euler="${v(p.rotation)}"`:''} friction=".9 .02 .001" solref=".012 1"/>`}
export const nameId=(p,type,name)=>p.mj.mj_name2id(p.model,p.mj.mjtObj[type].value,name);
export function createModel(engine,scene){
 const{mj,xml,meshes}=engine,profile=robotProfile(engine.id),vfs=new mj.MjVFS();Object.entries(meshes).forEach(([n,b])=>vfs.addBuffer(n,b));
 let base=xml.replace('meshdir="assets"','meshdir=""');const stripped=base.replace(/<mesh\b[^>]*\/>/g,'').replace(/<geom\b[^>]*type="mesh"[^>]*\/>/g,'').replace(/<geom\b[^>]*mesh="[^"]+"[^>]*\/>/g,'');const kmodel=mj.MjModel.from_xml_string(stripped,vfs),ghost=new mj.MjData(kmodel);
 let combined=base.replace(/<option\b[^>]*\/>/,`<option integrator="${scene.items.some(x=>x.soft)?'discrete':'implicitfast'}" timestep=".004" iterations="30" tolerance="1e-7" jacobian="sparse"/>`);
 let world=primitiveXML({shape:'box',size:[1.20,.94,.06],position:[.24,0,-.03],color:'#c1bdb3'})+scene.static.map(primitiveXML).join('');
 world+='<body name="grasp_anchor" mocap="true" pos=".3 0 .25"><geom type="sphere" size=".001" contype="0" conaffinity="0" rgba="0 0 0 0"/></body>';
 let equality='';
 if(scene.id==='laundry'){
  world+='<body name="washer_lid" pos=".34 .29 .132" gravcomp="1"><joint name="lid_hinge" axis="-1 0 0" range="0 1.60" damping=".04" frictionloss=".006"/><inertial pos="0 -.074 .004" mass=".045" diaginertia=".00016 .00016 .00016"/><geom type="box" pos="0 -.077 .002" size=".100 .077 .005" rgba=".88 .92 .94 1"/><geom type="capsule" fromto="-.026 -.140 .016 .026 -.140 .016" size=".005" mass=".008"/></body>';
  equality+='<connect name="grasp_lid" body1="washer_lid" body2="grasp_anchor" anchor="0 -.140 .016" active="false" solref=".012 1"/>';
 }
 for(const item of scene.items){
  if(item.soft){world+=`<flexcomp name="${item.id}" type="direct" dim="2" point="${v(item.mesh.points.flat())}" element="${item.mesh.indices.join(' ')}" mass="${item.mass}" radius="${item.kind==='plush'?'.0018':'.0026'}" rgba="${rgba(item.color)}"><edge equality="true" solref="${item.kind==='plush'?'.025':'.012'} 1"/><elasticity young="${item.bend}" poisson=".3" thickness="${item.kind==='plush'?'.003':'.0015'}" elastic2d="bend" damping=".003"/><contact selfcollide="none" friction="${item.kind==='plush'?'.7':'.42'} .015 .001" solref=".014 1"/></flexcomp>`;
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
 for(const item of scene.items){const obj={...item,eq:nameId(p,'mjOBJ_EQUALITY','grasp_'+item.id)};if(item.soft){obj.flex=nameId(p,'mjOBJ_FLEX',item.id);obj.vertexAdr=model.flex_vertadr[obj.flex];obj.vertexNum=model.flex_vertnum[obj.flex];obj.bodies=Array.from({length:obj.vertexNum},(_,i)=>nameId(p,'mjOBJ_BODY',item.id+'_'+i));}else{obj.body=nameId(p,'mjOBJ_BODY',item.id);const j=nameId(p,'mjOBJ_JOINT','free_'+item.id);obj.qadr=model.jnt_qposadr[j];obj.vadr=model.jnt_dofadr[j];}p.items.set(item.id,obj)}
 p.anchor=nameId(p,'mjOBJ_BODY','grasp_anchor');p.mocap=model.body_mocapid[p.anchor];if(scene.id==='laundry'){p.lid=nameId(p,'mjOBJ_BODY','washer_lid');p.lidEq=nameId(p,'mjOBJ_EQUALITY','grasp_lid');const j=nameId(p,'mjOBJ_JOINT','lid_hinge');p.lidAdr=model.jnt_qposadr[j];}
 p.initial=Array.from(data.qpos);p.ctrlInitial=Array.from(data.ctrl);p.eqInitial=Array.from(data.eq_active);
 p.dispose=()=>{data.delete();ghost.delete();model.delete();kmodel.delete()};return p;
}
export function objectPosition(p,item){const obj=typeof item==='string'?p.items.get(item):item;if(obj.soft){const points=p.data.flexvert_xpos,a=obj.vertexAdr*3;let pos=[0,0,0];for(let i=0;i<obj.vertexNum;i++)for(let j=0;j<3;j++)pos[j]+=points[a+i*3+j]/obj.vertexNum;return pos}return Array.from(p.data.qpos).slice(obj.qadr,obj.qadr+3)}
export function pinchPosition(p,item){const obj=typeof item==='string'?p.items.get(item):item;if(!obj.soft)return objectPosition(p,obj);const a=obj.vertexAdr*3;let best=0;for(let i=1;i<obj.vertexNum;i++)if(p.data.flexvert_xpos[a+i*3+2]>p.data.flexvert_xpos[a+best*3+2])best=i;obj.pinch=best;return Array.from(p.data.flexvert_xpos).slice(a+best*3,a+best*3+3)}
export function moveRigid(p,item,pos){const obj=typeof item==='string'?p.items.get(item):item;p.data.qpos.set([...pos,1,0,0,0],obj.qadr);p.data.qvel.fill(0,obj.vadr,obj.vadr+6)}
export function attach(p,id,tool){const obj=p.items.get(id),eq=obj.eq;const base=eq*p.mj.mjNEQDATA;
 if(obj.soft){p.model.eq_obj1id[eq]=obj.bodies[obj.pinch??0];p.model.eq_data.set([0,0,0,0,0,0],base)}else p.model.eq_data.set([0,0,0,0,0,0,1,0,0,0,.05],base);
 p.data.mocap_pos.set([tool[0],tool[1],tool[2]-.012],p.mocap*3);p.data.mocap_quat.set([1,0,0,0],p.mocap*4);p.data.eq_active[eq]=1;
}
export function detach(p,id){p.data.eq_active[p.items.get(id).eq]=0}
