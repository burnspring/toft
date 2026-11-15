package integrations

import (
	"context"
	"testing"
	"time"
)

// A token is handed out from the cache only while it has more than the margin
// left, so whoever receives one always has ten minutes on it.
func TestACachedTokenAlwaysHasTheMarginLeft(t *testing.T) {
	c := NewTokenCache()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	mints := 0
	mint := func(context.Context) (AccessToken, error) {
		mints++
		return AccessToken{Value: "t", ExpiresAt: now.Add(time.Hour)}, nil
	}

	for _, step := range []struct {
		at    time.Duration
		mints int
	}{
		{0, 1},
		{49 * time.Minute, 1},
		{50 * time.Minute, 2}, // exactly the margin left: replaced
	} {
		now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC).Add(step.at)
		tok, err := c.Get(context.Background(), "k", mint)
		if err != nil {
			t.Fatal(err)
		}
		if mints != step.mints {
			t.Errorf("at +%v: %d mints, want %d", step.at, mints, step.mints)
		}
		if tok.ExpiresAt.Sub(now) <= TokenMargin {
			t.Errorf("at +%v: handed out a token with %v left", step.at, tok.ExpiresAt.Sub(now))
		}
	}
}

func TestTheKeyChangesWithEveryPart(t *testing.T) {
	if TokenKey("a", "b") == TokenKey("a", "c") || TokenKey("ab", "") == TokenKey("a", "b") {
		t.Error("two different credentials share a key")
	}
}
