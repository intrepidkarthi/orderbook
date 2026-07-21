package types

import (
	"errors"
	"testing"
)

func TestNewTrailingStop_Validation(t *testing.T) {
	o := mustOrder(t, SideSell, OrderTypeMarket, "0", "1", TIFImmediateOrCancel)
	if _, err := NewTrailingStop(nil, dec("5")); !errors.Is(err, ErrNilOrder) {
		t.Errorf("nil err = %v, want ErrNilOrder", err)
	}
	if _, err := NewTrailingStop(o, dec("0")); !errors.Is(err, ErrInvalidStopPrice) {
		t.Errorf("zero trail err = %v, want ErrInvalidStopPrice", err)
	}
}

func TestTrailingStop_SellRatchetsUp(t *testing.T) {
	o := mustOrder(t, SideSell, OrderTypeMarket, "0", "1", TIFImmediateOrCancel)
	ts, _ := NewTrailingStop(o, dec("5"))

	ts.Observe(dec("100"))
	if !ts.StopPrice().Equal(dec("95")) {
		t.Errorf("stop after 100 = %s, want 95", ts.StopPrice())
	}
	ts.Observe(dec("110")) // new high ⇒ stop ratchets up
	if !ts.StopPrice().Equal(dec("105")) {
		t.Errorf("stop after 110 = %s, want 105", ts.StopPrice())
	}
	ts.Observe(dec("108")) // lower, but stop must not retreat
	if !ts.StopPrice().Equal(dec("105")) {
		t.Errorf("stop after 108 = %s, want 105 (no retreat)", ts.StopPrice())
	}
	if ts.ShouldTrigger(dec("106")) {
		t.Error("should not trigger at 106 (> stop 105)")
	}
	if !ts.ShouldTrigger(dec("105")) {
		t.Error("should trigger at 105")
	}
}

func TestTrailingStop_BuyRatchetsDown(t *testing.T) {
	o := mustOrder(t, SideBuy, OrderTypeMarket, "0", "1", TIFImmediateOrCancel)
	ts, _ := NewTrailingStop(o, dec("5"))

	ts.Observe(dec("100"))
	if !ts.StopPrice().Equal(dec("105")) {
		t.Errorf("stop after 100 = %s, want 105", ts.StopPrice())
	}
	ts.Observe(dec("90")) // new low ⇒ stop ratchets down
	if !ts.StopPrice().Equal(dec("95")) {
		t.Errorf("stop after 90 = %s, want 95", ts.StopPrice())
	}
	if !ts.ShouldTrigger(dec("95")) {
		t.Error("buy trailing should trigger at 95")
	}
}
