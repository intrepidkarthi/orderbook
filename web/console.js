// console.js — renderer for the live market console. All market logic lives in
// the WASM bridge (cmd/obwasm): this file draws what obStep/obSignals/obAlerts
// return and never computes a signal itself (docs/CONSOLE-SPEC.md).
(async function () {
  const go = new Go();
  let result;
  try {
    result = await WebAssembly.instantiateStreaming(fetch("obook.wasm"), go.importObject);
  } catch {
    const bytes = await (await fetch("obook.wasm")).arrayBuffer();
    result = await WebAssembly.instantiate(bytes, go.importObject);
  }
  go.run(result.instance);

  const $ = (id) => document.getElementById(id);
  $("boot").hidden = true;
  $("ui").hidden = false;

  // ---- state ----------------------------------------------------------------
  const TICK_MS = 100;          // 10 steps/second at 1×
  const RING = 300;             // sparkline history
  const TAPE_MAX = 60;
  let running = !matchMedia("(prefers-reduced-motion: reduce)").matches;
  let speed = 1;
  let alertCount = 0;
  const mids = [], ofis = [], cvds = [];

  const push = (arr, v) => { arr.push(v); if (arr.length > RING) arr.shift(); };
  const fmtP = (v) => v ? v.toFixed(2) : "—";
  const fmtS = (v, dp = 2) => (v > 0 ? "+" : "") + v.toFixed(dp);

  if (!running) $("c-run").textContent = "Run";

  // ---- sparklines -----------------------------------------------------------
  // 2px single-hue line on a recessive (grid-free) surface; the current value
  // is direct-labeled in the panel header, hover shows the value under the
  // cursor. Identity comes from the panel title, so no legend.
  function drawSpark(canvas, series, hoverX) {
    const dpr = devicePixelRatio || 1;
    const w = canvas.clientWidth, h = canvas.clientHeight;
    if (canvas.width !== w * dpr) { canvas.width = w * dpr; canvas.height = h * dpr; }
    const ctx = canvas.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    if (series.length < 2) return null;

    let min = Math.min(...series), max = Math.max(...series);
    if (max - min < 1e-9) { max += 1; min -= 1; }
    const pad = (max - min) * 0.12;
    min -= pad; max += pad;
    const x = (i) => (i / (series.length - 1)) * (w - 10) + 5;
    const y = (v) => h - 6 - ((v - min) / (max - min)) * (h - 12);

    const css = getComputedStyle(document.documentElement);
    const line = css.getPropertyValue("--link").trim() || "#58a6ff";
    const muted = css.getPropertyValue("--muted").trim() || "#8b949e";

    ctx.strokeStyle = line; ctx.lineWidth = 2; ctx.lineJoin = "round"; ctx.beginPath();
    series.forEach((v, i) => (i ? ctx.lineTo(x(i), y(v)) : ctx.moveTo(x(i), y(v))));
    ctx.stroke();
    ctx.fillStyle = line; ctx.beginPath();
    ctx.arc(x(series.length - 1), y(series[series.length - 1]), 3, 0, 7);
    ctx.fill();

    if (hoverX != null) {
      const i = Math.max(0, Math.min(series.length - 1, Math.round(((hoverX - 5) / (w - 10)) * (series.length - 1))));
      ctx.strokeStyle = muted; ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(x(i), 4); ctx.lineTo(x(i), h - 4); ctx.stroke();
      return series[i];
    }
    return null;
  }

  const sparks = [
    { canvas: $("sp-mid"), series: mids, cur: $("cur-mid"), fmt: fmtP },
    { canvas: $("sp-ofi"), series: ofis, cur: $("cur-ofi"), fmt: (v) => fmtS(v, 1) },
    { canvas: $("sp-cvd"), series: cvds, cur: $("cur-cvd"), fmt: (v) => fmtS(v, 1) },
  ];
  for (const s of sparks) {
    s.hoverX = null;
    s.canvas.addEventListener("mousemove", (e) => { s.hoverX = e.offsetX; renderSpark(s); });
    s.canvas.addEventListener("mouseleave", () => { s.hoverX = null; renderSpark(s); });
  }
  function renderSpark(s) {
    const hv = drawSpark(s.canvas, s.series, s.hoverX);
    const last = s.series[s.series.length - 1];
    if (last !== undefined) s.cur.textContent = s.fmt(hv != null ? hv : last);
  }

  // ---- ladder ---------------------------------------------------------------
  let lastTradePx = null, flashPx = null;
  function renderLadder() {
    const snap = JSON.parse(obSnapshot(10));
    const rows = [];
    const maxSz = Math.max(1e-9, ...snap.asks.map((l) => +l.size), ...snap.bids.map((l) => +l.size));
    const row = (cls, l) => {
      const flash = flashPx === l.price ? " flash" : "";
      return `<div class="lrow ${cls}${flash}"><span class="bar" style="width:${(100 * l.size) / maxSz}%"></span>` +
        `<span class="p">${(+l.price).toFixed(2)}</span><span class="q">${(+l.size).toFixed(3)}</span></div>`;
    };
    rows.push(`<div class="lsec">ASKS · price / size</div>`);
    for (const l of [...snap.asks].reverse()) rows.push(row("ask", l));  // best ask adjacent to mid
    rows.push(`<div class="lmid"><span>mid <b>${snap.mid || "—"}</b></span><span>spread <b>${snap.spread || "—"}</b></span><span>last <b>${snap.last_trade}</b></span></div>`);
    for (const l of snap.bids) rows.push(row("bid", l));
    rows.push(`<div class="lsec">BIDS</div>`);
    $("ladder").innerHTML = rows.join("");
    flashPx = null;
    return snap;
  }

  // ---- tape -----------------------------------------------------------------
  const tapeEl = $("tape");
  let tapeCount = 0;
  function appendTrades(trades) {
    if (!trades.length) return;
    if (tapeCount === 0) tapeEl.innerHTML = "";
    for (const t of trades) {
      const buy = t.taker_side === "BUY";
      const div = document.createElement("div");
      div.className = "trow " + (buy ? "b" : "s");
      div.innerHTML = `<span class="side">${buy ? "▲ B" : "▼ S"}</span><span class="px">${t.price}</span><span class="qy">${t.quantity}</span>`;
      tapeEl.prepend(div);
      tapeCount++;
      flashPx = t.price;
      lastTradePx = t.price;
    }
    while (tapeEl.children.length > TAPE_MAX) tapeEl.lastChild.remove();
  }

  // ---- alerts ---------------------------------------------------------------
  function drainAlerts() {
    const res = JSON.parse(obAlerts(alertCount));
    if (!res.alerts.length) return;
    if (alertCount === 0) $("alist").innerHTML = "";
    for (const a of res.alerts) {
      const div = document.createElement("div");
      div.className = "arow";
      div.innerHTML = `<span class="chip">${a.kind}</span><span class="who">${a.user}</span><span class="det">${a.detail} (seq ${a.seq})</span>`;
      $("alist").prepend(div);
    }
    alertCount = res.total;
    $("cur-alerts").textContent = `${alertCount} alert${alertCount === 1 ? "" : "s"}`;
  }

  // ---- signals --------------------------------------------------------------
  function renderSignals() {
    const s = JSON.parse(obSignals());
    $("mv-mid").textContent = fmtP(s.mid);
    $("mv-spread").textContent = fmtP(s.spread);
    $("mv-last").textContent = fmtP(s.last);
    $("mv-step").textContent = s.step;
    if (s.mid) push(mids, s.mid);
    push(ofis, s.ofi);
    push(cvds, s.cvd);
    sparks.forEach(renderSpark);

    const imb = s.imbalance || 0;
    $("cur-imb").textContent = fmtS(imb);
    const fill = $("g-fill");
    fill.className = "fill " + (imb >= 0 ? "pos" : "neg");
    fill.style.width = Math.min(50, Math.abs(imb) * 50) + "%";

    if (s.lambda && s.lambda.n) {
      $("lam-v").textContent = s.lambda.lambda.toFixed(3);
      $("lam-r2").textContent = `R² ${s.lambda.r2.toFixed(2)} · n=${s.lambda.n}`;
    }
  }

  // ---- main loop ------------------------------------------------------------
  function tick() {
    if (!running || document.hidden) return;
    const out = JSON.parse(obStep(speed));
    appendTrades(out.trades);
    renderLadder();
    renderSignals();
    drainAlerts();
  }
  setInterval(tick, TICK_MS);
  renderLadder();
  renderSignals();

  // ---- controls -------------------------------------------------------------
  $("c-run").addEventListener("click", () => {
    running = !running;
    $("c-run").textContent = running ? "Pause" : "Run";
  });
  $("c-speed").addEventListener("change", (e) => { speed = +e.target.value; });
  $("c-reset").addEventListener("click", () => {
    obReset(+($("c-seed").value || 1));
    mids.length = ofis.length = cvds.length = 0;
    tapeEl.innerHTML = '<div class="tape-empty">No prints yet.</div>';
    tapeCount = 0;
    alertCount = 0;
    $("alist").innerHTML = '<div class="surv-empty">Quiet — the detector is watching every event this page generates. Provoke it: <b>Run a spoof</b>.</div>';
    $("cur-alerts").textContent = "0 alerts";
    $("lam-v").textContent = "—";
    $("lam-r2").textContent = "warming up";
    renderLadder();
    renderSignals();
  });
  $("c-buy").addEventListener("click", () => { obSubmit("you", "BUY", "MARKET", "0", "1.0"); tick(); });
  $("c-sell").addEventListener("click", () => { obSubmit("you", "SELL", "MARKET", "0", "1.0"); tick(); });
  $("c-spoof").addEventListener("click", () => {
    const res = JSON.parse(obSpoof());
    if (res.error) return;
    const btn = $("c-spoof");
    btn.disabled = true;
    btn.textContent = "Spoof resting…";
    // Re-arm once the bridge has auto-cancelled it and the detector has spoken.
    const iv = setInterval(() => {
      if (alertCount > 0 || !btn.disabled) { btn.disabled = false; btn.textContent = "Run a spoof"; clearInterval(iv); }
    }, 500);
  });
})();
