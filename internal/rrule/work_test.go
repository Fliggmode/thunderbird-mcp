package rrule

import (
	"context"
	"testing"
	"time"
)

func TestWorkStopsEmptyPeriods(t *testing.T) {
	r, err := StrToRRule("FREQ=DAILY;BYMONTH=2;BYMONTHDAY=30")
	if err != nil {
		t.Fatal(err)
	}
	r.DTStart(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	calls := 0
	r.Work = func() error {
		calls++
		if calls == 10 {
			return context.Canceled
		}
		return nil
	}
	if _, ok := r.Iterator()(); ok || r.Err != context.Canceled || calls != 10 {
		t.Fatalf("calls=%d err=%v", calls, r.Err)
	}
}
