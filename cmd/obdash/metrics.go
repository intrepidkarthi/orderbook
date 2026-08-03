package main

import (
	"bufio"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// scraper polls the venue's admin /metrics — the same Prometheus text any
// monitoring stack reads, parsed with the same tolerance: comment lines are
// skipped, NaN is a value (an empty book reports NaN, not zero, because zero
// is a price), and a name we don't know is ignored rather than fatal.
type scraper struct {
	url string

	mu     sync.Mutex
	values map[string]float64
	ok     bool
	err    string
}

func newScraper(url string) *scraper {
	return &scraper{url: url, values: map[string]float64{}}
}

func (s *scraper) run(every time.Duration) {
	for {
		s.scrape()
		time.Sleep(every)
	}
}

func (s *scraper) scrape() {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(s.url)
	if err != nil {
		s.fail(err.Error())
		return
	}
	defer resp.Body.Close()
	vals, err := parseMetrics(resp.Body)
	if err != nil {
		s.fail(err.Error())
		return
	}
	s.mu.Lock()
	s.values = vals
	s.ok = true
	s.err = ""
	s.mu.Unlock()
}

func (s *scraper) fail(msg string) {
	s.mu.Lock()
	s.ok = false
	s.err = msg
	s.mu.Unlock()
}

// metricsState is what the dashboard forwards: the curated gauges plus scrape
// health. Forwarding health matters more than it looks — a dashboard that
// silently shows the last good scrape is a dashboard that shows a healthy
// venue for as long as the venue has been unreachable.
type metricsState struct {
	OK     bool               `json:"ok"`
	Err    string             `json:"err,omitempty"`
	Values map[string]float64 `json:"values"`
}

func (s *scraper) state() metricsState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := metricsState{OK: s.ok, Err: s.err, Values: make(map[string]float64, len(s.values))}
	for k, v := range s.values {
		out.Values[k] = v
	}
	return out
}

// parseMetrics reads Prometheus text exposition: "name value" per line, with
// optional labels we don't need (the venue exposes none on its gauges) and
// comment lines starting with '#'.
func parseMetrics(r io.Reader) (map[string]float64, error) {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		out[fields[0]] = v
	}
	return out, sc.Err()
}

// jsonSafe replaces NaN with nil-able sentinel handling at encode time.
// encoding/json refuses NaN, and the venue legitimately reports NaN for an
// empty side — so NaN is dropped from the payload and the page shows "—",
// which is what NaN meant.
func jsonSafe(vals map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(vals))
	for k, v := range vals {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			out[k] = v
		}
	}
	return out
}
