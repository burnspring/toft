package procstat

import "testing"

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if d := got - want; d > tol || d < -tol {
		t.Errorf("%s = %v, want %v (±%v)", name, got, want, tol)
	}
}

func TestParse(t *testing.T) {
	// A real-shaped line: pid, comm, then 50 fields. utime=1000, stime=500
	// (field 14/15), rss=2048 pages (field 24).
	fields := make([]string, 50)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"     // state, the first field after comm
	fields[11] = "1000" // utime
	fields[12] = "500"  // stime
	fields[21] = "2048" // rss in pages

	line := "4242 (toft-app) " + joinFields(fields)
	got, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	approx(t, "cpuSeconds", got.CPUSeconds, 15, 0.001) // (1000+500)/100
	if want := int64(2048) * pageSize; got.RSSBytes != want {
		t.Errorf("rssBytes = %d, want %d", got.RSSBytes, want)
	}
}

// A process whose name contains spaces and parentheses shifts every field if
// the line is tokenised from the start. Getting this wrong yields plausible
// numbers that are silently about the wrong column.
func TestParseHandlesAwkwardProcessNames(t *testing.T) {
	fields := make([]string, 50)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[11] = "300"
	fields[12] = "200"
	fields[21] = "1024"

	line := "4242 (my app) (weird)) " + joinFields(fields)
	got, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	approx(t, "cpuSeconds", got.CPUSeconds, 5, 0.001)
	if want := int64(1024) * pageSize; got.RSSBytes != want {
		t.Errorf("rssBytes = %d, want %d", got.RSSBytes, want)
	}
}

func TestParseRejectsShortLines(t *testing.T) {
	for _, line := range []string{"", "4242", "4242 (app) S 1 2 3"} {
		if _, err := Parse(line); err == nil {
			t.Errorf("%q was accepted", line)
		}
	}
}

func joinFields(f []string) string {
	out := ""
	for i, s := range f {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
