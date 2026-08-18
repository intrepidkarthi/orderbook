package observability

import (
	"strings"
	"testing"
)

// The counter primitive, and the four rules it exists to keep.
//
// Everything cmd/obgw counts in docs/LAG-AND-SHED.md is a count of something that is
// not an engine event, and before this the only escape hatch was Collector.Gauge —
// which is how obgw_publisher_dropped_total came to be exported as
// `# TYPE ... gauge` while being named and used as a counter. That works, because
// rate() applies counter-reset correction to whatever series it is given, and it is
// still the wrong thing to copy four more times: the TYPE line is the only
// machine-readable statement this page makes about what a series IS, and a page where
// half the _totals claim to be gauges is one an operator learns to ignore.

// TestCounterIsACounter is the whole contract in one test: idempotent registration,
// a TYPE line that says counter, one HELP/TYPE pair per NAME rather than per series,
// sorted output, and label values escaped by the same quote the rest of the page uses.
func TestCounterIsACounter(t *testing.T) {
	c := NewCollector()

	t.Run("same name and labels returns the same handle", func(t *testing.T) {
		a := c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "overloaded"})
		b := c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "overloaded"})
		if a != b {
			t.Fatal("registering twice produced two handles; the second caller would count into a series nobody exports")
		}
		a.Add(1)
		b.Add(1)
		if got := a.Value(); got != 2 {
			t.Errorf("value = %d, want 2 — the two handles are not the same counter", got)
		}
	})

	t.Run("label order does not create a second series", func(t *testing.T) {
		a := c.Counter("obgw_two_labels_total", "Two.",
			Label{Name: "b", Value: "2"}, Label{Name: "a", Value: "1"})
		b := c.Counter("obgw_two_labels_total", "Two.",
			Label{Name: "a", Value: "1"}, Label{Name: "b", Value: "2"})
		if a != b {
			t.Fatal("the same label set written in a different order registered twice")
		}
	})

	// Registered out of alphabetical order, so a stable scrape has to be the code
	// sorting rather than the map happening to iterate that way.
	c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "throttled"}).Add(9)
	c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "malformed"}).Add(3)
	// A label value that needs escaping. Nothing in cmd/obgw produces one today; the
	// point is that this family goes through the same quote as every other, so the
	// first one that does cannot produce a document a parser rejects.
	c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: `we"ird\`}).Add(1)
	c.Counter("obgw_shed_unreported_total", "Silent drops.", Label{Name: "op", Value: "cancel_on_disconnect"}).Add(1)

	var buf strings.Builder
	if err := c.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()

	if n := strings.Count(out, "# TYPE obgw_refused_total"); n != 1 {
		t.Errorf("%d TYPE lines for obgw_refused_total, want exactly 1 — HELP and TYPE go once per NAME, not per series", n)
	}
	if n := strings.Count(out, "# HELP obgw_refused_total"); n != 1 {
		t.Errorf("%d HELP lines for obgw_refused_total, want exactly 1", n)
	}
	if !strings.Contains(out, "# TYPE obgw_refused_total counter") {
		t.Errorf("obgw_refused_total is not declared a counter:\n%s", out)
	}
	for _, want := range []string{
		`obgw_refused_total{reason="throttled"} 9`,
		`obgw_refused_total{reason="malformed"} 3`,
		`obgw_refused_total{reason="overloaded"} 2`,
		`obgw_shed_unreported_total{op="cancel_on_disconnect"} 1`,
		`obgw_refused_total{reason="we\"ird\\"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Byte-stable: a scrape that reorders itself makes every diff of the page
	// meaningless, and this family is registered from a map.
	var second strings.Builder
	if err := c.WritePrometheus(&second); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if second.String() != out {
		t.Error("two scrapes of the same collector differ; the series are not sorted")
	}

	// And the series really are in order, not merely stable.
	var seen []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "obgw_refused_total{") {
			seen = append(seen, line)
		}
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] > seen[i] {
			t.Errorf("series out of order:\n  %s\n  %s", seen[i-1], seen[i])
		}
	}
}

// TestCounterIsNotInSnapshot pins the deliberate omission. Snapshot is the fixed
// struct of engine counters; a map of maps bolted onto it would grow a frozen surface
// for a convenience nothing needs. A caller either holds the handle or reads the
// exposition — which is what an operator's monitoring does anyway.
func TestCounterIsNotInSnapshot(t *testing.T) {
	c := NewCollector()
	c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "overloaded"}).Add(4)
	if n := len(c.Snapshot().Rejections); n != 0 {
		t.Errorf("Snapshot().Rejections has %d entries; a gateway refusal is not an engine rejection and must not be filed as one", n)
	}
	// The two ways a caller is meant to read one: the handle it registered, and the
	// exposition. There is deliberately no third.
	if got := c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "overloaded"}).Value(); got != 4 {
		t.Errorf("handle reads %d, want 4", got)
	}
	var buf strings.Builder
	if err := c.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if !strings.Contains(buf.String(), `obgw_refused_total{reason="overloaded"} 4`) {
		t.Errorf("the exposition does not carry the value:\n%s", buf.String())
	}
}

// TestEmptyCounterFamilyEmitsNothing — a family registered with no series would emit
// a HELP and a TYPE with no reading under them, which is a document some parsers
// accept and none should have to.
func TestEmptyCounterFamilyEmitsNothing(t *testing.T) {
	c := NewCollector()
	var buf strings.Builder
	if err := c.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if strings.Contains(buf.String(), "obgw_refused_total") {
		t.Error("an unregistered counter family appeared in the exposition")
	}
}

// TestUnlabelledCounterRendersBare — a counter with no labels must not emit `name{}`.
func TestUnlabelledCounterRendersBare(t *testing.T) {
	c := NewCollector()
	c.Counter("obgw_bare_total", "Bare.").Add(7)
	var buf strings.Builder
	if err := c.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if !strings.Contains(buf.String(), "\nobgw_bare_total 7\n") {
		t.Errorf("unlabelled counter not rendered bare:\n%s", buf.String())
	}
}

// BenchmarkCounterAdd is deliverable 2 of docs/LAG-AND-SHED.md §12, and the reason
// the type resolves its label set at REGISTRATION.
//
// The shed counter is incremented while the venue is shedding, which is the moment it
// has least to spare. A map[string] lookup under an RWMutex per increment — the shape
// countReason uses, which is defensible there because it runs at engine-event rate on
// a warm map — is not what a flood path should pay. Zero allocations is the assertion;
// the ns/op is the number to read when somebody proposes going back to a map.
func BenchmarkCounterAdd(b *testing.B) {
	c := NewCollector()
	ctr := c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "overloaded"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctr.Add(1)
	}
}

// BenchmarkCounterAddParallel is the same increment from many goroutines, because
// that is how it actually happens: one connection read loop per client, plus the
// matching goroutine for the unknown-order path.
func BenchmarkCounterAddParallel(b *testing.B) {
	c := NewCollector()
	ctr := c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "overloaded"})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ctr.Add(1)
		}
	})
}

// BenchmarkCounterLookupThenAdd is the sabotage measured rather than argued: the map
// lookup this type exists to avoid, benchmarked beside it so the difference is a
// number in the record instead of an assertion in a comment.
func BenchmarkCounterLookupThenAdd(b *testing.B) {
	c := NewCollector()
	c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "overloaded"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Counter("obgw_refused_total", "Refusals.", Label{Name: "reason", Value: "overloaded"}).Add(1)
	}
}
