// Fleet controls follow every visible cell, while telemetry follows the selected arm.
export function fleetState(cells,focus,{ready=true,stopped=false}={}){
 const focused=cells.find(c=>c.id===focus)?.stats||{},states=cells.map(c=>c.stats.status||'loading');
 const status=stopped?'stopped':states.includes('error')?'error':!ready?'loading':states.includes('running')?'running':states.includes('paused')?'paused':states.every(s=>s==='complete')?'complete':focused.status||'idle';
 const out={...focused,status,ready};
 if(status==='running'||status==='paused'){
  const active=cells.find(c=>c.id===focus&&['running','paused'].includes(c.stats.status))||cells.find(c=>['running','paused'].includes(c.stats.status));
  if(active&&active.id!==focus){out.step=active.stats.step;out.result=null}
  out.progress=cells.reduce((n,c)=>n+(c.stats.progress||0),0)/Math.max(1,cells.length);
 }
 if(status==='complete'&&cells.length>1){out.result=Object.fromEntries(['en','zh'].map(lang=>[lang,cells.map(c=>`${c.profile.short}: ${c.stats.result?.[lang]||''}`).join(' · ')]))}
 return out;
}
