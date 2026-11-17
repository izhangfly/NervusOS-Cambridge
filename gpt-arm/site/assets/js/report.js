/* GPT Arm report: page behaviour (theme, sound, nav, videos, reveal, figure toggles, sortable tables).
   No dependencies. Everything degrades: without JS the article, tables and posters are all readable. */
(function () {
  "use strict";
  const root = document.documentElement;
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch (e) { /* private mode: fine */ } },
  };
  const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  /* ---- theme: light by default, follows the OS, the toggle wins both ways ---- */
  const themeBtn = document.getElementById("theme-btn");
  function isDark() {
    const t = root.getAttribute("data-theme");
    if (t) return t === "dark";
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  }
  function paintThemeBtn() {
    if (themeBtn) themeBtn.setAttribute("aria-pressed", String(isDark()));
  }
  const saved = store.get("gar-theme");
  if (saved === "dark" || saved === "light") root.setAttribute("data-theme", saved);
  paintThemeBtn();
  if (themeBtn) themeBtn.addEventListener("click", () => {
    const next = isDark() ? "light" : "dark";
    root.setAttribute("data-theme", next);
    store.set("gar-theme", next);
    paintThemeBtn();
    document.dispatchEvent(new CustomEvent("gar:theme"));
  });
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
    if (!root.getAttribute("data-theme")) { paintThemeBtn(); document.dispatchEvent(new CustomEvent("gar:theme")); }
  });

  /* ---- sound: a soft synthesised click on the main links (two oscillators, ~50 ms), like jrhu.me ---- */
  const soundBtn = document.getElementById("sound-btn");
  let soundOn = store.get("gar-sound") !== "off" && !reduced;
  let ctx = null;
  function paintSound() { if (soundBtn) soundBtn.setAttribute("aria-pressed", String(soundOn)); }
  paintSound();
  function blip(pitch) {
    if (!soundOn) return;
    try {
      ctx = ctx || new (window.AudioContext || window.webkitAudioContext)();
      if (ctx.state === "suspended") ctx.resume();
      const t = ctx.currentTime, f = pitch || 1040;
      const out = ctx.createGain();
      out.gain.setValueAtTime(0.0001, t);
      out.gain.exponentialRampToValueAtTime(0.045, t + 0.004);
      out.gain.exponentialRampToValueAtTime(0.0001, t + 0.07);
      out.connect(ctx.destination);
      const a = ctx.createOscillator(), b = ctx.createOscillator(), bg = ctx.createGain();
      a.type = "sine"; a.frequency.setValueAtTime(f, t); a.frequency.exponentialRampToValueAtTime(f * 0.82, t + 0.06);
      b.type = "triangle"; b.frequency.setValueAtTime(f / 2, t); bg.gain.value = 0.5;
      a.connect(out); b.connect(bg); bg.connect(out);
      a.start(t); b.start(t); a.stop(t + 0.08); b.stop(t + 0.08);
    } catch (e) { /* no audio: fine */ }
  }
  if (soundBtn) soundBtn.addEventListener("click", () => {
    soundOn = !soundOn; store.set("gar-sound", soundOn ? "on" : "off"); paintSound(); if (soundOn) blip(1180);
  });
  document.addEventListener("click", (e) => {
    const el = e.target.closest("[data-sound], .nav ol a, .btn, .chip");
    if (!el) return;
    const p = el.matches(".chip") ? 1320 : el.matches(".btn") ? 880 : 1040;
    blip(p);
  });

  /* ---- nav: highlight the section on screen ---- */
  const links = [...document.querySelectorAll(".nav ol a[href^='#']")];
  const targets = links.map((a) => document.querySelector(a.getAttribute("href"))).filter(Boolean);
  let spyQueued = false;
  function spy() {
    spyQueued = false;
    const line = window.innerHeight * 0.35;
    let cur = null;
    targets.forEach((t) => { if (t.getBoundingClientRect().top <= line) cur = t; });   // the last section that started above the line
    links.forEach((a) => a.setAttribute("aria-current", String(!!cur && a.getAttribute("href") === "#" + cur.id)));
  }
  if (targets.length) {
    window.addEventListener("scroll", () => { if (!spyQueued) { spyQueued = true; requestAnimationFrame(spy); } }, { passive: true });
    spy();
  }

  /* ---- videos: load when near the screen, play only while visible, pause control ---- */
  const icons = {
    pause: '<svg viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><rect x="3" y="2" width="3.5" height="12" rx="1"/><rect x="9.5" y="2" width="3.5" height="12" rx="1"/></svg>',
    play: '<svg viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M4 2.5v11a.5.5 0 0 0 .77.42l8.5-5.5a.5.5 0 0 0 0-.84l-8.5-5.5A.5.5 0 0 0 4 2.5z"/></svg>',
  };
  const vids = [...document.querySelectorAll("video[data-src]")];
  vids.forEach((v) => {
    v.muted = true; v.playsInline = true; v.loop = v.hasAttribute("loop");
    const wrap = v.closest(".vid, .phone, .screen");
    if (wrap && !wrap.querySelector(".ctl") && !v.hasAttribute("data-noctl")) {
      const ctl = document.createElement("div"); ctl.className = "ctl";
      const b = document.createElement("button"); b.type = "button"; b.setAttribute("aria-label", "Pause video"); b.innerHTML = icons.pause;
      b.addEventListener("click", () => {
        v.dataset.user = v.paused ? "play" : "pause";
        if (v.paused) { v.play().catch(() => {}); } else { v.pause(); }
      });
      v.addEventListener("play", () => { b.innerHTML = icons.pause; b.setAttribute("aria-label", "Pause video"); });
      v.addEventListener("pause", () => { b.innerHTML = icons.play; b.setAttribute("aria-label", "Play video"); });
      ctl.appendChild(b); wrap.appendChild(ctl);
    }
  });
  function load(v) { if (!v.src && v.dataset.src) { v.src = v.dataset.src; v.load(); } }
  if ("IntersectionObserver" in window) {
    const near = new IntersectionObserver((es) => es.forEach((e) => { if (e.isIntersecting) { load(e.target); near.unobserve(e.target); } }), { rootMargin: "600px 0px" });
    const vis = new IntersectionObserver((es) => es.forEach((e) => {
      const v = e.target;
      // load first: this observer can fire before the "near" one has set the source
      if (e.isIntersecting && e.intersectionRatio > 0.35) { load(v); if (!reduced && v.dataset.user !== "pause") v.play().catch(() => {}); }
      else if (!v.paused) v.pause();
    }), { threshold: [0, 0.35, 0.7] });
    vids.forEach((v) => { near.observe(v); vis.observe(v); });
  } else vids.forEach(load);

  /* hero: unmute button for the owner's edit (it has sound) */
  const unmute = document.getElementById("unmute");
  if (unmute) {
    const hv = document.getElementById("hero-video");
    unmute.addEventListener("click", () => {
      load(hv); hv.muted = !hv.muted; if (!hv.muted) hv.play().catch(() => {});
      unmute.setAttribute("aria-pressed", String(!hv.muted));
      unmute.setAttribute("aria-label", hv.muted ? "Turn sound on" : "Turn sound off");
    });
  }

  /* ---- reveal on scroll ---- */
  if (!reduced && "IntersectionObserver" in window) {
    const rv = new IntersectionObserver((es) => es.forEach((e) => { if (e.isIntersecting) { e.target.classList.add("in"); rv.unobserve(e.target); } }), { rootMargin: "0px 0px -8% 0px" });
    document.querySelectorAll(".rv").forEach((el) => rv.observe(el));
  } else document.querySelectorAll(".rv").forEach((el) => el.classList.add("in"));

  /* ---- figure "show as table" toggles ---- */
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-toggle-table]");
    if (!b) return;
    const fig = b.closest(".fig");
    const on = fig.classList.toggle("as-table");
    b.setAttribute("aria-pressed", String(on));
    b.textContent = on ? "Show chart" : "Show as table";
  });

  /* ---- sortable tables (th with a button; cells may carry data-v for the sort value) ---- */
  window.GAR = window.GAR || {};
  window.GAR.sortable = function (table) {
    table.querySelectorAll("th").forEach((th, i) => {
      const btn = th.querySelector("button");
      if (!btn) return;
      btn.addEventListener("click", () => {
        const dir = th.getAttribute("aria-sort") === "ascending" ? "descending" : "ascending";
        table.querySelectorAll("th").forEach((x) => x.removeAttribute("aria-sort"));
        th.setAttribute("aria-sort", dir);
        const rows = [...table.tBodies[0].rows];
        const val = (r) => { const c = r.cells[i]; const v = c.dataset.v !== undefined ? c.dataset.v : c.textContent.trim(); const n = parseFloat(v); return isNaN(n) ? v.toLowerCase() : n; };
        rows.sort((a, b) => { const x = val(a), y = val(b); return (x > y ? 1 : x < y ? -1 : 0) * (dir === "ascending" ? 1 : -1); });
        rows.forEach((r) => table.tBodies[0].appendChild(r));
      });
    });
  };
  window.GAR.blip = blip;
})();
