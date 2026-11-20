import{objectPosition,pinchPosition}from'./model.js';import{BASKETS,BASKET,WASHER,DESK,classifyToy,classifyFinish}from'./scene.js';
const text=(en,zh)=>({en,zh}),step=(target,en,zh,extra={})=>({target,label:text(en,zh),duration:.65,...extra});
export function planJobs(p,action){
 const items=[...p.items.values()],jobs=[];
 if(p.scene.id==='laundry'){
  // Garments are chosen when each job starts, never in advance: always the one on top of its pile
  // (pulling one from underneath drags the rest along), and never one that is already in the washer.
  jobs.push({type:'lid'});
  if(action.id==='all')for(const _ of items.filter(x=>x.area==='desk'))jobs.push({type:'transfer',pick:'desk',dest:[...BASKET],category:'basket',label:text('Collect table garment','收集桌面衣物')});
  for(const _ of items.filter(x=>action.id==='all'||x.area==='basket'))jobs.push({type:'transfer',pick:action.id==='all'?'any':'basket',dest:[WASHER[0],WASHER[1],.025],category:'washer',label:text('Load garment','放入衣物')});
 }else if(p.scene.id==='kitting'){
  // Classify from observed demo features rather than storing a route in each app.
  for(const item of items){const category=classifyToy(item);jobs.push({type:'transfer',id:item.id,category,dest:BASKETS[category],label:text({blocks:'Block',cars:'Toy car',plush:'Soft animal'}[category],{blocks:'积木',cars:'玩具汽车',plush:'柔软玩偶'}[category])})}
 }else if(p.scene.id==='quality'){const st=p.scene.conveyor.station;for(const item of items.filter(x=>x.onBelt).sort((a,b)=>a.position[0]-b.position[0])){const category=classifyFinish(item);jobs.push({type:'index',id:item.id,dest:st},{type:'transfer',id:item.id,category,dest:[category==='pass'?.26:.47,.19,.036],label:text(category==='pass'?'Pass housing':'Rework housing',category==='pass'?'合格壳体':'返工壳体')})}}
 else if(p.scene.id==='assembly'){
  // the belt brings each carrier to the station; the seated module rides on as the next one arrives
  const st=p.scene.conveyor.station;for(const carrier of items.filter(x=>x.carrier).sort((a,b)=>a.position[0]-b.position[0])){const n=carrier.id.split('_')[1];jobs.push({type:'index',id:carrier.id,dest:st},{type:'transfer',id:'bearing_'+n,category:'assembled',dest:[st[0],st[1],.079],carrier:carrier.id,label:text('Seat bearing','放入轴承')})}
  jobs.push({type:'outfeed',distance:.95});
 }
 else{jobs.push({type:'legacy',action})}
 return jobs;
}
const near=(pos,c,r)=>Math.hypot(pos[0]-c[0],pos[1]-c[1])<r;
export const inWasher=(p,item)=>{const c=objectPosition(p,item);return near(c,WASHER,.1)&&c[2]<.2};
// how high a garment lies in its pile: the mean height of its vertices (a single corner can poke up from below)
function topVertex(p,item){return objectPosition(p,item)[2]}
// the garment on top of the chosen pile, or null when that pile is empty
export function pickTop(p,pick){
 const cands=[...p.items.values()].filter(it=>it.soft&&!inWasher(p,it)&&(pick==='any'||(pick==='desk'?near(objectPosition(p,it),DESK,.13):near(objectPosition(p,it),BASKET,.14))));
 let best=null,bz=-1;for(const it of cands){const z=topVertex(p,it);if(z>bz){bz=z;best=it}}return best;
}
export function jobSteps(p,job){
 if(job.pick){const it=pickTop(p,job.pick);if(!it)return[];job.id=it.id;(p.trace||(p.trace=[])).push({pick:job.pick,id:it.id,category:job.category})}
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
export function resultText(p,counts){switch(p.scene.id){case'laundry':return text(`${counts.washer||0} garments loaded. The lid was opened by the gripper.`,`${counts.washer||0} 件衣物已放入洗衣机，机盖由夹爪打开。`);case'kitting':return text(`All 13 toys sorted: ${counts.blocks||0} blocks, ${counts.cars||0} cars and ${counts.plush||0} soft animals.`,`十三件玩具已归类：${counts.blocks||0} 件积木、${counts.cars||0} 辆汽车、${counts.plush||0} 只柔软玩偶。`);case'quality':return text(`Batch inspected: ${counts.pass||0} pass, ${counts.rework||0} rework.`,`批次检查完成：${counts.pass||0} 件合格、${counts.rework||0} 件返工。`);case'assembly':return text(`${counts.assembled||0} wheel modules assembled and conveyed out.`,`${counts.assembled||0} 个轮组已装配并沿产线送出。`);default:return text('Route complete.','路线完成。')}}

// ---------------------------------------------------------------- physical grasp of a rigid part
// No weld joint: the fingers turn across two parallel faces, close with the gripper's limited force,
// and friction at the pads carries the part. Touch sensors in both pads confirm the grip.
const quatAxes=(w,x,y,z)=>[[1-2*(y*y+z*z),2*(x*y+w*z),2*(x*z-w*y)],[2*(x*y-w*z),1-2*(x*x+z*z),2*(y*z+w*x)],[2*(x*z+w*y),2*(y*z-w*x),1-2*(x*x+y*y)]];
export function graspPlan(p,item){
 const q=Array.from(p.data.qpos).slice(item.qadr,item.qadr+7),c=q.slice(0,3),[ax,ay]=quatAxes(q[3],q[4],q[5],q[6]),gap=2*p.profile.open-.008;
 let yaw,width,height;
 if(item.proxy==='box'){
  const [sx,sy,sz]=item.size,opts=[[ax,sx],[ay,sy]].filter(([,w])=>w<=gap).sort((a,b)=>a[1]-b[1]);
  const [axis,w]=opts[0]||[ax,Math.min(sx,sy)];yaw=Math.atan2(axis[1],axis[0]);width=w;height=sz;
 }else{const r=item.proxy==='sphere'?item.size[0]*.56:item.size[0];width=2*r;height=item.proxy==='sphere'?2*r:item.size[2];yaw=Math.atan2(c[1],c[0])+Math.PI/2}
 // pad centre at the part's middle; the PiPER's pads sit 15 mm behind its fingertips (the tool point)
 const padOffset=p.profile.id==='piper'?.015:0,z=Math.max(.006,c[2]-padOffset);
 return{center:c,grasp:[c[0],c[1],z],yaw,width,height,padOffset};
}
function rigidTransfer(p,job){
 const item=p.items.get(job.id),g=graspPlan(p,item),high=Math.max(.27,g.grasp[2]+.14),yaw=g.yaw;
 const release=job.category==='assembled'?job.dest[2]+.012-g.padOffset+g.height/2:job.dest[2]+.07;
 const dest=[job.dest[0],job.dest[1],release],hover=[dest[0],dest[1],high],over=[g.grasp[0],g.grasp[1],high];
 const s=(t,en,zh,x={})=>step(t,en,zh,{yaw,...x});
 return[
  s(over,`Wrist scan · ${job.label.en}`,`腕部扫描 · ${job.label.zh}`,{vision:job.category,item:job.id,duration:.7}),
  s([g.grasp[0],g.grasp[1],g.grasp[2]+.05],'Turn the fingers across two flat faces','转动夹爪对准两个平行面',{duration:.6}),
  s(g.grasp,'Lower the open fingers around the part','张开夹爪下降到零件两侧',{duration:.7}),
  s(g.grasp,'Squeeze · limited gripper force','夹紧 · 限力',{grip:true,duration:.5}),
  s(g.grasp,'Touch sensors · check the grip','触觉传感器 · 确认夹持',{grip:true,event:'grip-check',item:job.id,duration:.25}),
  s([g.grasp[0],g.grasp[1],high],'Lift · the part is held by friction','抬起 · 靠摩擦力夹持',{grip:true,duration:.95}),
  s(hover,`Transfer to ${job.category}`,`转运到${job.category}`,{grip:true,duration:1.0}),
  s(dest,'Lower over the receiving place','下降到放置位置',{grip:true,duration:.8}),
  s(dest,'Open the fingers · release','张开夹爪 · 放下',{event:'release',item:job.id,duration:.45}),
  s(hover,'Retreat · check placement','抬起 · 检查放置',{duration:.9}),
 ];
}

// ---------------------------------------------------------------- physical grasp of a soft item
// Garments: straddle the highest fold of the chosen garment, fingertips just under its top but never
// below the surface it lies on, squeeze, and lift only if both pads feel the fabric. A retry comes in at
// right angles. Plush animals: grasp around the body like a ball.
function softTransfer(p,job){
 const item=p.items.get(job.id),a=item.vertexAdr*3,f=p.data.flexvert_xpos;let top=0,low=1e9;
 for(let i=0;i<item.vertexNum;i++){const z=f[a+i*3+2];if(z>f[a+top*3+2])top=i;low=Math.min(low,z)}
 const pin=[f[a+top*3],f[a+top*3+1],f[a+top*3+2]],plush=item.kind==='plush',c=objectPosition(p,item);
 const tries=(job.tries=(job.tries||0)+1),yaw=Math.atan2(pin[1],pin[0])+Math.PI/2+(tries%2?0:Math.PI/2);
 const pad=p.profile.id==='piper'?.015:0;
 // fingertips press ~3 mm into the fabric top, so closing drags a fold up between the pads
 const grasp=plush?[c[0],c[1],Math.max(.006,c[2]-pad)]:[pin[0],pin[1],Math.max(low+.004,Math.min(pin[2]-.004,low+.009))];
 const high=Math.max(.27,grasp[2]+.15),over=[grasp[0],grasp[1],high];
 const drop=job.category==='washer'?.205:job.dest[2]+.13,dest=[job.dest[0],job.dest[1],drop],hover=[dest[0],dest[1],high];
 const s=(t,en,zh,x={})=>step(t,en,zh,{yaw,...x});
 return[
  s(over,`Wrist scan · ${job.label.en}`,`腕部扫描 · ${job.label.zh}`,{vision:job.category,item:job.id,duration:.7}),
  s([grasp[0],grasp[1],grasp[2]+.05],plush?'Turn the fingers around the toy':'Find the highest fold',plush?'夹爪对准玩偶':'找到最高的褶皱',{duration:.6}),
  s(grasp,plush?'Lower around the toy':'Lower the fingertips beside the fold',plush?'下降到玩偶两侧':'指尖下降到褶皱两侧',{duration:.7}),
  s(grasp,plush?'Squeeze the soft body':'Close slowly · gather a fold of fabric',plush?'夹住柔软身体':'慢慢合拢 · 收拢布料褶皱',{grip:true,duration:plush?.55:1.1}),
  s(grasp,'Touch sensors · check the grip','触觉传感器 · 确认夹持',{grip:true,event:'grip-check',item:job.id,soft:true,duration:.25}),
  s(over,'Lift slowly · the fabric hangs from the pinch','缓慢抬起 · 布料从夹点垂下',{grip:true,duration:1.2}),
  s(hover,`Transfer to ${job.category}`,`转运到${job.category}`,{grip:true,duration:1.1}),
  s(dest,'Lower the draping fabric','放低垂落的布料',{grip:true,duration:.85}),
  s(dest,'Open the fingers · release','张开夹爪 · 松开',{event:'release',item:job.id,duration:.45}),
  s(hover,'Retreat · check placement','抬起 · 检查放置',{duration:.9}),
 ];
}
