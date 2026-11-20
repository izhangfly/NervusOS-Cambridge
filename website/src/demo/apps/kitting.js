export default {
 id:'kitting',name:{en:'Toy tidy',zh:'玩具整理'},subtitle:{en:'A mixed-up table. Three baskets. Everything in its place.',zh:'一桌混放的玩具，三只篮子，逐件归类。'},
 description:{en:'Turn the kitting bench into a household toy station. A fresh scatter includes seven block shapes in different colours, miniature cars and soft animal toys. The wrist view scans each item; the app sorts it into the blocks, cars or plush basket.',zh:'把配料工作台变成家用玩具整理台。每次重新生成七种形状与不同颜色的积木、小汽车和柔软动物玩偶。腕部视角逐件观察，应用将它们分到积木、汽车和毛绒玩具篮中。'},
 icon:'Blocks',color:'#4a8a73',category:{en:'Home',zh:'家用'},scope:'home',version:'0.2',publisher:{en:'NervusOS demo collection',zh:'NervusOS 演示合集'},price:0,
 permissions:[{id:'motion',label:{en:'Move the arm and gripper',zh:'控制机械臂与夹爪'}},{id:'wrist-camera',label:{en:'Use the simulated wrist camera',zh:'使用模拟腕部摄像头'}}],
 origin:{en:'Randomized toys · geometric classification in simulated perception · deformable plush surfaces. No trained vision policy is connected.',zh:'随机生成玩具 · 模拟感知中的几何分类 · 可变形玩偶表面。没有连接已训练的视觉策略。'},
 actions:[{id:'sort',name:{en:'Sort every toy',zh:'整理所有玩具'},description:{en:'Inspect and sort 13 toys into the correct baskets.',zh:'逐件观察十三件玩具，放入正确的篮子。'}},{id:'shuffle',name:{en:'New scatter & sort',zh:'重新打乱并整理'},description:{en:'Generate a new arrangement and run the same sorting app.',zh:'生成新的随机摆放，再运行同一个整理应用。'}}]
};
