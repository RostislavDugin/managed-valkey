package api_test

import (
	"testing"
	"time"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
)

type mutableClock struct{ now time.Time }

func (c *mutableClock) Now() time.Time { return c.now }

func Test_AllowRequest_WithSlidingWindow_LimitsEachClientIndependently(t *testing.T) {
	clock := &mutableClock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	limiter := api.NewRateLimiter(clock, 2, time.Minute)

	for range 2 {
		if allowed, _ := limiter.Allow("192.0.2.1"); !allowed {
			t.Fatal("запрос до лимита отклонён")
		}
	}
	if allowed, retry := limiter.Allow("192.0.2.1"); allowed || retry != 60 {
		t.Errorf("allowed=%v retry=%d, ожидались false и 60", allowed, retry)
	}

	clock.now = clock.now.Add(30*time.Second + 500*time.Millisecond)
	if allowed, retry := limiter.Allow("192.0.2.1"); allowed || retry != 30 {
		t.Errorf("allowed=%v retry=%d, ожидались false и 30", allowed, retry)
	}

	clock.now = clock.now.Add(30 * time.Second)
	if allowed, _ := limiter.Allow("192.0.2.1"); !allowed {
		t.Fatal("окно не освободилось")
	}
	if allowed, _ := limiter.Allow("198.51.100.2"); !allowed {
		t.Fatal("другой адрес попал под чужой лимит")
	}
}
