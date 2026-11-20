import laundry from './laundry';import kitting from './kitting';import quality from './quality';import assembly from './assembly';import teach from './teach';
export const apps=[laundry,kitting,quality,assembly,{...teach,scope:'lab',price:0,version:'0.1',publisher:{en:'NervusOS demo collection',zh:'NervusOS 演示合集'}}];
