"""Adapt the locally imported Menagerie descriptions for the browser capability contract."""
from pathlib import Path
import xml.etree.ElementTree as E

ROOT = Path(__file__).resolve().parents[1] / 'public/models'
for robot, flange, pos, quat in [('ur5e','wrist_3_link','0 .1 0','-1 1 0 0'),('gen3','bracelet_link','0 0 -.061525','0 1 0 0')]:
    tree=E.parse(ROOT / robot / 'upstream.xml'); doc=tree.getroot()
    doc.set('model',robot); doc.find('compiler').set('autolimits','true')
    for key in doc.findall('keyframe'): doc.remove(key)
    for body in doc.findall('.//worldbody//body'): body.set('gravcomp','1')
    for parent in doc.findall('.//worldbody//body'):
        for geom in list(parent.findall('geom')):
            if geom.get('class')=='collision':
                if robot=='gen3': parent.remove(geom)
                else:
                    geom.set('name','self_'+parent.get('name')+'_'+str(list(parent).index(geom)))
                    geom.set('contype','4');geom.set('conaffinity','4');geom.set('margin','.003')
    if robot=='gen3':
        for joint in doc.findall('.//worldbody//joint'):
            if 'range' not in joint.attrib: joint.set('range','-6.283 6.283')
    parent=next(b for b in doc.findall('.//body') if b.get('name')==flange)
    hand=E.SubElement(parent,'body',name='demo_hand',pos=pos,quat=quat,gravcomp='1')
    E.SubElement(hand,'inertial',pos='0 0 .025',mass='.25',diaginertia='.0004 .0004 .0004')
    E.SubElement(hand,'geom',name='hand_visual',type='box',size='.033 .052 .015',pos='0 0 .015',group='2',contype='0',conaffinity='0',rgba='.16 .22 .25 1',density='0')
    E.SubElement(hand,'geom',name='palm',type='box',size='.033 .052 .015',pos='0 0 .015',group='3',contype='8',conaffinity='1',density='0')
    E.SubElement(hand,'site',name='tool',pos='0 0 .089',size='.005',rgba='0 0 0 0')
    for suffix,rotation in [('l','1 0 0 0'),('r','0 0 0 1')]:
        finger=E.SubElement(hand,'body',name='finger_'+suffix,pos='0 0 .032',quat=rotation,gravcomp='1')
        E.SubElement(finger,'inertial',pos='0 .01 .026',mass='.025',diaginertia='.00001 .00001 .00001')
        E.SubElement(finger,'joint',name='finger_joint_'+suffix,type='slide',axis='0 1 0',range='0 .04',damping='1')
        E.SubElement(finger,'geom',type='box',name='finger_visual_'+suffix,size='.008 .009 .026',pos='0 .011 .024',group='2',contype='0',conaffinity='0',rgba='.25 .31 .35 1',density='0')
        E.SubElement(finger,'geom',type='box',name='pad_'+suffix,size='.009 .005 .012',pos='0 .003 .045',group='3',contype='8',conaffinity='1',condim='4',friction='1.8 .02 .002',solref='.004 1',density='0',rgba='0 0 0 0')
        E.SubElement(finger,'geom',type='box',name='skin_'+suffix,size='.009 .005 .012',pos='0 .003 .045',group='3',contype='0',conaffinity='16',margin='.004',gap='0',friction='1.8 .02 .002',density='0',rgba='0 0 0 0')
        E.SubElement(finger,'site',name='pad_'+suffix,type='box',size='.011 .01 .015',pos='0 .003 .045',rgba='0 0 0 0')
    actuator=doc.find('actuator'); joints=doc.findall('.//worldbody//joint')[:-2]
    for elem in list(actuator): actuator.remove(elem)
    for joint in joints:
        E.SubElement(actuator,'position',name=joint.get('name'),joint=joint.get('name'),kp='550',kv='65',forcerange='-120 120',ctrlrange=joint.get('range','-6.283 6.283'))
    E.SubElement(actuator,'position',name='gripper',joint='finger_joint_l',kp='900',kv='30',forcerange='-20 20',ctrlrange='0 .04')
    for suffix in ['l','r']: E.SubElement(actuator,'adhesion',name='adhere_'+suffix,body='finger_'+suffix,ctrlrange='0 1',gain='3')
    equality=E.SubElement(doc,'equality');E.SubElement(equality,'joint',joint1='finger_joint_l',joint2='finger_joint_r',solref='.004 1')
    contact=E.SubElement(doc,'contact');E.SubElement(contact,'exclude',body1='finger_l',body2='finger_r')
    sensors=E.SubElement(doc,'sensor')
    for suffix in ['l','r']: E.SubElement(sensors,'touch',name='touch_'+suffix,site='pad_'+suffix)
    E.indent(doc);tree.write(ROOT/robot/(robot+'.xml'),encoding='unicode')
    print(robot,'adapted')
