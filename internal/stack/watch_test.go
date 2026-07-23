package stack

import (
	"context"
	"errors"
	"testing"
	"time"
)

func watchStatus(summary string) StackStatus {
	return StackStatus{
		GitHub:  GitHub{Available: true},
		Stack:   Stack{Size: 1, BaseChainOK: true, Counts: map[State]int{StateOpen: 1}},
		Commits: []Commit{{State: StateOpen, PR: &PR{Number: 9, Checks: Checks{Summary: summary, Failing: []string{"ci"}}}}},
	}
}

func TestWatchTransitionsToPassing(t *testing.T) {
	states := []string{"pending", "pending", "passing"}
	i := 0
	var events []WatchEvent
	res, err := watchWith(WatchOptions{Interval: time.Millisecond, Timeout: time.Second, OnEvent: func(e WatchEvent) { events = append(events, e) }}, func(Params) (StackStatus, error) {
		state := states[i]
		if i < len(states)-1 {
			i++
		}
		return watchStatus(state), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "passing" || len(events) != 2 || events[0].To != "pending" || events[1].To != "passing" {
		t.Fatalf("watch result = %+v, events=%+v", res, events)
	}
}

func TestWatchFailing(t *testing.T) {
	res, err := watchWith(WatchOptions{}, func(Params) (StackStatus, error) { return watchStatus("failing"), nil })
	if !errors.Is(err, ErrChecksFailed) || res.Outcome != "failing" {
		t.Fatalf("result=%+v error=%v", res, err)
	}
}

func TestWatchTimeoutAndCancellation(t *testing.T) {
	pending := func(Params) (StackStatus, error) { return watchStatus("pending"), nil }
	res, err := watchWith(WatchOptions{Interval: time.Millisecond, Timeout: 3 * time.Millisecond}, pending)
	if !errors.Is(err, ErrWatchTimeout) || res.Outcome != "timeout" {
		t.Fatalf("timeout result=%+v error=%v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = watchWith(WatchOptions{Context: ctx, Interval: time.Second}, pending)
	if !errors.Is(err, context.Canceled) || res.Outcome != "cancelled" {
		t.Fatalf("cancel result=%+v error=%v", res, err)
	}
}

func TestWatchRejectsBrokenTopology(t *testing.T) {
	st := watchStatus("pending")
	st.Stack.BaseChainOK = false
	res, err := watchWith(WatchOptions{}, func(Params) (StackStatus, error) { return st, nil })
	if err == nil || res.Outcome != "error" {
		t.Fatalf("result=%+v error=%v", res, err)
	}
}
