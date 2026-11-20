import{garmentMesh}from'./cloth.js';
const label=(en,zh)=>({en,zh});
export const COLORS=['#e96b57','#d8ae41','#4b87bb','#64a47d','#a57cb7','#eb9455','#62aeb1'];
export const BLOCKS=['cube','brick','sphere','cylinder','triangle','arch','star'];
export const BASKETS={blocks:[.18,.19,.025],cars:[.33,.19,.025],plush:[.48,.19,.025]};
export const WASHER=[.34,.20,.132],BASKET=[.27,-.20,.018],DESK=[.43,-.19,.012];
const box=(id,size,position,color,extra={})=>({id,shape:'box',size,position,color,...extra});
function basket(id,pos,size=[.135,.13,.075],color='#bed0be'){
 const[x,y,z]=pos,[w,d,h]=size;
 return[box(id+'-floor',[w,d,.009],[x,y,z],color),box(id+'-front',[w,.006,h],[x,y-d/2,z+h/2],color),box(id+'-back',[w,.006,h],[x,y+d/2,z+h/2],color),box(id+'-left',[.006,d,h],[x-w/2,y,z+h/2],color),box(id+'-right',[.006,d,h],[x+w/2,y,z+h/2],color)];
}
export function garment(kind,position,size=.13){return garmentMesh(kind,position,size)}
function plush(position){
 const points=[],uv=[],indices=[],segments=8,rings=4,r=.027;
 points.push([position[0],position[1],position[2]+r]);uv.push(.5,1);
 for(let j=1;j<rings;j++)for(let i=0;i<segments;i++){const a=i/segments*2*Math.PI,b=j/rings*Math.PI;points.push([position[0]+r*Math.sin(b)*Math.cos(a),position[1]+r*.8*Math.sin(b)*Math.sin(a),position[2]+r*Math.cos(b)]);uv.push(i/segments,1-j/rings)}
 const bottom=points.length;points.push([position[0],position[1],position[2]-r]);uv.push(.5,0);
 for(let i=0;i<segments;i++)indices.push(0,1+i,1+(i+1)%segments);
 for(let j=0;j<rings-2;j++)for(let i=0;i<segments;i++){const a=1+j*segments+i,b=1+j*segments+(i+1)%segments,c=a+segments,d=b+segments;indices.push(a,c,b,b,c,d)}
 for(let i=0;i<segments;i++)indices.push(bottom,1+(rings-2)*segments+(i+1)%segments,1+(rings-2)*segments+i);
 return{points,indices,uv};
}
const rigid=(id,kind,pos,color,size=[.034,.034,.032],extra={})=>({id,kind,position:pos,color,size,proxy:kind==='sphere'?'sphere':kind==='cylinder'||kind==='housing'||kind==='bearing'?'cylinder':'box',...extra});
export function makeScene(app,seed=Math.floor(Math.random()*0xffffffff)){
 let state=seed>>>0;const rand=()=>{state=(1664525*state+1013904223)>>>0;return state/4294967296};
 const scene={id:app.id,seed,static:[],props:[],items:[],labels:[],conveyor:null};
 if(app.id==='laundry'){
  scene.static.push(...basket('laundry',BASKET,[.215,.18,.095],'#d8d0c2'));
  const[x,y,z]=WASHER;scene.static.push(box('washer-floor',[.18,.18,.01],[x,y,.005],'#a7b7c3'));
  for(let i=0;i<16;i++){const a=i/16*Math.PI*2;scene.static.push(box('drum-'+i,[.012,.043,.116],[x+.084*Math.cos(a),y+.084*Math.sin(a),.061],'#cbd3d8',{rotation:[0,0,a]}))}
  scene.props.push({kind:'washer',position:[x,y,0]},{kind:'laundry-basket',position:[...BASKET]});
  scene.labels.push({text:label('Top-loading washer','波轮洗衣机'),position:[x,y+.12,.23]},{text:label('Laundry basket','衣篮'),position:[.26,-.33,.12]},{text:label('Table pile','桌面衣物'),position:[.45,-.29,.09]});
  const kinds=['trousers','jacket','tshirt','trousers','jacket','tshirt'],textures={tshirt:'cotton',jacket:'knit',trousers:'denim'},colors=['#476484','#ba7456','#e6c462','#4e7593','#80a891','#cd725d'];
  kinds.forEach((kind,i)=>{const area=i<3?'basket':'desk',center=i<3?BASKET:DESK,pos=[center[0]+(i%3-1)*.005,center[1]+(i%3-1)*.007,.041+(i%3)*.018];scene.items.push({id:'garment_'+i,kind,area,position:pos,color:colors[i],fabric:textures[kind],soft:true,mesh:garment(kind,pos),mass:.026,bend:kind==='trousers'?2500:kind==='jacket'?700:1200})});
 }else if(app.id==='kitting'){
  Object.entries(BASKETS).forEach(([kind,pos],i)=>{scene.static.push(...basket(kind,pos,[.132,.128,.080],['#7ca68c','#769cc0','#ba967c'][i]));scene.labels.push({text:label({blocks:'Blocks · 7 shapes',cars:'Toy cars',plush:'Soft animals'}[kind],{blocks:'积木 · 七种形状',cars:'玩具汽车',plush:'柔软玩偶'}[kind]),position:[pos[0],pos[1]+.06,.17]})});
  const specs=[...BLOCKS.map(kind=>({kind,category:'blocks'})),...Array.from({length:3},()=>({kind:'car',category:'cars'})),...Array.from({length:3},()=>({kind:'plush',category:'plush'}))];
  const shuffled=[...specs].sort(()=>rand()-.5);
  shuffled.forEach((spec,i)=>{const pos=[(i===12?.34:.20+Math.floor(i/4)*.037+(i%4)*.077)+(rand()-.5)*.018,-.25+Math.floor(i/4)*.076+(rand()-.5)*.018,.028],color=COLORS[Math.floor(rand()*COLORS.length)];const item=rigid('toy_'+i,spec.kind,pos,color,spec.kind==='car'?[.06,.03,.024]:[.035,.035,.035],{category:spec.category,angle:rand()*Math.PI});if(spec.kind==='plush'){item.soft=true;item.mesh=plush([pos[0],pos[1],.033]);item.mass=.025;item.bend=500;item.fabric='fleece';item.position[2]=.033}scene.items.push(item)});
 }else if(app.id==='quality'||app.id==='assembly'){
  const quality=app.id==='quality',y=quality?-.18:.18;
  scene.conveyor={position:[.30,y,0],station:[.35,y,.055],quality};scene.props.push({kind:'conveyor',position:[.30,y,0]});scene.static.push(box('belt-bed',[1.30,.17,.012],[.30,y,.029],'#424b4e'));
  if(quality){scene.static.push(...basket('pass',[.26,.19,.018],[.18,.16,.055],'#75a98a'),...basket('rework',[.47,.19,.018],[.18,.16,.055],'#c68d63'));scene.labels.push({text:label('PASS','合格'),position:[.26,.25,.14]},{text:label('REWORK','返工'),position:[.47,.25,.14]});
   for(let i=0;i<6;i++)scene.items.push(rigid('housing_'+i,'housing',[.56+i*.052,y,.057],rand()>.33?'#8baba5':'#cf836a',[.031,.024,.025],{finish:rand()>.38?'pass':'rework',onBelt:true}));
  }else{scene.static.push(...basket('feed',[.31,-.20,.017],[.27,.18,.028],'#bdc9d6'));scene.labels.push({text:label('Bearing feeder','轴承供料盘'),position:[.31,-.29,.12]},{text:label('Assembly cell','装配工位'),position:[.35,.25,.16]});
   for(let i=0;i<6;i++){scene.items.push(rigid('bearing_'+i,'bearing',[.22+(i%3)*.075,-.235+Math.floor(i/3)*.078,.043],'#b7c4d4',[.019,.019,.017]));scene.items.push(rigid('carrier_'+i,'carrier',[.56+i*.052,y,.052],'#657a8c',[.075,.065,.020],{onBelt:true,carrier:true}))}
  }
 }else{scene.static=[...app.environment];scene.items=[rigid('workpiece','cube',app.object.position,app.object.color,app.object.size)];scene.labels=[]}
 return scene;
}
export function classifyToy(item){if(item.soft||item.fabric==='fleece')return'plush';if(item.kind==='car')return'cars';if(BLOCKS.includes(item.kind))return'blocks';throw Error('Unknown toy geometry')}
export function classifyFinish(item){return item.finish==='pass'?'pass':'rework'}
