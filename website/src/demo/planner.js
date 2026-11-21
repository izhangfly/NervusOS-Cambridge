import{objectPosition,pinchPosition,garmentState}from'./model.js';import{BASKETS,BASKET,WASHER,DESK,classifyToy,classifyFinish}from'./scene.js';
const text=(en,zh)=>({en,zh}),step=(target,en,zh,extra={})=>({target,label:text(en,zh),duration:.65,...extra});
export function planJobs(p,action){
 const items=[...p.items.values()],jobs=[];
 if(p.scene.id==='laundry'){
  // Garments are chosen when each job starts, never in advance: always the one on top of its pile
  // (pulling one from underneath drags the rest along), and never one that is already in the washer.
  jobs.push({type:'lid'});
  // Load from each existing pile directly: an unnecessary restack in the basket created tangles and extra grasps.
  for(const _ of items.filter(x=>action.id==='all'||x.area==='basket'))jobs.push({type:'transfer',pick:action.id==='all'?'any':'basket',dest:[WASHER[0],WASHER[1],.025],category:'washer',label:text('Load garment','放入衣物')});
 }else if(p.scene.id==='kitting'){
  // Classify from observed demo features rather than storing a route in each app.
  const slots={blocks:0,cars:0,plush:0};
  for(const item of items){const category=classifyToy(item);jobs.push({type:'transfer',id:item.id,category,dest:toyDestination(category,slots[category]++),label:text({blocks:'Block',cars:'Toy car',plush:'Soft animal'}[category],{blocks:'积木',cars:'玩具汽车',plush:'柔软玩偶'}[category])})}
 }else if(p.scene.id==='quality'){const st=p.scene.conveyor.station,slots={pass:0,rework:0};for(const item of items.filter(x=>x.onBelt).sort((a,b)=>a.position[0]-b.position[0])){const category=classifyFinish(item),n=slots[category]++,dest=[(category==='pass'?.26:.47)+(n%2-.5)*.072,.19+(Math.floor(n/2)%2-.5)*.064,.036+Math.floor(n/4)*.025];jobs.push({type:'index',id:item.id,dest:st},{type:'transfer',id:item.id,category,dest,label:text(category==='pass'?'Pass housing':'Rework housing',category==='pass'?'合格壳体':'返工壳体')})}}
 else if(p.scene.id==='assembly'){
  // the belt brings each carrier to the station; the seated module rides on as the next one arrives
  const st=p.scene.conveyor.station;for(const carrier of items.filter(x=>x.carrier).sort((a,b)=>a.position[0]-b.position[0])){const n=carrier.id.split('_')[1];jobs.push({type:'index',id:carrier.id,dest:st},{type:'transfer',id:'bearing_'+n,category:'assembled',dest:[st[0],st[1],.079],carrier:carrier.id,label:text('Seat bearing','放入轴承')})}
  jobs.push({type:'outfeed',distance:.95});
 }
 else{jobs.push({type:'legacy',action})}
 return jobs;
}
// Spread a batch inside each basket rather than dropping every item onto the same point.
function toyDestination(category,index){const c=BASKETS[category],offset=category==='blocks'?[(index%3-1)*.04,(Math.floor(index/3)-1)*.04]:category==='cars'?[0,(index-1)*.036]:[(index%2-.5)*.056,(Math.floor(index/2)-.5)*.056];return[c[0]+offset[0],c[1]+offset[1],c[2]]}
export function inToyBasket(p,item,category){const c=objectPosition(p,item),b=BASKETS[category];return c.every(Number.isFinite)&&Math.abs(c[0]-b[0])<.066&&Math.abs(c[1]-b[1])<.064&&c[2]>.012&&c[2]<.14}
export function inQualityBasket(p,item,category){const c=objectPosition(p,item),floor=p.scene.static.find(s=>s.id===category+'-floor');return c.every(Number.isFinite)&&Math.abs(c[0]-floor.position[0])<floor.size[0]/2&&Math.abs(c[1]-floor.position[1])<floor.size[1]/2&&c[2]>.012&&c[2]<.14}
export function qualityCounts(p){const counts={pass:0,rework:0};for(const item of p.items.values()){const category=classifyFinish(item);if(inQualityBasket(p,item,category))counts[category]++}return counts}
export function toyCounts(p){const counts={blocks:0,cars:0,plush:0};for(const item of p.items.values()){const category=classifyToy(item);if(inToyBasket(p,item,category))counts[category]++}return counts}
const near=(pos,c,r)=>Math.hypot(pos[0]-c[0],pos[1]-c[1])<r;
// in the drum: centre inside the 84 mm drum radius and below the rim (a garment lying on the deck is not in)
export const inWasher=(p,item)=>{const c=objectPosition(p,item);return near(c,WASHER,.075)&&c[2]<.11};
// how high a garment lies in its pile: the mean height of its vertices (a single corner can poke up from below)
function topVertex(p,item){return objectPosition(p,item)[2]}
// the garment on top of the chosen pile, or null when that pile is empty
export function pickTop(p,pick,candidates){
 const cands=[...p.items.values()].filter(it=>it.soft&&(!candidates||candidates.includes(it.id))&&!inWasher(p,it)&&(pick==='any'||(pick==='desk'?near(objectPosition(p,it),DESK,.13):near(objectPosition(p,it),BASKET,.14))));
 let best=null,bz=-Infinity;for(const it of cands){const z=topVertex(p,it)-(it.failedPicks||0)*.3;if(z>bz){bz=z;best=it}}return best;
}
export function jobSteps(p,job){
 if(job.pick){const it=pickTop(p,job.pick,job.candidates);if(!it)return[];if(job.id&&job.id!==it.id)garmentState(p,p.items.get(job.id),'free');job.id=it.id;garmentState(p,it,'held');(p.trace||(p.trace=[])).push({pick:job.pick,id:it.id,category:job.category})}
 if(job.type==='lid'){
  const hinge=[.34,.29,.132],r=.14,arc=a=>[hinge[0],hinge[1]-r*Math.cos(a)+.016*Math.sin(a),hinge[2]+r*Math.sin(a)+.016*Math.cos(a)+.012];
  // The fingers close across the handle bar (along the world y axis, across its 10 mm diameter) with the
  // fingertips just above the lid, then follow the hinge arc: the lid turns because the hand holds it.
  const pad=p.profile.id==='piper'?.015:0,yaw=Math.PI/2,handle=a=>{const h=arc(a);return[h[0],h[1],h[2]-.012-Math.min(pad,.004)*Math.cos(a)]};
  const s=(t,en,zh,x={})=>step(t,en,zh,{yaw,...x});
  return[s([.34,.15,.27],'Wrist camera · locate lid handle','腕部摄像头 · 定位机盖把手',{vision:'lid'}),s(handle(0).map((v,i)=>i===2?v+.05:v),'Turn the fingers across the handle','转动夹爪对准把手'),s(handle(0),'Lower around the handle bar','下降到把手两侧',{duration:.6}),s(handle(0),'Squeeze the handle','夹紧把手',{grip:true,duration:.5}),s(handle(0),'Touch sensors · check the grip','触觉传感器 · 确认夹持',{grip:true,event:'grip-check',duration:.25}),...[.35,.75,1.15,1.52].map(a=>s(handle(a),'Lift the hinged lid','提起铰链机盖',{grip:true,duration:.8})),s(handle(1.52),'Release the open lid','松开已打开的机盖',{event:'lid-release',duration:.5}),step([.28,0,.30],'Move clear of the lid','离开机盖',{duration:.7})];
 }
 if(job.type==='transfer')return p.items.get(job.id).soft?softTransfer(p,job):rigidTransfer(p,job);
 if(job.type==='transfer'){
  const item=p.items.get(job.id),pin=pinchPosition(p,item),from=[pin[0],pin[1],Math.max(.046,pin[2]+.012)],high=Math.max(.27,from[2]+.12);
  const basketBottom=job.dest[2],drop=item.soft?.205:job.category==='assembled'?.105:basketBottom+.077;
  const dest=[job.dest[0],job.dest[1],drop],hover=[dest[0],dest[1],high];
  return[step([from[0],from[1],high],`Wrist scan · ${job.label.en}`,`腕部扫描 · ${job.label.zh}`,{vision:job.category,item:job.id,duration:.7}),step(from,'Locate the grasp point','定位抓取点',{duration:.7}),step(from,item.soft?'Pinch the flexible surface':'Close the gripper',item.soft?'夹住柔软表面':'合拢夹爪',{event:'grasp',item:job.id,grip:true,duration:.45}),step([from[0],from[1],high],'Lift clear of the table','抬起并离开桌面',{grip:true,duration:.95}),step(hover,`Transfer to ${job.category}`,`转运到${{blocks:'积木篮',cars:'汽车篮',plush:'玩偶篮',basket:'衣篮',washer:'洗衣机',pass:'合格通道',rework:'返工通道',assembled:'装配工位'}[job.category]}`,{grip:true,duration:1.0}),step(dest,item.soft?'Lower the draping fabric':'Align over the receiving pocket',item.soft?'放低垂落的布料':'对准接收位置',{grip:true,duration:.85}),step(dest,'Open the gripper · release','打开夹爪 · 松开物品',{event:'release',item:job.id,duration:.45}),step(hover,'Retreat · check placement','抬起 · 检查放置',{duration:1.0})];
 }
 if(job.type==='legacy')return job.action.steps.map(s=>({...s,item:'workpiece',event:s.carry?'grasp':s.release?'release':undefined}));
 return[];
}
export function resultText(p,counts){switch(p.scene.id){case'laundry':{const n=[...p.items.values()].filter(it=>it.soft&&inWasher(p,it)).length,r=counts.missed||0;   // what is in the drum at the end, not per-job tallies
   return text(`${n} garment${n===1?'':'s'} in the washer${r?`, ${r} re-picked after the wrist camera saw a miss`:''}. The lid was opened by the gripper.`,`洗衣机内共 ${n} 件衣物${r?`，腕部摄像头发现 ${r} 次未放好并重新抓取`:''}。机盖由夹爪打开。`)}case'kitting':{const n=(counts.blocks||0)+(counts.cars||0)+(counts.plush||0),all=p.items.size;return text(`${n===all?`All ${all}`:`${n} of ${all}`} toys sorted: ${counts.blocks||0} blocks, ${counts.cars||0} cars and ${counts.plush||0} soft animals.`,`${n===all?`${all} 件玩具全部`:`${all} 件玩具中 ${n} 件`}已归类：${counts.blocks||0} 件积木、${counts.cars||0} 辆汽车、${counts.plush||0} 只柔软玩偶。`)}case'quality':return text(`Batch inspected: ${counts.pass||0} pass, ${counts.rework||0} rework.`,`批次检查完成：${counts.pass||0} 件合格、${counts.rework||0} 件返工。`);case'assembly':return text(`${counts.assembled||0} wheel modules assembled and conveyed out.`,`${counts.assembled||0} 个轮组已装配并沿产线送出。`);default:return text('Route complete.','路线完成。')}}

// ---------------------------------------------------------------- physical grasp of a rigid part
// No weld joint: the fingers turn across two parallel faces, close with the gripper's limited force,
// and friction at the pads carries the part. Touch sensors in both pads confirm the grip.
const quatAxes=(w,x,y,z)=>[[1-2*(y*y+z*z),2*(x*y+w*z),2*(x*z-w*y)],[2*(x*y-w*z),1-2*(x*x+z*z),2*(y*z+w*x)],[2*(x*z+w*y),2*(y*z-w*x),1-2*(x*x+y*y)]];
// How much room the two open fingers have at a candidate angle: the distance from the finger blocks to
// the nearest other part or fixed obstacle taller than the grasp (basket walls, washer). Higher is better.
export function fingerClearance(p,item,c,yaw,open,minTop=c[2]-.005){
 const ux=Math.cos(yaw),uy=Math.sin(yaw),reach=open+.026,half=.03;let clear=.2;
 for(const o of p.items.values()){if(o===item||o.soft)continue;const q=objectPosition(p,o),dx=q[0]-c[0],dy=q[1]-c[1],along=Math.abs(dx*ux+dy*uy),off=Math.abs(-dx*uy+dy*ux);const rad=Math.max(o.size[0],o.size[1])/2;if(along<reach+rad)clear=Math.min(clear,Math.max(0,off-half-rad))}
 // each open finger is a ~25 x 52 mm block: test its corners and edge midpoints against obstacles taller than the grasp
 const pts=[];for(const sgn of[1,-1])for(const a of[-.0125,0,.0125])for(const b of[-.026,0,.026]){const d=open+.0125+a;pts.push([c[0]+sgn*d*ux-b*uy,c[1]+sgn*d*uy+b*ux])}
 for(const b of p.scene.static||[]){if(b.shape!=='box')continue;const top=b.position[2]+b.size[2]/2;if(top<minTop)continue;
  for(const[fx,fy]of pts){const gx=Math.abs(fx-b.position[0])-b.size[0]/2,gy=Math.abs(fy-b.position[1])-b.size[1]/2;clear=Math.min(clear,Math.max(0,Math.hypot(Math.max(gx,0),Math.max(gy,0))-.002))}}
 return clear;
}
export function graspPlan(p,item){
 const q=Array.from(p.data.qpos).slice(item.qadr,item.qadr+7),c=q.slice(0,3),[ax,ay]=quatAxes(q[3],q[4],q[5],q[6]),gap=2*p.profile.open-.008;
 let yaw,width,height;
 if(item.proxy==='box'){
  const [sx,sy,sz]=item.size,opts=[[ax,sx],[ay,sy]].filter(([,w])=>w<=gap).sort((a,b)=>(fingerClearance(p,item,c,Math.atan2(b[0][1],b[0][0]),b[1]/2+.01)-fingerClearance(p,item,c,Math.atan2(a[0][1],a[0][0]),a[1]/2+.01))||a[1]-b[1]);
  const [axis,w]=opts[0]||[ax,Math.min(sx,sy)];yaw=Math.atan2(axis[1],axis[0]);width=w;height=sz;
 }else{
  // round part: any angle grips it, so choose the one whose finger line passes furthest from its neighbours
  const r=item.proxy==='sphere'?item.size[0]*.56:item.size[0];width=2*r;height=item.proxy==='sphere'?2*r:item.size[2];
  const base=Math.atan2(c[1],c[0])+Math.PI/2;let best=-1;yaw=base;
  for(let k=0;k<12;k++){const t=base+k*Math.PI/12,cl=fingerClearance(p,item,c,t,r+.01);if(cl>best+1e-4){best=cl;yaw=t}}
 }
 // pad centre at the part's middle; the PiPER's pads sit 15 mm behind its fingertips (the tool point)
 // fingertips stay 3 mm above whatever the part rests on (table, tray floor), never pushing into it
 const padOffset=p.profile.id==='piper'?.015:0,z=Math.max(.006,c[2]-height/2+.003,c[2]-padOffset);
 // open only as wide as the part needs, so the fingers don't land on its neighbours
 return{center:c,grasp:[c[0],c[1],z],yaw,width,height,padOffset,openTo:Math.min(p.profile.open,width/2+.01)};
}
function rigidTransfer(p,job){
 const item=p.items.get(job.id),g=graspPlan(p,item),high=Math.max(.27,g.grasp[2]+.14),yaw=g.yaw;
 let to=job.dest;if(job.carrier){const cp=objectPosition(p,job.carrier);to=[cp[0],cp[1],cp[2]-.004]}   // pocket floor, read from the carrier's real pose
 const release=job.category==='assembled'?to[2]+g.height/2+.006-g.padOffset:p.scene.id==='kitting'?.15:to[2]+.07;
 const dest=[to[0],to[1],release],hover=[dest[0],dest[1],high],over=[g.grasp[0],g.grasp[1],high];
 const s=(t,en,zh,x={})=>step(t,en,zh,{yaw,openTo:g.openTo,...x});
 return[
  s(over,`Wrist scan · ${job.label.en}`,`腕部扫描 · ${job.label.zh}`,{vision:job.category,item:job.id,duration:.7}),
  s([g.grasp[0],g.grasp[1],g.grasp[2]+.05],'Turn the fingers across two flat faces','转动夹爪对准两个平行面',{duration:.6}),
  s(g.grasp,'Lower the open fingers around the part','张开夹爪下降到零件两侧',{duration:.7}),
  s(g.grasp,'Squeeze · limited gripper force','夹紧 · 限力',{grip:true,duration:.5}),
  // touch in both pads, and the fingers stopped at the part's width (not on an edge, a corner or a neighbour)
  s(g.grasp,'Touch sensors · check the grip','触觉传感器 · 确认夹持',{grip:true,event:'grip-check',item:job.id,expect:g.width/2,duration:.25}),
  s([g.grasp[0],g.grasp[1],g.grasp[2]+.04],'Lift · touch sensors watch for slip','抬起 · 触觉监测滑脱',{grip:true,event:'grip-check',item:job.id,slip:true,duration:.4}),
  s([g.grasp[0],g.grasp[1],high],'Lift · the part is held by friction','抬起 · 靠摩擦力夹持',{grip:true,duration:.65}),
  s(hover,`Transfer to ${job.category}`,`转运到${job.category}`,{grip:true,duration:1.0}),
  s(dest,'Lower over the receiving place','下降到放置位置',{grip:true,duration:.8}),
  s(dest,'Open the fingers · release','张开夹爪 · 放下',{event:'release',item:job.id,duration:.45,openTo:p.profile.open}),
  s(hover,'Retreat · check placement','抬起 · 检查放置',{duration:.9,openTo:p.profile.open}),
 ];
}

// ---------------------------------------------------------------- physical grasp of a soft item
// Garments: straddle the highest fold of the chosen garment, fingertips just under its top but never
// below the surface it lies on, squeeze, and lift only if both pads feel the fabric. A retry comes in at
// right angles. Plush animals: grasp around the body like a ball.
function softTransfer(p,job){
 const item=p.items.get(job.id),a=item.vertexAdr*3,f=p.data.flexvert_xpos,open=p.profile.open,V=i=>[f[a+i*3],f[a+i*3+1],f[a+i*3+2]];
 let low=1e9;const order=[];for(let i=0;i<item.vertexNum;i++){low=Math.min(low,f[a+i*3+2]);order.push(i)}order.sort((i,j)=>f[a+j*3+2]-f[a+i*3+2]);
 const plush=item.kind==='plush',c=objectPosition(p,item),tries=(job.tries=(item.graspAttempts||0)+1);item.graspAttempts=tries;
 // Pinch point: the highest point of the garment where both open fingers clear the basket walls (and anything
 // else taller than the cloth) by 20 mm, at one of four finger angles. A fold draped over a wall is skipped for
 // a lower point inside, so a finger never lands on the far side of the wall. A retry prefers a new angle.
 let pin=V(order[0]),yaw=Math.atan2(pin[1],pin[0])+Math.PI/2,bestKey=-1;
 for(const i of order){const v=V(i);
  for(let k=0;k<4;k++){const t=Math.atan2(v[1],v[0])+Math.PI/2+k*Math.PI/4,cl=fingerClearance(p,item,[v[0],v[1],low+.006],t,open,low+.002);
   const key=(cl>=.02?1+v[2]:cl)+(k===(tries-1)%4?.002:0);if(key>bestKey){bestKey=key;pin=v;yaw=t}}}
 // Panda's smaller fingertip pads need fabric on both sides of the pinch. Choose the midpoint of
 // two adjacent cloth vertices, rather than an isolated peak at the hem that only touches one pad.
 if(!plush&&p.profile.id==='panda'){
  const seen=new Set();let best=-Infinity;
  for(let t=0;t<item.mesh.indices.length;t+=3)for(let e=0;e<3;e++){
   const i=item.mesh.indices[t+e],j=item.mesh.indices[t+(e+1)%3],key=Math.min(i,j)+':'+Math.max(i,j);if(seen.has(key))continue;seen.add(key);
   const A=V(i),B=V(j),d=Math.hypot(B[0]-A[0],B[1]-A[1]);if(d<.012||d>.036||Math.abs(B[2]-A[2])>.02)continue;
   const mid=A.map((v,k)=>(v+B[k])/2),angle=Math.atan2(B[1]-A[1],B[0]-A[0]),z=Math.max(.006,mid[2]-.001),cl=fingerClearance(p,item,[mid[0],mid[1],z],angle,.028,z-.009);
   let palm=.2;const ux=Math.cos(angle),uy=Math.sin(angle);
   for(const box of p.scene.static){if(box.shape!=='box'||box.position[2]+box.size[2]/2<z+.045)continue;
    for(const a of[-.1,0,.1])for(const b of[-.03,0,.03]){const x=mid[0]+a*ux-b*uy,y=mid[1]+a*uy+b*ux,dx=Math.abs(x-box.position[0])-box.size[0]/2,dy=Math.abs(y-box.position[1])-box.size[1]/2;palm=Math.min(palm,Math.hypot(Math.max(dx,0),Math.max(dy,0)))}
   }
   const clear=Math.min(cl,palm),score=(clear>=.008?1+mid[2]:clear);if(score>best){best=score;pin=mid;yaw=angle}
  }
 }
 // what the cloth rests on around the pinch: its lowest point within 35 mm
 let local=pin[2];for(let i=0;i<item.vertexNum;i++){const v=V(i);if(Math.hypot(v[0]-pin[0],v[1]-pin[1])<.035)local=Math.min(local,v[2])}
 const pad=p.profile.id==='piper'?.015:0;
 // fingertips 2 mm below the cloth's mid-plane where it rests (just above the table, basket floor or the garment
 // beneath, which the hand passes through while that one sleeps), so the pads close across the sheet and gather it
 const grasp=plush?[c[0],c[1],Math.max(.006,c[2]-pad)]:[pin[0],pin[1],Math.max(.006,p.profile.id==='panda'?pin[2]-.001:local-.002)];
 const high=Math.max(.27,grasp[2]+.15),over=[grasp[0],grasp[1],high];
 // lower the hanging garment until it rests on the drum floor (or the basket floor / the clothes already there)
 // before letting go: it can't catch on the rim, and the open fingers slide out of a garment that is already supported
 const washer=job.category==='washer',drop=washer?(plush?.165:.115):job.dest[2]+(plush?.13:.075),dest=[job.dest[0],job.dest[1]-(washer?.015:0),drop],hover=[dest[0],dest[1],high];
 // over the washer the hand turns to lie along the hinge (fingers closing along x): a wide hand (Panda's is 20 cm)
 // pointing across the drum would meet the open lid as it lowers
 const put=washer?{yaw:0}:{};
 // on cloth the fingers stop 2 mm short of closed: squeezing a thin sheet all the way shut pushes it out of the pinch
 const openTo=plush?Math.min(p.profile.open,.027+.01):Math.min(p.profile.open,.028),closeTo=plush?0:.002,s=(t,en,zh,x={})=>step(t,en,zh,{yaw,openTo,closeTo,...x});
 return[
  s(over,`Wrist scan · ${job.label.en}`,`腕部扫描 · ${job.label.zh}`,{vision:job.category,item:job.id,duration:.7}),
  s([grasp[0],grasp[1],grasp[2]+.05],plush?'Turn the fingers around the toy':'Find the highest fold',plush?'夹爪对准玩偶':'找到最高的褶皱',{duration:.6}),
  s(grasp,plush?'Lower around the toy':'Lower the fingertips beside the fold',plush?'下降到玩偶两侧':'指尖下降到褶皱两侧',{duration:.7}),
  s(grasp,plush?'Squeeze the soft body':'Close slowly · gather a fold of fabric',plush?'夹住柔软身体':'慢慢合拢 · 收拢布料褶皱',{grip:true,ramp:!plush,duration:plush?.55:1.1}),
  s(grasp,plush?'Touch sensors · check the grip':'Grip pads on · touch sensors check the fabric',plush?'触觉传感器 · 确认夹持':'抓取垫启动 · 触觉确认布料',{grip:true,adhere:!plush,event:'grip-check',item:job.id,soft:true,duration:.3}),
  s([grasp[0],grasp[1],grasp[2]+.05],'Lift slowly · touch sensors watch for slip','缓慢抬起 · 触觉监测滑脱',{grip:true,adhere:!plush,event:'grip-check',item:job.id,soft:true,slip:true,duration:.6}),
  s(over,'Clear of the pile · touch check again','离开衣堆 · 再次触觉确认',{grip:true,adhere:!plush,event:'grip-check',item:job.id,soft:true,slip:true,duration:.8}),
  s(hover,`Transfer to ${job.category}`,`转运到${job.category}`,{grip:true,adhere:!plush,duration:1.1,...put}),
  s(dest,'Lower the draping fabric','放低垂落的布料',{grip:true,adhere:!plush,duration:.85,...put}),
  s(dest,'Open the fingers · release','张开夹爪 · 松开',{event:'release',item:job.id,duration:.45,openTo:p.profile.open,...put}),
  s(hover,'Retreat · check placement','抬起 · 检查放置',{duration:.9,openTo:p.profile.open,...put}),
 ];
}
