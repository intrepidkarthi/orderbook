//go:build js && wasm

// Command obwasm compiles the matching engine to WebAssembly and exposes a small
// JSON API to JavaScript, so the browser demo and the live console run the *real*
// engine rather than a reimplementation (docs/DEMO-SPEC.md §3, docs/CONSOLE-SPEC.md).
//
// The engine works in integer ticks/lots; this bridge stays decimal-facing for
// JavaScript, converting at an Instrument boundary (cent ticks, milli lots) so
// the front-end keeps sending and receiving human decimals.
//
// Everything the console shows is computed HERE, by the shipping library code —
// sim.NoiseTrader drives the flow, pkg/signals computes OFI/CVD/imbalance/λ,
// pkg/surveillance watches every event. The page is a renderer; if a panel lies,
// the library lies.
//
// Build:  GOOS=js GOARCH=wasm go build -o web/obook.wasm ./cmd/obwasm
//
// JS globals installed: obReset(seedOrSymbol), obSubmit(user,side,type,price,qty),
// obSnapshot(depth), obStep(n), obSignals(), obAlerts(since), obCancel(id,user),
// obSpoof(), obFlood(), obOpenOrders(user). Each returns a JSON string.
package main

import (
	"encoding/json"
	"math/rand"
	"syscall/js"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/signals"
	"github.com/intrepidkarthi/orderbook/pkg/sim"
	"github.com/intrepidkarthi/orderbook/pkg/surveillance"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

const (
	bookDepth    = 10
	initialTicks = 10000 // $100.00 at cent ticks
	lambdaWindow = 240   // rolling step-pairs the λ fit sees
	lambdaMinN   = 30    // below this the fit is noise; the panel says "warming up"

	// The spoof the console lets a visitor run: layered size far beyond anything
	// the noise flow posts, pulled a few steps later — inside the detector's
	// lifetime window by construction, so the alert is the shipping detector
	// firing on real events, not a scripted outcome.
	spoofLayers   = 3
	spoofSize     = 8000 // lots (8.0 units); noise flow tops out at 2.0
	spoofMinSize  = 5000 // detector threshold: noise flow can never trip it
	spoofLifetime = 500  // seq window for "cancelled soon after placement"
	spoofSteps    = 3    // auto-cancel this many steps after placement
	spoofUser     = "spoofer"

	// The flood: a burst of far-from-touch IOC placements that never rest and
	// never fill — quote stuffing's signature, and exactly what an
	// order-to-trade ratio scores. Tuned so the burst trips the detector in one
	// press while fifty noise identities, each placing a handful of orders per
	// window and trading a third of them, never do.
	floodOrders   = 30
	floodUser     = "flooder"
	otrWindow     = 600 // event seqs
	otrMinOrders  = 25
	otrMaxRatio   = 15.0
)

// console is the whole market: the engine, the agents that trade against it,
// and the signal/surveillance state fed by everything that happens.
type console struct {
	inst   types.Instrument
	engine *matching.Engine
	rng    *rand.Rand
	agents []sim.Agent
	step   int
	seq    uint64

	ofi     *signals.OFI
	cvd     *signals.CVD
	monitor *surveillance.Monitor

	flows   []float64 // per-step signed aggressive flow (lots)
	dprices []float64 // per-step mid change (ticks), paired with flows

	spoofIDs []int64
	spoofAt  int
}

var c *console

// demoInstrument prices to the cent and sizes to the milli-unit — fine grained
// enough for anything the demo UI sends, while staying exact integers inside.
func demoInstrument(symbol string) types.Instrument {
	return types.NewInstrument(symbol, decimal.RequireFromString("0.01"), decimal.RequireFromString("0.001"))
}

func newConsole(symbol string, seed int64) *console {
	nt := sim.DefaultNoiseTrader("flow")
	nt.MinSize, nt.MaxSize = 100, 2000 // 0.1–2.0 units at milli lots
	nt.MaxOffsetTicks = 6
	return &console{
		inst:   demoInstrument(symbol),
		engine: matching.NewEngine(matching.DefaultConfig(symbol)),
		rng:    rand.New(rand.NewSource(seed)),
		agents: []sim.Agent{nt},
		ofi:    signals.NewOFI(),
		cvd:    signals.NewCVD(),
		monitor: surveillance.NewMonitor(
			surveillance.NewSpoofDetector(surveillance.SpoofConfig{
				MinSize: spoofMinSize, MaxLifetime: spoofLifetime,
			}),
			surveillance.NewOTRDetector(surveillance.OTRConfig{
				Window: otrWindow, MinOrders: otrMinOrders, MaxRatio: otrMaxRatio,
			}),
		),
	}
}

func main() {
	c = newConsole("DEMO", 1)
	js.Global().Set("obReset", js.FuncOf(reset))
	js.Global().Set("obSubmit", js.FuncOf(submit))
	js.Global().Set("obSnapshot", js.FuncOf(snapshot))
	js.Global().Set("obStep", js.FuncOf(stepN))
	js.Global().Set("obSignals", js.FuncOf(signalsOut))
	js.Global().Set("obAlerts", js.FuncOf(alertsOut))
	js.Global().Set("obCancel", js.FuncOf(cancel))
	js.Global().Set("obSpoof", js.FuncOf(spoof))
	js.Global().Set("obFlood", js.FuncOf(flood))
	js.Global().Set("obOpenOrders", js.FuncOf(openOrders))
	select {} // keep the Go runtime alive for callbacks
}

// reset(seedOrSymbol) — a number reseeds the simulated market (same seed, same
// market, every time — determinism demonstrated rather than claimed); a string
// keeps the older demo-page behavior of naming the symbol.
func reset(_ js.Value, args []js.Value) any {
	symbol, seed := "DEMO", int64(1)
	if len(args) > 0 {
		switch args[0].Type() {
		case js.TypeString:
			symbol = args[0].String()
		case js.TypeNumber:
			seed = int64(args[0].Int())
		}
	}
	c = newConsole(symbol, seed)
	return `{"ok":true}`
}

// observe routes one processed order through the same path everything takes:
// surveillance sees the placement and every print, CVD accumulates the trades.
// A visitor's order and a noise agent's order are indistinguishable here, which
// is what makes the spoof demo honest.
func (c *console) observe(o *types.Order, res *matching.MatchResult) []tradeOut {
	c.seq++
	c.monitor.Observe(surveillance.Event{
		Kind: surveillance.OrderPlaced, Seq: c.seq, UserID: o.UserID,
		OrderID: o.ID, Side: o.Side, Price: o.Price, Quantity: o.Quantity,
	})
	c.cvd.Observe(res.Trades)
	out := make([]tradeOut, 0, len(res.Trades))
	for _, tr := range res.Trades {
		c.seq++
		taker := tr.BuyerUserID
		if tr.TakerSide == types.SideSell {
			taker = tr.SellerUserID
		}
		c.monitor.Observe(surveillance.Event{
			Kind: surveillance.Trade, Seq: c.seq, UserID: taker,
			Side: tr.TakerSide, Price: tr.Price, Quantity: tr.Quantity,
			MakerOrderID: tr.MakerOrderID, TakerOrderID: tr.TakerOrderID,
		})
		out = append(out, tradeOut{
			Price:    c.inst.TicksToPrice(tr.Price).String(),
			Quantity: c.inst.LotsToQty(tr.Quantity).String(),
			Taker:    string(tr.TakerSide),
		})
	}
	return out
}

func (c *console) cancelObserved(orderID int64, user string) error {
	o, err := c.engine.Cancel(orderID, user)
	if err != nil {
		return err
	}
	c.seq++
	c.monitor.Observe(surveillance.Event{
		Kind: surveillance.OrderCancelled, Seq: c.seq, UserID: user,
		OrderID: o.ID, Side: o.Side, Price: o.Price, Quantity: o.Quantity,
	})
	return nil
}

func midTicks(s *orderbook.Snapshot) (float64, bool) {
	if len(s.Bids) == 0 || len(s.Asks) == 0 {
		return 0, false
	}
	return (float64(s.Bids[0].Price) + float64(s.Asks[0].Price)) / 2, true
}

// stepOnce advances the market one simulation step: pending spoofs age out,
// agents act on the current view, and the signal state absorbs what happened.
func (c *console) stepOnce() []tradeOut {
	if c.spoofIDs != nil && c.step-c.spoofAt >= spoofSteps {
		for _, id := range c.spoofIDs {
			_ = c.cancelObserved(id, spoofUser) // already-gone is fine; the detector only sees real cancels
		}
		c.spoofIDs = nil
	}

	pre := c.engine.Snapshot(bookDepth)
	ref := int64(initialTicks)
	if m, ok := midTicks(pre); ok {
		ref = int64(m)
	} else if pre.LastTradePrice > 0 {
		ref = pre.LastTradePrice
	}
	view := sim.View{
		Symbol: c.inst.Symbol, Step: c.step, Snapshot: pre,
		Ref: ref, HasBook: len(pre.Bids) > 0 || len(pre.Asks) > 0,
	}

	var trades []tradeOut
	stepFlow := int64(0)
	for _, a := range c.agents {
		for _, o := range a.Act(view, c.rng) {
			res := c.engine.Process(o)
			stepFlow += signals.SignedFlow(res.Trades)
			trades = append(trades, c.observe(o, res)...)
		}
	}

	post := c.engine.Snapshot(bookDepth)
	c.ofi.Observe(post)
	if pm, ok := midTicks(pre); ok {
		if cm, ok2 := midTicks(post); ok2 {
			c.flows = append(c.flows, float64(stepFlow))
			c.dprices = append(c.dprices, cm-pm)
			if len(c.flows) > lambdaWindow {
				c.flows = c.flows[1:]
				c.dprices = c.dprices[1:]
			}
		}
	}
	c.step++
	return trades
}

type stepOut struct {
	Step   int        `json:"step"`
	Trades []tradeOut `json:"trades"`
	Alerts int        `json:"alerts_total"`
}

// stepN(n) — advance the simulation n steps, returning the new trades.
func stepN(_ js.Value, args []js.Value) any {
	n := 1
	if len(args) > 0 && args[0].Type() == js.TypeNumber {
		n = args[0].Int()
	}
	if n < 1 {
		n = 1
	}
	if n > 200 {
		n = 200 // one call must never wedge the frame
	}
	var trades []tradeOut
	for range n {
		trades = append(trades, c.stepOnce()...)
	}
	if trades == nil {
		trades = []tradeOut{}
	}
	return toJSON(stepOut{Step: c.step, Trades: trades, Alerts: len(c.monitor.Alerts())})
}

// flood() — the quote-stuffing provocation: a burst of tiny IOC orders priced
// where they can never trade. None rests (IOC), none fills, every placement is
// observed — and the OTR detector, scoring placements per fill, names the
// account. As with spoof(), nothing here talks to the detector; it only trades.
func flood(_ js.Value, _ []js.Value) any {
	snap := c.engine.Snapshot(1)
	if len(snap.Bids) == 0 {
		return errJSON("no book to flood yet — let the market run a moment")
	}
	placed := 0
	for i := range floodOrders {
		price := snap.Bids[0].Price - int64(10+i%5)
		o, err := types.NewOrder(floodUser, c.inst.Symbol, types.SideBuy,
			types.OrderTypeLimit, price, 100, types.TIFImmediateOrCancel)
		if err != nil {
			continue
		}
		res := c.engine.Process(o)
		c.observe(o, res)
		placed++
	}
	return toJSON(map[string]any{"ok": true, "user": floodUser, "placed": placed})
}

type openOrderOut struct {
	ID    int64  `json:"id"`
	Side  string `json:"side"`
	Price string `json:"price"`
	Qty   string `json:"qty"`
}

// openOrders(user) — the user's resting orders, so the page can show them,
// mark their price levels on the ladder, and cancel them by id.
func openOrders(_ js.Value, args []js.Value) any {
	user := "you"
	if len(args) > 0 && args[0].Type() == js.TypeString {
		user = args[0].String()
	}
	orders := c.engine.OpenOrdersFor(user)
	out := make([]openOrderOut, 0, len(orders))
	for _, o := range orders {
		out = append(out, openOrderOut{
			ID:    o.ID,
			Side:  string(o.Side),
			Price: c.inst.TicksToPrice(o.Price).String(),
			Qty:   c.inst.LotsToQty(o.RemainingQty).String(),
		})
	}
	return toJSON(map[string]any{"orders": out})
}

type lambdaOut struct {
	Lambda float64 `json:"lambda"` // ticks per lot of net aggressive flow
	R2     float64 `json:"r2"`
	N      int     `json:"n"`
}

type signalsJSON struct {
	Step          int       `json:"step"`
	Digest        string    `json:"digest"` // EngineSnapshot.Digest, first 12 hex
	Mid           float64   `json:"mid"`
	Spread        float64   `json:"spread"`
	Last          float64   `json:"last"`
	OFI           float64   `json:"ofi"`            // cumulative, display qty units
	CVD           float64   `json:"cvd"`            // cumulative signed volume, display units
	Imbalance     float64   `json:"imbalance"`      // top-5 depth, -1..1
	BestImbalance float64   `json:"best_imbalance"` // top-of-book, -1..1
	Lambda        lambdaOut `json:"lambda"`
}

// signalsOut() — the current value of every signal panel, computed by the same
// pkg/signals code the research studies run.
func signalsOut(_ js.Value, _ []js.Value) any {
	snap := c.engine.Snapshot(bookDepth)
	out := signalsJSON{
		Step:          c.step,
		Digest:        c.engine.TakeSnapshot().Digest()[:12],
		OFI:           c.ofi.Cumulative() / 1000, // milli lots → units
		CVD:           float64(c.cvd.Value()) / 1000,
		Imbalance:     signals.DepthImbalance(snap, 5),
		BestImbalance: signals.BestImbalance(snap),
	}
	// The engine's floored MidPrice, not the float midpoint, so this number and
	// the ladder's mid strip can never disagree in the last cent.
	if m, ok := c.engine.MidPrice(); ok {
		out.Mid = float64(m) / 100
	}
	if sp, ok := c.engine.Spread(); ok {
		out.Spread = float64(sp) / 100
	}
	if snap.LastTradePrice > 0 {
		out.Last = float64(snap.LastTradePrice) / 100
	}
	if len(c.flows) >= lambdaMinN {
		fit := signals.EstimateLambda(c.flows, c.dprices)
		// The fit is per lot, and a lot here is a milli-unit; scale to ticks per
		// 1.0 displayed quantity so the panel shows a number a human can hold.
		out.Lambda = lambdaOut{Lambda: fit.Lambda * 1000, R2: fit.R2, N: fit.N}
	}
	return toJSON(out)
}

type alertOut struct {
	Kind   string `json:"kind"`
	UserID string `json:"user"`
	Seq    uint64 `json:"seq"`
	Detail string `json:"detail"`
}

// alertsOut(since) — surveillance alerts after index since, so the page drains
// incrementally instead of re-reading history every frame.
func alertsOut(_ js.Value, args []js.Value) any {
	since := 0
	if len(args) > 0 && args[0].Type() == js.TypeNumber {
		since = args[0].Int()
	}
	all := c.monitor.Alerts()
	if since < 0 || since > len(all) {
		since = len(all)
	}
	out := make([]alertOut, 0, len(all)-since)
	for _, a := range all[since:] {
		out = append(out, alertOut{Kind: a.Kind, UserID: a.UserID, Seq: a.Seq, Detail: a.Detail})
	}
	return toJSON(map[string]any{"alerts": out, "total": len(all)})
}

// cancel(id, user) — ownership enforced by the engine, observed by surveillance.
func cancel(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return errJSON("cancel needs (id, user)")
	}
	if err := c.cancelObserved(int64(args[0].Int()), args[1].String()); err != nil {
		return errJSON(err.Error())
	}
	return `{"ok":true}`
}

// spoof() — place layered, far-from-touch size under a throwaway account. The
// bridge auto-cancels it a few steps later, and the SpoofDetector — the same
// code a venue would run — names the account in the alert feed. Nothing here
// talks to the detector; it only trades.
func spoof(_ js.Value, _ []js.Value) any {
	if c.spoofIDs != nil {
		return errJSON("a spoof is already resting")
	}
	snap := c.engine.Snapshot(1)
	if len(snap.Bids) == 0 {
		return errJSON("no bid to spoof behind yet — let the market run a moment")
	}
	ids := make([]int64, 0, spoofLayers)
	for i := range spoofLayers {
		price := snap.Bids[0].Price - int64(2+i)
		o, err := types.NewOrder(spoofUser, c.inst.Symbol, types.SideBuy,
			types.OrderTypeLimit, price, spoofSize, types.TIFGoodTillCancel)
		if err != nil {
			continue
		}
		res := c.engine.Process(o)
		c.observe(o, res)
		if res.Status != types.OrderStatusRejected {
			ids = append(ids, o.ID)
		}
	}
	if len(ids) == 0 {
		return errJSON("spoof orders were rejected")
	}
	c.spoofIDs, c.spoofAt = ids, c.step
	return toJSON(map[string]any{"ok": true, "user": spoofUser, "orders": ids, "cancels_in_steps": spoofSteps})
}

type tradeOut struct {
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
	Taker    string `json:"taker_side"`
}

type submitOut struct {
	ID     int64      `json:"id"`
	Status string     `json:"status"`
	Error  string     `json:"error,omitempty"`
	Trades []tradeOut `json:"trades"`
}

// submit(user, side, type, price, qty) — price ignored for market orders. The
// order takes the same observe path as agent flow, and the returned id is what
// obCancel wants.
func submit(_ js.Value, args []js.Value) any {
	if len(args) < 5 {
		return errJSON("submit needs (user, side, type, price, qty)")
	}
	user := args[0].String()
	side := types.Side(args[1].String())
	otype := types.OrderType(args[2].String())

	price, err := decimal.NewFromString(args[3].String())
	if err != nil {
		price = decimal.Zero
	}
	qty, err := decimal.NewFromString(args[4].String())
	if err != nil {
		return errJSON("bad quantity")
	}

	tif := types.TIFGoodTillCancel
	if otype == types.OrderTypeMarket {
		tif = types.TIFImmediateOrCancel
	}
	order, err := c.inst.NewOrder(user, side, otype, price, qty, tif)
	if err != nil {
		return errJSON(err.Error())
	}

	res := c.engine.Process(order)
	trades := c.observe(order, res)
	out := submitOut{ID: order.ID, Status: string(res.Status), Trades: trades}
	if res.RejectionReason != nil {
		out.Error = res.RejectionReason.Error()
	}
	return toJSON(out)
}

type levelOut struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

type snapOut struct {
	Bids      []levelOut `json:"bids"`
	Asks      []levelOut `json:"asks"`
	LastTrade string     `json:"last_trade"`
	Spread    string     `json:"spread"`
	Mid       string     `json:"mid"`
}

// snapshot(depth) — top-of-book view.
func snapshot(_ js.Value, args []js.Value) any {
	depth := 12
	if len(args) > 0 && args[0].Type() == js.TypeNumber {
		depth = args[0].Int()
	}
	s := c.engine.Snapshot(depth)

	out := snapOut{
		Bids:      make([]levelOut, 0, len(s.Bids)),
		Asks:      make([]levelOut, 0, len(s.Asks)),
		LastTrade: c.inst.TicksToPrice(s.LastTradePrice).String(),
	}
	for _, b := range s.Bids {
		out.Bids = append(out.Bids, levelOut{Price: c.inst.TicksToPrice(b.Price).String(), Size: c.inst.LotsToQty(b.Quantity).String()})
	}
	for _, a := range s.Asks {
		out.Asks = append(out.Asks, levelOut{Price: c.inst.TicksToPrice(a.Price).String(), Size: c.inst.LotsToQty(a.Quantity).String()})
	}
	if sp, ok := c.engine.Spread(); ok {
		out.Spread = c.inst.TicksToPrice(sp).String()
	}
	if mid, ok := c.engine.MidPrice(); ok {
		out.Mid = c.inst.TicksToPrice(mid).String()
	}
	return toJSON(out)
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return errJSON("marshal failed")
	}
	return string(b)
}

func errJSON(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}
