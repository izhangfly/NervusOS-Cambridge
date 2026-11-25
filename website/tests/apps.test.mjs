import test from'node:test';import assert from'node:assert/strict';import laundry from'../src/demo/apps/laundry.js';import toys from'../src/demo/apps/kitting.js';import quality from'../src/demo/apps/quality.js';import assembly from'../src/demo/apps/assembly.js';import teach from'../src/demo/apps/teach.js';import{makeScene,classifyToy,BLOCKS}from'../src/demo/scene.js';import{ROBOTS}from'../src/demo/robots.js';import{demoReceipt,validPurchases}from'../src/demo/marketplace.js';
const apps=[laundry,toys,quality,assembly,teach];
test('all five catalog apps have bilingual names, two tasks and packaged permissions',()=>{for(const app of apps){assert.ok(app.name.en&&app.name.zh&&app.description.en&&app.description.zh);assert.equal(app.actions.length,2);assert.equal(new Set(app.actions.map(a=>a.id)).size,2);assert.ok(app.permissions.length);for(const action of app.actions)assert.ok(action.name.en&&action.name.zh&&action.description.en&&action.description.zh)}});
test('household apps use wrist perception and factories have distinct paid applications',()=>{for(const app of[laundry,toys]){assert.equal(app.scope,'home');assert.equal(app.price,0);assert.ok(app.permissions.some(p=>p.id==='wrist-camera'))}assert.notEqual(quality.id,assembly.id);for(const app of[quality,assembly]){assert.equal(app.scope,'factory');assert.ok(app.price>0);assert.ok(app.publisher.en.includes('example'));assert.ok(app.permissions.some(p=>p.id==='line'))}});
test('a toy scatter has every requested family and all seven block shapes, with reproducible random layouts',()=>{const first=makeScene(toys,42),same=makeScene(toys,42),next=makeScene(toys,43);assert.deepEqual(first,same);assert.notDeepEqual(first.items,next.items);assert.equal(first.items.length,13);const count={};for(const item of first.items){const family=classifyToy(item);count[family]=(count[family]||0)+1}assert.deepEqual(count,{blocks:7,plush:3,cars:3});assert.deepEqual(new Set(first.items.filter(i=>classifyToy(i)==='blocks').map(i=>i.kind)),new Set(BLOCKS));assert.ok(first.items.filter(i=>i.kind==='plush').every(i=>i.soft&&i.mesh.points.length>20))});
test('laundry has a table pile, basket load, three garment patterns and physical deformable meshes',()=>{const scene=makeScene(laundry,42);assert.equal(scene.items.filter(i=>i.area==='basket').length,3);assert.equal(scene.items.filter(i=>i.area==='desk').length,3);assert.deepEqual(new Set(scene.items.map(i=>i.fabric)),new Set(['cotton','denim','knit']));for(const i of scene.items){assert.ok(i.soft);assert.ok(i.mesh.indices.length>30);assert.ok(i.mesh.indices.every(n=>n>=0&&n<i.mesh.points.length));assert.ok(i.mesh.points.flat().every(Number.isFinite))}});
test('profile switching changes joint count and limits while retaining the common tool contract',()=>{assert.equal(ROBOTS.piper.dof,6);assert.equal(ROBOTS.panda.dof,7);assert.notEqual(ROBOTS.piper.model,ROBOTS.panda.model);for(const r of Object.values(ROBOTS)){assert.equal(r.home.length,r.dof+2);assert.equal(r.limits.length,r.dof);assert.equal(r.gripper,r.dof);r.home.slice(0,r.dof).forEach((q,i)=>assert.ok(q>=r.limits[i][0]&&q<=r.limits[i][1]));assert.match(r.sdk,/github/)} });
test('demo receipts unlock both robot profiles and malformed entitlements are rejected',()=>{const receipt=demoReceipt(quality,1234);assert.equal(receipt.amount,quality.price);assert.equal(receipt.currency,'GBP');assert.deepEqual(receipt.profiles,['piper','panda']);assert.deepEqual(validPurchases({quality:receipt},apps),{quality:receipt});assert.deepEqual(validPurchases({quality:{...receipt,amount:1},laundry:receipt},apps),{});assert.deepEqual(validPurchases(null,apps),{})});

test('fine garment surfaces retain necklines, sleeves and the gap between trouser legs',async()=>{
 const {garmentOutline}=await import('../src/demo/cloth-mesh.js');
 const covers=(mesh,[x,y])=>{for(let i=0;i<mesh.indices.length;i+=3){const [a,b,c]=mesh.indices.slice(i,i+3).map(k=>mesh.uv.slice(2*k,2*k+2));const cross=(p,q)=>(q[0]-p[0])*(y-p[1])-(q[1]-p[1])*(x-p[0]);if(cross(a,b)>=-1e-9&&cross(b,c)>=-1e-9&&cross(c,a)>=-1e-9)return true}return false};
 const tee=garmentOutline('tshirt'),trousers=garmentOutline('trousers');
 assert.equal(covers(tee,[.5,.94]),false,'neck opening');assert.equal(covers(tee,[.9,.69]),true,'sleeve');
 assert.equal(covers(trousers,[.5,.25]),false,'leg gap');assert.equal(covers(trousers,[.5,.9]),true,'waist');
 assert.ok(tee.uv.some(v=>Math.abs(v*40-Math.round(v*40))>.01),'outline edges are clipped between grid samples');
});


test('LineSight batches vary in count and sequence with at least two parts of each finish',()=>{
 const mixes=new Set(),orders=new Set();
 for(let seed=1;seed<=80;seed++){
  const scene=makeScene(quality,seed),parts=scene.items;
  assert.equal(parts.length,6);assert.deepEqual(scene,makeScene(quality,seed));
  const pass=parts.filter(p=>p.finish==='pass').length;
  assert.ok(pass>=2&&pass<=4);mixes.add(pass);orders.add(parts.map(p=>p.finish).join(','));
  for(const p of parts)assert.equal(p.color,p.finish==='pass'?'#8baba5':'#cf836a');
 }
 assert.equal(mixes.size,3);assert.ok(orders.size>10);
});
