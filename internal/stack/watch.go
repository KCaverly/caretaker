package stack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrChecksFailed = errors.New("stack checks failed")
	ErrWatchTimeout = errors.New("timed out waiting for stack checks")
)

type WatchOptions struct {
	Params
	Context  context.Context
	Interval time.Duration
	Timeout  time.Duration
	OnEvent  func(WatchEvent)
}

type WatchEvent struct {
	At      string   `json:"at"`
	PR      int      `json:"pr"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to"`
	Failing []string `json:"failing,omitempty"`
}

type WatchResult struct {
	Status  StackStatus  `json:"status"`
	Outcome string       `json:"outcome"`
	Events  []WatchEvent `json:"events"`
}

type statusFunc func(Params) (StackStatus, error)

func Watch(o WatchOptions) (WatchResult, error) {
	return watchWith(o, Status)
}

func watchWith(o WatchOptions, status statusFunc) (WatchResult, error) {
	res := WatchResult{Events: []WatchEvent{}}
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	interval := o.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	var cancel context.CancelFunc
	if o.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	previous := map[int]string{}
	first := true
	for {
		p := o.Params
		p.Fetch = first
		first = false
		st, err := status(p)
		res.Status = st
		if err != nil {
			res.Outcome = "error"
			return res, err
		}
		if !st.GitHub.Available {
			res.Outcome = "error"
			return res, fmt.Errorf("GitHub is unavailable: %s", strings.Join(st.GitHub.Warnings, "; "))
		}
		if !st.Stack.BaseChainOK || len(st.Stack.Orphans) > 0 || st.Stack.Counts[StateClosed] > 0 || st.Stack.Counts[StateDuplicateID] > 0 {
			res.Outcome = "error"
			return res, fmt.Errorf("stack topology changed while watching (next action: %s)", st.Stack.NextAction)
		}

		pending, failing := false, false
		for _, c := range st.Commits {
			if c.State != StateOpen || c.PR == nil {
				continue
			}
			summary := c.PR.Checks.Summary
			if old, ok := previous[c.PR.Number]; !ok || old != summary {
				e := WatchEvent{At: time.Now().UTC().Format(time.RFC3339), PR: c.PR.Number, From: old, To: summary, Failing: c.PR.Checks.Failing}
				previous[c.PR.Number] = summary
				res.Events = append(res.Events, e)
				if o.OnEvent != nil {
					o.OnEvent(e)
				}
			}
			switch summary {
			case "failing":
				failing = true
			case "pending":
				pending = true
			}
		}
		if failing {
			res.Outcome = "failing"
			return res, ErrChecksFailed
		}
		if !pending {
			res.Outcome = "passing"
			return res, nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.Outcome = "timeout"
				return res, ErrWatchTimeout
			}
			res.Outcome = "cancelled"
			return res, ctx.Err()
		case <-timer.C:
		}
	}
}
