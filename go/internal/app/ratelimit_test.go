package app

import (
	"testing"
	"time"
)

func TestRateLimiterBurstAndRefill(t *testing.T) {
	limiter := newRateLimiter()
	now := time.Unix(100, 0)
	for i := 0; i < 2; i++ {
		if allowed, _ := limiter.Allow("token", 2, 2, now); !allowed {
			t.Fatalf("burst request %d refused", i)
		}
	}
	if allowed, retry := limiter.Allow("token", 2, 2, now); allowed || retry <= 0 {
		t.Fatalf("third allowed=%v retry=%v", allowed, retry)
	}
	if allowed, _ := limiter.Allow("token", 2, 2, now.Add(500*time.Millisecond)); !allowed {
		t.Fatal("one token did not refill")
	}
	if allowed, _ := limiter.Allow("other-token", 2, 2, now); !allowed {
		t.Fatal("separate token did not get a separate bucket")
	}
	if allowed, _ := limiter.Allow("unlimited", 0, 0, now); !allowed {
		t.Fatal("zero limit must be unlimited")
	}
}
