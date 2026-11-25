// The physics clock must not depend on WebGL frame rate. Work remains budgeted, and
// sleeping/hidden/paused/stopped workcells own no active timer.
export class WorkerClock{
 constructor({advance,frame,awake,blocked}){Object.assign(this,{advance,frame,awake,blocked,visible:true,budget:12});this.lastFrame=0}
 configure({visible=this.visible,budget=this.budget}={}){this.visible=visible;this.budget=budget;this.wake()}
 wake(settle=false){this.halted=false;clearTimeout(this.timer);this.timer=null;this.last=performance.now();this.settleUntil=settle?this.last+2000:0;this.schedule()}
 alive(){return this.awake()||performance.now()<(this.settleUntil||0)}
 schedule(delay=0){if(this.halted||!this.visible||this.blocked()||!this.alive())return;this.timer=setTimeout(()=>this.step(),delay)}
 step(){this.timer=null;if(this.halted||!this.visible||this.blocked()||!this.alive())return;const start=performance.now(),dt=Math.min(.05,Math.max(.001,(start-this.last)/1000));this.last=start;
  this.advance(dt,this.budget);if(this.halted)return;const now=performance.now();if(now-this.lastFrame>=1000/30||!this.alive()){this.frame();this.lastFrame=now}
  this.schedule(Math.max(0,1000/60-(performance.now()-start)));
 }
 stop(){this.halted=true;clearTimeout(this.timer);this.timer=null}
}
