package deploysteps

import "testing"

// Every step in Order has a phase. A step added to one and not the other would
// report an empty phase to every client watching a deploy — which is the same
// answer a pre-columns row gives, so nothing would look broken while a whole
// stage went missing from the progress display.
func TestEveryStepHasAPhase(t *testing.T) {
	for _, s := range Order {
		if phaseOf[s] == "" {
			t.Errorf("step %q is in Order but has no phase", s)
		}
	}
	for s := range phaseOf {
		if Index(string(s)) == -1 {
			t.Errorf("step %q has a phase but is not in Order", s)
		}
	}
}

// Phases are contiguous in Order. This is the property the whole model rests
// on: a progress bar, a stage report and a "which phase failed" answer all
// assume a deploy enters each phase once and never returns to it.
func TestPhasesAreContiguous(t *testing.T) {
	seen := map[Phase]bool{}
	var current Phase
	for _, s := range Order {
		p := phaseOf[s]
		if p == current {
			continue
		}
		if seen[p] {
			t.Fatalf("phase %q resumes at step %q after another phase intervened", p, s)
		}
		seen[p] = true
		current = p
	}
	for _, p := range Phases() {
		if !seen[p] {
			t.Errorf("phase %q is declared but no step belongs to it", p)
		}
	}
}

// Phases() is the order they actually occur in, not an arbitrary list.
func TestPhasesAreInOrder(t *testing.T) {
	want := Phases()
	var got []Phase
	var current Phase
	for _, s := range Order {
		if p := phaseOf[s]; p != current {
			got = append(got, p)
			current = p
		}
	}
	if len(got) != len(want) {
		t.Fatalf("Order passes through %v; Phases() says %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Order passes through %v; Phases() says %v", got, want)
		}
	}
}

// An unknown step is not an error and is not a phase. Rows written before the
// columns existed, and rows written by a newer binary mid-upgrade, both land
// here.
func TestUnknownStepHasNoPhase(t *testing.T) {
	for _, s := range []string{"", "verify", "Setup"} {
		if p := PhaseOf(s); p != "" {
			t.Errorf("PhaseOf(%q) = %q, want empty", s, p)
		}
		if i := Index(s); i != -1 {
			t.Errorf("Index(%q) = %d, want -1", s, i)
		}
	}
}

// The conditional steps are the two that only a bundle upload performs. If a
// step ever becomes conditional it must also be one a git deploy can skip
// without the timeline claiming it happened.
func TestConditionalStepsAreTheUploadAndServiceOnes(t *testing.T) {
	if !Conditional(string(Receive)) || !Conditional(string(Unpack)) {
		t.Error("receiving and unpacking a bundle are conditional steps")
	}
	if !Conditional(string(Start)) || !Conditional(string(Ready)) {
		t.Error("starting and waiting on a service are conditional: a batch deploy has neither")
	}
	for _, s := range []Step{Setup, Fetch, Scan, Build, Flip} {
		if Conditional(string(s)) {
			t.Errorf("step %q happens on every deploy and must not be conditional", s)
		}
	}
}

// Every step classifies. A step in Order with no default reason would report an
// empty one on every failure at that step — which is the same answer a row
// written before the columns existed gives, so nothing would look broken while
// a whole class of failure went unclassified.
func TestEveryStepHasAReason(t *testing.T) {
	for _, s := range Order {
		if reasonOf[s] == "" {
			t.Errorf("step %q is in Order but has no default reason", s)
		}
	}
	for s := range reasonOf {
		if Index(string(s)) == -1 {
			t.Errorf("step %q has a reason but is not in Order", s)
		}
	}
}

// Reasons() is the whole vocabulary. A constant missing from it would be a
// reason the builder can write and no client can recognise — KnownReason would
// call the install's own answer unknown.
func TestReasonsListsEveryReason(t *testing.T) {
	listed := map[Reason]bool{}
	for _, r := range Reasons() {
		if listed[r] {
			t.Errorf("reason %q is listed twice", r)
		}
		listed[r] = true
		if !KnownReason(string(r)) {
			t.Errorf("reason %q is listed but KnownReason says otherwise", r)
		}
	}
	for _, r := range reasonOf {
		if !listed[r] {
			t.Errorf("step default %q is not in Reasons()", r)
		}
	}
}

// The two answers an unknown key gets, and they are deliberately different:
// an unrecognised step classifies as nothing, and an unrecognised reason is
// simply not known. Neither is an internal error, which is a claim about the
// platform that a newer install's vocabulary does not justify.
func TestUnknownKeysClassifyAsNothing(t *testing.T) {
	if got := ReasonOf("teleport"); got != "" {
		t.Errorf("ReasonOf(unknown) = %q, want empty", got)
	}
	if got := ReasonOf(""); got != "" {
		t.Errorf("ReasonOf(empty) = %q, want empty", got)
	}
	if KnownReason("teleport") || KnownReason("") {
		t.Error("KnownReason accepted a key that is not in the vocabulary")
	}
}
