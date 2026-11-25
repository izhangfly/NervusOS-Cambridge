export function toolPosition(p,data=p.data,site=p.tool){return Array.from(data.site_xpos).slice(site*3,site*3+3)}
function solve(A,b){const n=b.length,m=A.map((r,i)=>[...r,b[i]]);for(let i=0;i<n;i++){let pivot=i;for(let k=i+1;k<n;k++)if(Math.abs(m[k][i])>Math.abs(m[pivot][i]))pivot=k;[m[i],m[pivot]]=[m[pivot],m[i]];const d=m[i][i]||1e-10;for(let j=i;j<=n;j++)m[i][j]/=d;for(let k=0;k<n;k++)if(k!==i){const a=m[k][i];for(let j=i;j<=n;j++)m[k][j]-=a*m[i][j]}}return m.map(r=>r[n])}
// yaw (optional): heading of the finger-closing axis (the tool's y axis) in the table plane
function solveIK(p,target,seed=p.profile.home,yaw,maxTilt=9,preferredTilt){
 const{mj,profile,kin}=p,{model,ghost,tool}=kin,n=profile.dof,q=[...seed].slice(0,profile.home.length),weight=profile.id==='piper'?.14:.09;
 const tilt=profile.id==='piper'?Math.max(0,Math.min(1.12,Math.max((target[2]-.075)*4.8,(Math.hypot(target[0],target[1])-.36)*5.4))):Math.max(0,Math.min(.9,(.28-Math.hypot(target[0],target[1]))*8)),az=Math.atan2(target[1],target[0]);const tl=preferredTilt??Math.min(tilt,maxTilt),zdir=[Math.sin(tl)*Math.cos(az),Math.sin(tl)*Math.sin(az),-Math.cos(tl)];const desired=[...target,weight*zdir[0],weight*zdir[1],weight*zdir[2]];
 if(yaw!==undefined){let y=[Math.cos(yaw),Math.sin(yaw),0];const d=y[0]*zdir[0]+y[1]*zdir[1]+y[2]*zdir[2];y=y.map((v,i)=>v-d*zdir[i]);const n=Math.hypot(...y)||1;desired.push(...y.map(v=>weight*v/n))}
 const pairs=collisionPairs(p),poseRows=yaw!==undefined?9:6;desired.push(...pairs.map(()=>0));
 const features=()=>{const m=ghost.site_xmat,o=tool*9,f=[...toolPosition(p,ghost,tool),weight*m[o+2],weight*m[o+5],weight*m[o+8]];if(yaw!==undefined)f.push(weight*m[o+1],weight*m[o+4],weight*m[o+7]);return f.concat(collisionDistances(p).map(d=>3*Math.min(0,d-.007)))};const rows=poseRows+pairs.length;const h=.0003;
 for(let it=0;it<500;it++){
  ghost.qpos.set(q);mj.mj_forward(model,ghost);const f=features(),e=desired.map((x,i)=>x-f[i]);if(Math.hypot(...e.slice(0,3))<.0015&&Math.hypot(...e.slice(3,poseRows))<.006&&e.slice(poseRows).every(v=>v<.0009))break;
  const J=Array.from({length:rows},()=>Array(n).fill(0));for(let j=0;j<n;j++){ghost.qpos[j]=q[j]+h;mj.mj_forward(model,ghost);const f2=features();for(let k=0;k<rows;k++)J[k][j]=(f2[k]-f[k])/h;ghost.qpos[j]=q[j]}
  const A=Array.from({length:n},(_,i)=>Array.from({length:n},(_,j)=>J.reduce((sum,row)=>sum+row[i]*row[j],0)+(i===j?.00014:0))),b=Array.from({length:n},(_,i)=>J.reduce((sum,row,k)=>sum+row[i]*e[k],0));const delta=solve(A,b);
  for(let i=0;i<n;i++)q[i]=Math.max(profile.limits[i][0]+.001,Math.min(profile.limits[i][1]-.001,q[i]+Math.max(-.12,Math.min(.12,delta[i]))));
 }
 ghost.qpos.set(q);mj.mj_forward(model,ghost);const m=ghost.site_xmat,o=tool*9;return{q,error:Math.hypot(...target.map((x,i)=>x-toolPosition(p,ghost,tool)[i])),yawError:yaw===undefined?0:Math.acos(Math.min(1,Math.abs(Math.cos(yaw)*m[o+1]+Math.sin(yaw)*m[o+4])/Math.max(1e-9,Math.hypot(m[o+1],m[o+4]))))};
}

// The fingers are symmetric: yaw and yaw+pi grip the same way. Use whichever needs less wrist turn.
function rawIK(p,target,seed=p.profile.home,yaw){
 if(yaw!==undefined){const{mj,kin}=p,{model,ghost,tool}=kin;ghost.qpos.set(seed);mj.mj_forward(model,ghost);const m=ghost.site_xmat,o=tool*9;if(Math.cos(yaw)*m[o+1]+Math.sin(yaw)*m[o+4]<0)yaw+=Math.PI}
 // a grasp keeps the fingers level (tilt capped at ~7 degrees) unless the arm can only reach it tilted
 if(yaw!==undefined){let lv=solveIK(p,target,seed,yaw,.12);if(lv.error>.006){const alt=solveIK(p,target,p.profile.home,yaw,.12);if(alt.error<lv.error)lv=alt}if(lv.error<=.006)return lv}
 let best=solveIK(p,target,seed,yaw);if(best.error>.018){const alt=solveIK(p,target,p.profile.home,yaw);if(alt.error<best.error)best=alt}
 // Panda can reach close to its base by leaning the wrist; the nominal down-facing orientation
 // otherwise rejects reachable recovery points near a basket rim. Keep normal grasps level first.
 if(best.error>.018&&['panda','gen3','ur5e'].includes(p.profile.id)&&Math.hypot(target[0],target[1])<.32&&target[2]<.24){
  for(const tilt of[-.45,.45,-.75,.75,-1.05,1.05]){for(const initial of[seed,p.profile.home]){const alt=solveIK(p,target,initial,yaw,9,tilt);if(alt.error<best.error)best=alt;if(best.error<.006)break}if(best.error<.006)break}
 }return best}


// Collision proxies remain in the lightweight FK model. Ignore gripper contacts; test the arm's
// own link geometry before accepting an IK branch or the curve connecting two poses.
function collisionPairs(p){
 if(!['panda','ur5e'].includes(p.profile.id))return [];
 if(p.kin.collisionPairs)return p.kin.collisionPairs;
 const m=p.kin.model,geoms=[];for(let i=0;i<m.ngeom;i++)if(m.geom_contype[i]&4)geoms.push(i);
 const pairs=[];for(let i=0;i<geoms.length;i++)for(let j=i+1;j<geoms.length;j++){
  const a=m.geom_bodyid[geoms[i]],b=m.geom_bodyid[geoms[j]],wa=m.body_weldid[a],wb=m.body_weldid[b];
  if(wa===wb||m.body_weldid[m.body_parentid[a]]===wb||m.body_weldid[m.body_parentid[b]]===wa)continue;
  pairs.push([geoms[i],geoms[j]]);
 }p.kin.collisionPairs=pairs;p.kin.distanceBuffer=new Float64Array(6);return pairs;
}
function collisionDistances(p){return collisionPairs(p).map(([a,b])=>p.mj.mj_geomDistance(p.kin.model,p.kin.ghost,a,b,.02,p.kin.distanceBuffer))}
export function selfContacts(p,q,clearance=.006){
 const {mj,kin}=p;kin.ghost.qpos.set(q);mj.mj_forward(kin.model,kin.ghost);
 return collisionDistances(p).filter(d=>d<clearance).length;
}
export function jointPathClear(p,from,to){
 if(!['panda','ur5e'].includes(p.profile.id))return true;
 const {mj,kin}=p;kin.ghost.qpos.set(from);mj.mj_forward(kin.model,kin.ghost);const initial=collisionDistances(p);
 for(let i=0;i<=20;i++){
  const x=i/20,q=from.map((v,j)=>v+(to[j]-v)*x);kin.ghost.qpos.set(q);mj.mj_forward(kin.model,kin.ghost);
  const distances=collisionDistances(p);
  // Actual contact may sit inside the planning buffer. Allow a retreat that does not deepen that
  // existing contact; rejecting its starting point would prevent every possible recovery motion.
  if(distances.some((d,j)=>d<.006&&(initial[j]<-.001||d<Math.min(.006,initial[j])-.0001)))return false;
 }return true;
}

export function inverseKinematics(p,target,seed=p.profile.home,yaw){
 let best=rawIK(p,target,seed,yaw);if(!['panda','ur5e'].includes(p.profile.id))return best;
 if(best.error<.026&&!selfContacts(p,best.q)&&jointPathClear(p,seed,best.q))return best;
 const seeds=[p.profile.home];
 if(p.profile.id==='ur5e')for(const elbow of[-2.2,2.2])for(const wrist of[-1.57,1.57]){const q=[...p.profile.home];q[2]=elbow;q[4]=wrist;seeds.push(q)}
 for(const shoulder of[-.9,.9])for(const elbow of[-.7,.7]){const q=[...p.profile.home];q[2]=shoulder;q[4]=elbow;seeds.push(q)}
 let accepted=null;
 for(const initial of seeds){const candidate=rawIK(p,target,initial,yaw);
  if(candidate.error<.026&&!selfContacts(p,candidate.q)){
   if(!accepted||candidate.error<accepted.error)accepted=candidate;
   if(jointPathClear(p,seed,candidate.q))return candidate;
  }
 }
 if(accepted)return accepted;
 return {...best,error:Math.max(best.error,.027),collision:true};
}
// Try a high clearance pose if the straight joint curve crosses the elbow/wrist. Every segment
// is sampled against the same contact proxies; an unresolved curve is rejected rather than animated.
export function motionRoute(p,from,to){
 if(jointPathClear(p,from,to))return [to];
 for(const target of[[.42,0,.40],[.38,-.15,.38],[.38,.15,.38]]){
  const middle=inverseKinematics(p,target,from);
  if(middle.error<.026&&jointPathClear(p,from,middle.q)&&jointPathClear(p,middle.q,to))return [middle.q,to];
 }
 throw Error('No clear arm path; reset the scene before retrying.');
}
