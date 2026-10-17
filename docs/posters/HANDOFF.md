# NervusOS · ADVX 2026 — Poster Triptych — Editing Handoff

A three-board hackathon poster set, each **600 × 900 mm portrait** (a printable 60 × 90 cm board). Built as self-contained HTML → rendered to print PDF via headless Chrome. This doc is everything you (or a fresh Claude in claude.ai) need to keep editing them in **Design Lab**.

---

## 0. Quick start in claude.ai Design Lab

1. Open a new chat on **claude.ai**, ideally in a browser **on your Mac** (the boards were first set in Chinese with the macOS font *Hiragino Sans GB*; they are now English — on Windows/Linux the Chinese will fall back or go blank unless a CJK webfont is added; see §4.1).
2. Paste the **full contents of one board's `.html` file** and say: *"Turn this into an artifact I can edit in Design Lab."* Do this once per board (they're independent files).
3. Paste **this handoff** as context so the assistant knows the conventions and doesn't break the print pipeline.
4. Edit visually / by instruction in Design Lab. When you want a print file again, re-export with the command in §6.

> Each board is one standalone HTML document. There is no shared stylesheet — every board carries its own `<style>`. Edit them independently.

---

## 1. The three boards

Left → right, as they hang:

| # | Role | Deliverables | Source HTML | Palette |
|---|---|---|---|---|
| **① left** | Technical architecture (the 4-layer stack; **L2 kernel is the star**) | `NervusOS-Board1-Technical.pdf` / `.png` | `board-a-layers.html` | Light — teal + per-layer hues, bright hardware highlights |
| **② middle** | Ecosystem + business model (mind-map + traction) | `NervusOS-Board2-Ecosystem.pdf` / `.png` | `board-c-central.html` | Dark — near-black + red/amber/yellow |
| **③ right** | Business prospects + the 5 sponsor tracks + updates QR | `NervusOS-Board3-Prospects.pdf` / `.png` | `board-3-tracks.html` | Dark — same as ② |

`board-b-flowchart.html` is the **dropped** system-flowchart board — ignore it (kept only for reference).

All files live in `/Users/ianzhang/nervus-poster/`. QR images and the WeChat data-URI are in `assets/`.

---

## 2. What each board says (so edits stay on-message)

**Positioning (all boards):** *NervusOS — Android for robots / "Android for robots"*: one OS + app store that runs on any brand of robot. Design contract: **Change one robot = Change one Provider, kernel zero changes**.

**Board ① technical**, top→bottom: header + thesis/stat bar → **Runtime Architecture** 4-layer stack (L1 apps · L2 **Nervus OS kernel = the 3× star** · L3 DimOS · L4 robot body) with the arbiter rail on the right → **Ring→PiPER live-demo** strip. Hardware we actually run is **bold + bright** with in use / connecting badges (PiPER, NERO, Limo Pro, GO2 Air). L2 shows 9 kernel modules (`internal/control` arbiter, `internal/safety`, `internal/motiongate`, permission, ipc, PREEMPT_RT scheduler, package manager, profile+resolve, AI gateway).

**Board ② ecosystem**, top→bottom: hero *Android robot* → intro → **mind-map of the six roles** (Personal user · Personal developer · We · Platform side · Industry user · Corporate developer · OEM) each tagged with a **¥ money marker** → business model → **traction band** (big **30+ / 4 / 1 / 2**) + the centered highlighted line *"Development chain, ecosystem chain, this week it's all running through…"* Bottom ~⅓ left open for **sponsors / team**. OEM node has **logo slots** (see §5).

**Board ③ prospects + tracks**: *Why now* prospects → **five track cards** (09 Dimensional/DimOS · 12 Slong AgileX · 20 D-Robotics/RDK · 22 Future Intelligence viaim · 23 Zilo Whisper) each explaining how we used that sponsor → **WeChat updates QR** (group, noted valid until 7/30). GitHub QR is on ① and ②.

All Chinese copy was run through the humanizer (direct, no AI-ish phrasing, no emoji). Keep that voice.

---

## 3. Design system

### 3.1 Board ① (light / technical) — `:root` tokens
- Layer hues: `--app #6E5A8C` · `--nervus #0F6E8C` (primary teal) · `--dimos #3F7A5E` · `--hw #8A6A3F` · `--danger #B4453C`
- Hardware highlight (bright, added in rework): `--hot-amber #E8890C` · `--hot-red #C0392B` · `--hot-yellow #F5B301`
- Accents/text/bg: `--accent`, `--accent-deep`, `--text-primary/secondary/muted`, `--bg-page/card`, `--emph #B07E2B`
- Corners: `--rs: 0` (square — set globally; every radius is `calc(N*var(--u)*var(--rs))` so 0 = square)

### 3.2 Boards ② & ③ (dark / pitch) — `:root` tokens
- Ground: `--ground #0B0A0A`, `--panel #17110F`, `--panel-line #3A2A22`
- The four brand colors: `--red-deep #7A0E0E` · `--red #E23B2E` · `--amber #C8860B` · `--yellow #FFD23E`
- Text: `--ink #F4ECDE` (warm off-white) · `--ink-dim` · `--ink-mute`
- Corners are square by default (no `--rs`).

### 3.3 Fonts (all boards)
- `--han` = `"Hiragino Sans GB","Heiti SC","STHeiti","Noto Sans SC","PingFang SC",...` → **Chinese + Latin**
- `--sans` = `"Inter"/"Helvetica Neue", var(--han)` → headings (still CJK-safe via han fallback)
- `--mono` = `ui-monospace,"SF Mono",Menlo,…` → code/labels, **NO CJK**

### 3.4 The `--u` unit (important)
Every size is `calc(N * var(--u))`. `--u` is small px on screen and **`1mm` in print** (an `@media print { :root { --u: 1mm } }` block at the bottom of each file). That one rule is what makes the PDF come out at exactly 600 × 900 mm. Font sizes use a `--fs-*` / `--h-*` scale, also in `--u`. To scale the whole board, don't touch geometry — just nudge the scale tokens.

---

## 4. Editing rules & gotchas (read before you change anything)

1. **Never put Chinese text inside a `.en` span or any `--mono` element.** Mono fonts have no CJK glyphs, so Chinese there renders **blank** (this bit us on the L4 badges). Chinese goes in `--han`/`--sans` elements (titles, `p`, `.tk-body`, badges use `--sans`). English/paths are fine in mono `.en`.
2. **Keep the `@media print { :root { --u: 1mm } … }` block and the `@page { size: 600mm 900mm }`.** Delete either and the print export breaks / rescales.
3. **QR codes are inline base64 `data:` URIs** (GitHub QR on ①②; WeChat QR on ③). Don't delete them. To replace: swap the `src="data:image/png;base64,…"` string. Regenerate a QR with `segno`/`qrencode` and base64 it (see `assets/`).
4. **Fit is manual, not automatic.** `.poster`/`.board` has `overflow:hidden`, so if you add content it can silently **clip at the bottom / collide with the footer**. After any content change, re-render (§6) and eyeball the bottom. To reclaim space: shorten copy, drop a cell, or reduce a padding/gap token.
5. **Board ① square corners** come from `--rs: 0`. Leave it 0 unless you want rounded again.
6. Board ② mind-map: connector **lines are an SVG** (`.mm-lines`, `viewBox 0 0 100 100`, `preserveAspectRatio=none`) drawn from the hub (50,50) to fixed node anchors; the nodes are an absolutely-positioned 3-col grid on top. If you move a node, move its line endpoint to match.

---

## 5. Where the things you'll want to edit live

**OEM logos (Board ②, `board-c-central.html`)** — in the `.oem-logos` block inside the OEM node. Each slot is:

```html
<div class="oem-item">
  <div class="oem-logo"><span>LOGO</span></div>   <!-- replace this with <img src="data:…"> -->
  <div class="oem-name">AgileX<span class="en">Current</span></div>
</div>
```

Drop a logo by replacing the `<div class="oem-logo">…</div>` with `<img class="oem-logo" src="data:image/png;base64,…" alt="AgileX">`. Confirmed slots: **AgileX** + **Unitree**. Shortlist for more (send me/paste the PNGs): DeepRobotics · Fourier · AgiBot · Kepler.

**Traction numbers (Board ②)** — the `.tr-stats` block (`30+ / 4 / 1 / 2`) and the `.tr-statement` line.

**Sponsors / team (bottom strip)** — Boards ② and ③ deliberately leave the lower area open (subtle glow only). Add a row of sponsor logos / team names there.

**Hardware badges (Board ①)** — L4 cells use `<span class="badge">Current</span>` (yellow) and `<span class="badge soon">In progress</span>` (grey). Add hardware by copying a `.cell.hw` block.

**The 5 track cards (Board ③)** — `.track.t1…t5` blocks; each has `.tk-no` (track 09/12/20/22/23), `.tk-name`, `.tk-body`.

---

## 6. Re-export a print PDF (after editing)

From `/Users/ianzhang/nervus-poster/` (needs Google Chrome + poppler `pdftoppm`):

```bash
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
"$CHROME" --headless=new --disable-gpu --no-pdf-header-footer \
  --print-to-pdf="board-a.pdf" "file://$PWD/board-a-layers.html"
# check it's 1700.88 x 2550.96 pts (= 600×900mm):
pdfinfo board-a.pdf | grep -i "page size"
# make a 150-DPI PNG proof:
pdftoppm -r 150 -png -singlefile board-a.pdf board-a
```

(Repeat with `board-c-central.html`→`board-c` and `board-3-tracks.html`→`board-3`.)

If you export from **Design Lab / claude.ai** instead, use the browser's Print → Save as PDF with margins **None** and paper size **600 × 900 mm** (or A1-ish); the `@page` rule should drive it.

---

## 7. Still to do

- **OEM logos** into the Board ② slots (§5).
- **Sponsors + team** strip on Boards ② and ③ (open bottom areas).
- **Print prep before the shop:** add 3 mm bleed + crop marks (bump `@page` to `606mm 906mm` with a 3 mm inset, or let the printer add marks) and confirm CMYK-safe colors — the teal/amber/red/yellow are all in-gamut; the bright yellow `#FFD23E` and red `#E23B2E` are the ones to soft-proof.
- Optional: if you'll edit off-Mac, embed a subset CJK webfont (e.g. Noto Sans SC) as base64 so Chinese survives anywhere.

---

## 8. Sources of truth (for accuracy when editing copy)

- The team's ADVX submission form + track Q&A (`~/Desktop/ADVX-Submission Form-NervusOS.md`).
- Repos: `github.com/Nervus-OS` — `nervud` (kernel), `nervus-ipc` (protocol), `nervus-app-sdk`, `nervus-packaging`, `nervus-system-software`, `nring`. Local apps: `~/piper-ring-app`, `~/nring`, `~/alien-wandering-app`.
- Voice: keep Chinese humanized (direct, varied rhythm, minimal bold, no emoji).
