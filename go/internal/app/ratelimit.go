package app

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rateBucket struct {
	tokens float64
	last   time.Time
}

const maxRateBuckets = 65536

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string]rateBucket)}
}

// Allow applies a token bucket. A zero rate is unlimited and does not allocate
// state. Configuration changes take effect on the next request.
func (l *rateLimiter) Allow(
	key string, rate, burst int, now time.Time,
) (bool, time.Duration) {
	if rate <= 0 {
		return true, 0
	}
	if burst <= 0 {
		burst = rate
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, found := l.buckets[key]
	if !found {
		l.prune(now)
		bucket = rateBucket{tokens: float64(burst), last: now}
	}
	elapsed := now.Sub(bucket.last).Seconds()
	bucket.tokens = math.Min(float64(burst), bucket.tokens+elapsed*float64(rate))
	bucket.last = now
	if bucket.tokens >= 1 {
		bucket.tokens--
		l.buckets[key] = bucket
		return true, 0
	}
	l.buckets[key] = bucket
	return false, time.Duration(math.Ceil((1-bucket.tokens)/float64(rate)*1000)) * time.Millisecond
}

func (l *rateLimiter) prune(now time.Time) {
	if len(l.buckets) < maxRateBuckets {
		return
	}
	cutoff := now.Add(-10 * time.Minute)
	for key, bucket := range l.buckets {
		if bucket.last.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
	if len(l.buckets) < maxRateBuckets {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, bucket := range l.buckets {
		if oldestKey == "" || bucket.last.Before(oldest) {
			oldestKey, oldest = key, bucket.last
		}
	}
	delete(l.buckets, oldestKey)
}

func clientAddress(r *http.Request, trustProxy bool) string {
	value := r.RemoteAddr
	if trustProxy {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
			value = forwarded
		}
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return host
	}
	return value
}

func retryAfter(duration time.Duration) string {
	seconds := int(math.Ceil(duration.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}
