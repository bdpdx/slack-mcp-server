package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestUnitRefreshWithRetryRetriesInsteadOfExiting(t *testing.T) {
	oldInitial, oldMax := cacheRetryInitialDelay, cacheRetryMaxDelay
	cacheRetryInitialDelay, cacheRetryMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { cacheRetryInitialDelay, cacheRetryMaxDelay = oldInitial, oldMax })

	core, logs := observer.New(zap.ErrorLevel)
	calls := 0
	refreshWithRetry("users", func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("slack unavailable")
		}
		return nil
	}, zap.New(core))

	if calls != 3 {
		t.Fatalf("refresh called %d times, want 3", calls)
	}
	if got := logs.FilterMessage("Failed to load Slack cache, will retry").Len(); got != 2 {
		t.Fatalf("logged %d retry errors, want 2", got)
	}
}
