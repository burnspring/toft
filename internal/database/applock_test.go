package database

import (
	"testing"
	"time"
)

// ResolveLockMessage is the fallback chain, and it is the piece most likely to
// be got subtly wrong: it is read from the proxy, the API and the UI, and the
// failure mode is a visitor refused with a blank explanation — the one outcome
// this whole family of messages exists to prevent.

func TestResolveLockMessage(t *testing.T) {
	const installWide = "Ask #it-help before using anything locked."

	cases := []struct {
		name string
		app  *App
		set  PlatformSettings
		want string
	}{
		{
			// The person closest to the decision wins. Only whoever locked this
			// particular app knows it was decommissioned.
			name: "the app's own message wins",
			app:  &App{LockedMessage: "Decommissioned — use Finance Hub."},
			set:  PlatformSettings{AppLockedMessage: installWide},
			want: "Decommissioned — use Finance Hub.",
		},
		{
			name: "no app message falls back to the install's",
			app:  &App{},
			set:  PlatformSettings{AppLockedMessage: installWide},
			want: installWide,
		},
		{
			name: "neither falls back to the built-in sentence",
			app:  &App{},
			set:  PlatformSettings{},
			want: DefaultAppLockedMessage,
		},
		{
			// Whitespace is not a message. Somebody who cleared the box and
			// left a newline has said nothing, and the reader must still be
			// told something.
			name: "whitespace is a gap, not a value",
			app:  &App{LockedMessage: "   \n "},
			set:  PlatformSettings{AppLockedMessage: "  "},
			want: DefaultAppLockedMessage,
		},
		{
			// The proxy resolves before it has an app in some error paths;
			// returning "" there would be the blank refusal.
			name: "a nil app still gets a sentence",
			app:  nil,
			set:  PlatformSettings{},
			want: DefaultAppLockedMessage,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveLockMessage(tc.app, tc.set); got != tc.want {
				t.Errorf("ResolveLockMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// Locked is asked at every enforcement point, so it has to be safe on the
// zero value and on nil — a caller that had to nil-check first would eventually
// be a caller that forgot to.
func TestAppLocked(t *testing.T) {
	var nilApp *App
	if nilApp.Locked() {
		t.Error("a nil app reported locked")
	}
	if (&App{}).Locked() {
		t.Error("an app with no locked_at reported locked")
	}
	now := time.Now().UTC()
	if !(&App{LockedAt: &now}).Locked() {
		t.Error("an app with locked_at reported unlocked")
	}
}
