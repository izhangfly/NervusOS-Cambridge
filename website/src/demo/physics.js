import loadMujoco from '@mujoco/mujoco';import wasmUrl from '@mujoco/mujoco/mujoco.wasm?url';import{robotProfile}from'./robots.js';import{loadRobotAssets}from'./assets.js';
export{createModel,objectPosition,pinchPosition,attach,detach,moveRigid,garmentState,settleGarments}from'./model.js';
let wasm;export const HOME=robotProfile('piper').home;
// MuJoCo (WebAssembly) plus the robot's description; runs inside a physics worker (cell.worker.js)
export async function loadPhysics(id='piper'){
 if(!wasm)wasm=loadMujoco({locateFile:p=>p.endsWith('.wasm')?wasmUrl:p}).catch(e=>{wasm=null;throw e});
 const[mj,assets]=await Promise.all([wasm,loadRobotAssets(id)]);return{mj,...assets};
}
export{toolPosition,inverseKinematics}from'./kinematics.js';
