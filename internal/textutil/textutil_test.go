package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	cases := map[string]string{
		"plain":        "plain",
		"a\x00b":       "a�b",
		"bad\xffbyte":  "bad�byte",
		"é ok":         "é ok",
		"\x00\xff\x00": "���",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClipCutsOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // two bytes each
	for n := 0; n <= len(s)+1; n++ {
		got := Clip(s, n)
		if !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("Clip(_, %d) = %q (len %d): invalid or too long", n, got, len(got))
		}
		if n >= 1 && len(got) < n-1 {
			t.Fatalf("Clip(_, %d) lost more than the straddling rune: len %d", n, len(got))
		}
	}
	if got := Clip("a\x00bc", 3); got != "a�b" {
		t.Errorf("Clip strips NUL: got %q", got)
	}
}

func TestClipTailStartsOnARuneBoundary(t *testing.T) {
	s := "x" + strings.Repeat("€", 5) // three bytes each
	for n := 0; n <= len(s)+1; n++ {
		got := ClipTail(s, n)
		if !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("ClipTail(_, %d) = %q: invalid or too long", n, got)
		}
		if !strings.HasSuffix(s, got) {
			t.Fatalf("ClipTail(_, %d) = %q is not a suffix", n, got)
		}
	}
}
