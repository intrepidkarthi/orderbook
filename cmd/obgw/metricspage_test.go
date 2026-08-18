package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// TestTheMetricsPageStaysWellFormed is deliverable 21 of docs/LAG-AND-SHED.md §12.
//
// The exposition roughly doubles with this slice — seventeen refusal series of which
// perhaps five are ever non-zero, one drop counter, three per-symbol families and
// three histograms — and §14 names that as a cost rather than a win. What must not
// also happen is the page becoming one a scraper rejects or a dashboard misreads, so
// this asserts the shape rather than the contents: HELP and TYPE exactly once per
// family, a declared type for every series, and a value that parses.
func TestTheMetricsPageStaysWellFormed(t *testing.T) {
	const every = 40 * time.Millisecond
	cfg, _ := checkpointingVenue(t, every)
	srv := durableServer(t, cfg)
	defer srv.Close()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}
	c.cancel("nope") // one refusal, so a counter series is non-zero
	time.Sleep(150 * time.Millisecond)

	code, body := adminGet(t, srv, "/metrics")
	if code != 200 {
		t.Fatalf("/metrics = %d", code)
	}

	helps := map[string]int{}
	types := map[string]string{}
	var series int
	for _, line := range strings.Split(body, "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "# HELP "):
			f := strings.SplitN(strings.TrimPrefix(line, "# HELP "), " ", 2)
			if len(f) != 2 || f[1] == "" {
				t.Errorf("HELP with no text: %q", line)
			}
			helps[f[0]]++
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(strings.TrimPrefix(line, "# TYPE "))
			if len(f) != 2 {
				t.Errorf("malformed TYPE line: %q", line)
				continue
			}
			if _, dup := types[f[0]]; dup {
				t.Errorf("%s declared twice", f[0])
			}
			types[f[0]] = f[1]
		default:
			series++
			f := strings.Fields(line)
			if len(f) != 2 {
				t.Errorf("malformed series line: %q", line)
				continue
			}
			if _, err := strconv.ParseFloat(f[1], 64); err != nil {
				t.Errorf("unparseable value in %q", line)
			}
			name, _, _ := strings.Cut(f[0], "{")
			// A histogram's series are name_bucket / name_sum / name_count under one
			// TYPE line for name.
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if types[name] == "" && strings.HasSuffix(name, suffix) {
					name = strings.TrimSuffix(name, suffix)
				}
			}
			if types[name] == "" {
				t.Errorf("%s has no TYPE line; an operator has to guess what it is", name)
			}
		}
	}
	for name, n := range helps {
		if n != 1 {
			t.Errorf("%s has %d HELP lines, want 1", name, n)
		}
	}
	if len(helps) != len(types) {
		t.Errorf("%d HELP lines and %d TYPE lines", len(helps), len(types))
	}
	if series == 0 {
		t.Fatal("no series emitted")
	}

	// Every family this slice adds is on the page, declared as what it is.
	for name, want := range map[string]string{
		refusedMetric:          "counter",
		shedUnreportedMetric:   "counter",
		snapshotFailuresMetric: "counter",
		walAppendLatencyMetric: "histogram",
		walSyncLatencyMetric:   "histogram",
		snapshotDurationMetric: "histogram",
		snapshotAgeMetric:      "gauge",
		recoveryDurationMetric: "gauge",
	} {
		if got := types[name]; got != want {
			t.Errorf("# TYPE %s %s, want %s — the TYPE line is the only machine-readable statement this "+
				"page makes about what a series is", name, got, want)
		}
	}
	t.Logf("%d families, %d series", len(types), series)
}
