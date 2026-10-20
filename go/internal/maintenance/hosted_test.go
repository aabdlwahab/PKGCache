package maintenance

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A pushed image is the only copy anywhere. However long nobody has pulled it, eviction
// makes room from what can be fetched again and passes it by.
func TestEvictionPassesHostedContentBy(t *testing.T) {
	h := newHarness(t)
	h.svc.Hosted = func(_, key string) bool { return strings.HasPrefix(key, "hosted/") }
	pushed := h.put(t, "p", "hosted/blob/a", "aaaa", h.now.Add(-30*24*time.Hour))
	cached := h.put(t, "p", "blob/b", "bbbb", h.now.Add(-time.Hour))

	result, err := h.svc.Evict(context.Background(), EvictOptions{TargetBytes: 1, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !h.blobs.Exists(pushed) {
		t.Fatal("the pushed content was evicted")
	}
	if h.blobs.Exists(cached) {
		t.Fatal("the cached copy was kept although the store is over its target")
	}
	if result.Pinned != 1 {
		t.Fatalf("result = %+v, want the pushed entry counted as pinned", result)
	}
}
