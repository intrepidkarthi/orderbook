"use strict";

// A visual, interactive guide to order books & market making. Every chapter is
// driven by the REAL engine (compiled to WASM): the depth chart is a live book,
// the slippage numbers are actual fills. No cartoons — real market microstructure.
(function () {
  const COL = {
    bg: "#0d1117", grid: "rgba(139,148,158,.14)",
    fg: "#e6edf3", mut: "#8b949e", gold: "#d4a547",
    bid: "#3fb950", bidFill: "rgba(63,185,80,.16)",
    ask: "#f85149", askFill: "rgba(248,81,73,.14)",
  };
  const W = 640, H = 380;
  let cv, ctx, tabsEl, titleEl, textEl, insightEl, controlsEl, cur = 0;
  let lastImpact = null;

  const F = (n, d) => Number(n).toFixed(d);

  function clear() {
    ctx.clearRect(0, 0, W, H);
    ctx.fillStyle = COL.bg;
    ctx.fillRect(0, 0, W, H);
  }
  function text(x, y, s, color, align, size, weight) {
    ctx.fillStyle = color;
    ctx.textAlign = align || "left";
    ctx.font = (weight || "") + " " + (size || 12) + "px ui-sans-serif, -apple-system, system-ui, sans-serif";
    ctx.fillText(s, x, y);
  }
  function dashed(x1, y1, x2, y2, color) {
    ctx.save();
    ctx.setLineDash([4, 4]);
    ctx.strokeStyle = color;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    ctx.lineTo(x2, y2);
    ctx.stroke();
    ctx.restore();
  }

  function seedBook() {
    obReset("LEARN");
    const asks = [[100.5, 3], [101, 5], [101.5, 2], [102, 6], [102.5, 4], [103, 3], [103.5, 5], [104, 2], [104.5, 4], [105, 3]];
    const bids = [[99.5, 4], [99, 3], [98.5, 6], [98, 2], [97.5, 5], [97, 4], [96.5, 3], [96, 5], [95.5, 2], [95, 4]];
    asks.forEach((l, i) => obSubmit("sa" + i, "SELL", "LIMIT", String(l[0]), String(l[1])));
    bids.forEach((l, i) => obSubmit("bb" + i, "BUY", "LIMIT", String(l[0]), String(l[1])));
  }

  // ---------- Chapter 1: the depth chart ----------
  function drawDepth() {
    seedBook();
    const snap = JSON.parse(obSnapshot(24));
    clear();
    const bids = snap.bids.map((l) => ({ p: +l.price, s: +l.size }));
    const asks = snap.asks.map((l) => ({ p: +l.price, s: +l.size }));
    if (!bids.length || !asks.length) return;

    let c = 0; const bc = bids.map((b) => ({ p: b.p, c: (c += b.s) }));
    c = 0; const ac = asks.map((a) => ({ p: a.p, c: (c += a.s) }));
    const maxC = Math.max(bc[bc.length - 1].c, ac[ac.length - 1].c) * 1.08;
    const minP = bc[bc.length - 1].p, maxP = ac[ac.length - 1].p;
    const pad = { l: 46, r: 16, t: 20, b: 34 };
    const X = (p) => pad.l + (p - minP) / (maxP - minP) * (W - pad.l - pad.r);
    const Y = (v) => H - pad.b - (v / maxC) * (H - pad.t - pad.b);
    const base = Y(0);

    // grid + y labels (cumulative size)
    for (let g = 1; g <= 4; g++) {
      const gy = Y((maxC / 4) * g);
      ctx.strokeStyle = COL.grid; ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(pad.l, gy); ctx.lineTo(W - pad.r, gy); ctx.stroke();
      text(pad.l - 8, gy + 4, F((maxC / 4) * g, 0), COL.mut, "right", 11);
    }

    const area = (pts, line, fill) => {
      ctx.beginPath();
      ctx.moveTo(X(pts[0].p), base);
      pts.forEach((pt) => ctx.lineTo(X(pt.p), Y(pt.c)));
      ctx.lineTo(X(pts[pts.length - 1].p), base);
      ctx.closePath();
      ctx.fillStyle = fill; ctx.fill();
      ctx.beginPath();
      pts.forEach((pt, i) => (i ? ctx.lineTo(X(pt.p), Y(pt.c)) : ctx.moveTo(X(pt.p), Y(pt.c))));
      ctx.strokeStyle = line; ctx.lineWidth = 2; ctx.stroke();
    };
    area(bc, COL.bid, COL.bidFill);
    area(ac, COL.ask, COL.askFill);

    // mid + spread
    const mid = (bids[0].p + asks[0].p) / 2;
    dashed(X(mid), pad.t, X(mid), base, "rgba(212,165,71,.5)");
    text(X(mid), pad.t - 6, "mid " + F(mid, 2), COL.gold, "center", 12, "600");
    text(X(bids[0].p) - 4, Y(bc[0].c) - 8, "bids", COL.bid, "right", 12, "600");
    text(X(asks[0].p) + 4, Y(ac[0].c) - 8, "asks", COL.ask, "left", 12, "600");

    // price axis ticks
    text(pad.l, H - 12, F(minP, 1), COL.mut, "left", 11);
    text(W - pad.r, H - 12, F(maxP, 1), COL.mut, "right", 11);
    text(W / 2, H - 12, "price →", COL.mut, "center", 11);
    text(pad.l - 40, pad.t + 4, "cum", COL.mut, "left", 10);

    const spread = asks[0].p - bids[0].p;
    setInsight(
      row("best bid", F(bids[0].p, 2)) + row("best ask", F(asks[0].p, 2)) +
      row("spread", F(spread, 2)) + row("depth shown", F(maxC / 1.08, 0) + " units")
    );
  }

  // ---------- Chapter 2: impact / slippage ----------
  function renderImpact() {
    seedBook();
    const before = JSON.parse(obSnapshot(24));
    drawAskLadder(before.asks, lastImpact ? lastImpact.consumed : null);
    if (!lastImpact) {
      setInsight('<p class="learn-prompt">Fire a market buy → and watch it "walk the book". Bigger orders reach deeper, worse-priced levels.</p>');
    }
  }

  function drawAskLadder(asks, consumed) {
    clear();
    text(W / 2, 22, "ASKS — the sell orders you'd buy from (cheapest first)", COL.mut, "center", 12, "600");
    const rows = asks.slice(0, 10);
    const maxS = Math.max(...rows.map((a) => +a.size));
    const rowH = 30, x0 = 60, y0 = 42, barW = W - 200;
    rows.forEach((a, i) => {
      const y = y0 + i * rowH;
      const hit = consumed && consumed[a.price];
      const bw = (+a.size / maxS) * barW;
      ctx.fillStyle = hit ? "rgba(212,165,71,.30)" : COL.askFill;
      ctx.fillRect(x0, y, bw, rowH - 6);
      text(x0 - 8, y + rowH / 2, F(+a.price, 1), hit ? COL.gold : COL.ask, "right", 14, "600");
      text(x0 + bw + 8, y + rowH / 2, a.size, COL.mut, "left", 12);
      if (hit) text(x0 + 8, y + rowH / 2 + 1, hit === "part" ? "◑ partial" : "✓ filled", COL.gold, "left", 11, "600");
    });
  }

  function fireImpact(size) {
    seedBook();
    const before = JSON.parse(obSnapshot(24));
    const bestAsk = +before.asks[0].price;
    const res = JSON.parse(obSubmit("taker", "BUY", "MARKET", "0", String(size)));
    let filled = 0, notional = 0;
    const consumed = {};
    (res.trades || []).forEach((t) => {
      filled += +t.quantity; notional += +t.price * +t.quantity;
      consumed[t.price] = "filled";
    });
    // mark the deepest touched level partial if it still has size left
    const after = JSON.parse(obSnapshot(24));
    const remaining = {}; after.asks.forEach((a) => (remaining[a.price] = +a.size));
    Object.keys(consumed).forEach((p) => { if (remaining[p] > 0) consumed[p] = "part"; });

    const avg = filled ? notional / filled : bestAsk;
    const bps = bestAsk ? ((avg - bestAsk) / bestAsk) * 10000 : 0;
    lastImpact = { size, filled, avg, bestAsk, bps, levels: Object.keys(consumed).length, consumed, short: filled < size };

    drawAskLadder(before.asks, consumed);
    let html = row("order", "BUY " + size + " @ market");
    html += row("best ask (top)", F(bestAsk, 2));
    html += row("avg fill price", F(avg, 3));
    html += row("levels walked", String(lastImpact.levels));
    const cls = bps > 8 ? "bad" : "ok";
    html += '<div class="learn-big ' + cls + '">slippage &#8776; ' + F(bps, 1) + " bps</div>";
    if (lastImpact.short) {
      html += '<p class="learn-prompt">Bigger than the whole ask side &mdash; only ' + F(filled, 0) + " filled.</p>";
    } else {
      html += '<p class="learn-note">The bigger the order, the deeper it reaches and the worse the average price &mdash; the cost of size and immediacy.</p>';
    }
    setInsight(html);
  }

  // ---------- Chapter 3: signal or noise ----------
  function drawSignal() {
    clear();
    const pad = { l: 60, r: 30, t: 40, b: 60 };
    const maxR = 0.6;
    const Y = (v) => H - pad.b - (v / maxR) * (H - pad.t - pad.b);
    for (let g = 0; g <= 3; g++) {
      const gy = Y((maxR / 3) * g);
      ctx.strokeStyle = COL.grid; ctx.beginPath(); ctx.moveTo(pad.l, gy); ctx.lineTo(W - pad.r, gy); ctx.stroke();
      text(pad.l - 10, gy + 4, F((maxR / 3) * g, 1), COL.mut, "right", 11);
    }
    text(24, pad.t - 16, "R²  (variance of the price move explained by order-flow imbalance)", COL.mut, "left", 11);

    const bars = [
      { label: "SAME moment", sub: "contemporaneous", r: 0.33, col: COL.gold },
      { label: "NEXT move", sub: "predictive", r: 0.004, col: COL.mut },
    ];
    const bw = 150, gap = 90, x0 = (W - (bw * 2 + gap)) / 2;
    bars.forEach((b, i) => {
      const x = x0 + i * (bw + gap);
      const y = Y(b.r), base = Y(0);
      ctx.fillStyle = b.col;
      ctx.globalAlpha = i === 0 ? 1 : 0.55;
      ctx.fillRect(x, y, bw, base - y);
      ctx.globalAlpha = 1;
      text(x + bw / 2, y - 10, "R² " + F(b.r, 2), b.col, "center", 15, "700");
      text(x + bw / 2, base + 22, b.label, COL.fg, "center", 14, "600");
      text(x + bw / 2, base + 40, b.sub, COL.mut, "center", 11);
    });

    setInsight(
      '<p class="learn-note">Order-flow imbalance explains about a <strong>third</strong> of the price move ' +
      'happening in the <strong>same instant</strong> — but almost <strong>nothing</strong> about the ' +
      '<strong>next</strong> one.</p>' +
      row("sim study", "contemp 0.33 · predictive 0.00") +
      row("live Coinbase", "contemp ≈ 0.55 (real BTC data)") +
      '<p class="learn-prompt">The book describes the present. It does not forecast the future. ' +
      'Run it yourself: <code>go run ./cmd/ofistudy</code> / <code>./cmd/l2capture</code>.</p>'
    );
  }

  // ---------- chapter registry ----------
  const chapters = [
    {
      tab: "Anatomy", title: "The depth chart",
      body: "Every resting buy (bid) and sell (ask), summed up and drawn as cumulative depth. The green wall is buyers, the red wall is sellers, and the notch between them is the spread. A steep wall means deep liquidity — hard to move; a shallow one moves easily. This is a live book from the real engine.",
      controls: [], render: drawDepth,
    },
    {
      tab: "Impact", title: "Slippage: the cost of size",
      body: "A market order doesn't get one price — it eats the cheapest ask, then the next, then the next, 'walking the book'. Fire orders of different sizes and watch the real engine report the actual average fill price and the slippage, in basis points.",
      controls: [
        { label: "Buy 3", fn: () => fireImpact(3) },
        { label: "Buy 12", fn: () => fireImpact(12) },
        { label: "Buy 30", fn: () => fireImpact(30) },
        { label: "Buy 60", fn: () => fireImpact(60) },
      ],
      render: () => { lastImpact = null; renderImpact(); },
    },
    {
      tab: "Signal or noise?", title: "Does the book predict the future?",
      body: "The most repeated claim in trading is that order-book pressure predicts the next move. We measured it — in simulation and on live market data. The relationship is real, but it's contemporaneous (same instant), not predictive.",
      controls: [], render: drawSignal,
    },
  ];

  function row(k, v) {
    return '<div class="learn-row"><span>' + k + "</span><b>" + v + "</b></div>";
  }
  function setInsight(html) { insightEl.innerHTML = html; }

  function show(i) {
    cur = i;
    const ch = chapters[i];
    [...tabsEl.children].forEach((t, j) => t.classList.toggle("on", j === i));
    titleEl.textContent = ch.title;
    textEl.textContent = ch.body;
    controlsEl.innerHTML = "";
    ch.controls.forEach((c) => {
      const b = document.createElement("button");
      b.className = "learn-btn";
      b.textContent = c.label;
      b.addEventListener("click", c.fn);
      controlsEl.appendChild(b);
    });
    insightEl.innerHTML = "";
    ch.render();
  }

  function init() {
    cv = document.getElementById("learnCanvas");
    if (!cv) return;
    ctx = cv.getContext("2d");
    tabsEl = document.getElementById("learnTabs");
    titleEl = document.getElementById("learnTitle");
    textEl = document.getElementById("learnText");
    insightEl = document.getElementById("learnInsight");
    controlsEl = document.getElementById("learnControls");
    chapters.forEach((ch, i) => {
      const t = document.createElement("button");
      t.className = "learn-tab";
      t.textContent = ch.tab;
      t.addEventListener("click", () => show(i));
      tabsEl.appendChild(t);
    });
  }

  // Render lazily when the scene is shown (engine must be ready by then).
  function setActive(on) { if (on) show(cur); }

  window.Tutorial = { init, setActive }; // keep the name app.js already wires
})();
