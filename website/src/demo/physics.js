import loadMujoco from '@mujoco/mujoco';import wasmUrl from '@mujoco/mujoco/mujoco.wasm?url';import{robotProfile}from'./robots.js';
export{createModel,objectPosition,pinchPosition,attach,detach,moveRigid}from'./model.js';
let wasm;const models=new Map();export const HOME=robotProfile('piper').home;
export async function loadPhysics(id='piper'){
 if(!wasm)wasm=loadMujoco({locateFile:p=>p.endsWith('.wasm')?wasmUrl:p}).catch(e=>{wasm=null;throw e});
 if(!models.has(id))models.set(id,(async()=>{const mj=await wasm,profile=robotProfile(id);const response=await fetch(profile.model);if(!response.ok)throw Error('Robot model unavailable');const xml=await response.text();const names=[...new Set([...xml.matchAll(/<mesh[^>]*file="([^"]+)"/g)].map(x=>x[1]))];const meshes=Object.fromEntries(await Promise.all(names.map(async name=>{const r=await fetch(`/models/${id}/assets/${name}`);if(!r.ok)throw Error('Robot mesh unavailable');return[name,new Uint8Array(await r.arrayBuffer())]})));return{mj,id,xml,meshes}})().catch(e=>{models.delete(id);throw e}));
 return models.get(id);
}
export function toolPosition(p,data=p.data,site=p.tool){return Array.from(data.site_xpos).slice(site*3,site*3+3)}
function solve(A,b){const n=b.length,m=A.map((r,i)=>[...r,b[i]]);for(let i=0;i<n;i++){let pivot=i;for(let k=i+1;k<n;k++)if(Math.abs(m[k][i])>Math.abs(m[pivot][i]))pivot=k;[m[i],m[pivot]]=[m[pivot],m[i]];const d=m[i][i]||1e-10;for(let j=i;j<=n;j++)m[i][j]/=d;for(let k=0;k<n;k++)if(k!==i){const a=m[k][i];for(let j=i;j<=n;j++)m[k][j]-=a*m[i][j]}}return m.map(r=>r[n])}
// yaw (optional): heading of the finger-closing axis (the tool's y axis) in the table plane
function solveIK(p,target,seed=p.profile.home,yaw){
 const{mj,profile,kin}=p,{model,ghost,tool}=kin,n=profile.dof,q=[...seed].slice(0,profile.home.length),weight=profile.id==='piper'?.14:.09;
 const tilt=profile.id==='piper'?Math.max(0,Math.min(1.12,Math.max((target[2]-.075)*4.8,(Math.hypot(target[0],target[1])-.36)*5.4))):Math.max(0,Math.min(.9,(.28-Math.hypot(target[0],target[1]))*8)),az=Math.atan2(target[1],target[0]);const zdir=[Math.sin(tilt)*Math.cos(az),Math.sin(tilt)*Math.sin(az),-Math.cos(tilt)];const desired=[...target,weight*zdir[0],weight*zdir[1],weight*zdir[2]];
 if(yaw!==undefined){let y=[Math.cos(yaw),Math.sin(yaw),0];const d=y[0]*zdir[0]+y[1]*zdir[1]+y[2]*zdir[2];y=y.map((v,i)=>v-d*zdir[i]);const n=Math.hypot(...y)||1;desired.push(...y.map(v=>weight*v/n))}
 const features=()=>{const m=ghost.site_xmat,o=tool*9,f=[...toolPosition(p,ghost,tool),weight*m[o+2],weight*m[o+5],weight*m[o+8]];if(yaw!==undefined)f.push(weight*m[o+1],weight*m[o+4],weight*m[o+7]);return f};const rows=yaw!==undefined?9:6;const h=.0003;
 for(let it=0;it<500;it++){
  ghost.qpos.set(q);mj.mj_forward(model,ghost);const f=features(),e=desired.map((x,i)=>x-f[i]);if(Math.hypot(...e.slice(0,3))<.0015&&Math.hypot(...e.slice(3))<.006*(rows/6))break;
  const J=Array.from({length:rows},()=>Array(n).fill(0));for(let j=0;j<n;j++){ghost.qpos[j]=q[j]+h;mj.mj_forward(model,ghost);const f2=features();for(let k=0;k<rows;k++)J[k][j]=(f2[k]-f[k])/h;ghost.qpos[j]=q[j]}
  const A=Array.from({length:n},(_,i)=>Array.from({length:n},(_,j)=>J.reduce((sum,row)=>sum+row[i]*row[j],0)+(i===j?.00014:0))),b=Array.from({length:n},(_,i)=>J.reduce((sum,row,k)=>sum+row[i]*e[k],0));const delta=solve(A,b);
  for(let i=0;i<n;i++)q[i]=Math.max(profile.limits[i][0]+.001,Math.min(profile.limits[i][1]-.001,q[i]+Math.max(-.12,Math.min(.12,delta[i]))));
 }
 ghost.qpos.set(q);mj.mj_forward(model,ghost);const m=ghost.site_xmat,o=tool*9;return{q,error:Math.hypot(...target.map((x,i)=>x-toolPosition(p,ghost,tool)[i])),yawError:yaw===undefined?0:Math.acos(Math.min(1,Math.abs(Math.cos(yaw)*m[o+1]+Math.sin(yaw)*m[o+4])/Math.max(1e-9,Math.hypot(m[o+1],m[o+4]))))};
}

// The fingers are symmetric: yaw and yaw+pi grip the same way. Use whichever needs less wrist turn.
export function inverseKinematics(p,target,seed=p.profile.home,yaw){
 if(yaw!==undefined){const{mj,kin}=p,{model,ghost,tool}=kin;ghost.qpos.set(seed);mj.mj_forward(model,ghost);const m=ghost.site_xmat,o=tool*9;if(Math.cos(yaw)*m[o+1]+Math.sin(yaw)*m[o+4]<0)yaw+=Math.PI}
 let best=solveIK(p,target,seed,yaw);if(best.error>.018){const alt=solveIK(p,target,p.profile.home,yaw);if(alt.error<best.error)best=alt}return best}
