package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

var fastPolicy = RetryPolicy{MaxAttempts: 5, BaseDelay: time.Microsecond, MaxDelay: time.Microsecond}

func alwaysRetry(error) bool { return true }
func neverRetry(error) bool  { return false }

func TestRetrySucceedsFirstTry(t *testing.T) {
	calls := 0
	err := fastPolicy.do(context.Background(), alwaysRetry, func() error { calls++; return nil })
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want nil/1", err, calls)
	}
}

func TestRetryRetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := fastPolicy.do(context.Background(), alwaysRetry, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want nil/3", err, calls)
	}
}

func TestRetryDoesNotRetryNonRetryable(t *testing.T) {
	calls := 0
	err := fastPolicy.do(context.Background(), neverRetry, func() error { calls++; return errors.New("permanent") })
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want err/1", err, calls)
	}
}

func TestRetryStopsAtMaxAttempts(t *testing.T) {
	calls := 0
	err := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Microsecond, MaxDelay: time.Microsecond}.
		do(context.Background(), alwaysRetry, func() error { calls++; return errors.New("transient") })
	if err == nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want err/3", err, calls)
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: after the first failed attempt, the backoff select returns immediately
	calls := 0
	err := RetryPolicy{MaxAttempts: 0, BaseDelay: time.Hour, MaxDelay: time.Hour}.
		do(ctx, alwaysRetry, func() error { calls++; return errors.New("transient") })
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want err/1 (no hang, no extra attempts)", err, calls)
	}
}

func TestIsTransientGitHubError(t *testing.T) {
	transient := []string{
		"API rate limit exceeded", "HTTP 502 Bad Gateway", "returned error: 503",
		"connection reset by peer", "could not resolve host: github.com",
		"network is unreachable", "i/o timeout",
	}
	for _, s := range transient {
		if !isTransientGitHubError(errors.New(s)) {
			t.Errorf("want transient: %q", s)
		}
	}
	permanent := []string{
		"pull request already exists", "could not resolve to an Issue",
		"not found", "validation failed", "authentication required", "",
		"could not resolve to an Issue with the number of 429",
	}
	for _, s := range permanent {
		if isTransientGitHubError(errors.New(s)) {
			t.Errorf("want permanent: %q", s)
		}
	}
	if isTransientGitHubError(nil) {
		t.Error("nil must be non-transient")
	}
}
