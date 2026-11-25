import {engine} from './physics-fixture.mjs';
import {CooperativeCell} from '../src/demo/cooperative-cell.js';
const ids=['piper','panda','ur5e','gen3'],engines=Object.fromEntries(ids.map(id=>[id,engine(id,{physicsOnly:true})]));
let failed=0;
const pairs=process.argv.includes('--all')?ids.flatMap(a=>ids.map(b=>[a,b])):process.argv.includes('--mixed')?ids.flatMap((a,i)=>ids.slice(i+1).map(b=>[a,b])):ids.map(id=>[id,id]);
for(const [left,right] of pairs)for(const [app,action] of [['carry','carry'],['carry','return'],['fixture','assemble'],['fixture','swap']]){
 let result;const events=[],c=new CooperativeCell([engines[left],engines[right]],e=>events.push(e));
 try{c.setApp(app);c.start(action);for(let n=0;n<10000&&c.action;n++)c.advance(.04,Infinity);result=events.findLast(e=>e.result);if(c.action||result?.status!=='complete')throw Error(result?.result?.en||'Incomplete task');console.log(JSON.stringify({left,right,app,action,status:'PASS',positions:c.p.items.map(it=>[it.id,c.pos(it.id)])}))}
 catch(e){failed++;console.log(JSON.stringify({left,right,app,action,status:'FAIL',error:e.message,stage:c.action?.index,poses:c.p&&[c.pose(0),c.pose(1)],positions:c.p?.items.map(it=>[it.id,c.pos(it.id)])}))}
 finally{c.p?.dispose()}
}
console.log(JSON.stringify({failed,status:failed?'FAIL':'PASS'}));process.exitCode=failed?1:0;
