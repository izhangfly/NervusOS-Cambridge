"""Reduce non-colliding UR5e visual meshes for browsers. Physics inertias/proxies are unchanged.
Run after adapt-arm-models.py with trimesh and fast-simplification available. Originals remain
in the ignored build/model-sources cache; missing source files can be fetched from Menagerie.
"""
from pathlib import Path
import json, shutil, urllib.request, xml.etree.ElementTree as E
import trimesh

root=Path(__file__).resolve().parents[1]
folder=root/'public/models/ur5e'; cache=root/'build/model-sources/ur5e'; cache.mkdir(parents=True,exist_ok=True)
tree=E.parse(folder/'ur5e.xml'); before=after=0; changes=[]
provenance=json.loads((folder/'provenance.json').read_text())
original_names={m['browser']:m['source'] for m in provenance.get('web_meshes',[])}
for node in tree.findall('.//asset/mesh'):
    previous=node.get('file'); name=original_names.get(previous,previous)
    if not name.endswith('.obj'): continue
    source=cache/name; original=folder/'assets'/name
    if not source.exists():
        if original.exists(): shutil.copy2(original,source)
        else: urllib.request.urlretrieve('https://raw.githubusercontent.com/google-deepmind/mujoco_menagerie/main/universal_robots_ur5e/assets/'+name,source)
    mesh=trimesh.load(source,force='mesh',process=False)
    # OBJ normal/texture seams duplicate vertices. Weld the geometric topology before
    # decimation; treating every seam as a boundary leaves overlapping triangle fragments.
    mesh.merge_vertices(merge_tex=True,merge_norm=True)
    n=len(mesh.faces)
    if n>2500: mesh=mesh.simplify_quadric_decimation(face_count=2500,aggression=4)
    out=Path(name).stem+'_web-v2.stl'; dest=folder/'assets'/out
    mesh.export(dest,file_type='stl')
    # MJCF implicit names strip the extension. Preserve each original reference explicitly.
    node.set('name',node.get('name',Path(name).stem));node.set('file',out)
    before+=source.stat().st_size;after+=dest.stat().st_size
    changes.append({'source':name,'browser':out,'source_faces':n,'browser_faces':len(mesh.faces)})
    if previous != out and previous.endswith('.stl'):
        (folder/'assets'/previous).unlink(missing_ok=True)
if changes:
    E.indent(tree);tree.write(folder/'ur5e.xml',encoding='unicode')
    for c in changes: (folder/'assets'/c['source']).unlink(missing_ok=True)
    provenance['web_meshes']=changes
    note='Reduced visual meshes and exported binary STL; retained explicit body inertias and contact proxies'
    if note not in provenance['changes']: provenance['changes'].append(note)
    (folder/'provenance.json').write_text(json.dumps(provenance,indent=2)+'\n')
print(json.dumps({'source_bytes':before,'browser_bytes':after,'parts':len(changes)}))
