package server

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// loginLimiter throttles password guessing on the unauthenticated auth routes
// (login, init's setup token, change-password's current password). Before it,
// anything that could reach the port could guess without limit; bcrypt's cost
// was the only brake.
//
// Deliberately a backoff, never a lockout: behind a reverse proxy every client
// shares one address, so a hard lockout would let an attacker lock the operator
// out for good. After loginFreeAttempts consecutive failures from an address,
// each new failure doubles the wait (1 s, 2 s, … capped at loginMaxBackoff). A
// success clears it; an address quiet for loginForgetAfter starts over.
//
// Known limit of keying by peer address: someone guessing continuously through
// the same reverse proxy keeps that shared bucket in backoff, so a new login
// from behind it can be delayed (never more than loginMaxBackoff per attempt).
// Sessions already signed in are unaffected. Keying by X-Forwarded-For instead
// would be worse: unless the proxy overwrites it, the client chooses the key.
type loginLimiter struct {
	mu   sync.Mutex
	m    map[string]*loginFailures
	now  func() time.Time // nil = time.Now (tests inject a clock)
	seen int              // records since the last prune
}

type loginFailures struct {
	count int
	last  time.Time
	until time.Time
}

const (
	loginFreeAttempts = 5
	loginMaxBackoff   = 60 * time.Second
	loginForgetAfter  = 15 * time.Minute
	loginPruneEvery   = 1024
)

func (l *loginLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// allow reports whether key may attempt now, or how long it must wait.
func (l *loginLimiter) allow(key string) (wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.m[key]
	if f == nil {
		return 0, true
	}
	now := l.clock()
	if now.Sub(f.last) > loginForgetAfter {
		delete(l.m, key)
		return 0, true
	}
	if now.Before(f.until) {
		return f.until.Sub(now), false
	}
	return 0, true
}

// fail records a failed attempt from key.
func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*loginFailures{}
	}
	now := l.clock()
	f := l.m[key]
	if f == nil || now.Sub(f.last) > loginForgetAfter {
		f = &loginFailures{}
		l.m[key] = f
	}
	f.count++
	f.last = now
	if over := f.count - loginFreeAttempts; over > 0 {
		backoff := loginMaxBackoff
		if over <= 7 { // 1 s << 6 = 64 s already exceeds the cap
			backoff = min(time.Second<<(over-1), loginMaxBackoff)
		}
		f.until = now.Add(backoff)
	}
	if l.seen++; l.seen >= loginPruneEvery { // bound memory under address churn
		l.seen = 0
		for k, v := range l.m {
			if now.Sub(v.last) > loginForgetAfter {
				delete(l.m, k)
			}
		}
	}
}

// succeed clears key's failures.
func (l *loginLimiter) succeed(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

// clientKey is the peer address. X-Forwarded-For is not trusted: a client could
// set it to anything and walk around the limit.
func clientKey(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// throttled writes 429 and reports true when r's address must wait.
func (s *Server) throttled(w http.ResponseWriter, r *http.Request) bool {
	wait, ok := s.loginLimit.allow(clientKey(r))
	if ok {
		return false
	}
	secs := int((wait + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeErr(w, http.StatusTooManyRequests, "尝试次数过多，请 "+strconv.Itoa(secs)+" 秒后再试")
	return true
}
