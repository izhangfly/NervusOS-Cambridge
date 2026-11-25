import loadMujoco from '@mujoco/mujoco';import bundles from'./model-bundles.json';import{robotProfile}from'./robots.js';import{loadRobotDescription}from'./assets.js';
export{createModel,objectPosition,pinchPosition,attach,detach,moveRigid,garmentState,settleGarments}from'./model.js';
let wasm;export const HOME=robotProfile('piper').home;
// MuJoCo (WebAssembly) plus the robot's description; runs inside a physics worker (cell.worker.js)
export async function loadPhysics(id='piper',module){
 if(!wasm)wasm=loadMujoco(module?{instantiateWasm(imports,receive){const instance=new WebAssembly.Instance(module,imports);receive(instance,module);return instance.exports}}:{locateFile:p=>p.endsWith('.wasm')?import.meta.env.BASE_URL+bundles.engine.wasm:p}).catch(e=>{wasm=null;throw e});
 const[mj,assets]=await Promise.all([wasm,loadRobotDescription(id)]);return{mj,...assets};
}
export{toolPosition,inverseKinematics}from'./kinematics.js';
