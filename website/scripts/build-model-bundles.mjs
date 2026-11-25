import{readFile,writeFile,readdir,unlink,mkdir}from'node:fs/promises';
import{createHash}from'node:crypto';
import{physicsXML}from'../src/demo/model-assets.js';
const root=new URL('../public/models/',import.meta.url),bundles={};
for(const id of ['piper','panda','ur5e','gen3']){
 const folder=new URL(id+'/',root),xml=await readFile(new URL(id+'.xml',folder),'utf8');
 const names=[...new Set([...xml.matchAll(/<mesh[^>]*file="([^"]+)"/g)].map(m=>m[1]))];
 const data=await Promise.all(names.map(name=>readFile(new URL('assets/'+name,folder))));
 const header=Buffer.from(JSON.stringify({id,xml,files:names.map((name,i)=>({name,bytes:data[i].length}))})),size=Buffer.alloc(4);size.writeUInt32LE(header.length);
 const packed=Buffer.concat([size,header,...data]),physical=Buffer.from(physicsXML(xml)),hash=b=>createHash('sha256').update(b).digest('hex').slice(0,12);
 const visual='visual-'+hash(packed)+'.bin',physics='physics-'+hash(physical)+'.xml';
 for(const name of await readdir(folder))if(/^(visual-[a-f0-9]+\.bin|physics-[a-f0-9]+\.xml)$/.test(name)&&![visual,physics].includes(name))await unlink(new URL(name,folder));
 await writeFile(new URL(visual,folder),packed);await writeFile(new URL(physics,folder),physical);
 bundles[id]={visual:'models/'+id+'/'+visual,physics:'models/'+id+'/'+physics};
}
const wasm=await readFile(new URL('../node_modules/@mujoco/mujoco/mujoco.wasm',import.meta.url)),engine=new URL('engine/',root),wasmName='mujoco-'+createHash('sha256').update(wasm).digest('hex').slice(0,12)+'.wasm';
await mkdir(engine,{recursive:true});await writeFile(new URL(wasmName,engine),wasm);
bundles.engine={wasm:'models/engine/'+wasmName};
await writeFile(new URL('../src/demo/model-bundles.json',import.meta.url),JSON.stringify(bundles,null,2)+'\n');
console.log('Prepared four versioned visual bundles and mesh-free physics descriptions');
