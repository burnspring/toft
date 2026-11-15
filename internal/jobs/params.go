package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ParamError refuses a run whose parameters do not satisfy the job's declared
// `params_spec`. Code is `param_required`, `param_type`, `param_choice` or
// `param_unknown`; the
// API answers 400 with each, because the request is wrong and sending it again
// will not help.
//
// Param and Example are what a hint is built from: the parameter at fault and
// a value it would accept, so the refusal can carry a command to paste.
type ParamError struct {
	Code    string
	Msg     string
	Param   string
	Example string
	// Type is the declared type in words ("an int"), on a
	// param_type refusal, so its hint can say what the job takes.
	Type string
	// Hint, when set, is the whole next step — for a name the job does not
	// declare, where "--param name=example" would be the mistake again.
	Hint string
}

func (e *ParamError) Error() string { return e.Msg }

// paramDecl is one entry of a definition's params_spec, as the manifest writes
// it (builder.manifestParam).
type paramDecl struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Default  any    `json:"default"`
	Help     string `json:"help,omitempty"`
	Choices  []any  `json:"choices,omitempty"`
}

// checkParams holds a run's parameters to the job's declaration, and returns
// them typed — every declared parameter in its declared type, every default
// that was not supplied filled in.
//
// A declaration that says `type` and `required` and enforces neither is worse
// than none: a job written against it trusts that `region` is there and is a
// string, and the first run that proves otherwise fails inside the job, far
// from whoever sent it. So a missing required parameter and a value of the
// wrong type are refused at the start, where the caller is still listening.
//
// The CLI's `--param k=v` sends strings only, so an int, float or bool
// parameter accepts a string that parses as one — "5" is an int — and the run
// receives the int. A string parameter accepts only a string. A declared
// `choices` list is a closed set: anything outside it is param_choice. **Once a
// job declares parameters, so is the set of names**: one it does not declare
// is param_unknown. Passed through, `regoin=emea` ran the job with its default
// region and said nothing. A job that declares none takes free-form
// parameters, because the declared types are scalars and a list or an object
// has no other way to travel. A spec that does not parse constrains nothing —
// it was validated when it was written, and refusing every run over it would
// turn a stored oddity into an outage.
func checkParams(spec, params []byte) ([]byte, error) {
	// Free-form means any keys, not any JSON: every SDK reads run.params as an
	// object, so "x", [1] or 5 is refused whether or not the job declares any
	// (BB-081).
	if err := checkParamsObject(params); err != nil {
		return nil, err
	}
	// null is no parameters, and is stored as leaving the field out is — {} —
	// never as a null every SDK would then read as a run without an object
	// (BB-095).
	if bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
		params = nil
	}
	if len(spec) == 0 {
		return params, nil
	}
	var decls map[string]paramDecl
	if err := json.Unmarshal(spec, &decls); err != nil || len(decls) == 0 {
		return params, nil
	}
	values := map[string]any{}
	if len(params) > 0 {
		// Numbers kept as written: decoded to float64, an int above 2^53
		// came back rounded and the job read an id nobody sent (code review
		// 2026-11-11, C-7).
		dec := json.NewDecoder(bytes.NewReader(params))
		dec.UseNumber()
		if err := dec.Decode(&values); err != nil {
			return nil, errParamsNotObject()
		}
		if values == nil {
			values = map[string]any{}
		}
	}

	names := make([]string, 0, len(decls))
	for name := range decls {
		names = append(names, name)
	}
	sort.Strings(names)

	var unknown []string
	for name := range values {
		if _, ok := decls[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, &ParamError{Code: "param_unknown", Param: unknown[0],
			Hint: "toft job list shows the parameters each job declares",
			Msg:  fmt.Sprintf("this job takes no parameter %s — it declares %s", quoteList(unknown), quoteList(names))}
	}

	var missing []string
	for _, name := range names {
		d := decls[name]
		v, ok := values[name]
		if !ok || v == nil {
			if d.Default != nil {
				values[name] = coerceParam(d.Type, d.Default)
				continue
			}
			if d.Required {
				missing = append(missing, name)
			}
			continue
		}
		if !paramTypeOK(d.Type, v) {
			return nil, &ParamError{Code: "param_type", Param: name, Example: exampleFor(d), Type: typeWord(d.Type),
				Msg: fmt.Sprintf("parameter %q must be %s, and %s is not", name, typeWord(d.Type), describeValue(v))}
		}
		typed := coerceParam(d.Type, v)
		if len(d.Choices) > 0 && !inChoices(typed, d.Choices) {
			return nil, &ParamError{Code: "param_choice", Param: name, Example: exampleFor(d),
				Msg: fmt.Sprintf("parameter %q must be one of %s, and %s is not",
					name, describeChoices(d.Choices), describeValue(v))}
		}
		values[name] = typed
	}
	if len(missing) > 0 {
		return nil, &ParamError{Code: "param_required", Param: missing[0], Example: exampleFor(decls[missing[0]]),
			Msg: fmt.Sprintf("this job requires the parameter %s, and the run was not given it", quoteList(missing))}
	}
	out, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func paramTypeOK(typ string, v any) bool {
	switch typ {
	case "", "string":
		_, ok := v.(string)
		return ok
	case "int":
		switch x := v.(type) {
		case json.Number:
			_, err := strconv.ParseInt(x.String(), 10, 64)
			return err == nil
		case float64:
			return x == float64(int64(x))
		case string:
			_, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			return err == nil
		}
		return false
	case "float":
		// Finite only: strconv reads "NaN" and "Inf", json.Marshal refuses
		// them, and the start answered 500 (code review 2026-11-11, C-8).
		switch x := v.(type) {
		case json.Number:
			f, err := strconv.ParseFloat(x.String(), 64)
			return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
		case float64:
			return !math.IsNaN(x) && !math.IsInf(x, 0)
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
		}
		return false
	case "bool":
		switch x := v.(type) {
		case bool:
			return true
		case string:
			_, err := strconv.ParseBool(strings.TrimSpace(x))
			return err == nil
		}
		return false
	}
	// A type the manifest would have refused. Nothing to hold the value to.
	return true
}

func typeWord(typ string) string {
	switch typ {
	case "int":
		return "an int"
	case "float":
		return "a float"
	case "bool":
		return "a bool"
	}
	return "a string"
}

func describeValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "that value"
	}
	if len(b) > 40 {
		b = append(b[:37], "..."...)
	}
	return string(b)
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	return strings.Join(q, ", ")
}

// coerceParam turns a value that passed paramTypeOK into the declared type: the
// CLI's "5" becomes 5, so the job reads what its declaration promised.
func coerceParam(typ string, v any) any {
	if n, ok := v.(json.Number); ok {
		switch typ {
		case "int":
			if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
				return i
			}
		case "float":
			if f, err := strconv.ParseFloat(n.String(), 64); err == nil {
				return floatParam(f)
			}
		}
		return n
	}
	if typ == "float" {
		switch x := v.(type) {
		case float64:
			return floatParam(x)
		case int64:
			return floatParam(x)
		}
	}
	str, isStr := v.(string)
	if !isStr {
		return v
	}
	str = strings.TrimSpace(str)
	switch typ {
	case "int":
		if n, err := strconv.ParseInt(str, 10, 64); err == nil {
			return n
		}
	case "float":
		if f, err := strconv.ParseFloat(str, 64); err == nil {
			return floatParam(f)
		}
	case "bool":
		if b, err := strconv.ParseBool(str); err == nil {
			return b
		}
	}
	return v
}

// floatParam is a declared float, encoded so it reads back as one: Go
// writes float64(2) as `2`, so `--param ratio=2` reached a Python job as the
// int 2 (BB-024). A whole value keeps its ".0"; Postgres jsonb keeps the
// scale it was given, so the job reads 2.0.
type floatParam float64

func (f floatParam) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(float64(f))
	if err != nil {
		return nil, err
	}
	if !bytes.ContainsAny(b, ".eE") {
		b = append(b, '.', '0')
	}
	return b, nil
}

// inChoices compares through JSON, so 3 and 3.0 and the int64 a coercion made
// are the same choice.
func inChoices(v any, choices []any) bool {
	want, _ := json.Marshal(normalizeNumber(v))
	for _, c := range choices {
		got, _ := json.Marshal(normalizeNumber(c))
		if string(got) == string(want) {
			return true
		}
	}
	return false
}

func normalizeNumber(v any) any {
	switch x := v.(type) {
	case floatParam:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return v
}

func describeChoices(choices []any) string {
	parts := make([]string, len(choices))
	for i, c := range choices {
		parts[i] = describeValue(c)
	}
	return strings.Join(parts, ", ")
}

// exampleFor is a value the declaration would accept, for a hint: the first
// choice, the default, or a placeholder of the right type.
func exampleFor(d paramDecl) string {
	pick := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	switch {
	case len(d.Choices) > 0:
		return pick(d.Choices[0])
	case d.Default != nil:
		return pick(d.Default)
	}
	switch d.Type {
	case "int":
		return "1"
	case "float":
		return "1.5"
	case "bool":
		return "true"
	}
	return "<value>"
}

// checkParamsObject refuses parameters that are JSON but not an object. Absent
// and null are no parameters.
func checkParamsObject(params []byte) error {
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] == '{' {
		return nil
	}
	return errParamsNotObject()
}

func errParamsNotObject() error {
	return &ParamError{Code: "param_type",
		Msg:  `parameters must be a JSON object, like {"region": "emea"}`,
		Hint: `send "params" as an object of names to values, or leave it out`}
}
