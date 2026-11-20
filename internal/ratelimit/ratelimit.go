// Package ratelimit holds the per-owner GitHub rate-limit state shared by the
// reconcilers that call GitHub: the not-before time after a rate limit, and the
// remaining-quota floor. One Limiter is constructed by the server and handed to
// each reconciler, so a limit seen by any holder stops them all.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter tracks, per owner, the time before which no further GitHub calls
// should be made. It is safe for concurrent use.
type Limiter struct {
	interval time.Duration
	floor    int

	mu        sync.Mutex
	notBefore map[string]time.Time
}

// New returns a Limiter. interval is the cool-off used when GitHub reports no
// usable reset time; floor is the remaining-quota level at or below which an
// owner is skipped (0 disables the quota check).
func New(interval time.Duration, floor int) *Limiter {
	return &Limiter{interval: interval, floor: floor, notBefore: make(map[string]time.Time)}
}

// Floor returns the remaining-quota floor; 0 means the check is disabled.
func (l *Limiter) Floor() int { return l.floor }

// Check reports whether owner is backed off as of now. If a previously set
// backoff has just expired, the entry is cleared and resumed is true — a
// one-shot signal for the caller to log the resume exactly once.
func (l *Limiter) Check(owner string, now time.Time) (backedOff, resumed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	nb, ok := l.notBefore[owner]
	if !ok {
		return false, false
	}
	if now.Before(nb) {
		return true, false
	}
	delete(l.notBefore, owner)
	return false, true
}

// Enter backs owner off until reset when reset is in the future, else for the
// Limiter's cool-off interval from now, and returns the not-before time. The
// future check keeps a stale or skewed reset from letting a later call for the
// same owner slip through.
func (l *Limiter) Enter(owner string, reset, now time.Time) time.Time {
	notBefore := now.Add(l.interval)
	if !reset.IsZero() && reset.After(now) {
		notBefore = reset
	}
	l.mu.Lock()
	l.notBefore[owner] = notBefore
	l.mu.Unlock()
	return notBefore
}

// BelowFloor reports whether remaining quota is at or below the floor. It is
// always false when the floor is disabled.
func (l *Limiter) BelowFloor(remaining int) bool {
	return l.floor > 0 && remaining <= l.floor
}
