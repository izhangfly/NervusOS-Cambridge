import {useEffect,useState} from 'react';

const active=()=>document.fullscreenElement||document.webkitFullscreenElement;
export function useSceneFullscreen(panelRef){
 const [expanded,setExpanded]=useState(false);
 useEffect(()=>{
  const panel=panelRef.current;
  const changed=()=>setExpanded(active()===panel);
  const escape=e=>{if(e.key==='Escape'&&!active())setExpanded(false)};
  document.addEventListener('fullscreenchange',changed);
  document.addEventListener('webkitfullscreenchange',changed);
  document.addEventListener('keydown',escape);
  return()=>{
   document.removeEventListener('fullscreenchange',changed);
   document.removeEventListener('webkitfullscreenchange',changed);
   document.removeEventListener('keydown',escape);
   if(active()===panel){const exit=document.exitFullscreen||document.webkitExitFullscreen;if(exit)Promise.resolve(exit.call(document)).catch(()=>{})}
  };
 },[]);
 useEffect(()=>{
  if(!expanded)return;
  const previous=document.body.style.overflow;
  document.body.style.overflow='hidden';
  return()=>{document.body.style.overflow=previous};
 },[expanded]);
 async function toggle(){
  if(expanded){
   const exit=document.exitFullscreen||document.webkitExitFullscreen;
   if(active()===panelRef.current&&exit){try{await exit.call(document)}catch{}}
   setExpanded(false);return;
  }
  setExpanded(true);
  const panel=panelRef.current,enter=panel.requestFullscreen||panel.webkitRequestFullscreen;
  // iPhone browsers without element fullscreen keep the same scene in a fixed, viewport-sized panel.
  if(enter){try{await enter.call(panel)}catch{}}
 }
 return [expanded,toggle];
}
