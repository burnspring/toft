package database

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The isolated sleep window's defaults: an evening-to-morning night, which is
// the shape somebody turning the window on most often wants. Stored whether or
// not the window is on, so switching it on starts from a sensible night.
const (
	DefaultSleepWindowStart = "19:00"
	DefaultSleepWindowEnd   = "07:00"
)

// ParseClock reads "HH:MM" (24-hour) as minutes after midnight.
func ParseClock(v string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, fmt.Errorf("%q is not a time of day; use HH:MM, such as 19:00", v)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%q is not a time of day; use HH:MM, such as 19:00", v)
	}
	return hh*60 + mm, nil
}

// SleepWindowLocation is the zone the window is read in: the install's display
// zone, or UTC when it has none. The display zone is the one every exact time
// on the install is printed in (ADR §154), so the window an administrator
// types is the one the pages show back.
func (s PlatformSettings) SleepWindowLocation() *time.Location {
	if s.DisplayTimeZone != "" {
		if loc, err := time.LoadLocation(s.DisplayTimeZone); err == nil {
			return loc
		}
	}
	return time.UTC
}

// IsolatedSleepAllowedAt reports whether an idle isolated app may be put to
// sleep at now: always while the window is off, and only inside it while it
// is on. The window is [start, end) and crosses midnight when end < start.
// Bounds that cannot be read allow nothing — a window nobody can state must
// not sleep apps at hours nobody chose.
func (s PlatformSettings) IsolatedSleepAllowedAt(now time.Time) bool {
	if !s.IsolatedSleepWindowEnabled {
		return true
	}
	start, err1 := ParseClock(s.IsolatedSleepWindowStart)
	end, err2 := ParseClock(s.IsolatedSleepWindowEnd)
	if err1 != nil || err2 != nil || start == end {
		return false
	}
	local := now.In(s.SleepWindowLocation())
	at := local.Hour()*60 + local.Minute()
	if start < end {
		return at >= start && at < end
	}
	return at >= start || at < end
}
