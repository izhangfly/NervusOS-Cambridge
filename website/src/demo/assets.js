import{robotProfile}from'./robots.js';
// A robot's MJCF description and STL meshes. The physics worker compiles them; the page draws the meshes.
// Both fetch the same URLs, so the second download comes from the HTTP cache.
const cache=new Map();
export function loadRobotAssets(id){
 const profile=robotProfile(id);
 if(!cache.has(profile.id))cache.set(profile.id,(async()=>{const response=await fetch(profile.model);if(!response.ok)throw Error('Robot model unavailable');const xml=await response.text();const names=[...new Set([...xml.matchAll(/<mesh[^>]*file="([^"]+)"/g)].map(x=>x[1]))];const meshes=Object.fromEntries(await Promise.all(names.map(async name=>{const r=await fetch(`/models/${profile.id}/assets/${name}`);if(!r.ok)throw Error('Robot mesh unavailable');return[name,new Uint8Array(await r.arrayBuffer())]})));return{id:profile.id,xml,meshes}})().catch(e=>{cache.delete(profile.id);throw e}));
 return cache.get(profile.id);
}
