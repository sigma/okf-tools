// Package request holds the per-request policy every HTTP backend needs: how long
// one attempt may take, how a stall is told apart from a cancelled run and named,
// and how long to wait before retrying.
//
// It sits deliberately BELOW the transport. transport.go states why rate limiting
// is not up there — a transaction is not a request, and scan and write-back
// traffic is not a transaction at all — and that reasoning holds. What it does not
// argue for is each backend inventing the policy separately, which is what
// happened: #184 had to make the same timeout fix twice, in two files, with the
// doc comment copied, and the two retry loops had already diverged in behaviour
// rather than only in code. The Docs client honoured no Retry-After and backed off
// without jitter; the Notion client did both.
//
// What stays per-backend is what genuinely differs: which statuses are retryable,
// which routes are safe to replay, and the pacing between writes. Those are
// judgements about an API. Everything here is not.
package request

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds ONE attempt: how long a client waits for a response
	// before giving up on it.
	//
	// Without a bound a publish has no deadline anywhere. HTTP/2 multiplexes every
	// call over one connection, so a half-dead connection parks the stream on a
	// reply that never arrives and nothing at any layer gives up — which wedged a
	// publish for 34 minutes (#184).
	//
	// Generous on purpose: the failure being bounded is unbounded, not slow. The
	// heaviest legitimate call any backend makes answers far inside this.
	DefaultTimeout = 60 * time.Second

	// BaseBackoff is the first retry's delay ceiling, doubling per attempt up to
	// MaxBackoff. Used only when the response carries no Retry-After.
	BaseBackoff = 500 * time.Millisecond
	// MaxBackoff caps the exponential growth.
	MaxBackoff = 8 * time.Second
	// MaxRetryAfter caps how long a server-sent Retry-After may park the run.
	MaxRetryAfter = 60 * time.Second
)

// WithTimeout bounds one attempt, so a retry gets its own full budget. It always
// returns a cancel function, so the caller releases the context whether or not a
// deadline was set; a non-positive budget means no deadline.
func WithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// Stalled reports whether err is the client's own per-attempt deadline firing
// rather than the caller giving up. caller must be the UNDERIVED context — the one
// passed in before WithTimeout wrapped it — or every stall would look like a
// cancellation.
//
// The distinction decides whether a retry is even considered: a cancelled run
// stops, it does not back off and try again.
func Stalled(caller context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && caller.Err() == nil
}

// WrapErr names a failed round trip. A stall is reported as a TIMEOUT naming the
// budget it exceeded rather than as a bare "context deadline exceeded", because
// the two read very differently to whoever finds the failed run: one says the
// destination stopped answering, the other looks like a bug in this program.
//
// backend prefixes the message ("notion", "gdocs") so a multi-backend log still
// says who failed.
func WrapErr(caller context.Context, backend, method, target string, timeout time.Duration, err error) error {
	if Stalled(caller, err) {
		return fmt.Errorf("%s: %s %s: no response within %v (request timeout): %w",
			backend, method, target, timeout, err)
	}
	return fmt.Errorf("%s: %s %s: %w", backend, method, target, err)
}

// Delay picks how long to wait before the next attempt: the server's Retry-After
// when it sent one, otherwise a jittered exponential backoff. attempt is 1-based.
//
// now supplies the clock for the HTTP-date form of Retry-After; nil uses
// time.Now. It is injectable because a test asserting on a date-form header
// cannot depend on the wall clock.
func Delay(header http.Header, attempt int, now func() time.Time) time.Duration {
	if d, ok := RetryAfter(header, now); ok {
		return d
	}
	return Backoff(attempt)
}

// Backoff is the jittered exponential delay for a 1-based attempt number,
// doubling from BaseBackoff up to MaxBackoff. The jitter is full — uniform over
// (0, backoff] — so a fleet of retries does not re-collide in lockstep.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := BaseBackoff << (attempt - 1)
	if backoff > MaxBackoff || backoff <= 0 {
		backoff = MaxBackoff
	}
	// rand.Int64N returns [0, n), so shift by one to make the range (0, backoff].
	return time.Duration(rand.Int64N(int64(backoff)) + 1)
}

// RetryAfter reads a Retry-After header in either of its two forms — a delay in
// seconds, or an HTTP-date — and reports the delay it asks for, clamped to
// MaxRetryAfter. A missing, unparseable, or already-elapsed value reports false so
// the caller backs off instead.
func RetryAfter(header http.Header, now func() time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return min(time.Duration(secs)*time.Second, MaxRetryAfter), true
	}
	if when, err := http.ParseTime(v); err == nil {
		if now == nil {
			now = time.Now
		}
		if d := when.Sub(now()); d > 0 {
			return min(d, MaxRetryAfter), true
		}
	}
	return 0, false
}

// Sleep waits d, or returns early if ctx is cancelled first. A non-positive
// delay does not wait at all, but still reports ctx's state, so a cancelled run
// stops rather than falling through into another attempt.
//
// It uses a stopped timer rather than time.After so a cancelled wait releases
// the timer immediately instead of holding it until it fires.
//
// This is the shape a retry loop's pause must have, and it is worth one shared
// function because getting it wrong is silent: a bare time.After leaks, and a
// select that forgets ctx.Done turns a cancelled publish into one that keeps
// backing off. Both clients now take it as an injectable field, which is what
// lets a retry test run at full speed instead of on the wall clock.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
