// Garments for the laundry scene.
//
// Physics: each garment is a coarse MuJoCo flex grid (N x N cells, a body per vertex), cut to the
// garment's outline. Garments collide with the table, baskets, washer and gripper but not with each
// other even while they sleep (see garmentState in model.js). Rendering: a fine surface (R times denser) follows the simulated
// triangles without interpolation overshoot, with smooth vertex normals instead of showing the
// coarse triangles, plus a painted garment texture (collar, hems, seams, pockets, zip) and a woven
// bump map. The simulation stays light enough for phones; the rendered surface retains the garment silhouette and thickness.
import * as THREE from 'three';
import { SHAPES, garmentMesh, garmentOutline } from './cloth-mesh.js';
export { garmentMesh };

// Catmull-Rom weights for t in [0,1] over four control points.
function crWeights(t) {
  const t2 = t * t, t3 = t2 * t;
  return [(-t3 + 2 * t2 - t) / 2, (3 * t3 - 5 * t2 + 2) / 2, (-3 * t3 + 4 * t2 + t) / 2, (t3 - t2) / 2];
}

// The rendered garment: a fine surface driven by the simulated vertices.
export class GarmentSurface {
  constructor(item, R = 8) {
    const { grid } = item.mesh, { N } = grid, shape = SHAPES[grid.kind] || SHAPES.tshirt, M = N * R, G = N + 1;
    this.item = item; this.N = N; this.G = G;
    this.index = Int32Array.from(grid.index);
    // rest positions of the whole grid (missing points too), for extrapolation
    this.rest = new Float64Array(G * G * 3);
    const p0 = item.mesh.points, first = p0[0], u0 = item.mesh.uv[0], v0 = item.mesh.uv[1];
    const ox = first[0] - (u0 - .5) * grid.size, oy = first[1] - (v0 - .5) * grid.H, oz = first[2];
    for (let j = 0; j < G; j++) for (let i = 0; i < G; i++) {
      const k = (j * G + i) * 3; this.rest[k] = ox + (i / N - .5) * grid.size; this.rest[k + 1] = oy + (j / N - .5) * grid.H; this.rest[k + 2] = oz;
    }
    // how each missing grid point is filled: linear extrapolation along a grid line, else nearest point's motion
    this.fill = [];
    for (let j = 0; j < G; j++) for (let i = 0; i < G; i++) {
      if (this.index[j * G + i] >= 0) continue;
      const has = (a, b) => a >= 0 && b >= 0 && a < G && b < G && this.index[b * G + a] >= 0;
      let rule = null;
      for (const [di, dj] of [[1, 0], [-1, 0], [0, 1], [0, -1]]) if (has(i + di, j + dj) && has(i + 2 * di, j + 2 * dj)) { rule = { k: j * G + i, a: (j + dj) * G + i + di, b: (j + 2 * dj) * G + i + 2 * di }; break; }
      if (!rule) {
        let best = -1, bd = 1e9;
        for (let b = 0; b < G; b++) for (let a = 0; a < G; a++) if (this.index[b * G + a] >= 0) { const d = (a - i) ** 2 + (b - j) ** 2; if (d < bd) { bd = d; best = b * G + a; } }
        rule = { k: j * G + i, near: best };
      }
      this.fill.push(rule);
    }
    // Fine silhouette vertices, including exact clipped outline intersections, with 4x4 grid stencils.
    const {uv:uvs,indices:tris}=garmentOutline(grid.kind,M),verts=new Array(uvs.length/2*3).fill(0),stencil=[];
    for(let k=0;k<uvs.length;k+=2){
      const u=uvs[k],v=uvs[k+1],x=u*N,y=v*N,i0=Math.min(N-1,Math.floor(x)),j0=Math.min(N-1,Math.floor(y));
      const fx=x-i0,fy=y-j0,ids=fx+fy<=1?[j0*G+i0,j0*G+i0+1,(j0+1)*G+i0]:[j0*G+i0+1,(j0+1)*G+i0+1,(j0+1)*G+i0];
      const ws=fx+fy<=1?[1-fx-fy,fx,fy]:[1-fy,fx+fy-1,1-fx];
      stencil.push(ids,ws);
    }
    this.stencil = stencil; this.full = new Float64Array(G * G * 3);
    // Real cloth has thickness: draw a front face, a back face (offset along the normal) and the rim
    // between them along the outline, instead of a single see-through sheet.
    const V = verts.length / 3; this.V = V; this.tris = tris; this.base = new Float32Array(V * 3); this.bn = new Float32Array(V * 3);
    this.thick = { tshirt: .002, jacket: .0035, trousers: .0028 }[grid.kind] || .0025;
    const count = new Map(); for (let t = 0; t < tris.length; t += 3) for (let e = 0; e < 3; e++) { const a = tris[t + e], b = tris[t + (e + 1) % 3], k = a < b ? a + ',' + b : b + ',' + a; count.set(k, (count.get(k) || 0) + 1); }
    const rim = []; for (let t = 0; t < tris.length; t += 3) for (let e = 0; e < 3; e++) { const a = tris[t + e], b = tris[t + (e + 1) % 3]; if (count.get(a < b ? a + ',' + b : b + ',' + a) === 1) rim.push(a, b); }
    this.rim = rim;
    const nRim = rim.length / 2, total = 2 * V + 4 * nRim, idx = [];
    for (let t = 0; t < tris.length; t += 3) { idx.push(tris[t], tris[t + 1], tris[t + 2]); idx.push(V + tris[t], V + tris[t + 2], V + tris[t + 1]); }
    for (let r = 0; r < nRim; r++) { const o = 2 * V + 4 * r; idx.push(o, o + 2, o + 1, o + 1, o + 2, o + 3); }
    const uvAll = new Float32Array(total * 2); uvAll.set(uvs, 0); uvAll.set(uvs, V * 2);
    for (let r = 0; r < nRim; r++) { const a = rim[2 * r], b = rim[2 * r + 1], o = (2 * V + 4 * r) * 2; uvAll.set([uvs[a * 2], uvs[a * 2 + 1], uvs[b * 2], uvs[b * 2 + 1], uvs[a * 2], uvs[a * 2 + 1], uvs[b * 2], uvs[b * 2 + 1]], o); }
    const g = new THREE.BufferGeometry();
    g.setAttribute('position', new THREE.Float32BufferAttribute(new Float32Array(total * 3), 3));
    g.setAttribute('normal', new THREE.Float32BufferAttribute(new Float32Array(total * 3), 3));
    g.setAttribute('uv', new THREE.Float32BufferAttribute(uvAll, 2));
    g.setIndex(idx);
    const tex = garmentTextures(grid.kind, item.color);
    const mat = new THREE.MeshStandardMaterial({
      map: tex.map, bumpMap: tex.bump, bumpScale: grid.kind === 'trousers' ? .0016 : grid.kind === 'jacket' ? .0022 : .0009,
      roughness: grid.kind === 'trousers' ? .86 : .95, metalness: 0, side: THREE.DoubleSide,
    });
    this.mesh = new THREE.Mesh(g, mat); this.mesh.castShadow = true; this.mesh.receiveShadow = true;
    this.mesh.frustumCulled = false;
  }
  // flex: Float64Array of all flex vertex positions; adr: this garment's first vertex
  update(flex, adr) {
    const { G, index, rest, full, fill } = this;
    for (let k = 0; k < G * G; k++) {
      const v = index[k]; if (v < 0) continue;
      const s = (adr + v) * 3, o = k * 3; full[o] = flex[s]; full[o + 1] = flex[s + 1]; full[o + 2] = flex[s + 2];
    }
    for (const r of fill) {
      const o = r.k * 3;
      if (r.near === undefined) for (let c = 0; c < 3; c++) full[o + c] = 2 * full[r.a * 3 + c] - full[r.b * 3 + c];
      else for (let c = 0; c < 3; c++) full[o + c] = rest[o + c] + (full[r.near * 3 + c] - rest[r.near * 3 + c]);
    }
    const P = this.base, N = this.bn, st = this.stencil, V = this.V, T = this.tris, h = this.thick / 2;
    for (let n = 0, m = 0; n < st.length; n += 2, m += 3) {
      const ids = st[n], ws = st[n + 1]; let x = 0, y = 0, z = 0;
      for (let q = 0; q < ids.length; q++) { const o = ids[q] * 3, w = ws[q]; x += w * full[o]; y += w * full[o + 1]; z += w * full[o + 2]; }
      P[m] = x; P[m + 1] = y; P[m + 2] = z;
    }
    N.fill(0);
    for (let t = 0; t < T.length; t += 3) {
      const a = T[t] * 3, b = T[t + 1] * 3, c = T[t + 2] * 3;
      const ux = P[b] - P[a], uy = P[b + 1] - P[a + 1], uz = P[b + 2] - P[a + 2], vx = P[c] - P[a], vy = P[c + 1] - P[a + 1], vz = P[c + 2] - P[a + 2];
      const nx = uy * vz - uz * vy, ny = uz * vx - ux * vz, nz = ux * vy - uy * vx;
      for (const k of [a, b, c]) { N[k] += nx; N[k + 1] += ny; N[k + 2] += nz; }
    }
    for (let k = 0; k < V * 3; k += 3) { const l = Math.hypot(N[k], N[k + 1], N[k + 2]) || 1; N[k] /= l; N[k + 1] /= l; N[k + 2] /= l; }
    const geo = this.mesh.geometry, pos = geo.attributes.position.array, nor = geo.attributes.normal.array;
    for (let k = 0; k < V * 3; k += 3) for (let c = 0; c < 3; c++) {
      pos[k + c] = P[k + c] + N[k + c] * h; nor[k + c] = N[k + c];
      pos[V * 3 + k + c] = P[k + c] - N[k + c] * h; nor[V * 3 + k + c] = -N[k + c];
    }
    const rim = this.rim;
    for (let r = 0; r < rim.length / 2; r++) {
      const a = rim[2 * r] * 3, b = rim[2 * r + 1] * 3, o = (2 * V + 4 * r) * 3;
      const ex = P[b] - P[a], ey = P[b + 1] - P[a + 1], ez = P[b + 2] - P[a + 2], nx = N[a] + N[b], ny = N[a + 1] + N[b + 1], nz = N[a + 2] + N[b + 2];
      let ox = ey * nz - ez * ny, oy = ez * nx - ex * nz, oz = ex * ny - ey * nx; const l = Math.hypot(ox, oy, oz) || 1; ox /= l; oy /= l; oz /= l;
      const put = (slot, src, sgn) => { for (let c = 0; c < 3; c++) pos[o + slot * 3 + c] = P[src + c] + N[src + c] * h * sgn; nor[o + slot * 3] = ox; nor[o + slot * 3 + 1] = oy; nor[o + slot * 3 + 2] = oz; };
      put(0, a, 1); put(1, b, 1); put(2, a, -1); put(3, b, -1);
    }
    geo.attributes.position.needsUpdate = true; geo.attributes.normal.needsUpdate = true; geo.computeBoundingSphere();
  }
  dispose() { this.mesh.geometry.dispose(); const m = this.mesh.material; m.map?.dispose(); m.bumpMap?.dispose(); m.dispose(); }
}

// ---------------------------------------------------------------- textures
const shade = (hex, f) => { const c = new THREE.Color(hex); c.multiplyScalar(f); return '#' + c.getHexString(); };
function canvas(n = 512) { const c = document.createElement('canvas'); c.width = c.height = n; return [c, c.getContext('2d')]; }

function garmentTextures(kind, color) {
  const S = 512, [c, x] = canvas(S), [bc, b] = canvas(S);
  const P = (u, v) => [u * S, (1 - v) * S];                       // garment uv -> canvas
  x.fillStyle = color; x.fillRect(0, 0, S, S); b.fillStyle = '#808080'; b.fillRect(0, 0, S, S);
  const stroke = (ctx, col, w, pts, dash) => { ctx.save(); ctx.strokeStyle = col; ctx.lineWidth = w; if (dash) ctx.setLineDash(dash); ctx.beginPath(); pts.forEach((p, i) => { const [X, Y] = P(...p); i ? ctx.lineTo(X, Y) : ctx.moveTo(X, Y); }); ctx.stroke(); ctx.restore(); };
  if (kind === 'trousers') {
    // denim twill: diagonal wale in colour and in the bump map
    for (let k = -S; k < S * 2; k += 5) {
      x.strokeStyle = 'rgba(255,255,255,.08)'; x.lineWidth = 1.4; x.beginPath(); x.moveTo(k, 0); x.lineTo(k + S, S); x.stroke();
      b.strokeStyle = '#a8a8a8'; b.lineWidth = 2; b.beginPath(); b.moveTo(k, 0); b.lineTo(k + S, S); b.stroke();
    }
    const thread = '#d9a650';
    x.fillStyle = shade(color, .82); x.fillRect(...P(.16, .985), .68 * S, .085 * S);           // waistband
    stroke(x, thread, 1.6, [[.16, .9], [.84, .9]], [5, 3]);
    for (const u of [.24, .4, .6, .76]) { x.fillStyle = shade(color, .7); x.fillRect(...P(u - .012, .99), .024 * S, .08 * S); }   // belt loops
    x.fillStyle = '#c9b58a'; x.beginPath(); x.arc(...P(.5, .945), 7, 0, 7); x.fill();                     // button
    stroke(x, thread, 1.6, [[.53, .9], [.53, .7], [.5, .66]], [5, 3]);                                   // fly
    stroke(x, thread, 1.6, [[.18, .88], [.24, .8], [.3, .78]], [5, 3]); stroke(x, thread, 1.6, [[.82, .88], [.76, .8], [.7, .78]], [5, 3]);   // pockets
    for (const [u, v] of [[.3, .78], [.7, .78]]) { x.fillStyle = '#c08a3e'; x.beginPath(); x.arc(...P(u, v), 3.5, 0, 7); x.fill(); }      // rivets
    stroke(x, thread, 1.6, [[.165, .6], [.13, .02]], [5, 3]); stroke(x, thread, 1.6, [[.835, .6], [.87, .02]], [5, 3]);   // outseams
    stroke(x, thread, 1.4, [[.47, .58], [.38, .02]], [5, 3]); stroke(x, thread, 1.4, [[.53, .58], [.62, .02]], [5, 3]);
    x.fillStyle = shade(color, .78); x.fillRect(0, S - .035 * S, S, .035 * S);                       // hems
  } else if (kind === 'jacket') {
    // knit: rows of small chevrons
    for (let y = 0; y < S; y += 9) for (let k = 0; k < S; k += 8) {
      x.strokeStyle = 'rgba(0,0,0,.12)'; x.lineWidth = 1.2; x.beginPath(); x.moveTo(k, y); x.lineTo(k + 4, y + 6); x.lineTo(k + 8, y); x.stroke();
      b.strokeStyle = '#c4c4c4'; b.lineWidth = 2.4; b.beginPath(); b.moveTo(k, y); b.lineTo(k + 4, y + 6); b.lineTo(k + 8, y); b.stroke();
    }
    const rib = (u0, v0, u1, v1) => { x.fillStyle = shade(color, .8); const [X0, Y0] = P(u0, v1), [X1, Y1] = P(u1, v0); x.fillRect(X0, Y0, X1 - X0, Y1 - Y0); for (let X = X0; X < X1; X += 4) { b.fillStyle = '#9a9a9a'; b.fillRect(X, Y0, 2, Y1 - Y0); } };
    rib(.24, 0, .76, .07);                                                           // hem rib
    rib(0, .3, .06, .7); rib(.94, .3, 1, .7);                                        // cuffs
    stroke(x, '#5f646b', 5, [[.5, .02], [.5, .74]]); stroke(x, '#c9ced3', 1.5, [[.5, .02], [.5, .74]], [3, 3]);   // zip
    x.fillStyle = '#c9ced3'; x.fillRect(...P(.49, .74), 10, 16);                     // zip pull
    stroke(x, shade(color, .75), 3, [[.5, .74], [.36, .9]]); stroke(x, shade(color, .75), 3, [[.5, .74], [.64, .9]]);   // collar
  } else {
    // cotton jersey: fine grain, collar rib, hems, shoulder seams, a small chest mark
    for (let y = 0; y < S; y += 3) { b.strokeStyle = y % 6 ? '#8a8a8a' : '#777'; b.lineWidth = 1; b.beginPath(); b.moveTo(0, y); b.lineTo(S, y); b.stroke(); }
    x.save(); x.strokeStyle = shade(color, .78); x.lineWidth = .028 * S; x.beginPath(); x.ellipse(...P(.5, .97), .135 * S, .135 * S, 0, 0, 7); x.stroke(); x.restore();   // collar rib
    stroke(x, shade(color, .7), 1.4, [[.24, .03], [.76, .03]], [4, 3]);                                   // hem stitch
    stroke(x, shade(color, .7), 1.4, [[.05, .58], [.02, .72]], [4, 3]); stroke(x, shade(color, .7), 1.4, [[.95, .58], [.98, .72]], [4, 3]);   // sleeve hems
    stroke(x, shade(color, .82), 1.2, [[.27, .76], [.27, .63]]); stroke(x, shade(color, .82), 1.2, [[.73, .76], [.73, .63]]);   // armhole seams
    x.fillStyle = shade(color, .62); x.beginPath(); x.arc(...P(.62, .72), 7, 0, 7); x.fill();         // chest mark
  }
  const map = new THREE.CanvasTexture(c); map.colorSpace = THREE.SRGBColorSpace; map.anisotropy = 4;
  const bump = new THREE.CanvasTexture(bc); bump.anisotropy = 4;
  return { map, bump };
}
