import laundry from './laundry.js';import kitting from './kitting.js';import quality from './quality.js';import assembly from './assembly.js';import teach from './teach.js';
export const apps=[laundry,kitting,quality,assembly,{...teach,scope:'lab',price:0,version:'0.1',publisher:{en:'NervusOS demo collection',zh:'NervusOS 演示合集'}}];
