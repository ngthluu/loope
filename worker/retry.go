package main

import (
	"context"
	"strings"
	"time"
)

// RetryPolicy retries a function with exponential backoff while it returns a
// retryable error. MaxAttempts == 0 means unbounded: retry until the function
// succeeds, returns a non-retryable error, or the context is cancelled.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func (p RetryPolicy) do(ctx context.Context, isRetryable func(error) bool, fn func() error) error {
	delay := p.BaseDelay
	if delay <= 0 {
		delay = time.Second // guard: never hot-loop on a zero-value policy
	}
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !isRetryable(err) {
			return err
		}
		if p.MaxAttempts > 0 && attempt >= p.MaxAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err // surface the last error on shutdown, don't hang
		case <-time.After(delay):
		}
		delay *= 2
		if p.MaxDelay > 0 && delay > p.MaxDelay {
			delay = p.MaxDelay
		}
	}
}

// transientSignatures are lowercase substrings that mark a GitHub/git failure as
// a rate limit or a transient outage worth retrying. Everything else (not found,
// already exists, auth, validation) is permanent and fails fast.
var transientSignatures = []string{
	"rate limit", "abuse detection", "submitted too quickly", "secondary rate limit",
	"http 429", "http 500", "http 502", "http 503", "http 504",
	"returned error: 429", "returned error: 500", "returned error: 502", "returned error: 503", "returned error: 504",
	"status 429",
	"internal server error", "bad gateway", "service unavailable", "gateway timeout",
	"timeout", "timed out", "connection reset", "connection refused",
	"could not resolve host", "couldn't connect", "tls handshake",
	"network is unreachable", "temporary failure in name resolution", "unexpected eof",
}

func isTransientGitHubError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range transientSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}
