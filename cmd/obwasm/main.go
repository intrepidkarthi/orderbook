//go:build js && wasm

// Command obwasm compiles the matching engine to WebAssembly and exposes a small
// JSON API to JavaScript, so the browser demo runs the *real* engine rather than
// a reimplementation (docs/DEMO-SPEC.md §3).
//
// Build:  GOOS=js GOARCH=wasm go build -o web/obook.wasm ./cmd/obwasm
//
// JS globals installed: obReset(symbol), obSubmit(user,side,type,price,qty),
// obSnapshot(depth). Each returns a JSON string.
package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

var engine *matching.Engine

func main() {
	engine = matching.NewEngine(matching.DefaultConfig("DEMO"))
	js.Global().Set("obReset", js.FuncOf(reset))
	js.Global().Set("obSubmit", js.FuncOf(submit))
	js.Global().Set("obSnapshot", js.FuncOf(snapshot))
	select {} // keep the Go runtime alive for callbacks
}

func reset(_ js.Value, args []js.Value) any {
	symbol := "DEMO"
	if len(args) > 0 && args[0].Type() == js.TypeString {
		symbol = args[0].String()
	}
	engine = matching.NewEngine(matching.DefaultConfig(symbol))
	return `{"ok":true}`
}

type tradeOut struct {
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
	Taker    string `json:"taker_side"`
}

type submitOut struct {
	Status string     `json:"status"`
	Error  string     `json:"error,omitempty"`
	Trades []tradeOut `json:"trades"`
}

// submit(user, side, type, price, qty) — price ignored for market orders.
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
	order, err := types.NewOrder(user, engine.Book().Symbol(), side, otype, price, qty, tif)
	if err != nil {
		return errJSON(err.Error())
	}

	res := engine.Process(order)
	out := submitOut{Status: string(res.Status), Trades: make([]tradeOut, 0, len(res.Trades))}
	if res.RejectionReason != nil {
		out.Error = res.RejectionReason.Error()
	}
	for _, tr := range res.Trades {
		out.Trades = append(out.Trades, tradeOut{
			Price:    tr.Price.String(),
			Quantity: tr.Quantity.String(),
			Taker:    string(tr.TakerSide),
		})
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
	s := engine.Snapshot(depth)

	out := snapOut{
		Bids:      make([]levelOut, 0, len(s.Bids)),
		Asks:      make([]levelOut, 0, len(s.Asks)),
		LastTrade: s.LastTradePrice.String(),
	}
	for _, b := range s.Bids {
		out.Bids = append(out.Bids, levelOut{Price: b.Price.String(), Size: b.Quantity.String()})
	}
	for _, a := range s.Asks {
		out.Asks = append(out.Asks, levelOut{Price: a.Price.String(), Size: a.Quantity.String()})
	}
	if sp, ok := engine.Spread(); ok {
		out.Spread = sp.String()
	}
	if mid, ok := engine.MidPrice(); ok {
		out.Mid = mid.String()
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
