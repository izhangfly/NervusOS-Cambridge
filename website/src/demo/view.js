import * as THREE from'three';import{RoundedBoxGeometry}from'three/examples/jsm/geometries/RoundedBoxGeometry.js';import{GLTFLoader}from'three/examples/jsm/loaders/GLTFLoader.js';
export function primitive(parent,p){let g;if(p.shape==='cylinder'){g=new THREE.CylinderGeometry(p.size[0],p.size[0],p.size[1],32);g.rotateX(Math.PI/2)}else if(p.shape==='sphere')g=new THREE.SphereGeometry(p.size[0],20,14);else g=new THREE.BoxGeometry(...p.size);const m=new THREE.Mesh(g,new THREE.MeshStandardMaterial({color:p.color,roughness:.7,metalness:.10}));m.position.set(...p.position);if(p.rotation)m.rotation.set(...p.rotation);m.castShadow=true;m.receiveShadow=true;parent.add(m);return m}
export function fabricTexture(kind){const c=document.createElement('canvas');c.width=c.height=128;const ctx=c.getContext('2d');ctx.fillStyle='#aeaeae';ctx.fillRect(0,0,128,128);for(let y=0;y<128;y+=kind==='knit'?9:3)for(let x=0;x<128;x+=kind==='knit'?8:3){ctx.strokeStyle=(x+y)%2?'#8c8c8c':'#d0d0d0';ctx.lineWidth=kind==='fleece'?1.5:.6;ctx.beginPath();if(kind==='knit'){ctx.moveTo(x,y);ctx.lineTo(x+4,y+5);ctx.lineTo(x+8,y)}else{ctx.moveTo(x,y);ctx.lineTo(x+3,y+3)}ctx.stroke()}const t=new THREE.CanvasTexture(c);t.wrapS=t.wrapT=THREE.RepeatWrapping;t.repeat.set(3,3);return t}
export function softMesh(item){const g=new THREE.BufferGeometry();g.setAttribute('position',new THREE.Float32BufferAttribute(item.mesh.points.flat(),3));g.setAttribute('uv',new THREE.Float32BufferAttribute(item.mesh.uv,2));g.setIndex(item.mesh.indices);g.computeVertexNormals();const m=new THREE.Mesh(g,new THREE.MeshStandardMaterial({color:item.color,side:THREE.DoubleSide,roughness:item.fabric==='denim'?.9:.98,bumpMap:fabricTexture(item.fabric),bumpScale:item.kind==='plush'?.003:.001,metalness:0}));m.castShadow=true;m.receiveShadow=true;return m}
const sphere=(parent,r,pos,color)=>primitive(parent,{shape:'sphere',size:[r],position:pos,color});
function latheRing(parent,r,h,color){const pts=[new THREE.Vector2(r*.52,-h/2),new THREE.Vector2(r,-h/2),new THREE.Vector2(r,h/2),new THREE.Vector2(r*.52,h/2),new THREE.Vector2(r*.52,-h/2)],g=new THREE.LatheGeometry(pts,32);g.rotateX(Math.PI/2);const mesh=new THREE.Mesh(g,new THREE.MeshStandardMaterial({color,roughness:.27,metalness:.65}));parent.add(mesh);mesh.castShadow=true;return mesh}
export function toyMesh(item){const group=new THREE.Group(),c=item.color,s=item.size;
 if(item.kind==='tray'||item.kind==='fixture'){
  const fixture=item.kind==='fixture';primitive(group,{shape:'box',size:fixture?[.20,.36,.012]:[.20,.28,.014],position:[0,0,0],color:c});
  for(const sign of[-1,1]){
   if(fixture){primitive(group,{shape:'box',size:[.024,.124,.048],position:[sign*.054,0,.030],color:c});primitive(group,{shape:'box',size:[.084,.020,.048],position:[0,sign*.052,.030],color:c});primitive(group,{shape:'box',size:[.046,.134,.012],position:[-.04,sign*.093,.012],color:c})}
   if(fixture)for(const x of[-.10,.02])primitive(group,{shape:'box',size:[.008,.008,.052],position:[x,sign*.15,.040],color:c});
   const handle=primitive(group,{shape:'cylinder',size:[.008,fixture?.120:.080],position:[fixture?-.04:0,sign*.15,fixture?.065:.021],color:'#ddaa45'});handle.rotation.y=Math.PI/2;
  }
 }else if(item.kind==='car'){primitive(group,{shape:'box',size:[.060,.028,.015],position:[0,0,0],color:c});const cabin=primitive(group,{shape:'box',size:[.027,.024,.015],position:[-.006,0,.012],color:c});primitive(group,{shape:'box',size:[.002,.024,.009],position:[.008,0,.014],color:'#b1d9e4'});for(const x of[-.020,.020])for(const y of[-.019,.019]){const wheel=primitive(group,{shape:'cylinder',size:[.008,.006],position:[x,y,-.004],color:'#292d31'});wheel.rotation.x=Math.PI/2;sphere(group,.003,[x,y*1.15,-.004],'#c7c7c3')}
 }else if(item.kind==='housing'||item.kind==='bearing'){latheRing(group,s[0],s[2],c);if(item.kind==='housing'){primitive(group,{shape:'box',size:[.016,.008,.003],position:[.008,0,s[2]/2+.001],color:item.finish==='pass'?'#569d7a':'#d9644c'});if(item.finish==='rework')primitive(group,{shape:'box',size:[.004,.025,.003],position:[0,0,s[2]/2+.001],color:'#70372d'})}}
 else if(item.kind==='carrier'){// matches the physics: a floor plate and four walls around an empty 46 mm pocket
  primitive(group,{shape:'box',size:[.075,.065,.006],position:[0,0,-.007],color:c});for(const x of[-.03025,.03025])primitive(group,{shape:'box',size:[.0145,.065,.014],position:[x,0,.003],color:c});for(const y of[-.02775,.02775])primitive(group,{shape:'box',size:[.046,.0095,.014],position:[0,y,.003],color:c});primitive(group,{shape:'box',size:[.046,.046,.001],position:[0,0,-.0035],color:'#3d4b55'});for(const x of[-.03,.03])for(const y of[-.0265,.0265])sphere(group,.0025,[x,y,.0105],'#dae1e4')}
 else if(item.kind==='triangle'){const sh=new THREE.Shape();sh.moveTo(-.022,-.017);sh.lineTo(.022,-.017);sh.lineTo(0,.024);sh.closePath();const g=new THREE.ExtrudeGeometry(sh,{depth:.028,bevelEnabled:true,bevelSize:.002,bevelThickness:.002,bevelSegments:2,steps:1});g.translate(0,0,-.014);group.add(new THREE.Mesh(g,new THREE.MeshStandardMaterial({color:c,roughness:.5})))}
 else if(item.kind==='star'){const sh=new THREE.Shape();for(let i=0;i<10;i++){const a=i*Math.PI/5,r=i%2?.012:.024;const x=Math.cos(a)*r,y=Math.sin(a)*r;if(!i)sh.moveTo(x,y);else sh.lineTo(x,y)}sh.closePath();const g=new THREE.ExtrudeGeometry(sh,{depth:.02,bevelEnabled:false});g.translate(0,0,-.01);group.add(new THREE.Mesh(g,new THREE.MeshStandardMaterial({color:c,roughness:.55})))}
 else if(item.kind==='arch'){for(const x of[-.018,.018])primitive(group,{shape:'box',size:[.012,.025,.037],position:[x,0,0],color:c});primitive(group,{shape:'box',size:[.048,.025,.010],position:[0,0,.018],color:c})}
 else primitive(group,{shape:item.kind==='sphere'?'sphere':item.kind==='cylinder'?'cylinder':'box',size:item.kind==='sphere'?[.022]:item.kind==='cylinder'?[.019,.037]:item.kind==='brick'?[.055,.027,.029]:s,position:[0,0,0],color:c});
 group.traverse(m=>{if(m.isMesh){m.castShadow=true;m.receiveShadow=true}});return group;
}
export function plushDetail(item){const g=new THREE.Group(),c=item.color;const head=sphere(g,.018,[0,0,.033],c);head.material.bumpMap=fabricTexture('fleece');head.material.bumpScale=.002;for(const x of[-.014,.014])sphere(g,.008,[x,0,.047],c);for(const x of[-.008,.008])sphere(g,.002,[x,-.015,.037],'#282a2b');sphere(g,.003,[0,-.019,.030],'#f2e1c6');for(const x of[-.025,.025])sphere(g,.010,[x,0,-.004],c);return g}
let assets;
export async function loadProps(){if(!assets){const loader=new GLTFLoader();assets=Promise.all(['conveyor'].map(async n=>[n,(await loader.loadAsync('/models/props/'+n+'.glb')).scene])).then(Object.fromEntries)}return assets}
function cloned(g){const c=g.clone(true);c.traverse(o=>{if(o.isMesh){o.geometry=o.geometry.clone();o.material=o.material.clone();o.castShadow=true;o.receiveShadow=true}});return c}
export function propsView(scene,parent,assets){const belts=[];for(const prop of scene.props){const group=new THREE.Group();parent.add(group);group.position.set(...prop.position);
 if(prop.kind==='washer')group.add(washerModel());
 if(prop.kind==='laundry-basket')group.add(laundryBasket(prop.size,prop.color));
 if(prop.kind==='conveyor'){group.scale.x=1.45;const model=cloned(assets.conveyor);
  // the downloaded model's belt surface is one static grey-blue mesh: hide it and draw a belt that moves
  model.traverse(o=>{if(o.isMesh&&(Array.isArray(o.material)?o.material:[o.material]).some(m=>m.name==='mat17'))o.visible=false});group.add(model);
  model.traverse(o=>{if(o.isMesh&&o.name==='group600698695')o.visible=false});const belt=new THREE.Group();group.add(belt);belt.userData.tex=beltTexture();
  const surface=new THREE.Mesh(new THREE.BoxGeometry(.896,.145,.003),new THREE.MeshStandardMaterial({map:belt.userData.tex,roughness:.85,metalness:.05}));surface.position.set(0,0,.0365);surface.receiveShadow=true;group.add(surface);belts.push(belt);for(const x of[-.14,.16]){primitive(group,{shape:'box',size:[.011,.014,.125],position:[x,.104,.070],color:'#6e8c9c'});sphere(group,.006,[x,.101,.136],'#68bc92')};primitive(group,{shape:'box',size:[.045,.020,.027],position:[-.13,-.122,.040],color:'#586c74'})}
 }return belts}
// rubber belt: grey-blue with transverse ribs; scrolled by the belt's real travel (see Simulator.syncView)
function beltTexture(){const c=document.createElement('canvas');c.width=256;c.height=64;const x=c.getContext('2d');x.fillStyle='#455a65';x.fillRect(0,0,256,64);
 for(let i=0;i<256;i+=16){x.fillStyle='rgba(0,0,0,.22)';x.fillRect(i,0,3,64);x.fillStyle='rgba(255,255,255,.07)';x.fillRect(i+3,0,2,64)}
 for(let k=0;k<400;k++){x.fillStyle=`rgba(255,255,255,${Math.random()*.05})`;x.fillRect(Math.random()*256,Math.random()*64,2,1)}
 const t=new THREE.CanvasTexture(c);t.wrapS=t.wrapT=THREE.RepeatWrapping;t.repeat.set(8,1);t.colorSpace=THREE.SRGBColorSpace;t.anisotropy=4;return t}
export function disposeGroup(g){g?.traverse(o=>{o.geometry?.dispose();if(o.material){for(const m of Array.isArray(o.material)?o.material:[o.material]){m.map?.dispose();m.bumpMap?.dispose();m.dispose()}}})}

// ---------------------------------------------------------------- top-loading washer
// Procedural, built to the physics: drum radius 84 mm (walls up to 119 mm), deck at 132 mm, lid hinged at
// y +90 mm from the drum centre, handle bar 140 mm in front of the hinge. Units are metres.
const enamel=()=>new THREE.MeshPhysicalMaterial({color:'#f4f5f2',roughness:.32,metalness:0,clearcoat:.6,clearcoatRoughness:.25});
function perforated(){const c=document.createElement('canvas');c.width=256;c.height=128;const x=c.getContext('2d');x.fillStyle='#c9ced2';x.fillRect(0,0,256,128);
 for(let j=0;j<8;j++)for(let i=0;i<24;i++){x.fillStyle='#5d666c';x.beginPath();x.arc(i*10.7+(j%2)*5.3+4,j*16+8,2.3,0,7);x.fill()}
 for(let k=0;k<256;k+=2){x.fillStyle=`rgba(255,255,255,${.05+Math.random()*.05})`;x.fillRect(k,0,1,128)}
 const t=new THREE.CanvasTexture(c);t.wrapS=t.wrapT=THREE.RepeatWrapping;t.repeat.set(3,1);t.colorSpace=THREE.SRGBColorSpace;return t}
// A plastic laundry basket built to the size of its collision boxes (a floor and four 6 mm walls), so garments
// rest and catch exactly where they appear to: slotted sides, rounded corners, a rolled rim.
function rrPoint(s,hx,hy,r){   // point and outward normal at arc length s along a rounded rectangle
 const q=Math.PI*r/2,segs=[[2*hy,[1,0],[hx+r,-hy],[0,1]],[q,[hx,hy],0],[2*hx,[0,1],[hx,hy+r],[-1,0]],[q,[-hx,hy],Math.PI/2],[2*hy,[-1,0],[-hx-r,hy],[0,-1]],[q,[-hx,-hy],Math.PI],[2*hx,[0,-1],[-hx,-hy-r],[1,0]],[q,[hx,-hy],1.5*Math.PI]];
 for(let k=0;k<8;k++){const[len,a,b,c]=segs[k];if(s<=len||k===7){if(c)return[b[0]+c[0]*s,b[1]+c[1]*s,a[0],a[1]];const t=b+Math.min(s,len)/r;return[a[0]+r*Math.cos(t),a[1]+r*Math.sin(t),Math.cos(t),Math.sin(t)]}s-=len}
}
function basketSlots(){const c=document.createElement('canvas');c.width=64;c.height=128;const x=c.getContext('2d');x.fillStyle='#fff';x.fillRect(0,0,64,128);x.fillStyle='#000';
 for(const[y0,y1]of[[22,60],[68,106]]){x.beginPath();x.roundRect(23,y0,18,y1-y0,8);x.fill()}const t=new THREE.CanvasTexture(c);t.wrapS=THREE.RepeatWrapping;return t}
function laundryBasket([w,d,h],color='#e9ecec'){
 const g=new THREE.Group(),t=.003,r=.02,hx=w/2-r,hy=d/2-r,L=4*hx+4*hy+2*Math.PI*r,N=160,slots=Math.round(L/.022);
 const mat=new THREE.MeshStandardMaterial({color,roughness:.5,metalness:0,side:THREE.DoubleSide,alphaMap:basketSlots(),alphaTest:.5});mat.alphaMap.repeat.set(1,1);
 const wall=(o)=>{const pos=[],uv=[],idx=[];for(let i=0;i<=N;i++){const[x,y,nx,ny]=rrPoint(i/N*L,hx,hy,r);for(const z of[-.0045,h]){pos.push(x+nx*o,y+ny*o,z);uv.push(i/N*slots,(z+.0045)/(h+.0045))}}
  for(let i=0;i<N;i++){const a=2*i,b=a+2;idx.push(...(o>0?[a,b,b+1,a,b+1,a+1]:[a,b+1,b,a,a+1,b+1]))}
  const geo=new THREE.BufferGeometry();geo.setAttribute('position',new THREE.Float32BufferAttribute(pos,3));geo.setAttribute('uv',new THREE.Float32BufferAttribute(uv,2));geo.setIndex(idx);geo.computeVertexNormals();
  const m=new THREE.Mesh(geo,mat);m.castShadow=true;m.receiveShadow=true;g.add(m)};
 wall(t);wall(-t);
 const ring=[];for(let i=0;i<N;i++){const[x,y]=rrPoint(i/N*L,hx,hy,r);ring.push(new THREE.Vector3(x,y,h))}
 const rim=new THREE.Mesh(new THREE.TubeGeometry(new THREE.CatmullRomCurve3(ring,true),N,.0045,8,true),new THREE.MeshStandardMaterial({color,roughness:.45}));rim.castShadow=true;g.add(rim);
 const sh=new THREE.Shape(),R=r+t,X=hx,Y=hy;sh.absarc(X,-Y,R,-Math.PI/2,0);sh.absarc(X,Y,R,0,Math.PI/2);sh.absarc(-X,Y,R,Math.PI/2,Math.PI);sh.absarc(-X,-Y,R,Math.PI,1.5*Math.PI);
 const floor=new THREE.Mesh(new THREE.ExtrudeGeometry(sh,{depth:.009,bevelEnabled:false,curveSegments:12}),new THREE.MeshStandardMaterial({color:'#d9dede',roughness:.6}));floor.position.z=-.0045;floor.receiveShadow=true;g.add(floor);
 // hand holds in the two short ends
 for(const sx of[-1,1]){const hold=new THREE.Mesh(new THREE.CapsuleGeometry(.007,.04,4,12),new THREE.MeshStandardMaterial({color:'#5d6a70',roughness:.8}));hold.rotation.x=Math.PI/2;hold.rotation.z=Math.PI/2;hold.position.set(sx*(w/2+t+.0002),0,h-.016);hold.scale.set(1,1,.08);g.add(hold)}
 return g;
}
export function washerModel(){
 const g=new THREE.Group(),W=.23,D=.24,H=.122,cy=-.004;   // cabinet centred a little forward of the drum
 const add=(geo,mat,x,y,z)=>{const m=new THREE.Mesh(geo,mat);m.position.set(x,y,z);m.castShadow=true;m.receiveShadow=true;g.add(m);return m};
 // Four cabinet walls leave the drum hollow; a solid box would cover the clothes through the opening.
 const wall=.012;
 for(const y of[cy-(D-wall)/2,cy+(D-wall)/2])add(new RoundedBoxGeometry(W,wall,H,3,.004),enamel(),0,y,H/2);
 for(const x of[-(W-wall)/2,(W-wall)/2])add(new RoundedBoxGeometry(wall,D-2*wall,H,3,.004),enamel(),x,cy,H/2);
 add(new THREE.BoxGeometry(W-.01,D-.01,.008),new THREE.MeshStandardMaterial({color:'#2f3438',roughness:.7}),0,cy,.004);   // plinth
 // top deck with the round opening
 const deck=new THREE.Shape();const hw=W/2-.004,hd=D/2-.004;deck.moveTo(-hw,-hd+cy);deck.lineTo(hw,-hd+cy);deck.lineTo(hw,hd+cy);deck.lineTo(-hw,hd+cy);deck.lineTo(-hw,-hd+cy);
 const hole=new THREE.Path();hole.absarc(0,0,.088,0,Math.PI*2,true);deck.holes.push(hole);
 add(new THREE.ExtrudeGeometry(deck,{depth:.004,bevelEnabled:true,bevelThickness:.0015,bevelSize:.002,bevelSegments:2,curveSegments:48}),enamel(),0,0,H);
 // stainless drum, open at the top, perforated; impeller plate at the bottom
 const steel=new THREE.MeshStandardMaterial({map:perforated(),color:'#e8edf0',metalness:.85,roughness:.32,side:THREE.DoubleSide});
 add(new THREE.CylinderGeometry(.084,.084,.112,48,1,true),steel,0,0,.064).rotation.x=Math.PI/2;
 add(new THREE.TorusGeometry(.086,.003,8,64),new THREE.MeshStandardMaterial({color:'#d6dbde',metalness:.9,roughness:.25}),0,0,H+.004);
 const imp=add(new THREE.CylinderGeometry(.05,.062,.01,40),new THREE.MeshStandardMaterial({color:'#b9c1c6',metalness:.6,roughness:.4}),0,0,.013);imp.rotation.x=Math.PI/2;
 for(let i=0;i<6;i++){const r=add(new THREE.BoxGeometry(.04,.006,.008),new THREE.MeshStandardMaterial({color:'#aab3b8',metalness:.6,roughness:.4}),0,0,.021);r.rotation.z=i*Math.PI/3;r.position.set(.022*Math.cos(i*Math.PI/3),.022*Math.sin(i*Math.PI/3),.021)}
 // control console behind the lid hinge: display, buttons, dial
 add(new RoundedBoxGeometry(W,.028,.05,4,.008),enamel(),0,.104,H+.02);
 add(new THREE.BoxGeometry(.06,.002,.018),new THREE.MeshStandardMaterial({color:'#0e2a33',emissive:'#1d6b7d',emissiveIntensity:.6,roughness:.3}),.03,.0895,H+.024);
 const dial=add(new THREE.CylinderGeometry(.011,.012,.008,32),new THREE.MeshStandardMaterial({color:'#c3cbd0',metalness:.7,roughness:.3}),-.06,.09,H+.024);dial.rotation.x=Math.PI/2;
 for(const x of[-.025,-.012])add(new THREE.CylinderGeometry(.004,.004,.003,16),new THREE.MeshStandardMaterial({color:'#7e8b93',roughness:.5}),x,.0895,H+.024).rotation.x=Math.PI/2;
 return g;
}
// the lid, in its hinge frame (hinge on the x axis, lid extends toward -y); handle bar where the physics has it
export function washerLid(){
 const g=new THREE.Group();
 const frame=new THREE.Mesh(new RoundedBoxGeometry(.20,.154,.010,3,.004),enamel());frame.position.set(0,-.077,.002);frame.castShadow=true;g.add(frame);
 const glass=new THREE.Mesh(new RoundedBoxGeometry(.15,.104,.004,3,.0015),new THREE.MeshPhysicalMaterial({color:'#2c4652',roughness:.08,metalness:0,transmission:.25,transparent:true,opacity:.82,clearcoat:1}));glass.position.set(0,-.077,.007);g.add(glass);
 const dark=new THREE.MeshStandardMaterial({color:'#3a4a54',roughness:.45,metalness:.2});
 for(const x of[-.026,.026]){const post=new THREE.Mesh(new THREE.CylinderGeometry(.0045,.0045,.016,16),dark);post.rotation.x=Math.PI/2;post.position.set(x,-.140,.012);g.add(post)}
 const bar=new THREE.Mesh(new THREE.CapsuleGeometry(.005,.052,6,16),dark);bar.rotation.z=Math.PI/2;bar.position.set(0,-.140,.016);bar.castShadow=true;g.add(bar);
 return g;
}
