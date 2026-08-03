# Tutorial — learn the order book by being every player in it

Status: **SPEC + v1 implemented** (`web/learn.html`) · Author: Karthikeyan NG ·
Last updated: 2026-08-03

Companion documents:
- [`LEARN.md`](LEARN.md) — the prose course this page is the hands-on half of.
- [`DEMO-SPEC.md`](DEMO-SPEC.md) — the scripted README animation. That one is watched;
  this one is *done*.
- [`CONSOLE-SPEC.md`](CONSOLE-SPEC.md) — where a graduate of this page goes next.

---

## 1. The claim, and the two devices that earn it

Every order-book explainer on the internet is a diagram or a faked animation. This
page can be different for one reason only: the real matching engine runs in it.
That permits two devices no static tutorial has:

**The ladder assembles itself.** Chapter 1 opens on an *empty market* — no prices,
no chart, nothing — because a market is a list of intentions and the list starts
empty. The learner posts the first order and watches a price come into existence.
Sizes appear when depth is the lesson. Bars appear when depth needs comparing. The
tape appears at the first trade. By the final chapter the full professional ladder
is on screen and the learner understands every pixel, because they watched each one
arrive and used it.

**Engine-verified objectives.** Every chapter sets a task — "offer to sell,"
"get filled before the rival," "buy 8 without paying more than one level" — and the
page marks it complete only when the *book state proves it happened*. No "click
next to continue": the check polls the real engine. A tutorial with tests, in the
same spirit as this repository's executable runbooks.

Honesty rule inherited from everything else here: nothing on the page is mocked.
Every number is engine output; every scripted counterparty is real orders through
`engine.Process`. If a chapter claims "trades happen at the maker's price," the
learner has just watched the print equal the resting order's price, on a trade they
caused.

## 2. The chapters — each one a role

| # | Role | Concept | Objective (engine-checked) |
|---|---|---|---|
| 1 | The first seller | A price is a commitment; bid, ask, spread | Your ask rests; a bid arrives; a spread exists |
| 2 | The wall builder | Size and depth | Three-plus orders resting at distinct prices |
| 3 | The taker | Crossing the spread; maker price rule; the tape | You cause a trade; the print equals the maker's price |
| 4 | The queue jumper | Price-time priority | Round 1: same price, the rival fills first. Round 2: improve a tick, you fill first |
| 5 | The whale | Walking the book, slippage, depth as protection | Same 8-lot order against a thin book and a deep one; the measured difference is the lesson |
| 6 | The market maker | Live flow; earning the spread | Quote both sides of a running market; both fill; P&L in ticks shown |

Chapter 3 onward runs on a scripted reset (a "crowd" builds the book) so each
lesson is deterministic and isolated — same actions, same market, every visitor,
which is itself the engine's determinism doing pedagogy.

The finale names where each thread continues: the live console (signals,
surveillance, the spoof), LEARN.md for the prose depth, the repository for the
code. And it says plainly what the learner just used: the same engine, compiled to
WebAssembly.

## 3. Visual bar

Professional and data-driven, never cartoonish. Site tokens throughout
(`web/style.css` palette); numbers in the monospace stack; the learner's own
orders carry the same ● marker the console uses. Chapter 5's lesson is delivered
as a measured comparison table (average fill, worst fill, levels swept — thin
versus deep), because the data *is* the lesson. No mascots, no confetti, no
gamification chrome beyond the objective checkmark. Color is reinforcement only:
side is always also position and text (the same deutan rule as the console).

## 4. What the bridge needed

Nothing. The page drives the existing `cmd/obwasm` API — `obReset`, `obSubmit`,
`obCancel`, `obSnapshot`, `obOpenOrders`, `obStep` — with scripted counterparties
as ordinary submits under crowd identities. That the tutorial needed zero new
engine surface is the point of the seam.

## 5. Non-goals (v1)

- **No accounts, progress storage, or completion badges.** Refresh restarts; the
  chapters take minutes.
- **No mobile-first layout.** Same policy as the console: degrades acceptably, a
  ladder wants a desktop.
- **No strategy teaching.** Chapter 6 shows what market making *is*, then says the
  honest sentence: everything hard about it starts when the price moves while you
  wait. Where signals do and don't work is the research docs' job, not a
  beginner page's.
