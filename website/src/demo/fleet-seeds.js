import{makeScene}from'./scene.js';
// Comparable layouts for household apps; independent, distinct inspection batches for QC.
export function fleetSceneSeeds(app,seed,ids){
 const out={},used=new Set();let candidate=seed>>>0;
 for(const id of ids){
  if(app!=='quality'){out[id]=seed;continue}
  let signature;
  do{signature=makeScene({id:'quality'},candidate).items.map(i=>i.finish).join(',');if(used.has(signature))candidate=(Math.imul(candidate,1664525)+1013904223)>>>0}while(used.has(signature));
  out[id]=candidate;used.add(signature);candidate=(Math.imul(candidate,1664525)+1013904223)>>>0;
 }
 return out;
}
