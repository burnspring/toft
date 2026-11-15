package main

import (
	"errors"
	"testing"
)

// A whole URL is not a path of this app, and joining one to the app's address
// made a 404 that read as the app being broken.
func TestFetchRefusesAnAbsoluteURL(t *testing.T) {
	for _, p := range []string{"https://example.com/", "http://x/y", "//example.com/a"} {
		err := refuseAbsoluteURL(p)
		var u *usageError
		if !errors.As(err, &u) || u.code != "invalid_path" {
			t.Errorf("%q: err = %v, want usage invalid_path", p, err)
		}
	}
	for _, p := range []string{"/", "/api/health", "/go?next=https://example.com/"} {
		if err := refuseAbsoluteURL(p); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
}

// -j 0 asked for no transfers, and the run that then copied everything with
// one worker answered a different question.
func TestJobsBelowOneIsRefused(t *testing.T) {
	for _, j := range []int{0, -3} {
		var u *usageError
		if err := refuseJobs(j); !errors.As(err, &u) || u.code != "invalid_jobs" {
			t.Errorf("-j %d: err = %v, want usage invalid_jobs", j, err)
		}
	}
	if err := refuseJobs(1); err != nil {
		t.Errorf("-j 1 refused: %v", err)
	}
}

// --param =x has no name, which is the same typo as a missing =.
func TestParamWithNoNameIsRefused(t *testing.T) {
	var p paramList
	for _, v := range []string{"=x", " =x", "noequals"} {
		if err := p.Set(v); err == nil {
			t.Errorf("--param %q accepted", v)
		}
	}
	if err := p.Set("k=a=b"); err != nil || p.values()["k"] != "a=b" {
		t.Errorf("k=a=b: %v, %v", err, p.values())
	}
}
