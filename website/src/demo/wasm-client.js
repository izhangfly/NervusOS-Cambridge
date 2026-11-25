import bundles from'./model-bundles.json';
// Compile once asynchronously; structured cloning shares compiled code with each worker.
let compiled;
export function preparePhysics(){
 if(!compiled)compiled=(async()=>{const response=await fetch(import.meta.env.BASE_URL+bundles.engine.wasm,{cache:'force-cache'});if(!response.ok)throw Error('Physics engine unavailable');return WebAssembly.compileStreaming(response)})().catch(e=>{compiled=null;throw e});
 return compiled;
}
