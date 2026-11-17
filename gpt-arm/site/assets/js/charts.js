/* GPT Arm report: every figure, drawn as SVG from data/report.json and data/holds.json.
   Colours come from CSS variables (so the theme switch needs no redraw); each chart redraws on resize.
   Model colour follows the model everywhere: GPT-6.1 Sol = --s1 (blue), GPT-5.6 Sol = --s2 (orange). */
(function () {
  "use strict";
  const NS = "http://www.w3.org/2000/svg";
  const MODEL = { "gpt-6.1-sol": { name: "GPT-6.1 Sol", c: "var(--s1)" }, "gpt-5.6-sol": { name: "GPT-5.6 Sol", c: "var(--s2)" },
                  "scripted": { name: "Scripted (no model)", c: "var(--ink-3)" } };
  const STAGES = ["No purposeful approach", "Gripper around the bottle", "Lifted off the table", "Released in the basket",
                  "Lifted out of the basket", "Put down on the table"];
  const fmt = (n, d = 0) => Number(n).toLocaleString("en-GB", { minimumFractionDigits: d, maximumFractionDigits: d });
  const short = (id) => id.slice(9, 15);
  const hhmm = (id) => id.slice(9, 11) + ":" + id.slice(11, 13);
  const day = (id) => (id.slice(6, 8) === "15" ? "15 Nov" : "16 Nov");
  const esc = (s) => String(s == null ? "" : s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

  function el(tag, attrs, parent, text) {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs || {}) if (attrs[k] !== undefined && attrs[k] !== null) e.setAttribute(k, attrs[k]);
    if (text !== undefined) e.textContent = text;
    if (parent) parent.appendChild(e);
    return e;
  }
  const lin = (d0, d1, r0, r1) => { const f = (v) => r0 + ((v - d0) / (d1 - d0 || 1)) * (r1 - r0); f.inv = (p) => d0 + ((p - r0) / (r1 - r0)) * (d1 - d0); return f; };
  const logs = (d0, d1, r0, r1) => { const a = Math.log10(d0), b = Math.log10(d1); return (v) => r0 + ((Math.log10(Math.max(v, d0)) - a) / (b - a)) * (r1 - r0); };
  function ticks(lo, hi, n) {
    const span = hi - lo, step0 = span / Math.max(1, n), mag = Math.pow(10, Math.floor(Math.log10(step0)));
    const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => span / s <= n) || 10 * mag;
    const out = []; for (let v = Math.ceil(lo / step) * step; v <= hi + 1e-9; v += step) out.push(+v.toFixed(10)); return out;
  }

  /* ---- tooltip ---- */
  const tip = document.createElement("div"); tip.className = "tip"; tip.setAttribute("role", "status");
  document.querySelector(".gar").appendChild(tip);
  function showTip(html, ev) {
    tip.innerHTML = html; tip.classList.add("show");
    const pad = 14, w = tip.offsetWidth, h = tip.offsetHeight;
    let x = ev.clientX + pad, y = ev.clientY + pad;
    if (x + w > window.innerWidth - 8) x = ev.clientX - w - pad;
    if (y + h > window.innerHeight - 8) y = ev.clientY - h - pad;
    tip.style.left = Math.max(8, x) + "px"; tip.style.top = Math.max(8, y) + "px";
  }
  function hideTip() { tip.classList.remove("show"); }
  function hover(node, html, svg, group) {
    node.addEventListener("pointerenter", (e) => { showTip(typeof html === "function" ? html() : html, e); if (svg && group) { svg.classList.add("dim"); group.forEach((g) => g.classList.add("on")); } });
    node.addEventListener("pointermove", (e) => showTip(typeof html === "function" ? html() : html, e));
    node.addEventListener("pointerleave", () => { hideTip(); if (svg && group) { svg.classList.remove("dim"); group.forEach((g) => g.classList.remove("on")); } });
    node.setAttribute("tabindex", "0");
    node.addEventListener("focus", () => { const r = node.getBoundingClientRect(); showTip(typeof html === "function" ? html() : html, { clientX: r.right, clientY: r.top }); });
    node.addEventListener("blur", hideTip);
  }

  function svgFor(host, height) {
    host.innerHTML = "";
    const w = Math.max(320, host.clientWidth);
    const svg = el("svg", { class: "chart", viewBox: `0 0 ${w} ${height}`, width: w, height, role: "img" }, host);
    return { svg, w, h: height };
  }
  function axisX(svg, x, y0, vals, fmtv, label, w) {
    const g = el("g", { class: "axis" }, svg);
    el("line", { x1: x(vals[0]), x2: x(vals[vals.length - 1]), y1: y0, y2: y0 }, g);
    vals.forEach((v) => { el("line", { x1: x(v), x2: x(v), y1: y0, y2: y0 + 4 }, g); el("text", { x: x(v), y: y0 + 17, "text-anchor": "middle" }, g, fmtv(v)); });
    if (label) el("text", { x: w / 2, y: y0 + 36, "text-anchor": "middle" }, g, label);
  }
  function gridY(svg, y, vals, x0, x1, fmtv) {
    const g = el("g", { class: "grid" }, svg);
    vals.forEach((v) => { el("line", { x1: x0, x2: x1, y1: y(v), y2: y(v) }, g); el("text", { x: x0 - 8, y: y(v) + 4, "text-anchor": "end" }, svg, fmtv(v)); });
  }
  function table(host, head, rows) {
    host.innerHTML = `<table class="data"><thead><tr>${head.map((h) => `<th${h.r ? ' class="r"' : ""}>${esc(h.t || h)}</th>`).join("")}</tr></thead><tbody>${
      rows.map((r) => `<tr>${r.map((c, i) => `<td${head[i].r ? ' class="r"' : ""}>${c}</td>`).join("")}</tr>`).join("")}</tbody></table>`;
  }
  const status = (r) => r.success ? '<span class="status ok">Complete</span>' : r.state === "stopped" ? '<span class="status na">Stopped by hand</span>' : '<span class="status bad">Failed</span>';

  /* ======================================================================================== */
  function drawOutcome(D) {
    const host = document.querySelector("#fig-outcome .fig-body"); if (!host) return;
    const runs = D.runs.filter((r) => r.benchmark);
    const { svg, w } = svgFor(host, 330);
    const m = { l: 150, r: 28, t: 18, b: 52 };
    const x = lin(0, 900, m.l, w - m.r), y = lin(0, 5, 330 - m.b, m.t);
    gridY(svg, y, [0, 1, 2, 3, 4, 5], m.l, w - m.r, (v) => `${v}  ${["approach", "around", "lifted", "in basket", "out", "complete"][v]}`);
    axisX(svg, x, 330 - m.b, [0, 200, 400, 600, 800], (v) => v + "k", "tokens per run (thousands)", w);
    const groups = {};
    runs.forEach((r, i) => {
      const k = r.model, jitter = (r.run_id.charCodeAt(13) % 3 - 1) * 5;
      const g = el("g", { class: "mk" }, svg);
      const c = el("circle", { cx: x(r.tokens / 1000), cy: y(+r.stage) + jitter, r: 6, style: `fill:${MODEL[k].c};fill-opacity:.35;stroke:${MODEL[k].c};stroke-width:1.5` }, g);
      (groups[k] = groups[k] || []).push(g);
      hover(c, () => `<b>${MODEL[k].name}</b> · run ${hhmm(r.run_id)}<br><span class="tm">${fmt(r.tokens)} tokens · ${fmt(r.duration_s / 60, 1)} min · ${r.decisions} decisions</span><br>Stage ${r.stage}: ${STAGES[r.stage]}${r.failure_mode ? `<div class="note">${esc(r.failure_mode)}</div>` : ""}`, svg, [g]);
    });
    const S = D.benchmark.models;
    Object.keys(S).forEach((k) => {
      const s = S[k]; const cx = x(s.mean_tokens / 1000), cy = y(s.mean_stage);
      const g = el("g", { class: "mk" }, svg);
      el("circle", { cx, cy, r: 11, style: `fill:${MODEL[k].c};stroke:var(--surface);stroke-width:2` }, g);
      const right = cx < w - 200;
      el("text", { x: cx + (right ? 18 : -18), y: cy + 4, "text-anchor": right ? "start" : "end", class: "t-ink t-big" }, g, `${MODEL[k].name}: ${s.successes}/${s.runs} complete`);
      hover(g, `<b>${MODEL[k].name}</b> (mean of ${s.runs} runs)<br><span class="tm">mean stage ${fmt(s.mean_stage, 2)} · ${fmt(s.mean_tokens)} tokens per run · median ${s.median_minutes} min</span>`, svg, groups[k].concat([g]));
    });
    const a = S["gpt-6.1-sol"], b = S["gpt-5.6-sol"];
    if (a && b) {
      const x1 = x(b.mean_tokens / 1000) - 14, y1 = y(b.mean_stage) - 8, x2 = x(a.mean_tokens / 1000) + 14, y2 = y(a.mean_stage) + 10;
      el("path", { d: `M${x1},${y1} L${x2},${y2}`, style: "stroke:var(--ink-3);stroke-width:1.2;fill:none", "marker-end": "url(#arr-o)" }, svg);
      const defs = el("defs", {}, svg); const mk = el("marker", { id: "arr-o", viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, orient: "auto-start-reverse" }, defs);
      el("path", { d: "M0,0 L10,5 L0,10 z", style: "fill:var(--ink-3)" }, mk);
      el("text", { x: (x1 + x2) / 2 + 10, y: (y1 + y2) / 2 + 2, class: "t-ink2" }, svg, `${fmt(b.mean_tokens / a.mean_tokens, 1)}x fewer tokens per run`);
      el("text", { x: (x1 + x2) / 2 + 10, y: (y1 + y2) / 2 + 18, class: "t-ink2" }, svg, `mean stage ${fmt(a.mean_stage, 1)} against ${fmt(b.mean_stage, 1)}`);
    }
    svg.setAttribute("aria-label", "Scatter of stage reached against tokens per run for the six benchmark runs. GPT-6.1 Sol reached stage 5 in all three runs with a mean of about 397 thousand tokens; GPT-5.6 Sol reached stages 4, 3 and 1 with a mean of about 698 thousand tokens.");
    table(document.querySelector("#fig-outcome .table-view"), [{ t: "Run" }, { t: "Model" }, { t: "Stage", r: 1 }, { t: "Tokens", r: 1 }, { t: "Minutes", r: 1 }, { t: "Outcome" }],
      runs.map((r) => [hhmm(r.run_id), MODEL[r.model].name, r.stage, fmt(r.tokens), fmt(r.duration_s / 60, 1), status(r)]));
  }

  /* ======================================================================================== */
  function drawStages(D) {
    const host = document.querySelector("#fig-stages .fig-body"); if (!host) return;
    const runs = D.runs.filter((r) => r.benchmark).sort((a, b) => (a.model < b.model ? 1 : -1) || a.run_id.localeCompare(b.run_id));
    const rowH = 34, top = 34, H = top + runs.length * rowH + 20 + 12;
    const { svg, w } = svgFor(host, H);
    const m = { l: 170, r: 24 };
    const x = lin(0, 5, m.l, w - m.r);
    for (let s = 0; s <= 5; s++) {
      el("line", { x1: x(s), x2: x(s), y1: top - 6, y2: H - 22, style: "stroke:var(--grid)" }, svg);
      const t = el("text", { x: x(s), y: top - 14, "text-anchor": s === 0 ? "start" : s === 5 ? "end" : "middle" }, svg, `${s} ${["", "around", "lifted", "in basket", "out", "complete"][s]}`.trim());
      if (w < 620 && s % 2 === 1 && s < 5) t.textContent = String(s);
    }
    runs.forEach((r, i) => {
      const yy = top + i * rowH + 6, k = r.model;
      el("text", { x: m.l - 12, y: yy + 15, "text-anchor": "end", class: "t-ink2" }, svg, `${MODEL[k].name} · ${hhmm(r.run_id)}`);
      el("rect", { x: x(0), y: yy + 4, width: x(5) - x(0), height: 16, rx: 3, style: "fill:var(--surface-2);stroke:var(--rule)" }, svg);
      const g = el("g", { class: "mk" }, svg);
      if (+r.stage > 0) el("rect", { x: x(0), y: yy + 4, width: x(+r.stage) - x(0), height: 16, rx: 3, style: `fill:${MODEL[k].c}` }, g);
      el("text", { x: x(+r.stage) + (+r.stage > 4.6 ? -8 : 8), y: yy + 16, "text-anchor": +r.stage > 4.6 ? "end" : "start", class: +r.stage > 4.6 ? "" : "t-ink2",
        style: +r.stage > 4.6 ? "fill:#fff;font-weight:600" : "" }, g, r.success ? "complete" : (r.failure_mode || "").replace(/\(.*\)/, "").slice(0, w < 620 ? 18 : 60));
      const hit = el("rect", { x: x(0), y: yy, width: x(5) - x(0), height: 24, class: "hit" }, svg);
      hover(hit, `<b>${MODEL[k].name}</b> · run ${hhmm(r.run_id)} · stage ${r.stage}<div class="note">${esc(r.summary || "")}</div>`);
    });
    svg.setAttribute("aria-label", "How far each benchmark run got, stage 0 to 5. All three GPT-6.1 Sol runs reached stage 5. GPT-5.6 Sol reached stage 4, 3 and 1.");
    table(document.querySelector("#fig-stages .table-view"), [{ t: "Run" }, { t: "Model" }, { t: "Stage", r: 1 }, { t: "What stopped it" }],
      runs.map((r) => [hhmm(r.run_id), MODEL[r.model].name, r.stage, esc(r.success ? "complete" : r.failure_mode)]));
  }

  /* ======================================================================================== */
  function drawTimeline(D, which) {
    const host = document.querySelector("#fig-timeline .fig-body"); if (!host) return;
    const runs = D.runs.filter((r) => (which === "all" ? r.benchmark || r.run_id === "20261116-130056-gpt-5.6-sol" : r.benchmark))
      .sort((a, b) => a.run_id.localeCompare(b.run_id));
    const maxMin = Math.max(...runs.map((r) => r.duration_s / 60));
    const rowH = 44, top = 10, H = top + runs.length * rowH + 48;
    const { svg, w } = svgFor(host, H);
    const m = { l: 128, r: 18 };
    const x = lin(0, Math.ceil(maxMin / 5) * 5, m.l, w - m.r);
    const tv = ticks(0, Math.ceil(maxMin / 5) * 5, w < 600 ? 4 : 8);
    tv.forEach((v) => el("line", { x1: x(v), x2: x(v), y1: top, y2: H - 40, style: "stroke:var(--grid)" }, svg));
    axisX(svg, x, H - 40, tv, (v) => v + "", "minutes since the run started", w);
    runs.forEach((r, i) => {
      const y0 = top + i * rowH, k = r.model, mid = y0 + rowH / 2;
      el("text", { x: m.l - 12, y: mid - 2, "text-anchor": "end", class: "t-ink2" }, svg, MODEL[k].name);
      el("text", { x: m.l - 12, y: mid + 13, "text-anchor": "end" }, svg, `${hhmm(r.run_id)} · ${r.success ? "complete" : "stage " + r.stage}`);
      el("rect", { x: m.l - 6, y: mid - 9, width: 3, height: 18, style: `fill:${MODEL[k].c}` }, svg);
      // held spans
      let holdStart = null;
      r.steps.forEach((s) => {
        const t = s.t_end_s / 60;
        if (s.holding && holdStart === null) holdStart = (s.t_end_s - s.motion_s) / 60;
        if (!s.holding && holdStart !== null) { el("rect", { x: x(holdStart), y: y0 + 6, width: Math.max(1, x(t) - x(holdStart)), height: rowH - 12, style: "fill:var(--hold)" }, svg); holdStart = null; }
      });
      if (holdStart !== null) el("rect", { x: x(holdStart), y: y0 + 6, width: x(r.duration_s / 60) - x(holdStart), height: rowH - 12, style: "fill:var(--hold)" }, svg);
      el("line", { x1: x(0), x2: x(r.duration_s / 60), y1: mid, y2: mid, style: "stroke:var(--rule)" }, svg);
      r.steps.forEach((s) => {
        const tEnd = s.t_end_s, mv = s.motion_s, th = s.think_s;
        const t0 = (tEnd - mv - th) / 60, t1 = (tEnd - mv) / 60, t2 = tEnd / 60;
        const g = el("g", { class: "mk" }, svg);
        el("rect", { x: x(t0), y: mid - 6, width: Math.max(0.6, x(t1) - x(t0) - 0.6), height: 12, style: "fill:var(--think)" }, g);
        if (mv > 0.2) el("rect", { x: x(t1), y: mid - 6, width: Math.max(1.2, x(t2) - x(t1)), height: 12, style: "fill:var(--s3)" }, g);
        if (s.action === "gripper") el("path", { d: `M${x(t2)},${mid - 10} l-4,-7 h8 z`, style: `fill:var(--ink)` }, g);
        if (!s.ok) el("path", { d: `M${x(t2) - 4},${mid + 9} l8,8 M${x(t2) + 4},${mid + 9} l-8,8`, style: "stroke:var(--bad);stroke-width:1.8" }, g);
        const hit = el("rect", { x: x(t0), y: y0 + 2, width: Math.max(4, x(t2) - x(t0)), height: rowH - 4, class: "hit" }, svg);
        hover(hit, () => `<b>#${s.n} ${esc(s.action)}</b> ${esc(s.target)} <span class="tm">${s.ok ? "" : "· refused/failed"}</span><br><span class="tm">thinking ${fmt(th, 1)} s · moving ${fmt(mv, 1)} s · at ${fmt(t2, 1)} min${s.tool_z_mm != null ? ` · tool z ${s.tool_z_mm} mm` : ""}</span><div class="note">${esc((s.note || "").replace(/\s*\(expects:.*$/, "").slice(0, 260))}</div><span class="tm">${esc((s.result || "").slice(0, 140))}</span>`, svg, [g]);
      });
      el("text", { x: x(r.duration_s / 60) + 6, y: mid + 4, style: `fill:${r.success ? "var(--ok)" : "var(--bad)"};font-weight:600` }, svg, r.success ? "✓" : "✕");
    });
    svg.setAttribute("aria-label", "One row per benchmark run: grey bars are the model thinking, green bars the arm moving, triangles gripper actions, crosses refused or failed moves, shaded spans the bottle being held.");
    table(document.querySelector("#fig-timeline .table-view"), [{ t: "Run" }, { t: "Model" }, { t: "Minutes", r: 1 }, { t: "Thinking", r: 1 }, { t: "Moving", r: 1 }, { t: "Decisions", r: 1 }, { t: "Refused or failed", r: 1 }],
      runs.map((r) => [hhmm(r.run_id), MODEL[r.model].name, fmt(r.duration_s / 60, 1), Math.round(100 * r.thinking_s / r.duration_s) + "%", Math.round(100 * r.moving_s / r.duration_s) + "%", r.decisions, r.refused_or_failed]));
  }

  /* ======================================================================================== */
  function drawLatency(D) {
    const host = document.querySelector("#fig-latency .fig-body"); if (!host) return;
    const runs = D.runs.filter((r) => r.benchmark);
    const models = ["gpt-6.1-sol", "gpt-5.6-sol"];
    const vals = {}; models.forEach((k) => vals[k] = []);
    runs.forEach((r) => r.steps.forEach((s) => { if (s.think_s > 0) vals[r.model].push({ v: s.think_s, s, r }); }));
    const H = 190, { svg, w } = svgFor(host, H), m = { l: 110, r: 20 };
    const hi = Math.ceil(Math.max(...models.flatMap((k) => vals[k].map((p) => p.v))) / 20) * 20;
    const x = lin(0, hi, m.l, w - m.r);
    const tv = ticks(0, hi, w < 600 ? 4 : 7);
    tv.forEach((v) => el("line", { x1: x(v), x2: x(v), y1: 10, y2: H - 44, style: "stroke:var(--grid)" }, svg));
    axisX(svg, x, H - 44, tv, (v) => v + " s", "seconds of model time per decision", w);
    models.forEach((k, i) => {
      const yc = 40 + i * 62, pts = vals[k].slice().sort((a, b) => a.v - b.v);
      el("text", { x: m.l - 12, y: yc + 4, "text-anchor": "end", class: "t-ink2" }, svg, MODEL[k].name);
      const g = el("g", { class: "mk" }, svg);
      pts.forEach((p, j) => {
        const c = el("circle", { cx: x(p.v), cy: yc + ((j * 7919) % 21) - 10, r: 3.2, style: `fill:${MODEL[k].c};fill-opacity:.55` }, g);
        hover(c, `<b>${MODEL[k].name}</b> · run ${hhmm(p.r.run_id)} · #${p.s.n} ${esc(p.s.action)}<br><span class="tm">${fmt(p.v, 1)} s thinking · ${fmt(p.s.tokens)} tokens</span>`);
      });
      const med = pts[Math.floor(pts.length / 2)].v;
      el("line", { x1: x(med), x2: x(med), y1: yc - 18, y2: yc + 18, style: "stroke:var(--ink);stroke-width:2" }, svg);
      el("text", { x: x(med) + 6, y: yc - 20, class: "t-ink" }, svg, `median ${fmt(med, 1)} s (n = ${pts.length})`);
    });
    svg.setAttribute("aria-label", "Seconds of model time per decision in the benchmark, one dot per decision, with each model's median.");
    table(document.querySelector("#fig-latency .table-view"), [{ t: "Model" }, { t: "Decisions", r: 1 }, { t: "Median s", r: 1 }, { t: "90th percentile s", r: 1 }, { t: "Max s", r: 1 }],
      models.map((k) => { const v = vals[k].map((p) => p.v).sort((a, b) => a - b); return [MODEL[k].name, v.length, fmt(v[Math.floor(v.length / 2)], 1), fmt(v[Math.floor(v.length * 0.9)], 1), fmt(v[v.length - 1], 1)]; }));
  }

  /* ======================================================================================== */
  // When each fix went in, from the commit times (16 Nov): walls + camera re-pose 11:42-12:04; top-down depth
  // map 12:15, finger direction and depth memory 12:25-12:28; checker pictures + stale-stop fix 13:00; hunting watch 13:22.
  const FIXES = [
    { after: "20261116-112819", t: "wall model, camera re-pose" },
    { after: "20261116-120425", t: "top-down depth map" },
    { after: "20261116-121848", t: "finger direction, depth memory" },
    { after: "20261116-125757", t: "checker pictures, stale-stop fix" },
    { after: "20261116-130056", t: "hunting watch" },
  ];
  function drawHistory(D) {
    const host = document.querySelector("#fig-history .fig-body"); if (!host) return;
    const runs = D.runs.slice().sort((a, b) => a.run_id.localeCompare(b.run_id));
    const H = 300, { svg, w } = svgFor(host, H), m = { l: 46, r: 16, t: 56, b: 60 };
    const n = runs.length, bw = (w - m.l - m.r) / n;
    const hi = Math.ceil(Math.max(...runs.map((r) => (r.duration_s || 0) / 60)) / 10) * 10;
    const y = lin(0, hi, H - m.b, m.t);
    gridY(svg, y, ticks(0, hi, 4), m.l, w - m.r, (v) => v + "");
    el("text", { x: 4, y: m.t - 14 }, svg, "minutes");
    FIXES.forEach((f) => {
      const i = runs.findIndex((r) => r.run_id.startsWith(f.after)); if (i < 0) return;
      const xx = m.l + (i + 1) * bw;
      el("line", { x1: xx, x2: xx, y1: m.t - 6, y2: H - m.b, style: "stroke:var(--accent);stroke-dasharray:3 3" }, svg);
      if (w >= 720) el("text", { x: xx + 4, y: m.t - 18 - (FIXES.indexOf(f) % 2) * 14, style: "fill:var(--ink-2);font-size:11px" }, svg, f.t);
    });
    runs.forEach((r, i) => {
      const cx = m.l + (i + 0.5) * bw, v = (r.duration_s || 0) / 60, k = r.model;
      const g = el("g", { class: "mk" }, svg);
      el("line", { x1: cx, x2: cx, y1: y(0), y2: y(v), style: `stroke:${MODEL[k].c};stroke-width:2` }, g);
      const ok = r.success, stopped = r.state === "stopped";
      if (ok) el("circle", { cx, cy: y(v), r: 6, style: `fill:${MODEL[k].c};stroke:var(--surface);stroke-width:2` }, g);
      else if (stopped) el("rect", { x: cx - 5, y: y(v) - 5, width: 10, height: 10, style: `fill:var(--surface);stroke:${MODEL[k].c};stroke-width:1.8` }, g);
      else el("path", { d: `M${cx - 5},${y(v) - 5} l10,10 M${cx + 5},${y(v) - 5} l-10,10`, style: `stroke:${MODEL[k].c};stroke-width:2.2` }, g);
      if (r.benchmark) el("text", { x: cx, y: H - m.b + 30, "text-anchor": "middle", style: "font-size:10px" }, svg, "bench");
      el("text", { x: cx, y: H - m.b + 16, "text-anchor": "middle", style: "font-size:10.5px" }, svg, w < 700 && i % 2 ? "" : hhmm(r.run_id));
      const hit = el("rect", { x: cx - bw / 2, y: m.t, width: bw, height: H - m.b - m.t, class: "hit" }, svg);
      hover(hit, `<b>${day(r.run_id)} ${hhmm(r.run_id)} · ${MODEL[k].name}</b><br><span class="tm">${r.task.replace("_", " ")} · ${fmt(v, 1)} min · ${r.decisions} decisions · ${ok ? "complete" : stopped ? "stopped by hand" : (r.failure_mode || r.reason)}</span><div class="note">${esc((r.summary || "").slice(0, 300))}</div>`, svg, [g]);
    });
    el("text", { x: m.l, y: H - 10 }, svg, "15 Nov evening ←→ 16 Nov");
    svg.setAttribute("aria-label", "Every run in order with its length in minutes: circles complete, crosses failed, squares stopped by hand; dashed lines mark the fixes made between runs.");
    table(document.querySelector("#fig-history .table-view"), [{ t: "When" }, { t: "Model" }, { t: "Task" }, { t: "Minutes", r: 1 }, { t: "Outcome" }],
      runs.map((r) => [`${day(r.run_id)} ${hhmm(r.run_id)}`, MODEL[r.model].name, r.task.replace("_", " "), fmt((r.duration_s || 0) / 60, 1), status(r) + (r.failure_mode ? ` <span class="muted small">${esc(r.failure_mode)}</span>` : "")]));
  }

  /* ======================================================================================== */
  function drawHeight(D) {
    const host = document.querySelector("#fig-height .fig-body"); if (!host) return;
    const A = D.runs.find((r) => r.run_id.startsWith("20261116-121848")), B = D.runs.find((r) => r.run_id.startsWith("20261116-122955"));
    if (!A || !B) return;
    const H = 260, { svg, w } = svgFor(host, H), m = { l: 52, r: 20, t: 16, b: 48 };
    const tmax = Math.ceil(Math.max(A.duration_s, B.duration_s) / 60);
    const x = lin(0, tmax, m.l, w - m.r), y = lin(0, 360, H - m.b, m.t);
    gridY(svg, y, [0, 100, 200, 300], m.l, w - m.r, (v) => v + " mm");
    axisX(svg, x, H - m.b, ticks(0, tmax, 6), (v) => v + "", "minutes since the run started", w);
    [[110, "basket rim"], [55, "top of the bottle lying in the basket"]].forEach(([v, t]) => {
      el("line", { x1: m.l, x2: w - m.r, y1: y(v), y2: y(v), style: "stroke:var(--ink-3);stroke-dasharray:2 4" }, svg);
      el("text", { x: w - m.r - 4, y: y(v) - 5, "text-anchor": "end", style: "font-size:11px" }, svg, t);
    });
    [[A, "var(--ink-3)", "6 4", "12:18 · hovered"], [B, "var(--s2)", null, "12:29 · first success"]].forEach(([r, c, dash, lab]) => {
      const pts = r.steps.filter((s) => s.tool_z_mm != null).map((s) => [x(s.t_end_s / 60), y(s.tool_z_mm)]);
      const d = pts.map((p, i) => (i ? "L" : "M") + p[0].toFixed(1) + "," + p[1].toFixed(1)).join(" ");
      const g = el("g", { class: "mk" }, svg);
      el("path", { d, style: `fill:none;stroke:${c};stroke-width:2;${dash ? "stroke-dasharray:" + dash : ""}` }, g);
      r.steps.filter((s) => s.tool_z_mm != null).forEach((s) => {
        const c2 = el("circle", { cx: x(s.t_end_s / 60), cy: y(s.tool_z_mm), r: s.action === "gripper" ? 5 : 3, style: `fill:${s.action === "gripper" ? "var(--surface)" : c};stroke:${c};stroke-width:1.6` }, g);
        hover(c2, `<b>${lab}</b> · #${s.n} ${esc(s.action)} ${esc(s.target)}<br><span class="tm">tool z ${s.tool_z_mm} mm at ${fmt(s.t_end_s / 60, 1)} min</span>`);
      });
      const last = pts[pts.length - 1];
      el("text", { x: Math.min(last[0] + 8, w - m.r - 140), y: last[1] - 8, style: `fill:${c};font-weight:600` }, svg, lab);
    });
    svg.setAttribute("aria-label", "Tool height after each move: the hovering run 12:18 stayed between 189 and 300 mm; the first success at 12:29 went down to 45 mm, closed the gripper and lifted.");
    table(document.querySelector("#fig-height .table-view"), [{ t: "Run" }, { t: "Decision" }, { t: "Action" }, { t: "Minute", r: 1 }, { t: "Tool z mm", r: 1 }],
      [A, B].flatMap((r) => r.steps.map((s) => [hhmm(r.run_id), "#" + s.n, esc(s.action + " " + s.target), fmt(s.t_end_s / 60, 1), s.tool_z_mm ?? ""])));
  }

  /* ======================================================================================== */
  function drawBuzz(Hd) {
    const host = document.querySelector("#fig-buzz .fig-body"); if (!host || !Hd) return;
    const holds = Hd.holds.map((h) => ({ ...h, worst: Math.max(...h.motor_speed_std.map((v) => v || 0)), j: h.motor_speed_std.indexOf(Math.max(...h.motor_speed_std.map((v) => v || 0))) + 1 }));
    const H = 220, { svg, w } = svgFor(host, H), m = { l: 150, r: 22 };
    const x = logs(0.002, 1, m.l, w - m.r);
    [0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1].forEach((v) => el("line", { x1: x(v), x2: x(v), y1: 14, y2: H - 44, style: "stroke:var(--grid)" }, svg));
    axisX(svg, x, H - 44, [0.002, 0.01, 0.05, 0.1, 0.5, 1], (v) => String(v), "worst motor's speed jitter while holding still (rad/s, std over the hold; log scale)", w);
    [[0.08, "driver nudges above this"], [0.03, "quiet holds"]].forEach(([v, t], i) => {
      el("line", { x1: x(v), x2: x(v), y1: 10, y2: H - 44, style: `stroke:${i ? "var(--ink-3)" : "var(--accent)"};stroke-dasharray:4 3` }, svg);
      el("text", { x: x(v) + 5, y: 22 + i * 14, style: "font-size:11px" }, svg, t);
    });
    [[false, "Before the fix", "run 130056"], [true, "After the fix", "6 benchmark runs"]].forEach(([fixed, lab, sub], i) => {
      const yc = 64 + i * 66, pts = holds.filter((h) => h.fixed === fixed);
      el("text", { x: m.l - 12, y: yc, "text-anchor": "end", class: "t-ink2" }, svg, lab);
      el("text", { x: m.l - 12, y: yc + 15, "text-anchor": "end" }, svg, `${sub} · ${pts.length} holds`);
      pts.forEach((h, j) => {
        const c = el("circle", { cx: x(Math.max(0.002, h.worst)), cy: yc + ((j * 7919) % 25) - 12, r: 3.4, style: `fill:${h.worst > 0.08 ? "var(--bad)" : "var(--ink-3)"};fill-opacity:${h.worst > 0.08 ? .9 : .45}` }, svg);
        hover(c, `<b>${lab}</b> · run ${hhmm(h.run_id)} after #${h.after_n} ${esc(h.action)}<br><span class="tm">worst J${h.j}: ${fmt(h.worst, 3)} rad/s over ${h.secs} s · nudges ${h.nudges}</span><br><span class="tm">${h.motor_speed_std.map((v, k) => "J" + (k + 1) + " " + (v == null ? "-" : v.toFixed(3))).join("  ")}</span>`);
      });
      const over = pts.filter((h) => h.worst > 0.15).length;
      el("text", { x: w - m.r, y: yc - 18, "text-anchor": "end", class: "t-ink" }, svg, `${over} of ${pts.length} above 0.15 rad/s`);
    });
    svg.setAttribute("aria-label", "Worst motor jitter per hold. Before the driver fix, 3 of 28 holds were above 0.15 rad/s; after it, 0 of 231.");
    const q = (arr, p) => { const s = arr.slice().sort((a, b) => a - b); return s[Math.min(s.length - 1, Math.floor(p * s.length))]; };
    table(document.querySelector("#fig-buzz .table-view"), [{ t: "" }, { t: "Holds", r: 1 }, { t: "Median rad/s", r: 1 }, { t: "90th pct rad/s", r: 1 }, { t: "Above 0.08", r: 1 }, { t: "Above 0.15", r: 1 }],
      [false, true].map((f) => { const v = holds.filter((h) => h.fixed === f).map((h) => h.worst); return [f ? "After the fix" : "Before the fix", v.length, fmt(q(v, .5), 3), fmt(q(v, .9), 3), v.filter((x) => x > .08).length, v.filter((x) => x > .15).length]; }));
  }

  /* ======================================================================================== */
  function runsTable(D) {
    const host = document.getElementById("runs-table"); if (!host) return;
    const M = window.GAR_MEDIA || { full: {} };
    const rows = D.runs.slice().sort((a, b) => a.run_id.localeCompare(b.run_id));
    host.innerHTML = `<table class="data" id="runs-tbl"><thead><tr>
      <th><button type="button">When</button></th><th><button type="button">Model</button></th><th><button type="button">Task</button></th>
      <th><button type="button">Outcome</button></th><th class="r"><button type="button">Stage</button></th><th class="r"><button type="button">Min</button></th>
      <th class="r"><button type="button">Decisions</button></th><th class="r"><button type="button">Tokens</button></th><th>Look</th></tr></thead><tbody>${
      rows.map((r) => {
        const cam = M.full[`${r.run_id}-cameras`], por = M.full[`${r.run_id}-portal`];
        const links = [
          r.steps.length ? `<a href="#replay" data-replay="${r.run_id}">replay</a>` : "",
          cam ? `<a href="${cam.path}" target="_blank" rel="noopener">cameras</a>` : "",
          por ? `<a href="${por.path}" target="_blank" rel="noopener">portal${por.partial ? " (end)" : ""}</a>` : "",
          `<a href="data/raw/${r.run_id}/run.json" target="_blank" rel="noopener">log</a>`].filter(Boolean).join(" · ");
        return `<tr data-model="${r.model}" data-bench="${r.benchmark ? 1 : 0}" data-ok="${r.success ? 1 : 0}">
          <td data-v="${r.run_id}">${day(r.run_id)} ${hhmm(r.run_id)}${r.benchmark ? ' <span class="tag">bench</span>' : ""}</td>
          <td><span class="swatch" style="background:${MODEL[r.model].c}"></span>${MODEL[r.model].name}</td>
          <td>${r.task.replace(/_/g, " ")}</td><td>${status(r)}<div class="muted small">${esc(r.success ? "" : (r.failure_mode || r.reason))}</div></td>
          <td class="r">${r.stage === "" || r.stage == null ? "–" : r.stage}</td><td class="r" data-v="${r.duration_s}">${fmt((r.duration_s || 0) / 60, 1)}</td>
          <td class="r">${r.decisions}</td><td class="r" data-v="${r.tokens || 0}">${r.tokens ? fmt(r.tokens) : "–"}</td><td class="small">${links}</td></tr>`;
      }).join("")}</tbody></table>`;
    const tbl = host.querySelector("table");
    if (window.GAR && window.GAR.sortable) window.GAR.sortable(tbl);
    document.querySelectorAll("#runs .filters .chip").forEach((b) => b.addEventListener("click", () => {
      document.querySelectorAll("#runs .filters .chip").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
      const f = b.dataset.f;
      [...tbl.tBodies[0].rows].forEach((tr) => {
        const show = f === "all" || (f === "bench" && tr.dataset.bench === "1") || (f === "ok" && tr.dataset.ok === "1") || tr.dataset.model === f;
        tr.hidden = !show;
      });
    }));
  }

  /* ======================================================================================== */
  let DATA = null, HOLDS = null, tlMode = "bench";
  function drawAll() {
    if (!DATA) return;
    hideTip();
    drawOutcome(DATA); drawStages(DATA); drawTimeline(DATA, tlMode); drawLatency(DATA); drawHistory(DATA); drawHeight(DATA); drawBuzz(HOLDS);
  }
  let rt = null;
  window.addEventListener("resize", () => { clearTimeout(rt); rt = setTimeout(drawAll, 160); });
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-tl]"); if (!b) return;
    tlMode = b.dataset.tl; document.querySelectorAll("[data-tl]").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
    drawTimeline(DATA, tlMode);
  });
  Promise.all([fetch("data/report.json").then((r) => r.json()), fetch("data/holds.json").then((r) => r.json()).catch(() => null),
               fetch("media/manifest.json").then((r) => r.json()).catch(() => ({ clips: {}, full: {} }))])
    .then(([d, h, man]) => {
      DATA = d; HOLDS = h; window.GAR_DATA = d; window.GAR_MEDIA = man;
      drawAll(); runsTable(d);
      document.dispatchEvent(new CustomEvent("gar:data", { detail: d }));
    })
    .catch((e) => { document.querySelectorAll(".fig-body").forEach((b) => { b.innerHTML = '<p class="muted small">The figure data could not be loaded (' + esc(e.message) + ").</p>"; }); });
})();
