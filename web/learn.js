// learn.js — the interactive tutorial (docs/TUTORIAL-SPEC.md). Two devices:
// the ladder assembles as concepts arrive (body classes gate its elements), and
// every objective is verified against the real engine's book state — no "click
// next to continue", the check polls the engine. All counterparties ("crowd",
// "rival") are ordinary orders through the same obSubmit the learner uses.
(async function () {
  const $ = (id) => document.getElementById(id);
  try {
    const go = new Go();
    let result;
    try {
      result = await WebAssembly.instantiateStreaming(fetch("obook.wasm"), go.importObject);
    } catch {
      const bytes = await (await fetch("obook.wasm")).arrayBuffer();
      result = await WebAssembly.instantiate(bytes, go.importObject);
    }
    go.run(result.instance);
  } catch (err) {
    $("boot").textContent = "The engine failed to load (" + err + "). This page needs WebAssembly — a current Firefox, Chrome, Safari or Edge.";
    return;
  }
  $("boot").hidden = true;
  $("ui").hidden = false;

  // ---- engine helpers -------------------------------------------------------
  const submit = (user, side, type, price, qty) => JSON.parse(obSubmit(user, side, type, String(price), String(qty)));
  const snap = () => JSON.parse(obSnapshot(8));
  const own = (u) => JSON.parse(obOpenOrders(u || "you")).orders;
  const fmt = (v) => (+v).toFixed(2);

  // ---- market panel ---------------------------------------------------------
  let flashPx = null;
  let tapeCount = 0;
  function levelClasses(on) { // which ladder elements exist yet
    for (const c of ["lv-sides", "lv-sizes", "lv-bars", "lv-mid", "lv-tape"]) {
      document.body.classList.toggle(c, on.includes(c));
    }
  }
  function renderMarket(note) {
    const s = snap();
    $("mkt-note").textContent = note || (s.bids.length + s.asks.length ? (s.bids.length + s.asks.length) + " levels" : "empty");
    const el = $("ladder");
    if (!s.bids.length && !s.asks.length) {
      el.innerHTML = '<div class="void">No prices. No chart. Nothing to see.<br/>A market is a <b>list of intentions</b> — and this list is empty.<br/>Someone has to go first.</div>';
      return s;
    }
    const yours = new Set(own().map((o) => fmt(o.price)));
    const maxSz = Math.max(1e-9, ...s.asks.map((l) => +l.size), ...s.bids.map((l) => +l.size));
    const row = (cls, l) => {
      const flash = flashPx === l.price ? " flash" : "";
      const dot = yours.has(fmt(l.price)) ? '<span class="own-dot" title="your order rests here">●</span>' : "";
      return `<div class="lrow ${cls}${flash}"><span class="bar" style="width:${(88 * l.size) / maxSz}%"></span>` +
        `<span class="p">${dot}${fmt(l.price)}</span><span class="q">${(+l.size).toFixed(3)}</span></div>`;
    };
    const rows = [];
    rows.push('<div class="lsec">ASKS — offers to sell (lowest first is best)</div>');
    for (const l of [...s.asks].reverse()) rows.push(row("ask", l));
    const spread = s.spread ? `spread <b class="hl">${fmt(s.spread)}</b>` : "";
    rows.push(`<div class="lmid"><span>mid <b>${s.mid ? fmt(s.mid) : "—"}</b></span><span>${spread}</span><span>last <b>${s.last_trade !== "0" ? fmt(s.last_trade) : "—"}</b></span></div>`);
    for (const l of s.bids) rows.push(row("bid", l));
    rows.push('<div class="lsec">BIDS — offers to buy (highest first is best)</div>');
    el.innerHTML = rows.join("");
    flashPx = null;
    return s;
  }
  function printTrades(trades) {
    if (!trades || !trades.length) return;
    document.body.classList.add("lv-tape");
    for (const t of trades) {
      const buy = t.taker_side === "BUY";
      const div = document.createElement("div");
      div.className = "trow " + (buy ? "b" : "s");
      div.innerHTML = `<span class="side">${buy ? "▲ B" : "▼ S"}</span><span class="px">${fmt(t.price)}</span><span class="qy">${t.quantity}</span>`;
      $("tape").prepend(div);
      tapeCount++;
      flashPx = t.price;
    }
    while ($("tape").children.length > 24) $("tape").lastChild.remove();
  }

  // ---- lesson chrome --------------------------------------------------------
  const CHAPTERS = [];
  let current = 0;
  let chapterState = {};
  function renderRail() {
    $("rail").innerHTML = CHAPTERS.map((c, i) => {
      const cls = i < current ? "done" : i === current ? "cur" : "";
      const mark = i < current ? "✓" : i + 1;
      return `<span class="st ${cls}"><span class="n">${mark}</span>${c.title}</span>`;
    }).join("");
  }
  function setGoal(met) {
    $("goal").classList.toggle("met", met);
    $("btn-next").disabled = !met;
  }
  function controlsErr(msg) { const e = $("ctl-err"); if (e) e.textContent = msg || ""; }

  function startChapter(i) {
    current = i;
    const c = CHAPTERS[i];
    chapterState = {};
    tapeCount = 0;
    $("tape").innerHTML = "";
    levelClasses(c.levels);
    c.setup();
    $("narrative").innerHTML = `<div class="role">${c.role}</div><h2>You are ${c.title.toLowerCase()}</h2>` + c.prose;
    $("goal-text").textContent = c.goal;
    $("controls").innerHTML = c.controls + '<div class="ctl-err" id="ctl-err"></div>';
    c.wire();
    setGoal(false);
    renderRail();
    renderMarket(c.note);
  }
  $("btn-next").addEventListener("click", () => {
    if (current + 1 < CHAPTERS.length) startChapter(current + 1);
  });
  function appendLesson(html) { $("narrative").insertAdjacentHTML("beforeend", html); }

  // ---- chapter 1: the first seller -----------------------------------------
  CHAPTERS.push({
    title: "The first seller",
    role: "Chapter 1 of 6",
    levels: ["lv-sides"],
    note: "empty",
    prose: `
      <p>Forget charts. A market starts as an empty list, and prices do not exist until
      someone commits to one. Not "I think it's worth about 101" — a commitment:
      <span class="datum">I will sell 5 at 101.00, to anyone, until I say otherwise.</span></p>
      <p>Make that commitment. The moment you do, this market has its first price — and you are its entire sell side.</p>`,
    goal: "Rest an offer to sell. Then hold — someone else is coming.",
    controls: `
      <div class="ctl">
        <label for="c1p">price</label><input id="c1p" class="cinp" value="101.00" inputmode="decimal" />
        <label for="c1q">quantity</label><input id="c1q" class="cinp" value="5" inputmode="decimal" />
        <button id="c1go" class="cbtn primary">Offer to sell</button>
      </div>`,
    setup() { obReset("LEARN"); },
    wire() {
      $("c1go").addEventListener("click", () => {
        const r = submit("you", "SELL", "LIMIT", $("c1p").value, $("c1q").value);
        if (r.error) { controlsErr(r.error); return; }
        controlsErr("");
        chapterState.asked = true;
        appendLesson(`<p>There it is — <span class="datum">${fmt($("c1p").value)}</span>, marked ● because it is yours.
          That number is now a price for one reason only: you are committed to honoring it.</p>`);
        setTimeout(() => {
          submit("crowd", "BUY", "LIMIT", 99.00, 5);
          chapterState.bidArrived = true;
          appendLesson(`<p>And someone answered — a buyer, resting at <span class="datum">99.00</span>. Look at the gap between
            you: the buyer will pay 99, you want 101. That gap is the <b>spread</b>: the size of the market's
            disagreement. Nothing trades until somebody crosses it.</p>`);
        }, 1400);
      }, { once: true });
    },
    check() {
      const s = snap();
      return chapterState.asked && s.asks.length > 0 && s.bids.length > 0;
    },
  });

  // ---- chapter 2: the wall builder -----------------------------------------
  CHAPTERS.push({
    title: "The wall builder",
    role: "Chapter 2 of 6",
    levels: ["lv-sides", "lv-sizes", "lv-bars", "lv-mid"],
    prose: `
      <p>Two new columns just appeared: <b>size</b>, and a bar to compare sizes at a glance. Because a price
      without size is just an opinion — what matters is how much conviction rests at each level.</p>
      <p>Size stacked in the book is called <b>depth</b>, and depth is the market's shock absorber:
      the more resting at each price, the more it takes to move the price. Build some.</p>`,
    goal: "Rest at least three more orders — either side, any prices near the market.",
    controls: `
      <div class="ctl">
        <select id="c2s" class="cinp" aria-label="Side"><option value="BUY">Buy</option><option value="SELL">Sell</option></select>
        <label for="c2p">price</label><input id="c2p" class="cinp" value="100.50" inputmode="decimal" />
        <label for="c2q">quantity</label><input id="c2q" class="cinp" value="3" inputmode="decimal" />
        <button id="c2go" class="cbtn primary">Place order</button>
      </div>`,
    setup() {},
    wire() {
      $("c2go").addEventListener("click", () => {
        const r = submit("you", $("c2s").value, "LIMIT", $("c2p").value, $("c2q").value);
        if (r.error) { controlsErr(r.error); return; }
        controlsErr("");
        if (r.trades && r.trades.length) {
          printTrades(r.trades);
          appendLesson(`<p>That one <i>traded</i> instead of resting — you priced it across the spread, so the engine
            matched it instantly. That is the subject of the next chapter. For now: an order rests only
            if its price does not already have a willing counterparty.</p>`);
        }
      });
    },
    check() { return own().length >= 4; },
  });

  // ---- chapter 3: the taker -------------------------------------------------
  CHAPTERS.push({
    title: "The taker",
    role: "Chapter 3 of 6",
    levels: ["lv-sides", "lv-sizes", "lv-bars", "lv-mid"],
    prose: `
      <p>Fresh market, and this time a crowd got here before you — resting orders on both sides,
      none of them yours. Everything on screen is someone else's patience.</p>
      <p>You are in a hurry. A <b>market order</b> does not name a price: it takes the best one
      available, right now. Send one, and watch two things — <i>where</i> the trade prints, and what
      appears under the ladder when it does.</p>`,
    goal: "Buy 3 at market, and cause the first trade this market has ever seen.",
    controls: `
      <div class="ctl">
        <button id="c3go" class="cbtn primary">Buy 3 at market</button>
      </div>`,
    setup() {
      obReset("LEARN");
      for (const [p, q] of [[101.0, 4], [101.5, 3], [102.0, 5]]) submit("crowd", "SELL", "LIMIT", p, q);
      for (const [p, q] of [[99.0, 4], [98.5, 3], [98.0, 5]]) submit("crowd", "BUY", "LIMIT", p, q);
    },
    wire() {
      $("c3go").addEventListener("click", () => {
        const r = submit("you", "BUY", "MARKET", 0, 3);
        if (r.error) { controlsErr(r.error); return; }
        printTrades(r.trades);
        chapterState.traded = r.trades && r.trades.length > 0;
        if (chapterState.traded) {
          const px = fmt(r.trades[0].price);
          appendLesson(`<p>Your trade printed at <span class="datum">${px}</span> — the <i>seller's</i> price, exactly.
            Not the midpoint, not a compromise. The rule in every real venue: <b>trades happen at the maker's
            price</b>. The maker set the terms by waiting; you paid the spread for speed.</p>
            <p>And the <b>tape</b> appeared below the ladder — the permanent record of every trade. The book is
            what people <i>intend</i>; the tape is what actually <i>happened</i>.</p>`);
        }
      }, { once: true });
    },
    check() { return !!chapterState.traded; },
  });

  // ---- chapter 4: the queue jumper -----------------------------------------
  CHAPTERS.push({
    title: "The queue jumper",
    role: "Chapter 4 of 6",
    levels: ["lv-sides", "lv-sizes", "lv-bars", "lv-mid", "lv-tape"],
    prose: `
      <p>One seller is already resting: a rival, 5 at <span class="datum">101.00</span>. Suppose you also want
      to sell at 101.00. Who gets filled when a buyer shows up — you, or them?</p>
      <p>Run the experiment. Join at the same price, then send in a buyer, and let the engine answer.</p>`,
    goal: "Get one of your orders filled before the rival's.",
    controls: `
      <div class="ctl">
        <button id="c4a" class="cbtn primary">1 · Join at 101.00</button>
        <button id="c4b" class="cbtn" disabled>2 · Send a buyer for 5</button>
        <button id="c4c" class="cbtn" disabled>3 · Improve to 100.99</button>
        <button id="c4d" class="cbtn" disabled>4 · Send another buyer</button>
      </div>`,
    setup() {
      obReset("LEARN");
      submit("rival", "SELL", "LIMIT", 101.0, 5);
      submit("crowd", "BUY", "LIMIT", 99.0, 6);
    },
    wire() {
      $("c4a").addEventListener("click", () => {
        const r = submit("you", "SELL", "LIMIT", 101.0, 5);
        if (r.error) { controlsErr(r.error); return; }
        $("c4a").disabled = true; $("c4b").disabled = false;
      }, { once: true });
      $("c4b").addEventListener("click", () => {
        const r = submit("buyer", "BUY", "MARKET", 0, 3);
        printTrades(r.trades);
        const rivalLeft = own("rival").reduce((a, o) => a + (+o.qty), 0);
        const youLeft = own("you").filter((o) => o.side === "SELL").reduce((a, o) => a + (+o.qty), 0);
        appendLesson(`<p>The buyer took 3 — every lot from the <i>rival</i>. Rival has <span class="datum">${rivalLeft}</span>
          left; you still hold <span class="datum">${youLeft}</span>, untouched. Same price — but the rival was there
          <i>first</i>. That is <b>time priority</b>: at equal prices, first come, first served, and you are at
          the back of the queue.</p>`);
        $("c4b").disabled = true; $("c4c").disabled = false;
      }, { once: true });
      $("c4c").addEventListener("click", () => {
        const mine = own("you").filter((o) => o.side === "SELL");
        for (const o of mine) obCancel(o.id, "you");
        const r = submit("you", "SELL", "LIMIT", 100.99, 5);
        if (r.error) { controlsErr(r.error); return; }
        appendLesson(`<p>You cancelled and re-quoted one tick better: <span class="datum">100.99</span>. You are now
          alone at the best price — the front of a queue of one. That is <b>price priority</b>, and it outranks
          time: a better price goes first, always.</p>`);
        $("c4c").disabled = true; $("c4d").disabled = false;
      }, { once: true });
      $("c4d").addEventListener("click", () => {
        const r = submit("buyer", "BUY", "MARKET", 0, 3);
        printTrades(r.trades);
        if (r.trades && r.trades.length) {
          chapterState.jumped = true;
          appendLesson(`<p>This buyer filled at <span class="datum">${r.trades[0].price}</span> — against <i>you</i>, not
            the rival waiting at 101.00. Price buys priority; time only breaks ties. Every queue-position battle in
            real markets — and a lot of the technology arms race — is these two rules and nothing else.</p>`);
        }
        $("c4d").disabled = true;
      }, { once: true });
    },
    check() { return !!chapterState.jumped; },
  });

  // ---- chapter 5: the whale -------------------------------------------------
  CHAPTERS.push({
    title: "The whale",
    role: "Chapter 5 of 6",
    levels: ["lv-sides", "lv-sizes", "lv-bars", "lv-mid", "lv-tape"],
    prose: `
      <p>A thin market: a little size at each price. You need 8 — more than the best level holds.
      A market order does not stop when the best price runs out. It keeps going, level by level,
      paying more at each step. Traders call it <b>walking the book</b>; the cost is <b>slippage</b>.</p>
      <p>Buy 8 and read your fills. Then the same order again, against a deep book, and compare.</p>`,
    goal: "Run the same 8-lot order against a thin book and a deep one.",
    controls: `
      <div class="ctl">
        <button id="c5a" class="cbtn primary">Buy 8 — thin book</button>
        <button id="c5b" class="cbtn" disabled>Buy 8 — deep book</button>
      </div>
      <div id="c5table"></div>`,
    setup() {
      obReset("LEARN");
      for (const [p, q] of [[101.0, 3], [102.0, 3], [103.0, 4], [104.0, 6]]) submit("crowd", "SELL", "LIMIT", p, q);
      submit("crowd", "BUY", "LIMIT", 99.0, 8);
    },
    wire() {
      const stats = (trades) => {
        let qty = 0, cost = 0;
        const prices = new Set();
        for (const t of trades) { qty += +t.quantity; cost += +t.price * +t.quantity; prices.add(t.price); }
        return { avg: cost / qty, worst: Math.max(...trades.map((t) => +t.price)), levels: prices.size };
      };
      $("c5a").addEventListener("click", () => {
        const r = submit("you", "BUY", "MARKET", 0, 8);
        printTrades(r.trades);
        chapterState.thin = stats(r.trades);
        appendLesson(`<p>Eight bought — across <span class="datum">${chapterState.thin.levels}</span> price levels. Your first
          fill was 101.00; your worst was <span class="datum">${fmt(chapterState.thin.worst)}</span>. Your own urgency
          moved the price against you, mid-order.</p>`);
        $("c5a").disabled = true;
        obReset("LEARN");
        for (const [p, q] of [[101.0, 20], [102.0, 20], [103.0, 20]]) submit("crowd", "SELL", "LIMIT", p, q);
        submit("crowd", "BUY", "LIMIT", 99.0, 20);
        renderMarket();
        appendLesson(`<p>Same market, one change: <b>depth</b>. Twenty resting at every level now. Send the identical order.</p>`);
        $("c5b").disabled = false;
      }, { once: true });
      $("c5b").addEventListener("click", () => {
        const r = submit("you", "BUY", "MARKET", 0, 8);
        printTrades(r.trades);
        chapterState.deep = stats(r.trades);
        const t = chapterState.thin, d = chapterState.deep;
        $("c5table").innerHTML = `<table class="cmp">
          <tr><th>same 8-lot buy</th><th>thin book</th><th>deep book</th></tr>
          <tr><td>average fill</td><td>${fmt(t.avg)}</td><td>${fmt(d.avg)}</td></tr>
          <tr><td>worst fill</td><td>${fmt(t.worst)}</td><td>${fmt(d.worst)}</td></tr>
          <tr><td>levels swept</td><td>${t.levels}</td><td>${d.levels}</td></tr></table>`;
        appendLesson(`<p>Identical order, <span class="datum">${fmt(t.avg - d.avg)}</span> per unit cheaper — because depth
          absorbed it. This is why depth is protection, why large orders get sliced into small ones, and why the
          shape of the book matters more than the last price printed on the tape.</p>`);
        $("c5b").disabled = true;
      }, { once: true });
    },
    check() { return !!(chapterState.thin && chapterState.deep); },
  });

  // ---- chapter 6: the market maker -----------------------------------------
  CHAPTERS.push({
    title: "The market maker",
    role: "Chapter 6 of 6",
    levels: ["lv-sides", "lv-sizes", "lv-bars", "lv-mid", "lv-tape"],
    note: "live",
    prose: `
      <p>Everything so far was a still life. This is a <b>live market</b> — dozens of simulated traders
      posting, taking, and cancelling against the same engine, several times a second. Every element on
      this screen is one you built or used in the last five chapters.</p>
      <p>One role remains: the one who quotes <i>both</i> sides. Rest a bid below the price and an ask above
      it, and if the flow fills both, you have bought low and sold high without ever guessing the direction.
      That is <b>market making</b> — being paid the spread for supplying patience.</p>`,
    goal: "Quote both sides of the live market and get both filled.",
    controls: `
      <div class="ctl">
        <label for="c6b">bid</label><input id="c6b" class="cinp" inputmode="decimal" />
        <label for="c6a">ask</label><input id="c6a" class="cinp" inputmode="decimal" />
        <button id="c6go" class="cbtn primary">Quote both sides (2 each)</button>
      </div>`,
    setup() {
      obReset(7);
      for (let i = 0; i < 40; i++) obStep(1);
      chapterState.timer = setInterval(() => {
        if (current !== 5) { clearInterval(chapterState.timer); return; }
        const out = JSON.parse(obStep(2));
        printTrades(out.trades);
      }, 160);
      setTimeout(() => {
        const st = snap();
        const mid = st.mid ? +st.mid : 100;
        $("c6b").value = fmt(mid - 0.01);
        $("c6a").value = fmt(mid + 0.01);
      }, 100);
    },
    wire() {
      $("c6go").addEventListener("click", () => {
        chapterState.fills = chapterState.fills || { BUY: null, SELL: null };
        if (chapterState.quoted) {
          // A re-quote: credit any side that fully filled, pull what still
          // rests, and chase the market to fresh prices.
          const mine = own();
          for (const side of ["BUY", "SELL"]) {
            const rest = mine.filter((o) => o.side === side).reduce((a, o) => a + +o.qty, 0);
            if (chapterState.quoted[side] != null && chapterState.fills[side] == null && rest < 2) {
              chapterState.fills[side] = chapterState.quoted[side]; // some of it traded — that is a fill
            }
          }
          for (const o of mine) obCancel(o.id, "you");
          const st = snap();
          const mid = st.mid ? +st.mid : 100;
          $("c6b").value = fmt(mid - 0.01);
          $("c6a").value = fmt(mid + 0.01);
        }
        const bid = +$("c6b").value, ask = +$("c6a").value;
        const b = chapterState.fills.BUY == null ? submit("you", "BUY", "LIMIT", bid, 2) : {};
        const a = chapterState.fills.SELL == null ? submit("you", "SELL", "LIMIT", ask, 2) : {};
        if (b.error || a.error) { controlsErr(b.error || a.error); return; }
        controlsErr("");
        chapterState.quoted = {
          BUY: chapterState.fills.BUY == null ? bid : null,
          SELL: chapterState.fills.SELL == null ? ask : null,
        };
        if (!chapterState.announced) {
          chapterState.announced = true;
          appendLesson(`<p>Both quotes are resting (marked ●). Now you wait — which is the whole job. And if the
            price drifts away from a quote, press the button again to <b>re-quote</b> around the new price.
            Chasing the market is also the job.</p>`);
          $("c6go").textContent = "Re-quote around the market";
        }
      });
    },
    check() {
      if (chapterState.done) return true;
      if (!chapterState.quoted) return false;
      chapterState.fills = chapterState.fills || { BUY: null, SELL: null };
      const mine = own();
      for (const side of ["BUY", "SELL"]) {
        const rest = mine.filter((o) => o.side === side).reduce((a, o) => a + +o.qty, 0);
        if (chapterState.quoted[side] != null && chapterState.fills[side] == null && rest < 2) {
          chapterState.fills[side] = chapterState.quoted[side]; // filled (fully or in part), at your price
        }
      }
      if (chapterState.fills.BUY != null && chapterState.fills.SELL != null) {
        chapterState.done = true;
        const earned = chapterState.fills.SELL - chapterState.fills.BUY;
        const verdict = earned >= 0
          ? `<span class="datum">+${earned.toFixed(2)}</span> per unit, earned from the spread, direction never guessed.`
          : `<span class="datum">${earned.toFixed(2)}</span> per unit — a loss. The price moved through your quotes between
             fills, and you paid for standing still. Managing exactly that is the market maker's whole day.`;
        appendLesson(`<p>Both sides filled. You bought at <span class="datum">${fmt(chapterState.fills.BUY)}</span> and sold at
          <span class="datum">${fmt(chapterState.fills.SELL)}</span>: ${verdict} The honest footnote every market maker
          knows: everything hard about this job is the price moving while you wait.</p>
          <p class="fin">That is the order book — every piece of it, used with your own hands, on a real matching engine.
          Where each thread continues: the <a href="console.html">live console</a> (order-flow signals, and a market
          surveillance system you can try to fool), the prose course
          <a href="https://github.com/intrepidkarthi/orderbook/blob/main/docs/LEARN.md">LEARN.md</a>, and the
          <a href="https://github.com/intrepidkarthi/orderbook">engine itself</a>.</p>`);
        $("btn-next").textContent = "Open the live console →";
        $("btn-next").onclick = () => { location.href = "console.html"; };
      }
      return !!chapterState.done;
    },
  });

  // ---- run ------------------------------------------------------------------
  setInterval(() => {
    const c = CHAPTERS[current];
    renderMarket(c.note === "live" ? "live" : undefined);
    if (!$("btn-next").disabled) return;
    if (c.check()) setGoal(true);
  }, 300);
  startChapter(0);
})();
