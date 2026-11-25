import test from'node:test';import assert from'node:assert/strict';import{WorkerClock}from'../src/demo/worker-clock.js';
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
test('physics advances without renderer ticks and stops when hidden, paused or asleep',async()=>{
 let steps=0,paused=false,awake=true;const clock=new WorkerClock({advance:()=>steps++,frame:()=>{},blocked:()=>paused,awake:()=>awake});
 try{clock.wake();await sleep(35);assert.ok(steps>0);clock.configure({visible:false});const hidden=steps;await sleep(25);assert.equal(steps,hidden);paused=true;clock.configure({visible:true});await sleep(25);assert.equal(steps,hidden);paused=false;clock.wake();await sleep(25);assert.ok(steps>hidden);awake=false;clock.wake();const idle=steps;await sleep(25);assert.equal(steps,idle);assert.equal(clock.timer,null);}finally{clock.stop()}
});
test('manual commands wake a settled cell long enough for actuator motion',async()=>{
 let steps=0;const clock=new WorkerClock({advance:()=>steps++,frame:()=>{},blocked:()=>false,awake:()=>false});
 try{clock.wake();await sleep(20);assert.equal(steps,0);clock.wake(true);await sleep(25);assert.ok(steps>0);clock.configure({visible:false});const stopped=steps;await sleep(20);assert.equal(steps,stopped);}finally{clock.stop()}
});
