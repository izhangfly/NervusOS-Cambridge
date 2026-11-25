import{robotProfile}from'./robots.js';
import bundles from'./model-bundles.json';import{decodeVisualBundle}from'./model-assets.js';
// One versioned display bundle per arm; only the renderer downloads it.
const cache=new Map(),descriptions=new Map();
export function loadRobotAssets(id){
 id=robotProfile(id).id;
 if(!cache.has(id))cache.set(id,(async()=>{const response=await fetch(import.meta.env.BASE_URL+bundles[id].visual,{cache:'force-cache'});if(!response.ok)throw Error('Robot model unavailable');return decodeVisualBundle(await response.arrayBuffer())})().catch(e=>{cache.delete(id);throw e}));
 return cache.get(id);
}
export function loadRobotDescription(id){
 id=robotProfile(id).id;
 if(!descriptions.has(id))descriptions.set(id,(async()=>{const response=await fetch(import.meta.env.BASE_URL+bundles[id].physics,{cache:'force-cache'});if(!response.ok)throw Error('Robot physics unavailable');return{id,xml:await response.text(),meshes:{}}})().catch(e=>{descriptions.delete(id);throw e}));
 return descriptions.get(id);
}
