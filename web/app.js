"use strict";

// Row element caches for keyed reconciliation (so size bars animate smoothly
// instead of being rebuilt each tick).
const asksRows = new Map();
const bidsRows = new Map();
let auto = null;

// --- boot the Go/WASM engine ---
async function boot() {
  const go = new Go();
  let result;
  try {
    result = await WebAssembly.instantiateStreaming(fetch("obook.wasm"), go.importObject);
  } catch (_) {
    // Fallback for servers that don't send application/wasm.
    const bytes = await (await fetch("obook.wasm")).arrayBuffer();
    result = await WebAssembly.instantiate(bytes, go.importObject);
  }
  go.run(result.instance); // runs the Go main (blocks in Go on select{}); returns here
  init();
}

function init() {
  seed();
  render();
  document.querySelector(".controls").addEventListener("click", onControl);
  document.getElementById("loading").classList.add("done");
}

// --- engine helpers ---
function sub(user, side, type, price, qty) {
  const res = JSON.parse(obSubmit(user, side, type, price, qty));
  if (res.trades && res.trades.length) addTrades(res.trades);
  return res;
}

function seed() {
  obReset("DEMO");
  [
    ["u1", "SELL", "LIMIT", "101.5", "4"],
    ["u2", "SELL", "LIMIT", "101.0", "3"],
    ["u3", "SELL", "LIMIT", "100.5", "5"],
    ["u4", "BUY", "LIMIT", "100.0", "5"],
    ["u5", "BUY", "LIMIT", "99.5", "3"],
    ["u6", "BUY", "LIMIT", "99.0", "6"],
  ].forEach((s) => obSubmit.apply(null, s));

  asksRows.clear();
  bidsRows.clear();
  document.getElementById("asks").innerHTML = "";
  document.getElementById("bids").innerHTML = "";
  document.getElementById("tape").innerHTML = "";
}

function rndSize() {
  return String(1 + Math.floor(Math.random() * 6));
}

function newUser() {
  return "t" + Math.floor(Math.random() * 1e6);
}

// --- controls ---
function onControl(e) {
  const act = e.target.dataset.act;
  if (!act) return;
  const mid = parseFloat(JSON.parse(obSnapshot(1)).mid) || 100;
  const u = newUser();
  switch (act) {
    case "buyLimit": sub(u, "BUY", "LIMIT", (mid - 0.5).toFixed(1), rndSize()); break;
    case "sellLimit": sub(u, "SELL", "LIMIT", (mid + 0.5).toFixed(1), rndSize()); break;
    case "buyMarket": sub(u, "BUY", "MARKET", "0", rndSize()); break;
    case "sellMarket": sub(u, "SELL", "MARKET", "0", rndSize()); break;
    case "reset": seed(); break;
    case "auto": toggleAuto(); return;
  }
  render();
}

function toggleAuto() {
  const btn = document.getElementById("autoBtn");
  if (auto) {
    clearInterval(auto);
    auto = null;
    btn.textContent = "▶ Auto flow";
    return;
  }
  btn.textContent = "⏸ Auto flow";
  auto = setInterval(() => {
    const mid = parseFloat(JSON.parse(obSnapshot(1)).mid) || 100;
    const u = newUser();
    if (Math.random() < 0.35) {
      sub(u, Math.random() < 0.5 ? "BUY" : "SELL", "MARKET", "0", rndSize());
    } else {
      const side = Math.random() < 0.5 ? "BUY" : "SELL";
      const off = Math.random() * 2;
      const px = side === "BUY" ? mid - off : mid + off;
      sub(u, side, "LIMIT", px.toFixed(1), rndSize());
    }
    render();
  }, 700);
}

// --- rendering ---
function render() {
  const snap = JSON.parse(obSnapshot(12));
  renderSide(document.getElementById("asks"), asksRows, snap.asks, true);
  renderSide(document.getElementById("bids"), bidsRows, snap.bids, false);
  document.getElementById("spread").textContent = "spread " + (snap.spread || "—");
  document.getElementById("mid").textContent = "mid " + (snap.mid || "—");
  document.getElementById("last").textContent = "last " + (snap.last_trade || "—");
  updateImbalance(snap);
}

function renderSide(container, rows, levels, isAsk) {
  const ordered = isAsk ? levels.slice().reverse() : levels;
  let maxSize = 1;
  for (const l of levels) maxSize = Math.max(maxSize, parseFloat(l.size));

  const present = new Set();
  ordered.forEach((lv, idx) => {
    present.add(lv.price);
    let row = rows.get(lv.price);
    if (!row) {
      row = document.createElement("div");
      row.className = "row";
      row.innerHTML = '<span class="px"></span><span class="sz"></span><span class="bar"></span>';
      rows.set(lv.price, row);
      container.appendChild(row);
      row.classList.add("flash");
      setTimeout(() => row.classList.remove("flash"), 500);
    }
    row.style.order = String(idx);
    row.querySelector(".px").textContent = lv.price;
    row.querySelector(".sz").textContent = lv.size;
    row.querySelector(".bar").style.width = (parseFloat(lv.size) / maxSize) * 100 + "%";
  });

  for (const [price, row] of rows) {
    if (!present.has(price)) {
      row.remove();
      rows.delete(price);
    }
  }
}

function updateImbalance(snap) {
  const b = snap.bids[0] ? parseFloat(snap.bids[0].size) : 0;
  const a = snap.asks[0] ? parseFloat(snap.asks[0].size) : 0;
  const imb = b + a ? (b - a) / (b + a) : 0;
  const bar = document.getElementById("imbBar");
  const w = Math.abs(imb) * 50;
  bar.style.width = w + "%";
  bar.style.left = imb >= 0 ? "50%" : 50 - w + "%";
  bar.style.background = imb >= 0 ? "var(--bid)" : "var(--ask)";
  document.getElementById("imbVal").textContent =
    imb.toFixed(2) + (imb > 0.02 ? " · buy pressure" : imb < -0.02 ? " · sell pressure" : "");
}

function addTrades(trades) {
  const tape = document.getElementById("tape");
  for (const tr of trades) {
    const el = document.createElement("div");
    const buy = tr.taker_side === "BUY";
    el.className = "t " + (buy ? "buy" : "sell");
    el.innerHTML =
      "<span>" + (buy ? "▲" : "▼") + " " + tr.price + "</span><span>" + tr.quantity + "</span>";
    tape.prepend(el);
  }
  while (tape.children.length > 60) tape.lastChild.remove();
}

boot();
