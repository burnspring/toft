package builder

import (
	"strconv"
	"strings"
	"time"
)

// Line times (ADR §155): when each line of a build log was written, kept beside
// the text rather than in it. The text is what `toft logs --build`, a download,
// the raw view and the failure tail all read, and a prefix on every line would
// have changed all four; a column of the same length changes none of them.
//
// One time per *line of the text*, in order: an entry handed to appendLog that
// holds a newline is several lines, each stamped with the entry's time. Lines
// appended to the row after the build (the restart sweep's ERROR) have none,
// and a reader pairs times with lines from the top, so a shorter list leaves
// only the tail unmarked — never a time on the wrong line.

const lineTimesVersion = "v1"

// LineTimes is the decoded form: every line's time, as milliseconds after
// StartMS (Unix milliseconds). JSON-shaped for the log routes.
type LineTimes struct {
	StartMS   int64   `json:"start_ms"`
	OffsetsMS []int64 `json:"offsets_ms"`
}

// EncodeLineTimes writes "v1:<start ms>:<offset>,<offset>,…" for entries and
// their times, index for index. Empty when there is nothing to say.
func EncodeLineTimes(entries []string, at []time.Time) string {
	n := len(entries)
	if len(at) < n {
		n = len(at)
	}
	if n == 0 {
		return ""
	}
	start := at[0].UnixMilli()
	var sb strings.Builder
	sb.WriteString(lineTimesVersion)
	sb.WriteByte(':')
	sb.WriteString(strconv.FormatInt(start, 10))
	sb.WriteByte(':')
	first := true
	for i := 0; i < n; i++ {
		off := strconv.FormatInt(at[i].UnixMilli()-start, 10)
		for range strings.Count(entries[i], "\n") + 1 {
			if !first {
				sb.WriteByte(',')
			}
			sb.WriteString(off)
			first = false
		}
	}
	return sb.String()
}

// DecodeLineTimes reads what EncodeLineTimes wrote. nil for an empty or
// unreadable value — a log with no times, which is every log before this.
func DecodeLineTimes(enc string) *LineTimes {
	parts := strings.SplitN(enc, ":", 3)
	if len(parts) != 3 || parts[0] != lineTimesVersion {
		return nil
	}
	start, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil
	}
	fields := strings.Split(parts[2], ",")
	offs := make([]int64, 0, len(fields))
	for _, f := range fields {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil
		}
		offs = append(offs, v)
	}
	return &LineTimes{StartMS: start, OffsetsMS: offs}
}

// StampLines expands entries into lines, each paired with its entry's time —
// the live socket's backlog when a reader asks for times.
func StampLines(entries []string, at []time.Time) (lines []string, ms []int64) {
	for i, e := range entries {
		var t int64 = -1
		if i < len(at) {
			t = at[i].UnixMilli()
		}
		for _, l := range strings.Split(e, "\n") {
			lines = append(lines, l)
			ms = append(ms, t)
		}
	}
	return lines, ms
}
