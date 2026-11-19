package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginLimiterBacksOffWithoutLockingOut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := &loginLimiter{now: func() time.Time { return now }}
	const ip = "203.0.113.7"
	for i := 0; i < loginFreeAttempts; i++ {
		if _, ok := l.allow(ip); !ok {
			t.Fatalf("attempt %d throttled inside the free allowance", i+1)
		}
		l.fail(ip)
	}
	// The allowance is spent only once the 5th failure is followed by a 6th.
	if _, ok := l.allow(ip); !ok {
		t.Fatal("throttled before exceeding the free allowance")
	}
	l.fail(ip) // 6th failure → 1 s
	if wait, ok := l.allow(ip); ok || wait != time.Second {
		t.Fatalf("after the 6th failure: ok=%v wait=%v, want 1s", ok, wait)
	}
	if _, ok := l.allow("198.51.100.1"); !ok {
		t.Fatal("another address was throttled")
	}
	for i := 0; i < 20; i++ { // keep failing: doubles, then caps
		now = now.Add(loginMaxBackoff)
		l.fail(ip)
	}
	if wait, ok := l.allow(ip); ok || wait != loginMaxBackoff {
		t.Fatalf("backoff not capped: ok=%v wait=%v, want %v", ok, wait, loginMaxBackoff)
	}
	now = now.Add(loginMaxBackoff) // waiting it out always works: never a lockout
	if _, ok := l.allow(ip); !ok {
		t.Fatal("still throttled after the capped backoff elapsed")
	}
	l.succeed(ip)
	l.fail(ip)
	if _, ok := l.allow(ip); !ok {
		t.Fatal("a success did not reset the failure count")
	}
	for i := 0; i < loginFreeAttempts+3; i++ {
		l.fail(ip)
	}
	now = now.Add(loginForgetAfter + time.Second)
	if _, ok := l.allow(ip); !ok {
		t.Fatal("a quiet address was not forgotten")
	}
}

func TestThrottledWrites429WithRetryAfter(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	r.RemoteAddr = "203.0.113.9:51234"
	for i := 0; i <= loginFreeAttempts; i++ {
		s.loginLimit.fail(clientKey(r))
	}
	w := httptest.NewRecorder()
	if !s.throttled(w, r) {
		t.Fatal("not throttled after exceeding the allowance")
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d Retry-After=%q, want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	// X-Forwarded-For is not trusted to pick a fresh bucket.
	r2 := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	r2.RemoteAddr = r.RemoteAddr
	r2.Header.Set("X-Forwarded-For", "192.0.2.55")
	if !s.throttled(httptest.NewRecorder(), r2) {
		t.Fatal("a spoofed X-Forwarded-For escaped the limit")
	}
}
