/* GPT Arm portal: one page, no build step, no libraries, no network requests beyond its own service.
   Contract: gpt-arm/API.md. Polls /api/status at 1 Hz and each camera at about 1 Hz while the page is
   visible (every 5 s / 10 s when hidden). Everything the service sends is shown with textContent, never
   parsed as HTML. */
(() => {
  'use strict';

  // ------------------------------------------------------------------ small helpers
  const $ = (id) => document.getElementById(id);

  /* el('span', {class: 'x', text: 'hi', onclick: fn, title: 't'}, child, ...) */
  function el(tag, props, ...kids) {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') n.className = v;
      else if (k === 'text') n.textContent = v;
      else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
      else n.setAttribute(k, v === true ? '' : v);
    }
    for (const c of kids.flat()) {
      if (c == null || c === false) continue;
      n.append(c instanceof Node ? c : String(c));
    }
    return n;
  }

  /* Native append() would print null as the text "null"; this skips empty children. */
  const put = (parent, ...kids) => parent.append(...kids.flat().filter((k) => k != null && k !== false));

  const DASH = '–';
  const pad = (v) => String(v).padStart(2, '0');
  const fmt = {
    num: (v, d = 0) => (v == null || !isFinite(v) ? DASH : Number(v).toFixed(d)),
    tok: (v) => (v == null ? DASH : v >= 1e6 ? (v / 1e6).toFixed(2) + 'M' : v >= 1e3 ? (v / 1e3).toFixed(1) + 'k' : String(v)),
    dur(s) {
      if (s == null || !isFinite(s)) return DASH;
      s = Math.max(0, Math.round(s));
      const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), x = s % 60;
      return h ? `${h}:${pad(m)}:${pad(x)}` : `${m}:${pad(x)}`;
    },
    clock(t) {
      if (!t) return DASH;
      const d = new Date(t * 1000);
      return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
    },
    date(t) {
      if (!t) return DASH;
      const d = new Date(t * 1000);
      return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
    },
    ago(sec) {
      if (sec == null || !isFinite(sec)) return DASH;
      if (sec < 60) return `${Math.round(sec)} s ago`;
      if (sec < 3600) return `${Math.round(sec / 60)} min ago`;
      if (sec < 86400) return `${(sec / 3600).toFixed(1)} h ago`;
      return `${Math.round(sec / 86400)} d ago`;
    },
    xy: (a) => (Array.isArray(a) ? '(' + a.map((v) => Math.round(v)).join(', ') + ')' : DASH),
    words: (s) => String(s).replace(/_/g, ' '),
  };

  const store = {
    get(k, d) { try { const v = localStorage.getItem('gptarm.' + k); return v == null ? d : JSON.parse(v); } catch (e) { return d; } },
    set(k, v) { try { localStorage.setItem('gptarm.' + k, JSON.stringify(v)); } catch (e) { /* private mode */ } },
  };

  // ------------------------------------------------------------------ API access
  async function fetchT(path, init = {}, ms = 5000) {
    const ac = new AbortController();
    const t = setTimeout(() => ac.abort(), ms);
    try { return await fetch(path, { cache: 'no-store', ...init, signal: ac.signal }); }
    finally { clearTimeout(t); }
  }

  /* JSON call. Throws Error(message); err.offline is true when the service did not answer at all. */
  async function api(path, opts = {}) {
    const init = { method: opts.method || 'GET', headers: {} };
    if (opts.body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(opts.body); }
    let r;
    try { r = await fetchT(path, init, opts.timeout || 6000); }
    catch (e) { const err = new Error('The GPT Arm service did not answer.'); err.offline = true; throw err; }
    let data = null;
    try { data = await r.json(); } catch (e) { /* not JSON */ }
    if (!r.ok) throw new Error((data && data.error) || `${init.method} ${path}: HTTP ${r.status}`);
    return data;
  }

  /* POST an action; errors go to the banner. Returns the reply or null. */
  async function act(path, body, what) {
    try {
      const r = await api(path, { method: 'POST', body: body || {} });
      pollNow();
      return r || {};
    } catch (e) {
      showError(e.offline ? `${what || path}: not sent, the service did not answer.` : `${what || path}: ${e.message}`);
      return null;
    }
  }

  const runFile = (id, f) => `api/runs/${encodeURIComponent(id)}/${encodeURIComponent(f)}`;

  // ------------------------------------------------------------------ app state
  const app = {
    s: null,                 // last /api/status
    online: true, fails: 0, lastOk: 0,
    tab: 'live',
    view: null,              // null: the live run; else the id of a past run shown read-only
    archived: null, archivedAt: 0,
    model: null, mode: null,
    annotated: store.get('annotated', false),
    camErr: { overhead: null, wrist: null },
    runs: [], runsAt: 0, runsKey: '',
    benchLast: null,         // GET /api/bench when status.bench is null
    ctxLoaded: false, ctxDirty: false,
    sigs: {},                // render signatures, to skip unchanged DOM work
  };
  const labelOf = (id) => ((app.s && app.s.models) || []).find((m) => m.id === id)?.label || id || DASH;
  const modeLabel = (id) => ((app.s && app.s.modes) || []).find((m) => m.id === id)?.label || id;
  const changed = (key, val) => { const j = JSON.stringify(val); if (app.sigs[key] === j) return false; app.sigs[key] = j; return true; };

  // ------------------------------------------------------------------ errors and offline state
  function showError(msg) {
    const box = $('errors');
    for (const e of box.children) {
      if (e.dataset.msg === msg) { const c = +e.dataset.n + 1; e.dataset.n = c; e.querySelector('.count').textContent = `x${c}`; return; }
    }
    while (box.children.length >= 4) box.firstElementChild.remove();
    const row = el('div', { class: 'err', role: 'alert', 'data-msg': msg, 'data-n': 1 },
      el('span', { text: msg }), el('span', { class: 'count' }),
      el('button', { class: 'btn small', text: 'Dismiss', onclick: () => row.remove() }));
    box.append(row);
  }

  function setOnline(on) {
    if (on) { app.fails = 0; app.lastOk = Date.now(); }
    if (on === app.online) return;
    app.online = on;
    document.body.classList.toggle('is-offline', !on);
    $('offline').hidden = on;
    if (!on) updateOffline();
    renderButtons();
    if (on) for (const c of ['overhead', 'wrist']) cams[c].kick();
  }
  function updateOffline() {
    $('offline-msg').textContent = app.lastOk
      ? `No answer from the GPT Arm service since ${fmt.clock(app.lastOk / 1000)} (${fmt.ago((Date.now() - app.lastOk) / 1000)}).`
      : 'The GPT Arm service is not answering. Is gptarm.service running?';
    const p = $('pill'); p.dataset.state = 'offline'; p.textContent = 'offline';
  }

  // ------------------------------------------------------------------ status polling
  let statusTimer = 0, statusBusy = false, statusAgain = false;
  async function pollStatus() {
    if (statusBusy) { statusAgain = true; return; }
    statusBusy = true; clearTimeout(statusTimer);
    try {
      const s = await api('api/status', { timeout: 4000 });
      setOnline(true);
      app.s = s;
      render();
    } catch (e) {
      // One missed poll can be a hiccup; two in a row means the service is gone.
      if (e.offline) { if (++app.fails >= 2) setOnline(false); if (!app.online) updateOffline(); }
      else showError('Status: ' + e.message);
    } finally {
      statusBusy = false;
      const wait = statusAgain ? 0 : document.hidden ? 5000 : app.online ? 1000 : 2000;
      statusAgain = false;
      statusTimer = setTimeout(pollStatus, wait);
    }
  }
  function pollNow() { clearTimeout(statusTimer); pollStatus(); }

  // ------------------------------------------------------------------ cameras
  /* Each camera is fetched as a blob so a 404 can show the service's {"error"} text, and the old frame stays
     on screen until the new one has arrived (no flicker). */
  function camera(name) {
    const box = $('cam-' + name), img = box.querySelector('[data-img]');
    let url = null, timer = 0, busy = false;
    async function tick() {
      clearTimeout(timer);
      if (!busy && !document.hidden && app.online) {
        busy = true;
        const q = name === 'overhead' && app.annotated ? 'annotated=1&' : '';
        try {
          const r = await fetchT(`api/camera/${name}.jpg?${q}t=${Date.now()}`, {}, 6000);
          if (r.ok) {
            const next = URL.createObjectURL(await r.blob());
            const prev = url; url = next; img.src = next;
            if (prev) setTimeout(() => URL.revokeObjectURL(prev), 3000);
            app.camErr[name] = null;
          } else {
            let msg = `No frame (HTTP ${r.status}).`;
            try { msg = (await r.json()).error || msg; } catch (e) { /* not JSON */ }
            app.camErr[name] = msg;
          }
        } catch (e) { /* the status poll reports an unreachable service */ }
        busy = false;
        renderCamera(name);
      }
      timer = setTimeout(tick, document.hidden ? 10000 : 1000);
    }
    return { start: tick, kick: tick };
  }
  const cams = { overhead: camera('overhead'), wrist: camera('wrist') };

  function renderCamera(name) {
    const box = $('cam-' + name), st = app.s && app.s.cameras && app.s.cameras[name];
    const err = (st && st.ok === false && (st.error || 'Camera not available.')) || app.camErr[name];
    const e = box.querySelector('[data-err]');
    e.hidden = !err;
    e.textContent = err || '';
    const age = box.querySelector('[data-age]');
    if (st) {
      const a = st.age_s;
      age.textContent = (a == null ? 'no frame' : `frame ${a < 10 ? a.toFixed(1) : Math.round(a)} s old`) +
        (Array.isArray(st.size) ? `  ·  ${st.size[0]}×${st.size[1]}` : '');
      age.classList.toggle('stale', a == null || a > 3);
    }
    if (st && Array.isArray(st.size) && st.size[0] && st.size[1]) {
      const r = `${st.size[0]} / ${st.size[1]}`;
      const f = box.querySelector('.frame');
      if (f.style.aspectRatio !== r) f.style.aspectRatio = r;
    }
  }

  // ------------------------------------------------------------------ top bar
  function machineState(s) {
    if (s.bench && s.bench.state === 'paused') return 'paused';
    if (s.busy === 'bench') return 'benchmark';
    if (s.busy === 'run') return 'running';
    if (s.busy === 'calibrate') return 'calibrating';
    if (s.busy === 'home') return 'homing';
    return s.busy ? String(s.busy) : 'idle';
  }

  function fillSelect(sel, items, current) {
    sel.textContent = '';
    for (const it of items) sel.append(el('option', { value: it.id, text: it.label || it.id }));
    if (current && items.some((m) => m.id === current)) sel.value = current;
  }

  function renderTop(s) {
    const p = $('pill'), st = machineState(s);
    if (p.dataset.state !== st || p.textContent !== st) { p.dataset.state = st; p.textContent = st; }
    if (changed('models', s.models)) {
      if (!app.model || !(s.models || []).some((m) => m.id === app.model)) app.model = s.model || (s.models[0] && s.models[0].id);
      fillSelect($('model'), s.models || [], app.model);
      buildBenchModels(s.models || []);
    }
    const modes = Array.isArray(s.modes) && s.modes.length ? s.modes : null;
    $('mode-wrap').hidden = !modes;
    $('bench-mode').hidden = $('bench-mode-label').hidden = !modes;
    if (modes && changed('modes', modes)) {
      if (!app.mode || !modes.some((m) => m.id === app.mode)) app.mode = s.mode || modes[0].id;
      fillSelect($('mode'), modes, app.mode);
      fillSelect($('bench-mode'), modes, app.mode);
    }
    renderButtons();
  }

  function renderButtons() {
    const s = app.s, busy = !s || !!s.busy || !app.online;
    for (const id of ['btn-look', 'btn-home', 'btn-cal', 'btn-run', 'btn-bench-start']) $(id).disabled = busy;
    const b = s && (s.bench || app.benchLast);
    const bs = b && b.state;
    $('btn-bench-stop').disabled = !(bs === 'running' || bs === 'paused');
    $('btn-bench-resume').disabled = bs !== 'paused';
    const runBtn = $('btn-run');
    runBtn.textContent = s && s.busy ? `Busy: ${machineState(s)}` : `Run with ${labelOf(app.model)}`;
  }

  // ------------------------------------------------------------------ scene, arm, specs
  const WHERE = { table: 'on the table', basket: 'in the basket', gripper: 'in the gripper', unknown: 'not seen' };
  const yesno = (v, y = 'yes', n = 'no') => (v == null ? el('span', { class: 'dim', text: DASH }) : el('span', { class: 'tag ' + (v ? 'yes' : 'no'), text: v ? y : n }));
  const dim = (t) => el('span', { class: 'dim', text: t });

  function kv(dl, rows) {
    dl.textContent = '';
    for (const r of rows) {
      if (!r) continue;
      dl.append(el('dt', { text: r[0] }), el('dd', { class: r[2] || null }, r[1]));
    }
  }

  function renderScene(s) {
    const sc = s.scene;
    if (!changed('scene', sc)) return;
    if (!sc) { kv($('scene'), [['Scene', dim('no perception yet')]]); return; }
    const b = sc.bottle, k = sc.basket;
    kv($('scene'), [
      ['Bottle', b ? [WHERE[b.where] || b.where, ' ', b.where === 'unknown' ? null : yesno(b.upright, 'upright', 'lying'),
        ' ', dim(`${fmt.xy(b.xy_mm)}  seen ${fmt.ago(b.seen_s_ago)}`)] : dim('not seen yet')],
      ['Holding', yesno(sc.holding)],
      ['Gripper', sc.gripper || DASH],
      ['Above', [fmt.words(sc.above || DASH), dim(`  at ${fmt.words(sc.height || DASH)} height`)]],
      ['Tool', el('span', { class: 'mono', text: fmt.xy(sc.tool_xyz_mm) + ' mm' })],
      ['Basket', k ? [fmt.xy(k.xy_mm), dim(`  seen ${fmt.ago(k.seen_s_ago)}`)] : dim('not seen yet')],
      ['Start spot', sc.start_spot_xy_mm ? fmt.xy(sc.start_spot_xy_mm) : dim('not seen yet')],
      ['Steps', sc.steps_used == null ? DASH : `${sc.steps_used} used, ${sc.steps_left} left`],
    ]);
  }

  function renderArm(s) {
    const a = s.arm;
    if (!changed('arm', a)) return;
    const flag = $('arm-flag');
    if (!a) { kv($('arm'), [['Arm', dim('no data')]]); flag.textContent = ''; return; }
    let f = ['', ''];
    if (!a.reachable) f = ['unreachable', 'bad'];
    else if (a.error) f = ['fault', 'bad'];
    else if (!a.enabled) f = ['disabled', 'warn'];
    else if (a.moving) f = ['moving', 'ok'];
    flag.textContent = f[0]; flag.className = 'flag ' + f[1];
    const t = a.tool_xyz_mm;
    kv($('arm'), [
      ['Status', [a.status || DASH, dim(a.reachable === false ? '  not reachable' : a.enabled ? '  motors on' : '  motors off')]],
      ['Tool x y z', el('span', { class: 'mono', text: Array.isArray(t) ? t.map((v) => fmt.num(v, 0).padStart(4)).join(' ') + '  mm' : DASH })],
      ['Lean', `${fmt.num(a.lean_deg, 1)}°`],
      ['Gripper', [`${fmt.num(a.gripper_mm, 1)} mm`, ' ', a.holding ? el('span', { class: 'tag yes', text: 'holding' }) : null]],
      ['Moving', yesno(a.moving)],
      a.guard ? ['Guard', a.guard, 'warn'] : null,
      a.error ? ['Error', a.error, 'bad'] : null,
    ]);
    const j = $('joints');
    j.textContent = '';
    (a.joints_deg || []).forEach((v, i) => j.append(el('div', {}, el('b', { text: `J${i + 1}` }), `${fmt.num(v, 1)}°`)));
  }

  function specValue(v) {
    if (v == null) return DASH;
    if (Array.isArray(v)) return v.join(' to ');
    if (typeof v === 'object') return Object.entries(v).map(([k, x]) => `${k} ${specValue(x)}`).join(',  ');
    return String(v);
  }
  function specLabel(k) {
    const m = k.match(/^(.*)_(mm|pct|deg|s|hz)$/);
    const unit = m && { mm: 'mm', pct: '%', deg: 'deg', s: 's', hz: 'Hz' }[m[2]];
    const base = fmt.words(m ? m[1] : k);
    return base.charAt(0).toUpperCase() + base.slice(1) + (unit ? ` (${unit})` : '');
  }
  function renderSpecs(s) {
    if (!changed('specs', [s.specs, s.calibration, s.version])) return;
    const c = s.calibration, rows = [];
    for (const [k, v] of Object.entries(s.specs || {})) rows.push([specLabel(k), specValue(v)]);
    rows.push(['Calibration', c ? (c.ok
      ? `${c.points ?? DASH} points, residual ${fmt.num(c.residual_px, 1)} px, ${c.when ? fmt.date(c.when) : 'time unknown'}`
      : 'not calibrated') : DASH, c && !c.ok ? 'warn' : null]);
    rows.push(['Service', s.version || DASH]);
    kv($('specs'), rows);
  }

  // ------------------------------------------------------------------ the run log
  const KIND = { user: 'Instruction', perception: 'Perception', decision: 'Decision', info: 'Info', error: 'Error', verdict: 'Verdict' };
  let logKey = null, logCount = 0, logLastSig = '';

  function nearBottom() { const sc = $('log-scroll'); return sc.scrollTop + sc.clientHeight >= sc.scrollHeight - 48; }
  function toBottom() { const sc = $('log-scroll'); sc.scrollTop = sc.scrollHeight; $('btn-jump').hidden = true; }

  function stepNode(run, st, i) {
    const kind = st.kind || 'info';
    if (kind === 'verdict') return verdictNode(run, st);
    const refused = kind === 'decision' && st.ok === false;
    const retry = kind === 'info' && /^retry\b/i.test(st.note || '');
    const li = el('li', { class: `step k-${kind}${refused ? ' refused' : ''}${retry ? ' k-retry' : ''}` });
    let kindText = KIND[kind] || kind;
    if (refused) kindText = /refus/i.test(st.detail || '') ? 'Refused' : 'Failed';
    if (retry) kindText = 'Retry';
    const head = el('div', { class: 'head' });
    if (st.skill) {
      head.append(el('span', { class: 'skill', text: st.skill }));
      if (st.target != null && st.target !== '') head.append(el('span', { class: 'arrow', text: '→' }), el('span', { class: 'target', text: String(st.target) }));
    }
    if (st.attempt > 1) head.append(el('span', { class: 'attempt', title: 'Repeated attempt within this cycle', text: `attempt ${st.attempt}` }));
    const body = el('div', { class: 'body' }, head.childElementCount ? head : null,
      st.note ? el('p', { class: 'note', text: st.note }) : null,
      st.detail && kind !== 'user' ? el('p', { class: 'detail', text: st.detail }) : null);
    const imgs = Object.entries(st.images || {}).filter(([, f]) => f);
    if (imgs.length) {
      body.append(el('div', { class: 'thumbs' }, imgs.map(([cam, f]) => el('button', {
        class: 'thumb', 'data-src': runFile(run.id, f), 'data-cap': `Step ${st.n ?? i}, ${cam} camera, run ${run.id}`,
        'aria-label': `Open the ${cam} image of step ${st.n ?? i}`,
      }, el('img', { src: runFile(run.id, f), alt: `${cam} camera at step ${st.n ?? i}`, loading: 'lazy', decoding: 'async' })))));
    }
    const m = el('div', { class: 'metrics' },
      st.llm_s != null ? el('span', { text: `LLM ${fmt.num(st.llm_s, 1)} s` }) : null,
      st.tokens != null ? el('span', { text: `${fmt.tok(st.tokens)} tok` }) : null,
      st.motion_s != null ? el('span', { text: `motion ${fmt.num(st.motion_s, 1)} s` }) : null,
      st.t ? el('time', { text: fmt.clock(st.t) }) : null);
    li.append(el('span', { class: 'n', text: st.n ?? i }), el('span', { class: 'kind', text: kindText }), body, m);
    return li;
  }

  function verdictNode(run, st) {
    const ok = st.ok === true, fail = st.ok === false;
    const checks = (run.outcome && run.outcome.checks) || {};
    const chips = [];
    for (const [k, v] of Object.entries(checks)) {
      if (v == null) continue;
      if (k === 'entered_basket') chips.push(el('span', { class: v ? 'pass' : 'miss', text: v ? 'entered the basket' : 'never entered the basket' }));
      else if (k === 'returned_mm') chips.push(el('span', { text: `returned ${fmt.num(v, 0)} mm from start` }));
      else if (k === 'upright') chips.push(el('span', { class: v ? 'pass' : 'miss', text: v ? 'upright' : 'not upright' }));
      else chips.push(el('span', { text: `${fmt.words(k)}: ${typeof v === 'boolean' ? (v ? 'yes' : 'no') : v}` }));
    }
    return el('li', { class: 'verdict-li' }, el('div', { class: `verdict ${ok ? 'ok' : fail ? 'fail' : ''}` },
      el('span', { class: 'big', text: ok ? '✓ Success' : fail ? '✕ Failure' : 'No verdict' }),
      el('span', { class: 'why', text: st.note || run.outcome?.reason || '' }),
      el('time', { text: fmt.clock(st.t) }),
      el('div', { class: 'checks' }, chips)));
  }

  function runStateBadge(run) {
    if (!run) return null;
    if (run.state === 'running') return el('span', { class: 'state running', text: 'running' });
    const succ = run.outcome && run.outcome.success;
    if (run.state === 'stopped') return el('span', { class: 'state stopped', text: 'stopped' });
    if (run.state === 'failed') return el('span', { class: 'state failed', text: 'failed' });
    if (succ === true) return el('span', { class: 'state success', text: 'success' });
    if (succ === false) return el('span', { class: 'state failure', text: 'failure' });
    return el('span', { class: 'state', text: run.state || DASH });
  }

  function renderRunHead(run, now) {
    const steps = run.steps || [];
    const decisions = steps.filter((x) => x.kind === 'decision');
    const refused = decisions.filter((x) => x.ok === false).length;
    const u = run.usage || {};
    const stat = (k, v, title) => el('div', { class: 'stat', title }, el('b', { text: k }), el('span', { text: v }));
    const head = $('run-head');
    head.textContent = '';
    put(head,
      el('div', {}, el('div', { class: 'model', text: labelOf(run.model) + (run.mode ? `  ·  ${modeLabel(run.mode)}` : '') }),
        el('div', { class: 'rid', text: run.id })),
      runStateBadge(run),
      stat('Elapsed', fmt.dur((run.ended || now) - run.started)),
      stat('Steps', String(decisions.length), 'Model decisions so far'),
      refused ? stat('Refused', String(refused), 'Decisions the governor refused or that failed') : null,
      stat('Tokens', fmt.tok(u.tokens)),
      stat('LLM', `${fmt.num(u.llm_s, 0)} s`, 'Time spent waiting for the model'),
      stat('Motion', `${fmt.num(u.motion_s, 0)} s`, 'Time the arm spent moving'),
      stat('Started', fmt.clock(run.started)),
      run.instruction ? el('div', { class: 'instr', text: '“' + run.instruction + '”' }) : null);
  }

  function renderPending(run, now) {
    const p = $('pending'), pd = run && run.state === 'running' ? run.pending : null;
    p.hidden = !pd;
    if (!pd) return;
    p.textContent = '';
    if (typeof pd !== 'object') { p.append(String(pd)); return; }
    const stage = pd.stage || pd.what;
    if (stage === 'llm') p.append(el('b', { text: `Waiting for ${labelOf(run.model)}` }), 'to choose the next step');
    else if (stage === 'motion') {
      p.append(el('b', { text: 'Moving' }));
      if (pd.skill) p.append(el('span', { class: 'skill', text: pd.skill }));
      if (pd.target) p.append(el('span', { class: 'arrow', text: '→' }), el('span', { class: 'target', text: String(pd.target) }));
      if (pd.note) p.append(el('span', { text: pd.note }));
    } else if (stage === 'perception') p.append(el('b', { text: 'Looking' }), 'photographing the table and detecting');
    else p.append(el('b', { text: String(stage || 'Working') }));
    if (pd.since) p.append(el('span', { class: 'since', text: `${fmt.num(Math.max(0, now - pd.since), 0)} s` }));
  }

  /* Steps are append-only, so only new rows are built; the last row is rebuilt if it changed. */
  function renderLog(run, now, archived) {
    const log = $('log');
    $('archive-bar').hidden = !archived;
    $('log-empty').hidden = !!run;
    if (!run) { log.textContent = ''; $('run-head').textContent = ''; $('pending').hidden = true; logKey = null; return; }
    const key = run.id + (archived ? '#a' : '');
    const stick = logKey !== key || nearBottom();
    if (logKey !== key) { log.textContent = ''; logKey = key; logCount = 0; logLastSig = ''; }
    const steps = run.steps || [];
    if (steps.length < logCount) { log.textContent = ''; logCount = 0; logLastSig = ''; }
    if (logCount > 0) {
      const sig = JSON.stringify(steps[logCount - 1]);
      if (sig !== logLastSig) log.children[logCount - 1].replaceWith(stepNode(run, steps[logCount - 1], logCount - 1));
    }
    const added = steps.length - logCount;
    for (let i = logCount; i < steps.length; i++) log.append(stepNode(run, steps[i], i));
    logCount = steps.length;
    logLastSig = steps.length ? JSON.stringify(steps[steps.length - 1]) : '';
    renderRunHead(run, now);
    renderPending(run, now);
    if (stick) toBottom();
    else if (added > 0) $('btn-jump').hidden = false;
  }

  function renderLive(s) {
    if (app.view) {
      // A past run that is still running (opened from the benchmark list) is refreshed every 2 s.
      const a = app.archived;
      if (a && a.state === 'running' && Date.now() - app.archivedAt > 2000) loadArchived(a.id, true);
      if (a) renderLog(a, s ? s.time : Date.now() / 1000, true);
      return;
    }
    renderLog(s && s.run, s ? s.time : Date.now() / 1000, false);
  }

  async function loadArchived(id, quiet) {
    app.archivedAt = Date.now();
    try {
      const run = await api('api/runs/' + encodeURIComponent(id));
      if (app.view !== id) return;
      app.archived = run;
      renderLive(app.s);
    } catch (e) {
      if (!quiet) { showError(`Run ${id}: ${e.message}`); backToLive(); }
    }
  }

  function openRun(id) {
    if (app.s && app.s.run && app.s.run.id === id && app.s.run.state === 'running') { backToLive(); setTab('live'); return; }
    app.view = id; app.archived = null;
    setTab('live', '#run/' + encodeURIComponent(id));
    loadArchived(id);
  }
  function backToLive() {
    app.view = null; app.archived = null;
    if (app.tab === 'live') history.replaceState(null, '', '#live');
    renderLive(app.s);
  }

  // ------------------------------------------------------------------ lightbox
  let lbItems = [], lbIndex = 0;
  function openLightbox(items, index) {
    lbItems = items; lbIndex = index;
    showLb();
    const d = $('lightbox');
    if (!d.open) d.showModal();
  }
  function showLb() {
    const it = lbItems[lbIndex];
    if (!it) return;
    $('lb-img').src = it.src;
    $('lb-img').alt = it.cap;
    $('lb-cap').textContent = it.cap + (lbItems.length > 1 ? `   (${lbIndex + 1} of ${lbItems.length})` : '');
    $('lb-prev').hidden = $('lb-next').hidden = lbItems.length < 2;
  }
  function lbStep(d) { if (lbItems.length > 1) { lbIndex = (lbIndex + d + lbItems.length) % lbItems.length; showLb(); } }

  // ------------------------------------------------------------------ confirm dialog
  function confirmBox(title, body, okText) {
    const d = $('confirm');
    $('cf-title').textContent = title; $('cf-body').textContent = body; $('cf-ok').textContent = okText;
    if (typeof d.showModal !== 'function') return Promise.resolve(window.confirm(`${title}\n\n${body}`));
    return new Promise((resolve) => {
      d.returnValue = 'cancel';
      d.addEventListener('close', () => resolve(d.returnValue === 'ok'), { once: true });
      d.showModal();
      $('cf-cancel').focus();
    });
  }

  // ------------------------------------------------------------------ runs tables
  function outcomeCell(r) {
    const badge = runStateBadge(r);
    const why = r.outcome && r.outcome.reason;
    return el('td', {}, badge, why && r.state !== 'running' ? el('span', { class: 'reason', title: why, text: why }) : null);
  }
  function runRow(r, extra) {
    const steps = r.n_steps ?? (r.usage && r.usage.llm_calls);
    const dur = r.ended ? r.ended - r.started : (app.s ? app.s.time - r.started : null);
    const tr = el('tr', { class: 'click', onclick: (e) => { if (!e.target.closest('button')) openRun(r.id); } },
      extra != null ? el('td', { class: 'num', text: extra }) : null,
      el('td', {}, el('button', { class: 'open', text: r.id, title: 'Open this run', onclick: () => openRun(r.id) })),
      el('td', { class: 'nowrap', text: fmt.date(r.started) }),
      el('td', { class: 'nowrap', text: labelOf(r.model) + (r.mode ? ` · ${modeLabel(r.mode)}` : '') }),
      // A benchmark's runs all share one instruction, so its table (the one with an index) leaves it out.
      extra != null ? null : el('td', { class: 'instr-cell', title: r.instruction || '', text: r.instruction || DASH }),
      outcomeCell(r),
      el('td', { class: 'num', text: steps ?? DASH }),
      el('td', { class: 'num', text: fmt.dur(dur) }),
      el('td', { class: 'num', text: fmt.tok(r.usage && r.usage.tokens) }));
    return tr;
  }
  function runsHead(table, withIndex) {
    const th = (t, num) => el('th', { scope: 'col', class: num ? 'num' : null, text: t });
    table.tHead.textContent = '';
    table.tHead.append(el('tr', {}, withIndex ? th('#', true) : null, th('Run'), th('Started'), th('Model'), withIndex ? null : th('Instruction'),
      th('Outcome'), th('Steps', true), th('Time', true), th('Tokens', true)));
  }

  async function loadRuns() {
    app.runsAt = Date.now();
    try {
      const d = await api('api/runs?limit=200');
      app.runs = (d && d.runs) || [];
      renderRunsTab();
      if (app.tab === 'bench') renderBench(app.s);
    } catch (e) { if (!e.offline) showError('Runs: ' + e.message); }
  }

  function renderRunsTab() {
    const t = $('runs-table');
    if (!changed('runsTab', app.runs)) return;
    if (!t.tHead.childElementCount) runsHead(t, false);
    const tb = t.tBodies[0];
    tb.textContent = '';
    if (!app.runs.length) tb.append(el('tr', {}, el('td', { colspan: 8, class: 'empty', text: 'No runs yet.' })));
    for (const r of app.runs) tb.append(runRow(r));
  }

  // ------------------------------------------------------------------ benchmark
  function buildBenchModels(models) {
    const box = $('bench-models'), was = new Set([...box.querySelectorAll('input:checked')].map((i) => i.value));
    const first = !box.childElementCount;
    box.textContent = '';
    for (const m of models) {
      box.append(el('label', {}, el('input', { type: 'checkbox', value: m.id, checked: first || was.has(m.id) }), m.label || m.id));
    }
  }

  function renderBench(s) {
    if (!s) return;
    const b = s.bench || app.benchLast;
    const st = $('bench-status');
    const runIds = (b && b.run_ids) || [];
    if (changed('benchStatus', b && { ...b, run_ids: runIds.length })) {
      st.textContent = '';
      if (!b || b.state === 'idle') {
        st.append(el('h2', { text: 'Benchmark' }), el('p', { class: 'hint', text: 'No benchmark has run yet. Choose models and press Start.' }));
      } else {
        const results = b.results || [];
        const nModels = (b.models && b.models.length) || results.length || 1;
        const total = (b.runs_per_model || 0) * nModels;
        const done = results.reduce((a, r) => a + (r.runs || 0), 0);
        const stat = (k, v) => el('div', { class: 'stat' }, el('b', { text: k }), el('span', {}, v));
        const cur = b.current;
        put(st, el('div', { class: 'card-head' }, el('h2', { text: 'Benchmark' }), el('span', { class: 'state ' + b.state, text: b.state })),
          el('div', { class: 'bstat' },
            stat('Progress', `${done} of ${total || DASH} runs`),
            cur ? stat('Now', `${labelOf(cur.model)}, run ${cur.index} of ${b.runs_per_model}`) : null,
            b.mode ? stat('Control', modeLabel(b.mode)) : null,
            stat('Started', fmt.date(b.started)),
            stat('Elapsed', el('span', { id: 'bench-elapsed' }))),
          b.instruction ? el('p', { class: 'hint', text: '“' + b.instruction + '”' }) : null,
          el('div', { class: 'progress' }, el('i', { style: `width:${total ? Math.min(100, (100 * done) / total) : 0}%` })));
        if (b.state === 'paused') {
          st.append(el('div', { class: 'bench-paused', role: 'alert' },
            el('div', {}, el('strong', { text: 'Paused. ' }), b.message || 'The scene needs resetting by hand.'),
            el('button', { class: 'btn', text: 'Resume', onclick: resumeBench })));
        } else if (b.message) st.append(el('p', { class: 'hint', text: b.message }));
      }
    }
    const be = $('bench-elapsed');
    if (be) be.textContent = fmt.dur(b && b.started ? (b.state === 'running' || b.state === 'paused' ? s.time : b.ended || s.time) - b.started : null);
    if (changed('benchResults', b && b.results)) {
      const tb = $('bench-results').tBodies[0];
      tb.textContent = '';
      const results = (b && b.results) || [];
      if (!results.length) tb.append(el('tr', {}, el('td', { colspan: 8, class: 'hint', text: 'Results appear after the first run finishes.' })));
      for (const r of results) {
        const rate = r.rate ?? (r.runs ? r.successes / r.runs : null);
        tb.append(el('tr', {},
          el('td', { text: labelOf(r.model) }),
          el('td', { class: 'num', text: r.runs ?? DASH }),
          el('td', { class: 'num', text: r.successes ?? DASH }),
          el('td', {}, el('div', { class: 'rate' }, el('div', { class: 'bar', role: 'img', 'aria-label': `${rate == null ? 'no' : Math.round(rate * 100) + '%'} success` },
            el('i', { style: `width:${rate == null ? 0 : Math.round(rate * 100)}%` })), el('span', { text: rate == null ? DASH : Math.round(rate * 100) + '%' }))),
          el('td', { class: 'num', text: fmt.num(r.mean_steps, 1) }),
          el('td', { class: 'num', text: fmt.dur(r.mean_s) }),
          el('td', { class: 'num', text: fmt.tok(r.tokens) }),
          el('td', {}, el('div', { class: 'fails' }, Object.entries(r.failures || {}).map(([k, n]) => el('span', { text: `${fmt.words(k)} ×${n}` }))))));
      }
    }
    // The benchmark's runs, newest first, from the runs list (refetched when new ids appear).
    const known = new Map(app.runs.map((r) => [r.id, r]));
    if (runIds.some((id) => !known.has(id)) && Date.now() - app.runsAt > 3000) loadRuns();
    const rows = runIds.map((id, i) => [i + 1, known.get(id) || { id, model: null, state: 'running', usage: {} }]).reverse();
    if (changed('benchRuns', rows)) {
      const t = $('bench-runs');
      if (!t.tHead.childElementCount) runsHead(t, true);
      const tb = t.tBodies[0];
      tb.textContent = '';
      if (!rows.length) tb.append(el('tr', {}, el('td', { colspan: 9, class: 'hint', text: 'No runs in this benchmark yet.' })));
      for (const [i, r] of rows) tb.append(runRow(r, i));
    }
  }

  function resumeBench() { act('api/bench/resume', {}, 'Resume'); }

  // ------------------------------------------------------------------ paused banner
  function renderPaused(s) {
    const b = s.bench, paused = !!(b && b.state === 'paused');
    $('paused').hidden = !paused;
    if (paused) $('paused-msg').textContent = b.message || 'Reset the scene, then resume.';
  }

  // ------------------------------------------------------------------ context
  async function loadContext() {
    try {
      const d = await api('api/context');
      $('ctx').value = (d && d.text) || '';
      app.ctxLoaded = true; app.ctxDirty = false;
      $('ctx-state').textContent = d && d.default ? 'Default context' : 'Edited context (saved)';
    } catch (e) { showError('Context: ' + e.message); }
  }
  async function saveContext() {
    const r = await act('api/context', { text: $('ctx').value }, 'Save context');
    if (r) { app.ctxDirty = false; $('ctx-state').textContent = `Saved at ${fmt.clock(Date.now() / 1000)}`; }
  }

  // ------------------------------------------------------------------ tabs
  function setTab(tab, hash) {
    app.tab = tab;
    for (const b of document.querySelectorAll('.tabs [role=tab]')) {
      const on = b.dataset.tab === tab;
      b.setAttribute('aria-selected', on); b.tabIndex = on ? 0 : -1;
      $('tab-' + b.dataset.tab).hidden = !on;
    }
    history.replaceState(null, '', hash || (tab === 'live' && app.view ? '#run/' + encodeURIComponent(app.view) : '#' + tab));
    if (tab === 'runs') loadRuns();
    if (tab === 'bench') {
      if (Date.now() - app.runsAt > 3000) loadRuns();
      if (app.s && !app.s.bench) api('api/bench').then((b) => { app.benchLast = b; renderBench(app.s); renderButtons(); }).catch(() => {});
      renderBench(app.s);
    }
    if (tab === 'context' && !app.ctxLoaded) loadContext();
    if (tab === 'live') { logKey = null; renderLive(app.s); }
  }

  // ------------------------------------------------------------------ render everything from status
  let lastRunSig = '';
  function render() {
    const s = app.s;
    renderTop(s);
    renderCamera('overhead'); renderCamera('wrist');
    renderScene(s); renderArm(s); renderSpecs(s);
    renderPaused(s);
    if (app.tab === 'live') renderLive(s);
    if (app.tab === 'bench') renderBench(s);
    // Refresh the runs list when a run starts or finishes.
    const sig = s.run ? s.run.id + s.run.state : '';
    if (sig !== lastRunSig) { lastRunSig = sig; if (app.tab === 'runs' || app.tab === 'bench') loadRuns(); }
    if (app.tab === 'runs' && Date.now() - app.runsAt > 15000) loadRuns();
    // First status: prefill the instruction boxes.
    if (!app.prefilled && s.default_instruction) {
      app.prefilled = true;
      if (!$('instr').value) $('instr').value = s.default_instruction;
      if (!$('bench-instr').value) $('bench-instr').value = s.default_instruction;
      buildChips(s.default_instruction);
    }
  }

  function buildChips(def) {
    const chips = [
      ['Full cycle', def],
      ['Into the basket', 'Put the bottle in the basket'],
      ['Back to the start', 'Take the bottle out of the basket and stand it where it started'],
      ['Look and report', 'Look at the table and tell me where the bottle is. Do not move it.'],
    ];
    const box = $('chips');
    box.textContent = '';
    for (const [label, text] of chips) {
      box.append(el('button', { type: 'button', class: 'chip', title: text, text: label,
        onclick: () => { const t = $('instr'); t.value = text; t.focus(); } }));
    }
  }

  // ------------------------------------------------------------------ wiring
  function runInstruction() {
    const text = $('instr').value.trim();
    if (!text) { showError('Type an instruction first.'); return; }
    if (app.s && app.s.busy) { showError(`Can't start: ${machineState(app.s)} in progress.`); return; }
    // The selects are the source of truth: they are what the operator sees.
    const body = { instruction: text, model: $('model').value || app.model };
    if (!$('mode-wrap').hidden && $('mode').value) body.mode = $('mode').value;
    app.view = null; app.archived = null;
    act('api/run', body, 'Run').then((r) => { if (r) { setTab('live'); toBottom(); } });
  }

  function wire() {
    for (const b of document.querySelectorAll('.tabs [role=tab]')) b.addEventListener('click', () => setTab(b.dataset.tab));
    document.querySelector('.tabs').addEventListener('keydown', (e) => {
      if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
      const tabs = [...document.querySelectorAll('.tabs [role=tab]')];
      const i = tabs.findIndex((t) => t.dataset.tab === app.tab);
      const n = tabs[(i + (e.key === 'ArrowRight' ? 1 : tabs.length - 1)) % tabs.length];
      setTab(n.dataset.tab); n.focus();
    });

    $('model').addEventListener('change', (e) => { app.model = e.target.value; renderButtons(); });
    $('mode').addEventListener('change', (e) => { app.mode = e.target.value; $('bench-mode').value = app.mode; });

    $('btn-stop').addEventListener('click', () => act('api/stop', {}, 'Stop'));
    $('btn-estop').addEventListener('click', () => act('api/estop', {}, 'E-stop'));
    $('btn-home').addEventListener('click', () => act('api/home', {}, 'Home'));
    $('btn-look').addEventListener('click', () => act('api/look', {}, 'Look'));
    $('btn-cal').addEventListener('click', async () => {
      if (await confirmBox('Calibrate the overhead camera?', 'The arm will move to a series of points over the table. Clear the table of anything it could hit, and keep hands away.', 'Calibrate, the arm will move')) act('api/calibrate', {}, 'Calibrate');
    });

    $('instruct').addEventListener('submit', (e) => { e.preventDefault(); runInstruction(); });
    $('instr').addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey && !e.isComposing) { e.preventDefault(); runInstruction(); }
    });

    $('annotated').checked = app.annotated;
    $('annotated').addEventListener('change', (e) => { app.annotated = e.target.checked; store.set('annotated', app.annotated); cams.overhead.kick(); });

    // Camera frames and log thumbnails open in the lightbox.
    for (const f of document.querySelectorAll('.frame')) {
      f.addEventListener('click', () => {
        const img = f.querySelector('img');
        if (img.getAttribute('src')) openLightbox([{ src: img.src, cap: `${f.dataset.cam === 'overhead' ? 'Overhead' : 'Wrist'} camera, live` }], 0);
      });
    }
    $('log').addEventListener('click', (e) => {
      const b = e.target.closest('.thumb');
      if (!b) return;
      const all = [...$('log').querySelectorAll('.thumb')];
      openLightbox(all.map((x) => ({ src: x.dataset.src, cap: x.dataset.cap })), all.indexOf(b));
    });
    $('lb-prev').addEventListener('click', () => lbStep(-1));
    $('lb-next').addEventListener('click', () => lbStep(1));
    $('lb-close').addEventListener('click', () => $('lightbox').close());
    $('lightbox').addEventListener('keydown', (e) => { if (e.key === 'ArrowLeft') lbStep(-1); if (e.key === 'ArrowRight') lbStep(1); });
    $('lightbox').addEventListener('click', (e) => { if (e.target === $('lightbox')) $('lightbox').close(); });

    $('log-scroll').addEventListener('scroll', () => { if (nearBottom()) $('btn-jump').hidden = true; }, { passive: true });
    $('btn-jump').addEventListener('click', toBottom);
    $('btn-back-live').addEventListener('click', backToLive);

    $('bench-form').addEventListener('submit', (e) => {
      e.preventDefault();
      const models = [...$('bench-models').querySelectorAll('input:checked')].map((i) => i.value);
      const n = parseInt($('bench-n').value, 10);
      const instruction = $('bench-instr').value.trim();
      if (!models.length) return showError('Benchmark: choose at least one model.');
      if (!(n >= 1 && n <= 100)) return showError('Benchmark: runs per model must be 1 to 100.');
      if (!instruction) return showError('Benchmark: type an instruction.');
      const body = { models, runs_per_model: n, instruction };
      if (!$('bench-mode').hidden && $('bench-mode').value) body.mode = $('bench-mode').value;
      app.benchLast = null;
      act('api/bench/start', body, 'Start benchmark');
    });
    $('btn-bench-stop').addEventListener('click', () => act('api/stop', {}, 'Stop benchmark'));
    $('btn-bench-resume').addEventListener('click', resumeBench);
    $('btn-resume-top').addEventListener('click', resumeBench);
    $('btn-runs-refresh').addEventListener('click', loadRuns);

    $('ctx').addEventListener('input', () => { app.ctxDirty = true; $('ctx-state').textContent = 'Unsaved changes'; });
    $('ctx').addEventListener('keydown', (e) => { if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); saveContext(); } });
    $('btn-ctx-save').addEventListener('click', saveContext);
    $('btn-ctx-reload').addEventListener('click', loadContext);
    $('btn-ctx-reset').addEventListener('click', async () => {
      if (!(await confirmBox('Reset the arm context?', 'This replaces the context with the default text. Your edits will be lost.', 'Reset'))) return;
      const r = await act('api/context', { reset: true }, 'Reset context');
      if (r) loadContext();
    });

    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) { pollNow(); cams.overhead.kick(); cams.wrist.kick(); }
    });
  }

  /* #live, #bench, #runs, #context or #run/<id> (a past run, read-only). setTab() rewrites the hash with
     replaceState, which does not fire hashchange, so this only reacts to links and typed URLs. */
  function route() {
    const h = decodeURIComponent(location.hash.slice(1));
    if (h.startsWith('run/')) openRun(h.slice(4));
    else setTab(['live', 'bench', 'runs', 'context'].includes(h) ? h : 'live');
  }

  function start() {
    wire();
    window.addEventListener('hashchange', route);
    route();
    pollStatus();
    cams.overhead.start(); cams.wrist.start();
  }
  start();
})();
