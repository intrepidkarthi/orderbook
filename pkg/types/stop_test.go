package types

import (
	"errors"
	"testing"
)

func TestNewStopOrder_Validation(t *testing.T) {
	o := mustOrder(t, SideSell, OrderTypeMarket, 0, 1, TIFImmediateOrCancel)
	if _, err := NewStopOrder(nil, 10); !errors.Is(err, ErrNilOrder) {
		t.Errorf("nil order err = %v, want ErrNilOrder", err)
	}
	if _, err := NewStopOrder(o, 0); !errors.Is(err, ErrInvalidStopPrice) {
		t.Errorf("zero stop err = %v, want ErrInvalidStopPrice", err)
	}
}

func TestStopOrder_TriggerDirection(t *testing.T) {
	// Sell stop (stop-loss for a long): triggers when price falls to/below stop.
	sell := mustOrder(t, SideSell, OrderTypeMarket, 0, 1, TIFImmediateOrCancel)
	ss, _ := NewStopOrder(sell, 95)
	if ss.ShouldTrigger(96) {
		t.Error("sell stop should NOT trigger above stop")
	}
	if !ss.ShouldTrigger(95) {
		t.Error("sell stop should trigger at stop")
	}
	if !ss.ShouldTrigger(94) {
		t.Error("sell stop should trigger below stop")
	}

	// Buy stop: triggers when price rises to/above stop.
	buy := mustOrder(t, SideBuy, OrderTypeMarket, 0, 1, TIFImmediateOrCancel)
	bs, _ := NewStopOrder(buy, 105)
	if bs.ShouldTrigger(104) {
		t.Error("buy stop should NOT trigger below stop")
	}
	if !bs.ShouldTrigger(105) {
		t.Error("buy stop should trigger at stop")
	}
}

func TestStopOrder_TriggerOnce(t *testing.T) {
	o := mustOrder(t, SideSell, OrderTypeMarket, 0, 1, TIFImmediateOrCancel)
	s, _ := NewStopOrder(o, 95)
	if s.IsTriggered() {
		t.Error("new stop should not be triggered")
	}
	s.Trigger()
	if !s.IsTriggered() {
		t.Error("stop should be triggered after Trigger()")
	}
	if s.Order.Status != OrderStatusNew {
		t.Errorf("underlying status = %q, want NEW after trigger", s.Order.Status)
	}
	if s.ShouldTrigger(90) {
		t.Error("already-triggered stop should not re-trigger")
	}
}
