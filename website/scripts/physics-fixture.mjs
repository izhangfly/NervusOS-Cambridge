import fs from 'node:fs';
import load from '@mujoco/mujoco';
export const mj=await load();
export function engine(id,{physicsOnly=false}={}){
 const root=new URL('../public/models/'+id+'/',import.meta.url);
 const xml=fs.readFileSync(new URL(id+'.xml',root),'utf8');
 if(physicsOnly)return{mj,id,xml:xml.replace(/<mesh\b[^>]*\/>/g,'').replace(/<geom\b[^>]*type="mesh"[^>]*\/>/g,'').replace(/<geom\b[^>]*mesh="[^"]+"[^>]*\/>/g,''),meshes:{}};
 const names=[...new Set([...xml.matchAll(/<mesh[^>]*file="([^"]+)"/g)].map(m=>m[1]))];
 return {mj,id,xml,meshes:Object.fromEntries(names.map(n=>[n,new Uint8Array(fs.readFileSync(new URL('assets/'+n,root)))]))};
}
