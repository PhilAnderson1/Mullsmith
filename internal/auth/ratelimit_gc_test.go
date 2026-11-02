package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiterGarbageCollectionPreservesUnreplenishedBudgets(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(nil)
	l.buckets = map[string]*bucket{
		"hourly exhausted": {tokens: 0, cap: 3, refill: 3.0 / 3600, last: now.Add(-11 * time.Minute)},
		"hourly partial":   {tokens: 2, cap: 3, refill: 3.0 / 3600, last: now.Add(-11 * time.Minute)},
		"hourly restored":  {tokens: 0, cap: 3, refill: 3.0 / 3600, last: now.Add(-time.Hour)},
		"minute restored":  {tokens: 0, cap: 10, refill: 10.0 / 60, last: now.Add(-time.Minute)},
	}
	l.gcLocked(now)
	for _, key := range []string{"hourly exhausted", "hourly partial"} {
		if l.buckets[key] == nil {
			t.Errorf("cleanup reset %s budget before replenishment", key)
		}
	}
	for _, key := range []string{"hourly restored", "minute restored"} {
		if l.buckets[key] != nil {
			t.Errorf("cleanup retained fully replenished %s budget", key)
		}
	}
}

func TestLimiterAutomaticCleanupPreservesHourlyBudget(t *testing.T) {
	l := NewLimiter(nil)
	for i := 0; i < 3; i++ {
		if !l.Allow("hourly", 3, time.Hour) {
			t.Fatal("initial allowance refused")
		}
	}
	// Advance this bucket's idle time without sleeping, then trigger ordinary GC.
	l.buckets["hourly"].last = time.Now().Add(-11 * time.Minute)
	for i := 0; i < gcThreshold; i++ {
		l.buckets[fmt.Sprint("other-", i)] = &bucket{tokens: 0, cap: 1, refill: 1.0 / 60, last: time.Now()}
	}
	if !l.Allow("trigger", 1, time.Minute) {
		t.Fatal("cleanup trigger refused")
	}
	if l.Allow("hourly", 3, time.Hour) {
		t.Fatal("automatic cleanup granted a fresh hourly allowance")
	}
}
