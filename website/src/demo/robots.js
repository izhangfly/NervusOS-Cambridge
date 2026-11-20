export const ROBOTS={
 piper:{id:'piper',name:'AgileX PiPER',short:'PiPER',dof:6,home:[0,1.15,-1.65,0,.8,0,.03,-.03],open:.035,gripper:6,model:'/models/piper/piper.xml',wristBody:'link6',source:'https://github.com/agilexrobotics/agx_arm_sim',sdk:'https://github.com/agilexrobotics/pyAgxArm',ros:'https://github.com/agilexrobotics/agx_arm_ros',limits:[[-2.618,2.618],[0,3.14],[-2.697,0],[-1.832,1.832],[-1.22,1.22],[-3.14,3.14]]},
 panda:{id:'panda',name:'Franka Emika Panda',short:'Panda',dof:7,home:[0,-.55,0,-2.2,0,1.65,.78,.04,.04],open:.04,gripper:7,model:'/models/panda/panda.xml',wristBody:'hand',source:'https://github.com/google-deepmind/mujoco_menagerie/tree/main/franka_emika_panda',sdk:'https://github.com/frankarobotics/libfranka',ros:'https://github.com/frankarobotics/franka_ros',limits:[[-2.8973,2.8973],[-1.7628,1.7628],[-2.8973,2.8973],[-3.0718,-.0698],[-2.8973,2.8973],[-.0175,3.7525],[-2.8973,2.8973]]}
};
export const robotProfile=id=>ROBOTS[id]||ROBOTS.piper;
export function savedRobot(){try{return robotProfile(localStorage.getItem('nervus-demo-robot')).id}catch{return'piper'}}
