"use strict";

// Boots the Go matching engine (WASM) and powers two live illustrations:
//  1) a real order-book ladder in the "what is an order book" section, and
//  2) a step-by-step matching walkthrough in "how it works".
// Both run the actual engine — no faked data.

async function boot() {
  const go = new Go();
  let result;
  try {
    result = await WebAssembly.instantiateStreaming(fetch("obook.wasm"), go.importObject);
  } catch (_) {
    const bytes = await (await fetch("obook.wasm")).arrayBuffer();
    result = await WebAssembly.instantiate(bytes, go.importObject);
  }
  go.run(result.instance);
  init();
}

function init() {
  renderBasics();
  wireMatching();
  document.getElementById("loading").classList.add("done");
}

// ---------- shared ladder renderer ----------
function buildSide(el, levels, side, maxS, bestIdx, hits, animate) {
  el.className = "ob-side " + (side === "ask" ? "a" : "b");
  el.innerHTML = levels
    .map((l, i) => {
      const w = ((parseFloat(l.size) / maxS) * 100).toFixed(1);
      const cls = "ob-row" + (i === bestIdx ? " best" : "") + (hits && hits[l.price] ? " hit" : "");
      const barW = animate ? "0" : w;
      return (
        '<div class="' + cls + '"><span class="p">' + l.price + '</span><span class="s">' +
        l.size + '</span><span class="bar" data-w="' + w + '" style="width:' + barW + '%"></span></div>'
      );
    })
    .join("");
  if (animate) {
    requestAnimationFrame(() => el.querySelectorAll(".bar").forEach((b) => (b.style.width = b.dataset.w + "%")));
  }
}

function renderLadder(cfg) {
  const s = cfg.snap;
  if (!s.asks.length && !s.bids.length) return;
  const asksDisp = s.asks.slice().reverse(); // worst → best, so best ask sits by the spread
  const maxS = Math.max(1, ...s.asks.map((a) => +a.size), ...s.bids.map((b) => +b.size));
  buildSide(cfg.asksEl, asksDisp, "ask", maxS, asksDisp.length - 1, cfg.hits, cfg.animate);
  buildSide(cfg.bidsEl, s.bids, "bid", maxS, 0, cfg.hits, cfg.animate);
  if (cfg.spreadEl) cfg.spreadEl.textContent = "spread " + (s.spread || "—");
  if (cfg.midEl) cfg.midEl.textContent = "mid " + (s.mid || "—");
  if (cfg.lastEl) cfg.lastEl.textContent = "last " + (s.last_trade || "—");
}

function submitMany(list) {
  list.forEach((o) => obSubmit(o[0], o[1], o[2], String(o[3]), String(o[4])));
}

// ---------- 1) basics ladder (static, real snapshot) ----------
function renderBasics() {
  obReset("BOOK");
  submitMany([
    ["s1", "SELL", "LIMIT", 102, 5], ["s2", "SELL", "LIMIT", 101.5, 2], ["s3", "SELL", "LIMIT", 101, 3],
    ["b1", "BUY", "LIMIT", 100, 4], ["b2", "BUY", "LIMIT", 99.5, 3], ["b3", "BUY", "LIMIT", 99, 6],
  ]);
  renderLadder({
    asksEl: document.getElementById("ladderAsks"),
    bidsEl: document.getElementById("ladderBids"),
    spreadEl: document.getElementById("obSpread"),
    midEl: document.getElementById("obMid"),
    snap: JSON.parse(obSnapshot(8)),
    animate: true,
  });
}

// ---------- 2) matching walkthrough ----------
let mIdx = -1;

function seedMatching() {
  obReset("MATCH");
  submitMany([
    ["mm1", "SELL", "LIMIT", 100.5, 2], ["mm2", "SELL", "LIMIT", 101, 3], ["mm3", "SELL", "LIMIT", 101.5, 4],
    ["mm4", "BUY", "LIMIT", 100, 3], ["mm5", "BUY", "LIMIT", 99.5, 2], ["mm6", "BUY", "LIMIT", 99, 4],
  ]);
  document.getElementById("mTape").innerHTML = "";
}

function renderMatching(hits) {
  renderLadder({
    asksEl: document.getElementById("mAsks"),
    bidsEl: document.getElementById("mBids"),
    spreadEl: document.getElementById("mSpread"),
    lastEl: document.getElementById("mLast"),
    snap: JSON.parse(obSnapshot(8)),
    hits,
  });
}

function sendMarket(qty) {
  const res = JSON.parse(obSubmit("taker", "BUY", "MARKET", "0", String(qty)));
  const hits = {};
  const tape = document.getElementById("mTape");
  (res.trades || []).forEach((t) => {
    hits[t.price] = 1;
    const el = document.createElement("div");
    el.className = "t buy";
    el.innerHTML = "<span>▲ bought " + t.quantity + " @ " + t.price + "</span><span></span>";
    tape.prepend(el);
  });
  while (tape.children.length > 8) tape.lastChild.remove();
  return hits;
}

const mSteps = [
  {
    text: "Here's a book of resting orders — sellers (asks) on top, buyers (bids) below. Nobody has traded yet; these makers are just offering liquidity.",
    run: () => { seedMatching(); renderMatching(); },
  },
  {
    text: "A trader wants in now, so they send a MARKET BUY for 2. It takes the cheapest ask (the maker at the top) — a trade prints, at the maker's price.",
    run: () => renderMatching(sendMarket(2)),
  },
  {
    text: "A bigger MARKET BUY for 6 can't fill at one level — it walks up through several asks. Each deeper level is pricier, so the average fill is worse. That's slippage.",
    run: () => renderMatching(sendMarket(6)),
  },
  {
    text: "The best ask has climbed as liquidity was consumed — the book just discovered a new price. That's the whole job: turn resting intentions into trades and prices. ↻ Restart to replay.",
    run: () => renderMatching(),
  },
];

function wireMatching() {
  const btn = document.getElementById("stepBtn");
  const reset = document.getElementById("stepReset");
  const txt = document.getElementById("stepText");
  seedMatching();
  renderMatching();

  btn.addEventListener("click", () => {
    mIdx = mIdx >= mSteps.length - 1 ? 0 : mIdx + 1;
    mSteps[mIdx].run();
    txt.textContent = mSteps[mIdx].text;
    btn.textContent = mIdx >= mSteps.length - 1 ? "↻ Restart" : "Next step →";
  });
  reset.addEventListener("click", () => {
    mIdx = -1;
    seedMatching();
    renderMatching();
    txt.textContent = "Press Start to build a book, then walk through a trade step by step.";
    btn.textContent = "▶ Start";
  });
}

boot();
