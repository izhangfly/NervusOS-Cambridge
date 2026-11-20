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
 }else if(p.scene.id==='quality')for(const item of items){const category=classifyFinish(item);jobs.push({type:'index',id:item.id,dest:p.scene.conveyor.station},{type:'transfer',id:item.id,category,dest:[category==='pass'?.26:.47,.19,.036],label:text(category==='pass'?'Pass housing':'Rework housing',category==='pass'?'合格壳体':'返工壳体')})}
 else if(p.scene.id==='assembly')for(const item of items.filter(x=>x.kind==='bearing')){const carrier='carrier_'+item.id.split('_')[1];jobs.push({type:'index',id:carrier,dest:p.scene.conveyor.station},{type:'transfer',id:item.id,category:'assembled',dest:[.35,.18,.079],carrier,label:text('Seat bearing','放入轴承')},{type:'outfeed',id:carrier,bearing:item.id,dest:[-.24,.18,.055]})}
 else{jobs.push({type:'legacy',action})}
 return jobs;
}
const near=(pos,c,r)=>Math.hypot(pos[0]-c[0],pos[1]-c[1])<r;
export const inWasher=(p,item)=>{const c=objectPosition(p,item);return near(c,WASHER,.1)&&c[2]<.2};
function topVertex(p,item){const a=item.vertexAdr*3,f=p.data.flexvert_xpos;let z=-1;for(let i=0;i<item.vertexNum;i++)z=Math.max(z,f[a+i*3+2]);return z}
// the garment on top of the chosen pile, or null when that pile is empty
export function pickTop(p,pick){
 const cands=[...p.items.values()].filter(it=>it.soft&&!inWasher(p,it)&&(pick==='any'||(pick==='desk'?near(objectPosition(p,it),DESK,.13):near(objectPosition(p,it),BASKET,.14))));
 let best=null,bz=-1;for(const it of cands){const z=topVertex(p,it);if(z>bz){bz=z;best=it}}return best;
}
export function jobSteps(p,job){
 if(job.pick){const it=pickTop(p,job.pick);if(!it)return[];job.id=it.id}
 if(job.type==='lid'){
  const hinge=[.34,.29,.132],r=.14,arc=a=>[hinge[0],hinge[1]-r*Math.cos(a)+.016*Math.sin(a),hinge[2]+r*Math.sin(a)+.016*Math.cos(a)+.012];
  return[step([.34,.15,.27],'Wrist camera · locate lid handle','腕部摄像头 · 定位机盖把手',{vision:'lid'}),step(arc(0),'Approach the lid handle','靠近机盖把手'),step(arc(0),'Grip the handle','夹住把手',{grip:true,event:'lid-grip',duration:.5}),...[.35,.75,1.15,1.52].map(a=>step(arc(a),'Lift the hinged lid','提起铰链机盖',{grip:true,duration:.8})),step(arc(1.52),'Release the open lid','松开已打开的机盖',{event:'lid-release',duration:.5}),step([.28,0,.30],'Move clear of the lid','离开机盖',{duration:.7})];
 }
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
