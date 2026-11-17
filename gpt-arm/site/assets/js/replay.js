/* GPT Arm report: the portal mock-up. Steps through a run's decisions with the three pictures the model was
   given, its note, the action and what the arm reported. Data: data/report.json (steps) and
   media/steps/<run id>/NNN-{overhead,wrist,depth}.webp. */
(function () {
  "use strict";
  const host = document.getElementById("replay-app");
  if (!host) return;
  const RUNS = ["20261116-141102-gpt-6.1-sol", "20261116-133324-gpt-6.1-sol", "20261116-150826-gpt-6.1-sol",
    "20261116-134633-gpt-5.6-sol", "20261116-142403-gpt-5.6-sol", "20261116-154924-gpt-5.6-sol",
    "20261116-130056-gpt-5.6-sol", "20261116-122955-gpt-5.6-sol", "20261116-121848-gpt-5.6-sol"];
  const NAME = { "gpt-6.1-sol": "GPT-6.1 Sol", "gpt-5.6-sol": "GPT-5.6 Sol" };
  const esc = (s) => String(s == null ? "" : s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const fmt = (n, d = 0) => Number(n).toLocaleString("en-GB", { minimumFractionDigits: d, maximumFractionDigits: d });
  let D = null, run = null, i = 0, timer = null;

  host.innerHTML = `
    <div class="rp">
      <div class="cams">
        <div class="cam wide"><span class="lab">overhead</span><img id="rp-oh" alt="Overhead camera picture the model was given" loading="lazy"></div>
        <div class="row">
          <div class="cam"><span class="lab">wrist</span><img id="rp-wr" alt="Wrist camera picture the model was given" loading="lazy"></div>
          <div class="cam depth"><span class="lab">depth map</span><img id="rp-dp" alt="Top-down depth height map the model was given" loading="lazy"></div>
        </div>
      </div>
      <div class="side">
        <div class="top">
          <label class="small muted" for="rp-run">Run</label>
          <select id="rp-run"></select>
          <span id="rp-outcome"></span>
        </div>
        <div class="meta">
          <div><b>Decision</b><span id="rp-k">–</span></div>
          <div><b>Elapsed</b><span id="rp-t">–</span></div>
          <div><b>Thinking</b><span id="rp-th">–</span></div>
          <div><b>Tool z after</b><span id="rp-z">–</span></div>
        </div>
        <div class="card" aria-live="polite">
          <div class="act" id="rp-act"></div>
          <p class="note" id="rp-note"></p>
          <div class="res" id="rp-res"></div>
          <div class="timing" id="rp-timing"></div>
        </div>
        <div class="scrub">
          <button class="iconbtn" type="button" id="rp-prev" aria-label="Previous decision"><svg viewBox="0 0 16 16" fill="currentColor"><path d="M10.5 3.5 5.5 8l5 4.5z"/></svg></button>
          <button class="iconbtn" type="button" id="rp-play" aria-label="Play through the run" aria-pressed="false"><svg class="off" viewBox="0 0 16 16" fill="currentColor"><path d="M5 3v10l8-5z"/></svg><svg class="on" viewBox="0 0 16 16" fill="currentColor"><rect x="4" y="3" width="3" height="10"/><rect x="9" y="3" width="3" height="10"/></svg></button>
          <button class="iconbtn" type="button" id="rp-next" aria-label="Next decision"><svg viewBox="0 0 16 16" fill="currentColor"><path d="M5.5 3.5 10.5 8l-5 4.5z"/></svg></button>
          <input type="range" id="rp-slider" min="0" max="0" value="0" aria-label="Decision">
          <span class="pos" id="rp-pos">0 / 0</span>
        </div>
        <div class="ticks" id="rp-ticks" aria-hidden="true"></div>
      </div>
    </div>`;
  const $ = (id) => document.getElementById(id);

  function img(rid, name) { return name ? `media/steps/${rid}/${name.replace(/\.jpg$/, ".webp")}` : ""; }
  function setRun(rid, at) {
    run = D.runs.find((r) => r.run_id === rid);
    if (!run) return;
    $("rp-run").value = rid;
    $("rp-slider").max = String(run.steps.length - 1);
    const st = run.success ? '<span class="status ok">Complete</span>' : run.state === "stopped" ? '<span class="status na">Stopped by hand</span>' : '<span class="status bad">Failed</span>';
    $("rp-outcome").innerHTML = st;
    const ticks = $("rp-ticks"); ticks.innerHTML = "";
    run.steps.forEach((s, k) => {
      if (s.action !== "gripper" && s.ok) return;
      const t = document.createElement("i");
      t.style.left = `calc(${(k / Math.max(1, run.steps.length - 1)) * 100}% - 1px)`;
      t.style.background = !s.ok ? "var(--bad)" : "var(--ink)";
      ticks.appendChild(t);
    });
    show(at || 0);
    // preload the next few pictures
  }
  function show(k) {
    if (!run) return;
    i = Math.max(0, Math.min(run.steps.length - 1, k));
    const s = run.steps[i], rid = run.run_id;
    $("rp-oh").src = img(rid, s.images.overhead); $("rp-wr").src = img(rid, s.images.wrist);
    const dp = $("rp-dp"); if (s.images.depth) { dp.src = img(rid, s.images.depth); dp.parentElement.hidden = false; } else dp.parentElement.hidden = true;
    $("rp-k").textContent = `#${s.n}`;
    $("rp-t").textContent = `${fmt(s.t_end_s / 60, 1)} min`;
    $("rp-th").textContent = `${fmt(s.think_s, 1)} s`;
    $("rp-z").textContent = s.tool_z_mm != null ? `${s.tool_z_mm} mm` : "–";
    $("rp-act").innerHTML = `<code class="skill${s.ok ? "" : " no"}">${esc(s.action)}</code>${s.target ? `<code>${esc(s.target)}</code>` : ""}${s.holding ? '<span class="tag">holding</span>' : ""}`;
    $("rp-note").textContent = (s.note || "").replace(/\s*\(expects:.*$/s, "");
    const res = $("rp-res"); res.textContent = s.result || ""; res.classList.toggle("no", !s.ok);
    const exp = (s.note || "").match(/\(expects:\s*(.*)\)\s*$/s);
    $("rp-timing").textContent = exp ? `Expected: ${exp[1]}` : "";
    $("rp-slider").value = String(i);
    $("rp-pos").textContent = `${i + 1} / ${run.steps.length}`;
    [1, 2].forEach((d) => { const n = run.steps[i + d]; if (n) { const p = new Image(); p.src = img(rid, n.images.overhead); } });
  }
  function stop() { clearInterval(timer); timer = null; $("rp-play").setAttribute("aria-pressed", "false"); }
  function play() {
    if (timer) return stop();
    $("rp-play").setAttribute("aria-pressed", "true");
    timer = setInterval(() => { if (i >= run.steps.length - 1) stop(); else show(i + 1); }, 1700);
  }

  document.addEventListener("gar:data", (e) => {
    D = e.detail;
    const sel = $("rp-run");
    sel.innerHTML = RUNS.map((rid) => {
      const r = D.runs.find((x) => x.run_id === rid); if (!r) return "";
      const what = r.task === "full_cycle" ? "full cycle" : r.task.replace(/_/g, " ");
      return `<option value="${rid}">${NAME[r.model] || r.model} · ${rid.slice(9, 11)}:${rid.slice(11, 13)} · ${what} · ${r.success ? "complete" : "failed"}</option>`;
    }).join("");
    setRun(RUNS[0]);
  });
  $("rp-run").addEventListener("change", (e) => { stop(); setRun(e.target.value); });
  $("rp-slider").addEventListener("input", (e) => { stop(); show(+e.target.value); });
  $("rp-prev").addEventListener("click", () => { stop(); show(i - 1); });
  $("rp-next").addEventListener("click", () => { stop(); show(i + 1); });
  $("rp-play").addEventListener("click", play);
  host.addEventListener("keydown", (e) => {
    if (e.target.tagName === "SELECT") return;
    if (e.key === "ArrowRight") { stop(); show(i + 1); e.preventDefault(); }
    if (e.key === "ArrowLeft") { stop(); show(i - 1); e.preventDefault(); }
  });
  document.addEventListener("click", (e) => {
    const a = e.target.closest("[data-replay]"); if (!a || !D) return;
    const rid = a.dataset.replay;
    if (![...$("rp-run").options].some((o) => o.value === rid)) {
      const r = D.runs.find((x) => x.run_id === rid); if (!r || !r.steps.length) return;
      const o = document.createElement("option"); o.value = rid; o.textContent = `${NAME[r.model] || r.model} · ${rid.slice(9, 11)}:${rid.slice(11, 13)} · ${r.task.replace(/_/g, " ")}`; $("rp-run").appendChild(o);
    }
    stop(); setRun(rid);
  });
})();
