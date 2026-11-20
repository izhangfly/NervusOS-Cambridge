// Garments for the laundry scene.
//
// Physics: each garment is a coarse MuJoCo flex grid (N x N cells, a body per vertex), cut to the
// garment's outline. Rendering: a fine surface (R times denser) interpolated from the simulated
// points every frame with a Catmull-Rom patch, so the fabric bends smoothly instead of showing the
// coarse triangles, plus a painted garment texture (collar, hems, seams, pockets, zip) and a woven
// bump map. The simulation stays light enough for phones; what you see is a real piece of clothing.
import * as THREE from 'three';

// Garment outlines in garment coordinates: u across (0..1), v from hem (0) to collar/waist (1).
const SHAPES = {
  tshirt(u, v) {
    const ax = Math.abs(u - .5);
    if (((u - .5) / .12) ** 2 + ((v - .97) / .12) ** 2 < 1) return false;            // neckline
    const shoulder = .86 - .32 * Math.max(0, ax - .1);
    if (ax <= .27) return v >= 0 && v <= shoulder;                                    // body
    const dx = ax - .27; if (dx > .2) return false;                                   // short sleeves
    const top = .86 - .32 * .17 - .55 * dx, bottom = .63 - .22 * dx;
    return v <= top && v >= bottom;
  },
  jacket(u, v) {
    const ax = Math.abs(u - .5);
    if (v > .74 && ax < (v - .74) * .55 + .015) return false;                         // V-neck / open front
    const shoulder = .88 - .3 * Math.max(0, ax - .12);
    if (ax <= .26) return v >= 0 && v <= shoulder;
    const dx = ax - .26; if (dx > .235) return false;                                  // long sleeves
    const top = .88 - .3 * .14 - .95 * dx, bottom = .64 - .95 * dx;
    return v <= top && v >= bottom;
  },
  trousers(u, v) {
    const ax = Math.abs(u - .5);
    if (v > .985) return false;
    if (v >= .6) return ax <= .34;                                                    // waist and seat
    const gap = .018 + .05 * (.6 - v) / .6, outer = .34 + .035 * (.6 - v) / .6;     // crotch opens, legs flare
    return ax >= gap && ax <= outer;
  },
};

// Coarse simulation mesh: grid points inside (or within half a cell of) the outline.
export function garmentMesh(kind, position, size = .13, N = 8) {
  const shape = SHAPES[kind] || SHAPES.tshirt, H = size * 1.12;
  const near = (u, v) => { const h = .5 / N; for (const [a, b] of [[0, 0], [h, 0], [-h, 0], [0, h], [0, -h], [h, h], [-h, h], [h, -h], [-h, -h]]) if (shape(u + a, v + b)) return true; return false; };
  const points = [], uv = [], indices = [], index = new Int32Array((N + 1) * (N + 1)).fill(-1);
  for (let j = 0; j <= N; j++) for (let i = 0; i <= N; i++) {
    const u = i / N, v = j / N; if (!near(u, v)) continue;
    index[j * (N + 1) + i] = points.length; uv.push(u, v);
    points.push([position[0] + (u - .5) * size, position[1] + (v - .5) * H, position[2] + .004 * Math.sin(i * 1.7 + j * .9)]);
  }
  const at = (i, j) => (i < 0 || j < 0 || i > N || j > N) ? -1 : index[j * (N + 1) + i];
  for (let j = 0; j < N; j++) for (let i = 0; i < N; i++) {
    const a = at(i, j), b = at(i + 1, j), c = at(i, j + 1), d = at(i + 1, j + 1);
    if (a >= 0 && b >= 0 && c >= 0) indices.push(a, b, c);
    if (b >= 0 && c >= 0 && d >= 0) indices.push(b, d, c);
  }
  return { points, indices, uv, grid: { N, index: Array.from(index), size, H, kind } };
}

// Catmull-Rom weights for t in [0,1] over four control points.
function crWeights(t) {
  const t2 = t * t, t3 = t2 * t;
  return [(-t3 + 2 * t2 - t) / 2, (3 * t3 - 5 * t2 + 2) / 2, (-3 * t3 + 4 * t2 + t) / 2, (t3 - t2) / 2];
}

// The rendered garment: a fine surface driven by the simulated vertices.
export class GarmentSurface {
  constructor(item, R = 4) {
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
    // fine vertices inside the outline, each with its 4x4 stencil on the grid
    const verts = [], uvs = [], fine = new Int32Array((M + 1) * (M + 1)).fill(-1), stencil = [];
    for (let b = 0; b <= M; b++) for (let a = 0; a <= M; a++) {
      const u = a / M, v = b / M; if (!shape(u, v)) continue;
      fine[b * (M + 1) + a] = verts.length / 3; verts.push(0, 0, 0); uvs.push(u, v);
      const x = u * N, y = v * N, i0 = Math.min(N - 1, Math.floor(x)), j0 = Math.min(N - 1, Math.floor(y));
      const wx = crWeights(x - i0), wy = crWeights(y - j0), ids = [], ws = [];
      for (let q = 0; q < 4; q++) for (let r = 0; r < 4; r++) {
        const gi = Math.max(0, Math.min(N, i0 - 1 + r)), gj = Math.max(0, Math.min(N, j0 - 1 + q));
        ids.push(gj * G + gi); ws.push(wx[r] * wy[q]);
      }
      stencil.push(ids, ws);
    }
    const tris = [], fi = (a, b) => fine[b * (M + 1) + a];
    for (let b = 0; b < M; b++) for (let a = 0; a < M; a++) {
      const p = fi(a, b), q = fi(a + 1, b), r = fi(a, b + 1), s = fi(a + 1, b + 1);
      if (p >= 0 && q >= 0 && r >= 0) tris.push(p, q, r);
      if (q >= 0 && s >= 0 && r >= 0) tris.push(q, s, r);
    }
    this.stencil = stencil; this.full = new Float64Array(G * G * 3);
    const g = new THREE.BufferGeometry();
    g.setAttribute('position', new THREE.Float32BufferAttribute(verts, 3));
    g.setAttribute('uv', new THREE.Float32BufferAttribute(uvs, 2));
    g.setIndex(tris);
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
    const pos = this.mesh.geometry.attributes.position.array, st = this.stencil;
    for (let n = 0, m = 0; n < st.length; n += 2, m += 3) {
      const ids = st[n], ws = st[n + 1]; let x = 0, y = 0, z = 0;
      for (let q = 0; q < 16; q++) { const o = ids[q] * 3, w = ws[q]; x += w * full[o]; y += w * full[o + 1]; z += w * full[o + 2]; }
      pos[m] = x; pos[m + 1] = y; pos[m + 2] = z;
    }
    const geo = this.mesh.geometry; geo.attributes.position.needsUpdate = true; geo.computeVertexNormals(); geo.computeBoundingSphere();
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
