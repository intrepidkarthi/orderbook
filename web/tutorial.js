"use strict";

// A small canvas tutorial: step-by-step animations that explain order books and
// market making from scratch. Each step draws a fresh frame from a time value
// `t` (seconds since the step started), so animations play in and then settle.
(function () {
  const C = {
    bg: "#0d1117", panel: "#161b22", line: "#30363d",
    fg: "#e6edf3", muted: "#8b949e",
    bid: "#22c55e", ask: "#f85149", gold: "#d4a547", blue: "#58a6ff",
  };

  let cv, ctx, W, H, titleEl, textEl, dotsEl, prevBtn, nextBtn;
  let cur = 0, start = 0, raf = null, active = false;

  const clamp01 = (x) => (x < 0 ? 0 : x > 1 ? 1 : x);
  const ease = (u) => (u < 0.5 ? 2 * u * u : 1 - Math.pow(-2 * u + 2, 2) / 2);
  const lerp = (a, b, u) => a + (b - a) * u;

  function rrect(x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
  }

  function chip(x, y, w, h, fill, top, bottom, textColor) {
    ctx.globalAlpha = 1;
    rrect(x, y, w, h, 8);
    ctx.fillStyle = fill;
    ctx.fill();
    ctx.fillStyle = textColor || "#0d1117";
    ctx.textAlign = "center";
    ctx.font = "600 15px -apple-system, system-ui, sans-serif";
    if (bottom) {
      ctx.fillText(top, x + w / 2, y + h / 2 - 3);
      ctx.font = "12px -apple-system, system-ui, sans-serif";
      ctx.fillText(bottom, x + w / 2, y + h / 2 + 14);
    } else {
      ctx.fillText(top, x + w / 2, y + h / 2 + 5);
    }
  }

  function label(x, y, text, color, align, size) {
    ctx.fillStyle = color;
    ctx.textAlign = align || "left";
    ctx.font = (size || 13) + "px -apple-system, system-ui, sans-serif";
    ctx.fillText(text, x, y);
  }

  // A price ladder. levels: [{price, size, side, hl}] top→bottom. Returns row Y map.
  function ladder(x, y, w, rowH, levels, appear) {
    const maxSize = Math.max(1, ...levels.map((l) => l.size));
    const ys = [];
    levels.forEach((l, i) => {
      const p = clamp01((appear === undefined ? 1 : appear) * levels.length - i);
      const a = ease(p);
      if (a <= 0) { ys.push(null); return; }
      const ry = y + i * rowH;
      ys.push(ry + rowH / 2);
      ctx.globalAlpha = a;
      const col = l.side === "bid" ? C.bid : C.ask;
      const dim = l.side === "bid" ? "rgba(34,197,94,.18)" : "rgba(248,81,73,.18)";
      // size bar
      const bw = (l.size / maxSize) * (w - 8) * a;
      rrect(x + w - bw, ry + 3, bw, rowH - 6, 4);
      ctx.fillStyle = l.hl ? (l.side === "bid" ? "rgba(34,197,94,.4)" : "rgba(248,81,73,.4)") : dim;
      ctx.fill();
      label(x + 6, ry + rowH / 2 + 4, l.price.toFixed(1), col, "left", 14);
      label(x + w - 6, ry + rowH / 2 + 4, String(l.size), C.muted, "right", 13);
    });
    ctx.globalAlpha = 1;
    return ys;
  }

  function flash(x, y, r, alpha) {
    ctx.globalAlpha = alpha;
    ctx.fillStyle = C.gold;
    ctx.beginPath();
    ctx.arc(x, y, r, 0, Math.PI * 2);
    ctx.fill();
    ctx.globalAlpha = 1;
  }

  function arrow(x1, y1, x2, y2, color) {
    ctx.strokeStyle = color;
    ctx.fillStyle = color;
    ctx.lineWidth = 2;
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    ctx.lineTo(x2, y2);
    ctx.stroke();
    const ang = Math.atan2(y2 - y1, x2 - x1);
    ctx.beginPath();
    ctx.moveTo(x2, y2);
    ctx.lineTo(x2 - 8 * Math.cos(ang - 0.4), y2 - 8 * Math.sin(ang - 0.4));
    ctx.lineTo(x2 - 8 * Math.cos(ang + 0.4), y2 - 8 * Math.sin(ang + 0.4));
    ctx.closePath();
    ctx.fill();
  }

  // ---- steps ----
  const baseBook = () => [
    { price: 102, size: 5, side: "ask" },
    { price: 101.5, size: 2, side: "ask" },
    { price: 101, size: 3, side: "ask" },
    { price: 100, size: 4, side: "bid" },
    { price: 99.5, size: 3, side: "bid" },
    { price: 99, size: 6, side: "bid" },
  ];

  const steps = [
    {
      title: "1 · A market is just buyers and sellers",
      text: "Two people want to trade. A buyer will pay up to $99. A seller wants at least $101. They don't agree yet — there's a gap. Every market is millions of these offers.",
      draw(t) {
        const u = ease(clamp01(t / 0.8));
        chip(lerp(-160, 120, u), 150, 200, 64, C.bid, "BUYER", "will pay $99", "#06210f");
        chip(lerp(W + 160, W - 320, u), 150, 200, 64, C.ask, "SELLER", "wants $101", "#2a0a08");
        if (u > 0.9) {
          const pulse = 0.5 + 0.5 * Math.sin(t * 3);
          ctx.globalAlpha = 0.5 + 0.5 * pulse;
          label(W / 2, 130, "gap!", C.gold, "center", 15);
          ctx.globalAlpha = 1;
        }
      },
    },
    {
      title: "2 · The order book: all offers, sorted by price",
      text: "Stack every buy order (bids, green) below and every sell order (asks, red) above, sorted by price. That ladder IS the order book. The size is how much they want.",
      draw(t) {
        label(W / 2, 40, "ORDER BOOK", C.muted, "center", 13);
        ladder(W / 2 - 150, 60, 300, 34, baseBook(), clamp01(t / 1.4));
        if (t > 1.5) {
          label(W / 2 - 165, 92, "asks", C.ask, "right", 12);
          label(W / 2 - 165, 92 + 34 * 3 + 6, "bids", C.bid, "right", 12);
        }
      },
    },
    {
      title: "3 · Best prices & the spread",
      text: "The best bid ($100) is the most anyone will pay. The best ask ($101) is the cheapest you can buy. The gap between them is the spread — tight means a busy, liquid market.",
      draw(t) {
        const book = baseBook();
        book[2].hl = true; // best ask 101
        book[3].hl = true; // best bid 100
        const ys = ladder(W / 2 - 150, 60, 300, 34, book, 1);
        const pulse = 0.4 + 0.4 * Math.sin(t * 3);
        ctx.globalAlpha = pulse;
        label(W / 2 + 160, ys[2] + 4, "◀ best ask 101", C.ask, "left", 13);
        label(W / 2 + 160, ys[3] + 4, "◀ best bid 100", C.bid, "left", 13);
        ctx.globalAlpha = 1;
        // spread bracket
        const midY = (ys[2] + ys[3]) / 2;
        label(W / 2 - 175, midY + 4, "spread", C.gold, "right", 13);
        arrow(W / 2 - 158, ys[2] + 8, W / 2 - 158, ys[3] - 8, C.gold);
        arrow(W / 2 - 158, ys[3] - 8, W / 2 - 158, ys[2] + 8, C.gold);
      },
    },
    {
      title: "4 · A limit order waits at your price",
      text: "Place a buy limit at $99.5 — below the best ask, so it can't trade yet. It just rests in the book and waits. You made an offer; now you're a 'maker'.",
      draw(t) {
        const book = baseBook();
        const ys = ladder(W / 2 - 150, 60, 300, 34, book, 1);
        const u = ease(clamp01(t / 1.2));
        const x = lerp(-140, W / 2 - 150, u);
        chip(x, ys[4] - 16, 130, 32, "rgba(34,197,94,.9)", "BUY 3 @ 99.5", null, "#06210f");
        if (u > 0.95) { label(W / 2 + 160, ys[4] + 4, "◀ rests & waits", C.bid, "left", 13); }
      },
    },
    {
      title: "5 · A market order trades right now",
      text: "A market buy doesn't wait — it grabs the cheapest ask ($101) instantly. A trade happens (gold flash!), that level clears, and the price ticks up. You 'took' liquidity.",
      draw(t) {
        const book = baseBook();
        const filled = t > 1.2;
        if (filled) book.splice(2, 1); // remove best ask 101 after fill
        const ys = ladder(W / 2 - 150, 60, 300, 34, book, 1);
        const askY = 60 + 34 * 2 + 17;
        const u = ease(clamp01(t / 1.2));
        if (!filled) {
          const x = lerp(-140, W / 2 - 150, u);
          chip(x, askY - 16, 150, 32, "rgba(88,166,255,.95)", "MARKET BUY 3", null, "#06210f");
          if (u > 0.9) flash(W / 2, askY, 10 + 20 * Math.sin(Math.min(1, (t - 1) * 6)), 0.6);
        } else {
          const f = clamp01((t - 1.2) / 0.4);
          flash(W / 2, askY, 30 * (1 - f), 0.5 * (1 - f));
          label(W / 2, 40, "TRADE: 3 @ 101 ✓  price ticks up", C.gold, "center", 14);
        }
      },
    },
    {
      title: "6 · Big orders pay more (slippage)",
      text: "One big market buy eats the cheapest ask, then the next, then the next — 'walking the book'. Each bite is pricier, so the average price is worse. Size costs you.",
      draw(t) {
        const eaten = Math.min(3, Math.floor(t / 0.8));
        const eatOrder = [101, 101.5, 102]; // cheapest (best) ask first
        const eatenPrices = eatOrder.slice(0, eaten);
        const remaining = baseBook().filter((l) => !(l.side === "ask" && eatenPrices.includes(l.price)));
        const ys = ladder(W / 2 - 150, 60, 300, 34, remaining, 1);
        const msg = eaten === 0 ? "big MARKET BUY incoming…" : "ate: " + eatenPrices.join(" → ") + (eaten >= 3 ? "  · avg price climbed!" : "…");
        label(W / 2, 40, msg, C.gold, "center", 14);
        if (eaten < 3 && ys.length) {
          const topAskY = ys.find((v) => v !== null) || 77;
          flash(W / 2, topAskY, 8 + 6 * Math.sin(t * 8), 0.5);
        }
      },
    },
    {
      title: "7 · Maker vs taker",
      text: "The resting order MAKES liquidity (patient, sets a price). The market order TAKES it (impatient, pays the spread). Exchanges usually charge takers more — that's the fee for immediacy.",
      draw(t) {
        const u = ease(clamp01(t / 0.8));
        ctx.globalAlpha = u;
        chip(W / 2 - 300, 150, 240, 70, "rgba(34,197,94,.15)", "MAKER", "posts a limit · waits · sets price", C.bid);
        chip(W / 2 + 60, 150, 240, 70, "rgba(248,81,73,.15)", "TAKER", "market order · pays spread · instant", C.ask);
        ctx.globalAlpha = 1;
        if (u > 0.9) arrow(W / 2 - 55, 185, W / 2 + 55, 185, C.gold);
      },
    },
    {
      title: "8 · Market making: earn the spread",
      text: "A market maker posts BOTH sides. Buyers hit its ask, sellers hit its bid — it buys low, sells high, and pockets the spread again and again. It's renting out immediacy, not betting on direction.",
      draw(t) {
        const cx = W / 2, cy = 130;
        chip(cx - 70, cy - 24, 140, 48, "rgba(212,165,71,.18)", "MARKET MAKER", null, C.gold);
        label(cx - 130, cy - 40, "bid 99.9", C.bid, "center", 13);
        label(cx + 130, cy - 40, "ask 100.1", C.ask, "center", 13);
        // pulsing arrows: sellers hit bid, buyers hit ask
        const s = 0.5 + 0.5 * Math.sin(t * 2);
        ctx.globalAlpha = s;
        arrow(cx - 180, cy, cx - 72, cy, C.ask);
        ctx.globalAlpha = 1 - s;
        arrow(cx + 180, cy, cx + 72, cy, C.bid);
        ctx.globalAlpha = 1;
        // profit counter
        const profit = (Math.floor(t / 1) * 0.2).toFixed(1);
        label(cx, cy + 90, "spread captured: $" + profit, C.gold, "center", 18);
      },
    },
    {
      title: "9 · The honest truth",
      text: "The book shows what's happening NOW — pressure and price move together in the same instant. But that's not a crystal ball: it describes the move, it doesn't predict the next one. (We measured it: R²≈0.33 same-moment, ≈0.00 next-moment.)",
      draw(t) {
        const cx = W / 2, cy = 120;
        // now: pressure + price move together
        const s = 0.5 + 0.5 * Math.sin(t * 2);
        label(cx - 150, 60, "NOW", C.fg, "center", 14);
        chip(cx - 230, cy - 18, 160, 36, "rgba(34,197,94," + (0.15 + 0.25 * s) + ")", "buy pressure ↑", null, C.bid);
        arrow(cx - 66, cy, cx - 20, cy, C.gold);
        chip(cx - 10, cy - 18, 110, 36, "rgba(212,165,71,.2)", "price ↑", null, C.gold);
        label(cx - 65, cy + 48, "same moment → linked (R²≈0.33)", C.muted, "center", 12);
        // next: unknown
        label(cx + 210, 60, "NEXT", C.fg, "center", 14);
        const q = 0.5 + 0.5 * Math.sin(t * 3);
        ctx.globalAlpha = 0.4 + 0.5 * q;
        label(cx + 210, cy + 6, "?", C.muted, "center", 40);
        ctx.globalAlpha = 1;
        label(cx + 210, cy + 48, "next move → ~random (R²≈0.00)", C.muted, "center", 12);
      },
    },
  ];

  function drawFrame(t) {
    ctx.clearRect(0, 0, W, H);
    ctx.fillStyle = C.bg;
    rrect(0, 0, W, H, 10);
    ctx.fill();
    steps[cur].draw(t);
  }

  function loop(now) {
    if (!active) return;
    const t = (now - start) / 1000;
    drawFrame(t);
    raf = requestAnimationFrame(loop);
  }

  function show(i) {
    cur = Math.max(0, Math.min(steps.length - 1, i));
    titleEl.textContent = steps[cur].title;
    textEl.textContent = steps[cur].text;
    prevBtn.disabled = cur === 0;
    nextBtn.textContent = cur === steps.length - 1 ? "Restart ↺" : "Next →";
    [...dotsEl.children].forEach((d, j) => d.classList.toggle("on", j === cur));
    start = performance.now();
    drawFrame(0);
  }

  function init() {
    cv = document.getElementById("tutStage");
    if (!cv) return;
    ctx = cv.getContext("2d");
    W = cv.width;
    H = cv.height;
    titleEl = document.getElementById("tutTitle");
    textEl = document.getElementById("tutText");
    dotsEl = document.getElementById("tutDots");
    prevBtn = document.getElementById("tutPrev");
    nextBtn = document.getElementById("tutNext");

    steps.forEach((_, i) => {
      const d = document.createElement("button");
      d.className = "tut-dot";
      d.addEventListener("click", () => show(i));
      dotsEl.appendChild(d);
    });
    prevBtn.addEventListener("click", () => show(cur - 1));
    nextBtn.addEventListener("click", () => show(cur === steps.length - 1 ? 0 : cur + 1));
    show(0);
  }

  function setActive(on) {
    active = on;
    if (on) {
      start = performance.now();
      raf = requestAnimationFrame(loop);
    } else if (raf) {
      cancelAnimationFrame(raf);
      raf = null;
    }
  }

  window.Tutorial = { init, setActive };
})();
