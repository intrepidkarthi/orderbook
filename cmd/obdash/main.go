// Command obdash is an operator dashboard for a running obgw — the phase-2 half
// of docs/CONSOLE-SPEC.md. It is deliberately a sidecar, not a feature of the
// venue: an ordinary market-data subscriber over the venue's own wire protocol
// plus a reader of the admin /metrics page, re-served to browsers as one page
// and one Server-Sent Events stream.
//
// Being an ordinary client is the design. obgw gains no dashboard code, no new
// port and no new attack surface; and the market-data protocol gets what
// docs/PROTOCOL.md always claimed it supports — a subscriber written by someone
// who only has the format — living outside the venue's own test tree. What the
// dashboard shows an operator is what the venue actually published, because it
// has no other source.
//
// The page leans on the two numbers RUNBOOKS.md says matter first: queue depth
// against capacity (the /readyz signal, with the 75% alert threshold drawn on
// the meter) and the event sequence advancing while commands wait — the only
// way a stalled matcher is distinguishable from a quiet market.
//
//	obgw  -addr :9000 -mdaddr :9001 -admin 127.0.0.1:9100 ...
//	obdash -md 127.0.0.1:9001 -admin http://127.0.0.1:9100/metrics -addr 127.0.0.1:8090
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

// payload is one SSE frame: the subscriber's book view, the venue's own
// gauges, and the display factors the operator configured (the wire carries
// integer ticks and lots; converting them is presentation, so the page does it).
type payload struct {
	Symbol   string       `json:"symbol"`
	TickSize float64      `json:"tick_size"`
	LotSize  float64      `json:"lot_size"`
	Feed     snapshot     `json:"feed"`
	Metrics  metricsState `json:"metrics"`
	SeqRate  float64      `json:"seq_rate"` // md sequences per second, measured here
}

func main() {
	var (
		mdAddr   = flag.String("md", "127.0.0.1:9001", "obgw market-data address")
		adminURL = flag.String("admin", "http://127.0.0.1:9100/metrics", "obgw admin /metrics URL")
		addr     = flag.String("addr", "127.0.0.1:8090", "dashboard listen address")
		symbol   = flag.String("symbol", "BTC-USD", "instrument label shown on the page")
		tickSize = flag.Float64("ticksize", 0.01, "price per tick, for display only")
		lotSize  = flag.Float64("lotsize", 0.001, "quantity per lot, for display only")
		depth    = flag.Int("depth", 10, "book depth shown")
	)
	flag.Parse()

	f := newFeed(*mdAddr)
	go f.run()
	s := newScraper(*adminURL)
	go s.run(time.Second)
	h := newHub()

	// One composer, one cadence: every broadcast is a complete frame, so a
	// browser that joins, drops, or is shed needs no history to be correct —
	// the same property the venue's own snapshot-then-deltas stream has, with
	// the deltas traded away for simplicity at dashboard rates.
	go func() {
		var lastSeq uint64
		var lastAt time.Time
		for {
			time.Sleep(500 * time.Millisecond)
			p := payload{
				Symbol: *symbol, TickSize: *tickSize, LotSize: *lotSize,
				Feed:    f.snapshot(*depth),
				Metrics: s.state(),
			}
			p.Metrics.Values = jsonSafe(p.Metrics.Values)
			now := time.Now()
			if !lastAt.IsZero() && p.Feed.Seq >= lastSeq {
				p.SeqRate = float64(p.Feed.Seq-lastSeq) / now.Sub(lastAt).Seconds()
			}
			lastSeq, lastAt = p.Feed.Seq, now
			b, err := json.Marshal(p)
			if err != nil {
				continue
			}
			h.broadcast(b)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})
	mux.Handle("/events", h)

	log.Printf("obdash: watching md %s and %s — dashboard on http://%s", *mdAddr, *adminURL, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
