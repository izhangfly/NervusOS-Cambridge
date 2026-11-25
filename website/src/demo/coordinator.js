// Local shared-workcell coordination. Providers keep their own joint solvers; resource ownership
// and touch barriers determine when a shared phase may start. This is a simulator, not a safety PLC.
export class Coordinator {
 constructor(){this.clear()}
 clear(){this.leases=new Map();this.grips=new Set();this.barriers=new Set();this.anchor=null}
 reserve(resource,owner){const current=this.leases.get(resource);if(current!==undefined&&current!==owner)throw Error('Shared work area is already reserved');this.leases.set(resource,owner)}
 release(resource,owner){if(this.leases.get(resource)!==owner)throw Error('Only the owning provider may release the work area');this.leases.delete(resource)}
 confirmGrip(id,ok){if(ok)this.grips.add(id);else this.grips.delete(id)}
 barrier(name,ids){if(ids.some(id=>!this.grips.has(id)))throw Error('Waiting for every required physical grip');this.barriers.add(name)}
 require(name){if(!this.barriers.has(name))throw Error('Shared motion cannot start before its grip barrier')}
 hold(owner,target){this.reserve('fixture',owner);this.anchor={owner,target:[...target]}}
 insertion(owner,holder,target,closed){if(this.anchor?.owner!==holder||!closed||target.some((v,i)=>Math.abs(v-this.anchor.target[i])>1e-5))throw Error('The support provider must keep the fixture still during insertion');this.reserve('insertion',owner)}
}
