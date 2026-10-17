(function () {
  'use strict';

  var T = {
    ink: '#111110', ink2: '#55534e', muted: '#8a8882', hair: '#e3e1da', axis: '#c8c5bc',
    surface: '#fbfaf7', accent: '#e0461a', blue: '#2a5caa', context: '#bdbab1',
    font: 'Inter, "Helvetica Neue", Helvetica, Arial, sans-serif'
  };
  var SESSIONS_URL = 'data/sessions.json';
  var SESSION_URL = 'data/session_';   // + round + '.json'
  var REFRESH_MS = 30000;
  var STEP_CHARTS = ['loss', 'grad', 'lr', 'rate', 'steptime'];
  var TIME_CHARTS = ['timeline', 'mem', 'rss', 'disk'];

  var state = {
    data: null, smooth: 0, range: 'all',
    scale: { loss: 'log', grad: 'log' },
    charts: {}, tables: {}, inited: false, bySteps: {},
    sessions: [], currentRound: null
  };

  // ---------- formatting ----------
  var nf = function (d) { return new Intl.NumberFormat('en-US', { minimumFractionDigits: d, maximumFractionDigits: d }); };
  var fmtInt = function (n) { return n == null ? '—' : Math.round(n).toLocaleString('en-US'); };
  var fmt = function (n, d) { return n == null || !isFinite(n) ? '—' : nf(d).format(n); };
  function compact(n) {
    if (n == null) return '—';
    var a = Math.abs(n);
    if (a >= 1e6) return (n / 1e6).toFixed(a >= 1e7 ? 0 : 1).replace(/\.0$/, '') + 'M';
    if (a >= 1e3) return (n / 1e3).toFixed(a >= 1e4 ? 0 : 1).replace(/\.0$/, '') + 'K';
    return String(Math.round(n));
  }
  function dur(s) {
    if (s == null || !isFinite(s)) return '—';
    s = Math.max(0, Math.round(s));
    var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
    if (h >= 1) return h + ' h ' + String(m).padStart(2, '0') + ' m';
    if (m >= 1) return m + ' m';
    return s + ' s';
  }
  var clockFmt = new Intl.DateTimeFormat('en-GB', { weekday: 'short', hour: '2-digit', minute: '2-digit', hour12: false });
  var fullFmt = new Intl.DateTimeFormat('en-GB', { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false });
  var clock = function (t) { return t == null || !isFinite(t) ? '—' : clockFmt.format(new Date(t * 1000)); };
  var full = function (ms) { return ms == null || !isFinite(ms) ? '—' : fullFmt.format(new Date(ms)); };
  var sci = function (v) {
    if (v == null) return '—';
    var e = v.toExponential(1).split('e');
    var sup = { '-': '⁻', '0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹' };
    return e[0] + ' × 10' + String(parseInt(e[1], 10)).split('').map(function (c) { return sup[c]; }).join('');
  };
  var $ = function (id) { return document.getElementById(id); };
  function setText(id, html) { var el = $(id); if (el) el.innerHTML = html; }
  function esc(s) { return String(s).replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }

  // debiased exponential moving average (same as TensorBoard smoothing)
  function ema(vals, a) {
    if (!a) return vals.slice();
    var last = 0, n = 0;
    return vals.map(function (v) {
      if (v == null) return null;
      last = last * a + (1 - a) * v; n++;
      return last / (1 - Math.pow(a, n));
    });
  }

  // ---------- chart scaffolding ----------
  function tooltipBox() {
    return {
      trigger: 'axis', confine: true, transitionDuration: 0,
      axisPointer: { type: 'line', lineStyle: { color: T.ink, width: 1, type: 'solid' }, label: { show: false } },
      backgroundColor: '#ffffff', borderColor: 'rgba(17,17,16,0.12)', borderWidth: 1, padding: [10, 12],
      textStyle: { color: T.ink, fontSize: 12, fontFamily: T.font },
      extraCssText: 'box-shadow:0 8px 28px rgba(17,17,16,0.10);border-radius:2px;'
    };
  }
  function row(color, name, value, dashed) {
    var sw = dashed
      ? '<span style="display:inline-block;width:14px;border-top:2px dashed ' + color + ';margin-right:8px;vertical-align:middle"></span>'
      : '<span style="display:inline-block;width:14px;height:2px;background:' + color + ';margin-right:8px;vertical-align:middle"></span>';
    return '<div style="display:flex;justify-content:space-between;gap:24px;line-height:20px">' +
      '<span style="color:' + T.ink2 + '">' + sw + esc(name) + '</span>' +
      '<span style="font-variant-numeric:tabular-nums;font-weight:500">' + value + '</span></div>';
  }
  function head(text) {
    return '<div style="font-size:11px;letter-spacing:.06em;text-transform:uppercase;color:' + T.muted +
      ';margin-bottom:6px;font-variant-numeric:tabular-nums">' + text + '</div>';
  }
  function stepHead(step) {
    var r = state.bySteps[step];
    var parts = ['Step ' + fmtInt(step)];
    if (r) { parts.push('Epoch ' + fmt(r.epoch, 2)); parts.push(clock(r.t)); }
    return head(parts.join(' · '));
  }

  function axisCommon() {
    return {
      axisLine: { show: true, lineStyle: { color: T.axis } },
      axisTick: { show: false },
      axisLabel: { color: T.muted, fontSize: 11, fontFamily: T.font, hideOverlap: true },
      splitLine: { show: false },
      nameTextStyle: { color: T.muted, fontSize: 11, fontFamily: T.font }
    };
  }
  function yAxis(extra) {
    return Object.assign({
      type: 'value', scale: false,
      axisLine: { show: false }, axisTick: { show: false },
      axisLabel: { color: T.muted, fontSize: 11, fontFamily: T.font },
      splitLine: { show: true, lineStyle: { color: T.hair, width: 1, type: 'solid' } },
      minorSplitLine: { show: false }
    }, extra || {});
  }
  function zoom(time) {
    return [
      { type: 'inside', xAxisIndex: 0, filterMode: 'filter', zoomOnMouseWheel: true, moveOnMouseMove: true, moveOnMouseWheel: false },
      {
        type: 'slider', xAxisIndex: 0, filterMode: 'filter', height: 16, bottom: 6, left: 48, right: 56,
        borderColor: 'transparent', backgroundColor: 'rgba(17,17,16,0.03)',
        fillerColor: 'rgba(17,17,16,0.07)',
        dataBackground: { lineStyle: { color: T.axis, width: 1 }, areaStyle: { opacity: 0 } },
        selectedDataBackground: { lineStyle: { color: T.ink2, width: 1 }, areaStyle: { opacity: 0 } },
        handleIcon: 'path://M0,0 L2,0 L2,16 L0,16 Z', handleSize: '100%',
        handleStyle: { color: T.ink, borderColor: T.ink },
        moveHandleSize: 0, brushSelect: false,
        showDetail: true,
        textStyle: { color: T.muted, fontSize: 10, fontFamily: T.font },
        labelFormatter: time ? function (v) { return isFinite(v) ? clock(v / 1000) : ''; } : function (v) { return isFinite(v) ? compact(v) : ''; },
        emphasis: { handleStyle: { color: T.accent, borderColor: T.accent } }
      }
    ];
  }
  function baseOption(opts) {
    opts = opts || {};
    var x = Object.assign(axisCommon(), opts.time
      ? { type: 'time', axisLabel: Object.assign(axisCommon().axisLabel, { formatter: function (v) { return clock(v / 1000).replace(/^\w+ /, ''); } }) }
      : { type: 'value', min: 0, max: 'dataMax', axisLabel: Object.assign(axisCommon().axisLabel, { showMaxLabel: false, formatter: function (v) { return compact(v); } }) });
    return {
      animation: true, animationDuration: 500, animationDurationUpdate: 0,
      textStyle: { fontFamily: T.font, color: T.ink2 },
      grid: { left: 8, right: 64, top: opts.legend ? 40 : 22, bottom: 40, containLabel: true },
      tooltip: tooltipBox(),
      legend: {
        show: !!opts.legend, top: 0, left: 8, icon: 'rect', itemWidth: 14, itemHeight: 2, itemGap: 20,
        textStyle: { color: T.ink2, fontSize: 12, fontFamily: T.font }, selectedMode: true, inactiveColor: T.axis,
        itemStyle: { borderWidth: 0 }, lineStyle: { width: 0 }
      },
      xAxis: x,
      yAxis: yAxis(opts.y),
      dataZoom: zoom(opts.time),
      title: { show: false },
      series: []
    };
  }
  function mergeAxis(base, extra) {
    var out = Object.assign({}, base, extra);
    ['axisLabel', 'axisLine', 'splitLine', 'axisTick'].forEach(function (k) {
      if (base[k] || extra[k]) out[k] = Object.assign({}, base[k] || {}, extra[k] || {});
    });
    return out;
  }
  function line(name, color, data, extra) {
    return Object.assign({
      name: name, type: 'line', data: data, showSymbol: false, symbol: 'circle', symbolSize: 8,
      lineStyle: { color: color, width: 2 }, itemStyle: { color: color, borderColor: T.surface, borderWidth: 2 },
      emphasis: { disabled: true }, sampling: 'lttb', connectNulls: false, z: 3
    }, extra || {});
  }

  // ---------- per-chart builders ----------
  function stepRows() {
    return state.data.series;
  }
  function pairs(rows, key, vals) {
    return rows.map(function (r, i) { var v = vals ? vals[i] : r[key]; return [r.step, v == null ? null : v]; });
  }
  function ckptLines() {
    return (state.data.checkpoints || []).map(function (c) {
      return { xAxis: c.step, label: { formatter: compact(c.step) } };
    });
  }

  var builders = {
    loss: function () {
      var rows = stepRows(), raw = rows.map(function (r) { return r.loss; });
      var sm = ema(raw, state.smooth), smoothing = state.smooth > 0;
      var best = null;
      rows.forEach(function (r) { if (r.loss != null && (!best || r.loss < best.loss)) best = r; });
      var series = [];
      var marks = {
        silent: true, symbol: 'none', animation: false,
        lineStyle: { color: T.axis, width: 1, type: 'solid' },
        label: { position: 'end', color: T.muted, fontSize: 10, fontFamily: T.font, distance: 4 },
        data: ckptLines()
      };
      var lastRow = rows[rows.length - 1];
      var bestMark = best && lastRow && best.step !== lastRow.step ? {
        silent: true, animation: false,
        data: [{ coord: [best.step, best.loss], symbol: 'circle', symbolSize: 9,
          itemStyle: { color: T.surface, borderColor: T.ink, borderWidth: 2 },
          label: { show: true, position: 'bottom', distance: 8, color: T.ink, fontSize: 11, fontFamily: T.font,
            formatter: 'Best ' + fmt(best.loss, 3) } }]
      } : undefined;
      if (smoothing) {
        series.push(line('Per 200 steps', T.context, pairs(rows, 'loss'), { lineStyle: { color: T.context, width: 1.25 }, z: 2, markPoint: bestMark }));
        series.push(line('Smoothed', T.accent, pairs(rows, null, sm), {
          markLine: marks,
          endLabel: { show: true, color: T.ink, fontSize: 12, fontWeight: 600, fontFamily: T.font, distance: 6,
            formatter: function (p) { return fmt(p.value[1], 3); } }
        }));
      } else {
        series.push(line('Loss', T.accent, pairs(rows, 'loss'), {
          markLine: marks, markPoint: bestMark,
          endLabel: { show: true, color: T.ink, fontSize: 12, fontWeight: 600, fontFamily: T.font, distance: 6,
            formatter: function (p) { return fmt(p.value[1], 3); } }
        }));
      }
      return {
        legend: { show: smoothing },
        grid: { top: smoothing ? 40 : 22 },
        yAxis: { type: state.scale.loss, scale: state.scale.loss === 'value', min: state.scale.loss === 'value' ? 0 : null,
          axisLabel: { formatter: function (v) { return v >= 10 ? fmt(v, 0) : v >= 1 ? fmt(v, 1) : fmt(v, 2); } } },
        tooltip: { formatter: function (ps) {
          var s = ps[0].value[0], out = stepHead(s);
          ps.forEach(function (p) { if (p.value[1] != null) out += row(p.color, p.seriesName, fmt(p.value[1], 3)); });
          return out;
        } },
        series: series
      };
    },

    grad: function () {
      var rows = stepRows(), raw = rows.map(function (r) { return r.grad_norm; });
      var sm = ema(raw, state.smooth), smoothing = state.smooth > 0;
      var clip = state.data.training.grad_clip_norm;
      var clipLine = clip ? {
        silent: true, symbol: 'none', animation: false,
        lineStyle: { color: T.ink2, width: 1, type: 'solid' },
        label: { position: 'insideEndTop', color: T.ink2, fontSize: 10, fontFamily: T.font, formatter: 'Clip threshold ' + fmt(clip, 0) },
        data: [{ yAxis: clip }]
      } : undefined;
      var series = smoothing
        ? [line('Per 200 steps', T.context, pairs(rows, 'grad_norm'), { lineStyle: { color: T.context, width: 1.25 }, z: 2, markLine: clipLine }),
           line('Smoothed', T.accent, pairs(rows, null, sm), { endLabel: { show: true, color: T.ink, fontSize: 12, fontWeight: 600, fontFamily: T.font, distance: 6, formatter: function (p) { return fmt(p.value[1], 1); } } })]
        : [line('Gradient norm', T.accent, pairs(rows, 'grad_norm'), { markLine: clipLine, endLabel: { show: true, color: T.ink, fontSize: 12, fontWeight: 600, fontFamily: T.font, distance: 6, formatter: function (p) { return fmt(p.value[1], 1); } } })];
      return {
        legend: { show: smoothing }, grid: { top: smoothing ? 40 : 22 },
        yAxis: { type: state.scale.grad, scale: state.scale.grad === 'value', min: state.scale.grad === 'value' ? 0 : null,
          axisLabel: { formatter: function (v) { return fmt(v, v < 10 ? 1 : 0); } } },
        tooltip: { formatter: function (ps) {
          var out = stepHead(ps[0].value[0]);
          ps.forEach(function (p) { if (p.value[1] != null) out += row(p.color, p.seriesName, fmt(p.value[1], 2)); });
          return out;
        } },
        series: series
      };
    },

    lr: function () {
      var rows = stepRows();
      var maxLr = Math.max.apply(null, rows.map(function (r) { return r.lr || 0; }).concat([1e-12]));
      return {
        legend: { show: false }, grid: { top: 22 },
        yAxis: { type: 'value', min: 0, max: maxLr * 2, interval: maxLr / 2, axisLabel: { formatter: function (v) { return v === 0 ? '0' : sci(v); } } },
        tooltip: { formatter: function (ps) { return stepHead(ps[0].value[0]) + row(T.accent, 'Learning rate', sci(ps[0].value[1])); } },
        series: [line('Learning rate', T.accent, pairs(rows, 'lr'), {
          endLabel: { show: true, color: T.ink, fontSize: 12, fontWeight: 600, fontFamily: T.font, distance: 6, formatter: function (p) { return sci(p.value[1]); } }
        })]
      };
    },

    rate: function () {
      var rows = stepRows();
      var comp = ema(rows.map(function (r) { return r.compute_steps_per_s; }), state.smooth);
      var wall = ema(rows.map(function (r) { return r.wall_steps_per_s; }), state.smooth);
      return {
        legend: { show: true }, grid: { top: 40 },
        yAxis: { type: 'value', min: 0, axisLabel: { formatter: function (v) { return fmt(v, 1); } } },
        tooltip: { formatter: function (ps) {
          var out = stepHead(ps[0].value[0]);
          ps.forEach(function (p) { if (p.value[1] != null) out += row(p.color, p.seriesName, fmt(p.value[1], 2) + ' steps/s'); });
          return out;
        } },
        series: [
          line('GPU compute', T.accent, pairs(rows, null, comp), { endLabel: { show: true, color: T.ink, fontSize: 12, fontWeight: 600, fontFamily: T.font, distance: 6, formatter: function (p) { return fmt(p.value[1], 2); } } }),
          line('Wall clock', T.blue, pairs(rows, null, wall))
        ]
      };
    },

    steptime: function () {
      var rows = stepRows();
      var up = ema(rows.map(function (r) { return r.update_s; }), state.smooth);
      var dl = ema(rows.map(function (r) { return r.data_s; }), state.smooth);
      function area(name, color, data) {
        return line(name, color, data, { stack: 'time', areaStyle: { color: color, opacity: 0.14 }, lineStyle: { color: color, width: 1.5 } });
      }
      return {
        legend: { show: true }, grid: { top: 40 },
        yAxis: { type: 'value', min: 0, axisLabel: { formatter: function (v) { return fmt(v, 2) + ' s'; } } },
        tooltip: { formatter: function (ps) {
          var out = stepHead(ps[0].value[0]), total = 0;
          ps.forEach(function (p) { if (p.value[1] != null) { total += p.value[1]; out += row(p.color, p.seriesName, fmt(p.value[1], 3) + ' s'); } });
          return out + '<div style="border-top:1px solid ' + T.hair + ';margin-top:6px;padding-top:4px">' + row('transparent', 'Total', fmt(total, 3) + ' s') + '</div>';
        } },
        series: [area('Model update', T.accent, pairs(rows, null, up)), area('Data loading', T.blue, pairs(rows, null, dl))]
      };
    },

    timeline: function () {
      var d = state.data, rows = d.series, run = d.run;
      var actual = rows.map(function (r) { return [r.t * 1000, r.step]; });
      var proj = [];
      var last = rows[rows.length - 1];
      if (last && run.eta_s && run.state === 'training') {
        proj = [[last.t * 1000, last.step], [(last.t + run.eta_s) * 1000, run.target_steps]];
      }
      return {
        legend: { show: true }, grid: { top: 40 },
        xAxis: { type: 'time' },
        yAxis: { type: 'value', min: 0, max: run.target_steps, axisLabel: { formatter: function (v) { return compact(v); } } },
        tooltip: { formatter: function (ps) {
          var t = ps[0].value[0] / 1000, out = head(full(t * 1000));
          ps.forEach(function (p) { out += row(p.color, p.seriesName, fmtInt(p.value[1]) + ' steps', p.seriesName === 'Projected'); });
          return out;
        } },
        series: [
          line('Completed', T.accent, actual, {
            markLine: { silent: true, symbol: 'none', animation: false, lineStyle: { color: T.axis, width: 1, type: 'solid' },
              label: { position: 'insideStartTop', color: T.muted, fontSize: 10, fontFamily: T.font, formatter: 'Target ' + fmtInt(run.target_steps) },
              data: [{ yAxis: run.target_steps }] }
          }),
          line('Projected', T.ink2, proj, { lineStyle: { color: T.ink2, width: 1.5, type: [4, 4] }, sampling: undefined,
            endLabel: { show: proj.length > 0, color: T.ink2, fontSize: 11, fontFamily: T.font, distance: 6, align: 'right', offset: [-4, -12],
              formatter: function (p) { return 'Finish ' + clock(p.value[0] / 1000); } } })
        ]
      };
    },

    mem: function () { return sysChart('mem_free_pct', 'Free memory', function (v) { return fmt(v, 0) + '%'; }, { min: 0, max: 100 }); },
    rss: function () { return sysChart('trainer_rss_mb', 'Trainer memory', function (v) { return fmtInt(v) + ' MB'; }, { min: 0 }); },
    disk: function () { return sysChart('disk_free_gb', 'Free disk', function (v) { return fmt(v, 1) + ' GB'; }, { min: 0 }); }
  };

  function sysChart(key, name, f, yx) {
    var rows = (state.data.system || []).filter(function (r) { return r[key] != null; });
    return {
      legend: { show: false }, grid: { top: 22, right: 72 },
      xAxis: { type: 'time' },
      yAxis: Object.assign({ type: 'value', axisLabel: { formatter: f } }, yx),
      tooltip: { formatter: function (ps) { return head(full(ps[0].value[0])) + row(T.accent, name, f(ps[0].value[1])); } },
      series: [line(name, T.accent, rows.map(function (r) { return [r.t * 1000, r[key]]; }), {
        endLabel: { show: true, color: T.ink, fontSize: 12, fontWeight: 600, fontFamily: T.font, distance: 6, formatter: function (p) { return f(p.value[1]); } }
      })]
    };
  }

  // ---------- tables (every chart has a table twin) ----------
  var tableCols = {
    loss: [['Step', 'step', 0], ['Epoch', 'epoch', 2], ['Loss', 'loss', 3]],
    grad: [['Step', 'step', 0], ['Epoch', 'epoch', 2], ['Gradient norm', 'grad_norm', 2]],
    lr: [['Step', 'step', 0], ['Learning rate', 'lr', 'sci']],
    rate: [['Step', 'step', 0], ['GPU compute, steps/s', 'compute_steps_per_s', 2], ['Wall clock, steps/s', 'wall_steps_per_s', 2]],
    steptime: [['Step', 'step', 0], ['Model update, s', 'update_s', 3], ['Data loading, s', 'data_s', 3]],
    timeline: [['Time', 't', 'time'], ['Step', 'step', 0], ['Samples', 'samples', 0]],
    mem: [['Time', 't', 'time'], ['Free memory, %', 'mem_free_pct', 0]],
    rss: [['Time', 't', 'time'], ['Trainer memory, MB', 'trainer_rss_mb', 0]],
    disk: [['Time', 't', 'time'], ['Free disk, GB', 'disk_free_gb', 1]]
  };
  function renderTable(key) {
    var box = document.querySelector('[data-chart="' + key + '"] .table-view');
    if (!box || box.hidden) return;
    var cols = tableCols[key];
    var rows = (TIME_CHARTS.indexOf(key) >= 0 && key !== 'timeline' ? state.data.system : state.data.series).slice().reverse();
    var h = '<table class="data"><thead><tr>' + cols.map(function (c, i) { return '<th class="' + (i ? 'num' : '') + '">' + c[0] + '</th>'; }).join('') + '</tr></thead><tbody>';
    rows.forEach(function (r) {
      h += '<tr>' + cols.map(function (c, i) {
        var v = r[c[1]], s;
        if (c[2] === 'time') s = full(v * 1000);
        else if (c[2] === 'sci') s = sci(v);
        else s = c[2] === 0 ? fmtInt(v) : fmt(v, c[2]);
        return '<td class="' + (i ? 'num' : '') + '">' + s + '</td>';
      }).join('') + '</tr>';
    });
    box.innerHTML = h + '</tbody></table>';
  }

  // ---------- page sections ----------
  function renderHeader() {
    var d = state.data, run = d.run, m = d.model, ds = d.dataset;
    var age = (Date.now() - Date.parse(d.generated_utc)) / 1000;
    var st = run.state, label;
    if (st === 'finished') { st = 'finished'; label = 'Training complete'; }
    else if (/^(fatal|gave_up)/.test(st)) { st = 'interrupted'; label = 'Interrupted'; }
    else if (st === 'stopped_by_user') { st = 'interrupted'; label = 'Stopped'; }
    else if (age > 300) { st = 'stale'; label = 'Telemetry delayed'; }
    else { st = 'training'; label = 'Training live'; }
    $('status').setAttribute('data-state', st);
    setText('status-text', label);

    setText('f-robot', esc(ds.robot));
    setText('f-policy', 'ACT · ' + fmt(m.params / 1e6, 1) + 'M parameters');
    setText('f-data', fmtInt(ds.episodes) + ' demonstrations · ' + fmtInt(ds.frames) + ' frames');
    setText('f-compute', esc(m.device));
    setText('lede-episodes', fmtInt(ds.episodes));
  }

  function renderProgress() {
    var d = state.data, run = d.run;
    var pct = Math.min(100, 100 * run.step / run.target_steps);
    $('fill').style.width = pct + '%';
    setText('p-count', fmtInt(run.step) + ' / ' + fmtInt(run.target_steps) + ' steps <span style="color:' + T.muted + ';font-weight:400">(' + fmt(pct, 1) + '%)</span>');
    if (run.state === 'finished') {
      var last = d.series[d.series.length - 1];
      setText('p-eta', 'Completed ' + (last ? clock(last.t) : ''));
    } else if (run.eta_s) {
      setText('p-eta', clock(Date.now() / 1000 + run.eta_s - (Date.now() - Date.parse(d.generated_utc)) / 1000) + ' <span style="color:' + T.muted + ';font-weight:400">· in ' + dur(run.eta_s) + '</span>');
    } else setText('p-eta', '—');
    $('ticks').innerHTML = (d.checkpoints || []).map(function (c) {
      return '<i style="left:' + (100 * c.step / run.target_steps) + '%" title="Checkpoint ' + fmtInt(c.step) + '"></i>';
    }).join('');
    var sc = [0, 0.25, 0.5, 0.75, 1];
    $('track-scale').innerHTML = sc.map(function (f) { return '<span style="left:' + (f * 100) + '%">' + compact(run.target_steps * f) + '</span>'; }).join('');
  }

  function renderKpis() {
    var d = state.data, run = d.run, rows = d.series, bs = d.training.batch_size || 8;
    if (!rows.length) return;
    var last = rows[rows.length - 1], first = rows[0];
    var sm = ema(rows.map(function (r) { return r.loss; }), state.smooth);
    var cur = sm[sm.length - 1];
    var best = rows.reduce(function (b, r) { return r.loss != null && (!b || r.loss < b.loss) ? r : b; }, null);
    var change = first.loss ? (cur - first.loss) / first.loss * 100 : null;

    setText('k-step', fmtInt(run.step));
    setText('k-step-sub', 'of ' + fmtInt(run.target_steps));
    setText('k-loss', fmt(cur, 3));
    setText('k-loss-sub', change != null ? '<span class="' + (change < 0 ? 'down' : 'up') + '">' + (change < 0 ? '↓ ' : '↑ ') + fmt(Math.abs(change), 1) + '%</span> since step ' + fmtInt(first.step) : '&nbsp;');
    setText('k-best', best ? fmt(best.loss, 3) : '—');
    setText('k-best-sub', best ? 'at step ' + fmtInt(best.step) : '&nbsp;');
    setText('k-rate', fmt(run.steps_per_s, 2) + '<span class="unit">steps/s</span>');
    setText('k-rate-sub', run.steps_per_s ? fmt(run.steps_per_s * bs, 1) + ' samples/s · batch ' + bs : '&nbsp;');
    setText('k-epoch', fmt(last.epoch, 2));
    setText('k-epoch-sub', fmtInt(last.samples) + ' samples seen');
    var endT = run.state === 'finished' ? last.t : Date.parse(d.generated_utc) / 1000;
    setText('k-elapsed', dur(run.started_t ? endT - run.started_t : null));
    setText('k-elapsed-sub', 'started ' + clock(run.started_t) + (run.restarts ? ' · ' + run.restarts + ' auto-resume' + (run.restarts > 1 ? 's' : '') : ''));

    // side notes
    setText('n-reduction', cur ? '×' + fmt(first.loss / cur, 1) : '—');
    setText('n-reduction-sub', fmt(first.loss, 3) + ' → ' + fmt(cur, 3));
    var cutoff = last.step - 2000, idx = -1;
    for (var i = rows.length - 1; i >= 0; i--) { if (rows[i].step <= cutoff) { idx = i; break; } }
    if (idx >= 0 && sm[idx]) {
      var ch = (cur - sm[idx]) / sm[idx] * 100;
      setText('n-recent', (ch <= 0 ? '−' : '+') + fmt(Math.abs(ch), 1) + '%');
      setText('n-recent-sub', 'from ' + fmt(sm[idx], 3) + ' at step ' + fmtInt(rows[idx].step));
    } else { setText('n-recent', '—'); setText('n-recent-sub', 'available after 2,000 steps'); }
    setText('n-samples', compact(last.samples));
    setText('n-samples-sub', fmt(last.epoch, 2) + ' passes over ' + fmtInt(d.dataset.frames) + ' frames');
    var n = (d.checkpoints || []).length, sf = d.training.save_freq || 5000;
    setText('n-ckpt', String(n));
    setText('n-ckpt-sub', run.state === 'finished' ? 'every ' + fmtInt(sf) + ' steps' : 'next at step ' + fmtInt((Math.floor(run.step / sf) + 1) * sf));
  }

  function renderCheckpoints() {
    var d = state.data, cks = (d.checkpoints || []).slice().reverse(), bs = d.training.batch_size || 8;
    var tb = document.querySelector('#ckpt-table tbody');
    if (!cks.length) {
      tb.innerHTML = '<tr><td colspan="6" class="empty">The first checkpoint is written at step ' + fmtInt(d.training.save_freq || 5000) + '.</td></tr>';
      return;
    }
    var bestStep = cks.reduce(function (b, c) { return c.loss != null && (!b || c.loss < b.loss) ? c : b; }, null);
    tb.innerHTML = cks.map(function (c) {
      return '<tr><td class="num">' + fmtInt(c.step) + (bestStep && c.step === bestStep.step && cks.length > 1 ? '<span class="best-tag">Lowest loss</span>' : '') + '</td>' +
        '<td>' + full(c.t * 1000) + '</td>' +
        '<td class="num">' + fmt(c.loss, 3) + '</td>' +
        '<td class="num">' + fmt(c.step * bs / d.dataset.frames, 2) + '</td>' +
        '<td class="num">' + fmtInt(c.model_mb) + ' MB</td>' +
        '<td>' + (c.published ? '<span class="avail"><i></i>Delivered to robot host</span>' : '<span class="avail local"><i></i>On training machine</span>') + '</td></tr>';
    }).join('');
  }

  function renderSpec() {
    var d = state.data, m = d.model, t = d.training, ds = d.dataset;
    function dl(id, title, items) {
      $(id).innerHTML = '<div class="spec-title">' + title + '</div>' + items.filter(function (x) { return x[1] != null && x[1] !== ''; })
        .map(function (x) { return '<div><dt>' + x[0] + '</dt><dd>' + x[1] + '</dd></div>'; }).join('');
    }
    var chunkSec = m.chunk_size && ds.fps ? ' (' + fmt(m.chunk_size / ds.fps, 1) + ' s)' : '';
    dl('spec-model', 'Model', [
      ['Architecture', 'Action Chunking Transformer'],
      ['Parameters', fmt(m.params / 1e6, 1) + 'M'],
      ['Vision backbone', m.vision_backbone === 'resnet18' ? 'ResNet-18, ImageNet' : esc(m.vision_backbone || '—')],
      ['Hidden size · heads', fmtInt(m.dim_model) + ' · ' + fmtInt(m.n_heads)],
      ['Encoder · decoder layers', m.n_encoder_layers + ' · ' + m.n_decoder_layers],
      ['Feed-forward width', fmtInt(m.dim_feedforward)],
      ['Action chunk', fmtInt(m.chunk_size) + ' steps' + chunkSec],
      ['CVAE latent · KL weight', m.use_vae ? m.latent_dim + ' · ' + fmt(m.kl_weight, 0) : 'Off'],
      ['Dropout', fmt(m.dropout, 1)]
    ]);
    dl('spec-training', 'Training', [
      ['Framework', esc(t.framework)],
      ['Device', esc(m.device)],
      ['Optimizer', (t.optimizer || 'adamw').replace('adamw', 'AdamW')],
      ['Learning rate · backbone', sci(m.optimizer_lr) + ' · ' + sci(m.optimizer_lr_backbone)],
      ['Weight decay', sci(m.optimizer_weight_decay)],
      ['Gradient clipping', fmt(t.grad_clip_norm, 0)],
      ['Batch size', fmtInt(t.batch_size)],
      ['Steps', fmtInt(t.steps)],
      ['Checkpoint interval', fmtInt(t.save_freq) + ' steps'],
      ['Image augmentation', t.image_augmentation ? 'Color & sharpness jitter' : 'Off']
    ]);
    dl('spec-dataset', 'Dataset', [
      ['Robot', esc(ds.robot)],
      ['Task', ds.task ? '<span style="white-space:normal">' + esc(ds.task.charAt(0).toUpperCase() + ds.task.slice(1)) + '</span>' : null],
      ['Collection', esc(ds.collection)],
      ['Demonstrations', fmtInt(ds.episodes)],
      ['Frames', fmtInt(ds.frames)],
      ['Total duration', fmt(ds.frames / ds.fps / 60, 1) + ' min'],
      ['Frame rate', ds.fps + ' fps'],
      ['Camera', esc(ds.camera)],
      ['State · action', ds.state_dim + '-D · ' + ds.action_dim + '-D joint positions']
    ]);
  }

  // ---------- charts lifecycle ----------
  function initCharts() {
    Object.keys(builders).forEach(function (key) {
      var card = document.querySelector('[data-chart="' + key + '"]');
      var el = card.querySelector('.chart');
      var c = echarts.init(el, null, { renderer: 'canvas' });
      var time = TIME_CHARTS.indexOf(key) >= 0;
      var opt = baseOption({ time: time, legend: false });
      c.setOption(opt);
      state.charts[key] = c;
      new ResizeObserver(function () { c.resize(); }).observe(el);
    });
    echarts.connect(STEP_CHARTS.map(function (k) { return state.charts[k]; }));
  }

  function updateCharts() {
    state.bySteps = {};
    state.data.series.forEach(function (r) { state.bySteps[r.step] = r; });
    Object.keys(builders).forEach(function (key) {
      var o = builders[key]();
      var time = TIME_CHARTS.indexOf(key) >= 0;
      var base = baseOption({ time: time });
      if (o.yAxis) o.yAxis = mergeAxis(base.yAxis, o.yAxis);
      if (o.xAxis) o.xAxis = mergeAxis(base.xAxis, o.xAxis);
      state.charts[key].setOption(o, { replaceMerge: ['series'] });
      renderTable(key);
    });
  }

  function applyRange() {
    var rows = state.data.series;
    if (!rows.length) return;
    var maxStep = rows[rows.length - 1].step;
    STEP_CHARTS.concat(TIME_CHARTS).forEach(function (k) {
      var c = state.charts[k];
      if (state.range === 'all') { c.dispatchAction({ type: 'dataZoom', start: 0, end: 100 }); return; }
      var from = Math.max(0, maxStep - Number(state.range));
      if (STEP_CHARTS.indexOf(k) >= 0) {
        c.dispatchAction({ type: 'dataZoom', startValue: from, endValue: maxStep });
      } else {
        var r = rows.find(function (x) { return x.step >= from; }) || rows[0];
        c.dispatchAction({ type: 'dataZoom', startValue: (r.t - 30) * 1000, endValue: Date.now() + 60000 });
      }
    });
  }

  function exportPNG(key) {
    var card = document.querySelector('[data-chart="' + key + '"]');
    var c = state.charts[key];
    var title = card.querySelector('h3').textContent, sub = card.querySelector('.sub').textContent;
    Object.keys(state.charts).forEach(function (k) {
      state.charts[k].dispatchAction({ type: 'hideTip' });
      state.charts[k].dispatchAction({ type: 'updateAxisPointer', currTrigger: 'leave' });
    });
    var legendOn = !!(c.getOption().legend[0] || {}).show;
    var gridTop = c.getOption().grid[0].top;
    c.setOption({
      title: { show: true, text: title, subtext: sub, left: 8, top: 6, itemGap: 6,
        textStyle: { fontFamily: T.font, fontSize: 17, fontWeight: 600, color: T.ink },
        subtextStyle: { fontFamily: T.font, fontSize: 12, color: T.ink2 } },
      grid: { top: (legendOn ? 92 : 70), bottom: 30 },
      legend: { top: 58 },
      graphic: { elements: [{ type: 'text', right: 12, bottom: 8, style: { text: 'PiPER · ACT policy training', fill: T.muted, font: '11px ' + T.font } }] }
    });
    var url = c.getDataURL({ type: 'png', pixelRatio: 3, backgroundColor: T.surface, excludeComponents: ['dataZoom'] });
    c.setOption({ title: { show: false }, grid: { top: gridTop, bottom: document.body.classList.contains('present') ? 12 : 40 }, legend: { top: 0 }, graphic: { elements: [] } }, { replaceMerge: ['graphic'] });
    var a = document.createElement('a');
    a.href = url; a.download = 'piper-act-' + key + '.png';
    document.body.appendChild(a); a.click(); a.remove();
  }

  function wire() {
    document.querySelectorAll('.seg[data-control]').forEach(function (seg) {
      seg.addEventListener('click', function (e) {
        var b = e.target.closest('button'); if (!b) return;
        seg.querySelectorAll('button').forEach(function (x) { x.setAttribute('aria-pressed', String(x === b)); });
        var ctl = seg.getAttribute('data-control');
        if (ctl === 'smooth') { state.smooth = Number(b.dataset.value); render(); }
        if (ctl === 'range') { state.range = b.dataset.value; applyRange(); }
      });
    });
    document.querySelectorAll('.seg[data-scale]').forEach(function (seg) {
      seg.addEventListener('click', function (e) {
        var b = e.target.closest('button'); if (!b) return;
        seg.querySelectorAll('button').forEach(function (x) { x.setAttribute('aria-pressed', String(x === b)); });
        state.scale[seg.getAttribute('data-scale')] = b.dataset.value;
        updateCharts();
      });
    });
    document.querySelectorAll('.card').forEach(function (card) {
      var key = card.getAttribute('data-chart');
      card.querySelector('.actions').addEventListener('click', function (e) {
        var b = e.target.closest('button[data-act]'); if (!b) return;
        var act = b.dataset.act;
        if (act === 'png') exportPNG(key);
        if (act === 'reset') state.charts[key].dispatchAction({ type: 'dataZoom', start: 0, end: 100 });
        if (act === 'table') {
          var tv = card.querySelector('.table-view'), ch = card.querySelector('.chart');
          var on = tv.hidden;
          tv.hidden = !on; ch.hidden = on;
          b.setAttribute('aria-pressed', String(on));
          b.textContent = on ? 'Chart' : 'Table';
          if (on) renderTable(key); else state.charts[key].resize();
        }
      });
    });
    var pb = $('present');
    var exit = document.createElement('button');
    exit.className = 'btn present-exit'; exit.textContent = 'Exit presentation';
    document.body.appendChild(exit);
    function setPresent(on) {
      document.body.classList.toggle('present', on);
      pb.setAttribute('aria-pressed', String(on));
      var u = new URL(location.href);
      if (on) u.searchParams.set('present', '1'); else u.searchParams.delete('present');
      history.replaceState(null, '', u);
      Object.keys(state.charts).forEach(function (k) {
        state.charts[k].setOption({ dataZoom: [{}, { show: !on }], grid: { bottom: on ? 12 : 40 } });
      });
      setTimeout(function () { Object.values(state.charts).forEach(function (c) { c.resize(); }); }, 50);
      nudgeExit();
    }
    var exitTimer = null;
    function nudgeExit() {
      if (!document.body.classList.contains('present')) return;
      exit.classList.add('visible');
      clearTimeout(exitTimer);
      exitTimer = setTimeout(function () { exit.classList.remove('visible'); }, 2000);
    }
    document.addEventListener('mousemove', nudgeExit, { passive: true });
    document.addEventListener('touchstart', nudgeExit, { passive: true });
    pb.addEventListener('click', function () { setPresent(!document.body.classList.contains('present')); });
    exit.addEventListener('click', function () { setPresent(false); });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && document.body.classList.contains('present')) setPresent(false); });
    if (new URLSearchParams(location.search).get('present') === '1') setPresent(true);
  }

  function renderUpdated() {
    if (!state.data) return;
    var age = Math.max(0, (Date.now() - Date.parse(state.data.generated_utc)) / 1000);
    setText('updated', 'Updated ' + (age < 60 ? Math.round(age) + ' s' : dur(age)) + ' ago');
    setText('colophon-time', 'Snapshot ' + full(Date.parse(state.data.generated_utc)));
  }

  function render() {
    renderHeader(); renderProgress(); renderKpis(); renderCheckpoints(); renderSpec();
    updateCharts(); renderUpdated();
  }

  function loadRound(n) {
    return fetch(SESSION_URL + n + '.json?t=' + Date.now(), { cache: 'no-store' })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (d) {
        state.data = d;
        state.currentRound = n;
        if (!state.inited) { initCharts(); wire(); state.inited = true; }
        render();
      })
      .catch(function (e) {
        if (window.console) console.error('Portal render failed:', e);
        if (!state.data) { $('status').setAttribute('data-state', 'stale'); setText('status-text', 'Waiting for telemetry'); }
      });
  }

  function renderSessions() {
    var el = $('sessions');
    if (!el || !state.sessions.length) return;
    el.innerHTML = state.sessions.map(function (s) {
      var cls = s.current ? 'on' : '';
      var label = 'Round ' + s.round;
      if (s.phase === 'training' || s.phase === 'gating' || s.phase === 'building') label += ' <span class="live">●</span>';
      if (s.phase === 'published') label += ' <span class="done">✓</span>';
      return '<button class="' + cls + '" data-round="' + s.round + '">' + label + '</button>';
    }).join('');
  }

  function loadSessions() {
    return fetch(SESSIONS_URL + '?t=' + Date.now(), { cache: 'no-store' })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (list) {
        state.sessions = list;
        var cur = null;
        list.forEach(function (s) { if (s.current) cur = s.round; });
        if (cur === null && list.length) cur = list[list.length - 1].round;
        if (state.currentRound === null) state.currentRound = cur;
        renderSessions();
        loadRound(state.currentRound);
      })
      .catch(function (e) {
        if (window.console) console.error('sessions load failed:', e);
        loadRound(state.currentRound === null ? 0 : state.currentRound);
      });
  }

  function wireSessions() {
    var el = $('sessions');
    if (!el) return;
    el.addEventListener('click', function (e) {
      var b = e.target.closest('button[data-round]'); if (!b) return;
      var n = Number(b.dataset.round);
      state.currentRound = n;
      el.querySelectorAll('button').forEach(function (x) { x.classList.toggle('on', Number(x.dataset.round) === n); });
      loadRound(n);
    });
  }

  function refreshCurrent() {
    // only auto-refresh the live round; archived rounds are static
    if (state.currentRound === null) return;
    var cur = state.sessions.find(function (s) { return s.round === state.currentRound; });
    if (cur && (cur.phase === 'training' || cur.phase === 'gating' || cur.phase === 'building')) {
      loadRound(state.currentRound);
    }
  }

  loadSessions();
  wireSessions();
  setInterval(refreshCurrent, REFRESH_MS);
  setInterval(renderUpdated, 1000);
})();
