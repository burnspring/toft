package jobs

import (
	"strings"
	"encoding/json"
	"errors"
	"testing"
)

func TestCheckParams(t *testing.T) {
	spec := `{"region":{"type":"string","required":true},
	          "rows":{"type":"int"},
	          "ratio":{"type":"float"},
	          "dry":{"type":"bool","default":false}}`

	cases := []struct {
		name   string
		params string
		code   string // "" is accepted
	}{
		{"nothing given", ``, "param_required"},
		{"empty object", `{}`, "param_required"},
		{"null is absent", `{"region":null}`, "param_required"},
		{"present", `{"region":"emea"}`, ""},
		{"string param given a number", `{"region":5}`, "param_type"},
		{"string param given a numeric string", `{"region":"5"}`, ""},
		{"int as a number", `{"region":"x","rows":500}`, ""},
		{"int as the CLI's string", `{"region":"x","rows":"500"}`, ""},
		{"int given a fraction", `{"region":"x","rows":1.5}`, "param_type"},
		{"int given a word", `{"region":"x","rows":"many"}`, "param_type"},
		{"float as a string", `{"region":"x","ratio":"0.25"}`, ""},
		{"bool as the CLI's string", `{"region":"x","dry":"true"}`, ""},
		{"bool given a word", `{"region":"x","dry":"maybe"}`, "param_type"},
		{"undeclared is refused by name", `{"region":"x","extra":[1,2]}`, "param_unknown"},
		// C-8: strconv reads NaN and Inf, json.Marshal refuses them, and the
		// start was a 500.
		{"float given NaN", `{"region":"x","ratio":"NaN"}`, "param_type"},
		{"float given Inf", `{"region":"x","ratio":"+Inf"}`, "param_type"},
		{"not an object", `[1]`, "param_type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := checkParams([]byte(spec), []byte(c.params))
			var pe *ParamError
			switch {
			case c.code == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.code != "" && !errors.As(err, &pe):
				t.Fatalf("accepted, want %s (err %v)", c.code, err)
			case c.code != "" && pe.Code != c.code:
				t.Fatalf("code %q, want %q (%s)", pe.Code, c.code, pe.Msg)
			}
		})
	}
}

func TestCheckParams_FillsDefaults(t *testing.T) {
	spec := `{"region":{"type":"string","required":true,"default":"emea"},"dry":{"type":"bool","default":true}}`
	out, err := checkParams([]byte(spec), nil)
	if err != nil {
		t.Fatalf("a required parameter with a default was refused: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["region"] != "emea" || got["dry"] != true {
		t.Fatalf("defaults not filled: %s", out)
	}

	// A supplied value wins over the default.
	in := []byte(`{"region":"apac","dry":false}`)
	out, err = checkParams([]byte(spec), in)
	if err != nil || string(out) != `{"dry":false,"region":"apac"}` {
		t.Fatalf("got %s, %v; want the supplied values", out, err)
	}
}

// A typo'd name on a job that declares parameters is refused, naming what it
// does declare — passed through, it ran with the default (jobs-regression #10).
func TestCheckParams_AnUndeclaredNameIsRefused(t *testing.T) {
	_, err := checkParams([]byte(`{"region":{"type":"string","default":"emea"}}`), []byte(`{"regoin":"apac"}`))
	var pe *ParamError
	if !errors.As(err, &pe) || pe.Code != "param_unknown" || pe.Param != "regoin" || pe.Hint == "" ||
		pe.Msg != `this job takes no parameter "regoin" — it declares "region"` {
		t.Fatalf("got %#v", err)
	}
}

// A job that declares none takes anything: the declared types are scalars,
// and a list or an object travels only this way.
func TestCheckParams_NoSpecConstrainsNothing(t *testing.T) {
	for _, spec := range []string{``, `{}`, `not json`} {
		in := []byte(`{"anything":5}`)
		out, err := checkParams([]byte(spec), in)
		if err != nil || string(out) != string(in) {
			t.Fatalf("spec %q: got %s, %v", spec, out, err)
		}
	}
}

// A value of the wrong type carries the declared type, so its hint can name it
// rather than borrow param_choice's (docs/bugs 2026-10-28 #7).
func TestAParamTypeRefusalNamesTheType(t *testing.T) {
	_, err := checkParams([]byte(`{"rows":{"type":"int"}}`), []byte(`{"rows":"lots"}`))
	var pe *ParamError
	if !errors.As(err, &pe) || pe.Code != "param_type" || pe.Type != "an int" || pe.Param != "rows" {
		t.Fatalf("err = %+v, want param_type on rows naming an int", pe)
	}
}

// BB-081: a job declaring no params takes any keys, but params are still an
// object — "x", [1] and 5 are param_type, with a hint naming no parameter.
func TestCheckParams_FreeFormIsStillAnObject(t *testing.T) {
	for _, p := range []string{`"x"`, `[1]`, `5`, `true`} {
		_, err := checkParams(nil, []byte(p))
		var pe *ParamError
		if !errors.As(err, &pe) || pe.Code != "param_type" || pe.Hint == "" || pe.Param != "" {
			t.Errorf("%s: %v, want a hinted param_type", p, err)
		}
	}
	for _, p := range []string{``, `null`, `{}`, ` {"a":[1]}`} {
		if _, err := checkParams(nil, []byte(p)); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
}

// BB-095: "params": null is no parameters — stored as {} like leaving it out,
// declared params or not.
func TestNullParamsAreNoParams(t *testing.T) {
	for _, spec := range []string{"", `{"region":{"type":"string","default":"emea"}}`} {
		out, err := checkParams([]byte(spec), []byte("null"))
		if err != nil {
			t.Fatalf("spec %q: %v", spec, err)
		}
		if string(out) == "null" {
			t.Errorf("spec %q: params stored as null", spec)
		}
	}
}

// An int above 2^53 goes through as written: decoded to float64 it came back
// rounded, and the job read an id nobody sent (code review 2026-11-11, C-7).
func TestABigIntParamKeepsItsDigits(t *testing.T) {
	spec := `{"id":{"type":"int"},"ratio":{"type":"float"}}`
	out, err := checkParams([]byte(spec), []byte(`{"id":1234567890123456789,"ratio":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "1234567890123456789") {
		t.Errorf("params = %s, want the id's digits intact", out)
	}
	if !strings.Contains(string(out), "2.0") {
		t.Errorf("params = %s, want the float as 2.0", out)
	}
}
