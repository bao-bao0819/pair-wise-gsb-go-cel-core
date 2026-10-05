// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cel_test

import (
	"errors"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

func TestLabels(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("secret", cel.StringType),
		cel.Variable("publicA", cel.StringType),
		cel.Variable("publicB", cel.StringType),
		cel.Variable("user", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("items", cel.ListType(cel.StringType)),
	)
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}

	lp, err := cel.NewLabelProgram()
	if err != nil {
		t.Fatalf("NewLabelProgram: %v", err)
	}

	tests := []struct {
		name   string
		expr   string
		input  map[string]any
		labels cel.LabelInput
		want   []string
	}{
		{
			name:   "scalar propagation",
			expr:   `secret + "!"`,
			input:  map[string]any{"secret": "shh"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{"confidential"},
		},
		{
			name:   "untouched public value",
			expr:   `publicA + "!"`,
			input:  map[string]any{"publicA": "hi"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{},
		},
		{
			name:   "control dependence true branch",
			expr:   `secret == "x" ? publicA : publicB`,
			input:  map[string]any{"secret": "nope", "publicA": "a", "publicB": "b"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{"confidential"},
		},
		{
			name:   "control dependence via field path",
			expr:   `user.role == "admin" ? publicA : publicB`,
			input:  map[string]any{"user": map[string]string{"role": "user"}, "publicA": "a", "publicB": "b"},
			labels: cel.LabelInput{"user.role": []string{"secret-role"}},
			want:   []string{"secret-role"},
		},
		{
			name:   "field selection data dependence",
			expr:   `user.email`,
			input:  map[string]any{"user": map[string]string{"email": "a@b.c"}},
			labels: cel.LabelInput{"user.email": []string{"pii"}},
			want:   []string{"pii"},
		},
		{
			name:   "list and map construction union",
			expr:   `[publicA, secret]`,
			input:  map[string]any{"publicA": "a", "secret": "s"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{"confidential"},
		},
		{
			name:   "short circuit or does not evaluate unneeded operand",
			expr:   `true || secret == "x"`,
			input:  map[string]any{"secret": "s"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{},
		},
		{
			name:   "short circuit and labels evaluated operand",
			expr:   `secret == "s" && true`,
			input:  map[string]any{"secret": "s"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{"confidential"},
		},
		{
			name:   "comprehension filter control dependence",
			expr:   `items.filter(x, x == secret).size()`,
			input:  map[string]any{"items": []string{"a", "b"}, "secret": "a"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{"confidential"},
		},
		{
			name:   "comprehension map data dependence",
			expr:   `items.map(x, x + secret)`,
			input:  map[string]any{"items": []string{"a"}, "secret": "z"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{"confidential"},
		},
		{
			name:   "comprehension variable shadows outer name",
			expr:   `[{"secret": "inner"}].map(secret, secret.secret)[0]`,
			input:  map[string]any{"secret": "outer"},
			labels: cel.LabelInput{"secret": []string{"outer-label"}},
			want:   []string{},
		},
		{
			name:   "exists macro control label",
			expr:   `items.exists(x, x == secret)`,
			input:  map[string]any{"items": []string{"a"}, "secret": "a"},
			labels: cel.LabelInput{"secret": []string{"confidential"}},
			want:   []string{"confidential"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ast, iss := env.Compile(tc.expr)
			if iss.Err() != nil {
				t.Fatalf("compile: %v", iss.Err())
			}
			prg, err := env.Program(ast, cel.LabelTracking(lp))
			if err != nil {
				t.Fatalf("program: %v", err)
			}
			res, err := cel.EvalLabeled(prg, tc.input, tc.labels)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			got := res.Labels.Slice()
			if !equalStringSlices(got, tc.want) {
				t.Fatalf("labels = %v, want %v (value %v)", got, tc.want, res.Value)
			}
		})
	}
}

func TestLabelsSameProgramTwoInputs(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("a", cel.StringType),
		cel.Variable("b", cel.StringType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(`a + b`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	lp, _ := cel.NewLabelProgram()
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}

	r1, err := cel.EvalLabeled(prg,
		map[string]any{"a": "1", "b": "2"},
		cel.LabelInput{"a": []string{"LA"}})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := cel.EvalLabeled(prg,
		map[string]any{"a": "3", "b": "4"},
		cel.LabelInput{"b": []string{"LB"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r1.Labels.Slice(); !equalStringSlices(got, []string{"LA"}) {
		t.Fatalf("first eval labels %v", got)
	}
	if got := r2.Labels.Slice(); !equalStringSlices(got, []string{"LB"}) {
		t.Fatalf("second eval labels %v, state leaked across evaluations", got)
	}
}

func TestLabelsSinkPolicyRunsBeforeBody(t *testing.T) {
	var calls int
	env, err := cel.NewEnv(
		cel.Variable("secret", cel.StringType),
		cel.Function("exfil",
			cel.Overload("exfil_string", []*cel.Type{cel.StringType}, cel.StringType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					calls++
					return v
				}),
			),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(`exfil(secret)`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}

	lp, err := cel.NewLabelProgram(
		cel.SinkFunction(func(c cel.SinkCall) error {
			if c.Labels.Contains("confidential") {
				return errors.New("policy denied confidential egress")
			}
			return nil
		}, "exfil_string", "exfil"),
	)
	if err != nil {
		t.Fatal(err)
	}
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}

	// Denied: body must not run.
	_, evalErr := cel.EvalLabeled(prg,
		map[string]any{"secret": "shh"},
		cel.LabelInput{"secret": []string{"confidential"}})
	if evalErr == nil || !strings.Contains(evalErr.Error(), "rejected") {
		t.Fatalf("expected rejection error, got %v", evalErr)
	}
	if calls != 0 {
		t.Fatalf("sink body invoked %d times despite rejection", calls)
	}

	// Allowed: body runs and value is preserved.
	res, err := cel.EvalLabeled(prg,
		map[string]any{"secret": "ok"},
		cel.LabelInput{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("sink body invoked %d times, want 1", calls)
	}
	if res.Value.Value() != "ok" {
		t.Fatalf("value = %v, want ok", res.Value)
	}
}

func TestLabelsSanitizer(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("secret", cel.StringType),
		cel.Function("redact",
			cel.Overload("redact_string", []*cel.Type{cel.StringType}, cel.StringType,
				cel.UnaryBinding(func(v ref.Val) ref.Val { return types.String("REDACTED") }),
			),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(`redact(secret) + "!"`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	lp, err := cel.NewLabelProgram(cel.SanitizingFunction("redact_string"))
	if err != nil {
		t.Fatal(err)
	}
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cel.EvalLabeled(prg,
		map[string]any{"secret": "shh"},
		cel.LabelInput{"secret": []string{"confidential"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Labels.Slice(); len(got) != 0 {
		t.Fatalf("sanitized result still labeled %v", got)
	}
}

func TestLabelsStrictModeRejectsUndeclaredExtension(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("secret", cel.StringType),
		cel.Function("mystery",
			cel.Overload("mystery_string", []*cel.Type{cel.StringType}, cel.StringType,
				cel.UnaryBinding(func(v ref.Val) ref.Val { return v }),
			),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(`mystery(secret)`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	lp, err := cel.NewLabelProgram(cel.StrictLabelMode())
	if err != nil {
		t.Fatal(err)
	}
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}
	_, evalErr := cel.EvalLabeled(prg,
		map[string]any{"secret": "s"},
		cel.LabelInput{"secret": []string{"c"}})
	if evalErr == nil || !strings.Contains(evalErr.Error(), "undeclared extension function") {
		t.Fatalf("expected strict rejection, got %v", evalErr)
	}
}

func TestLabelsValueAndErrorSemantics(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("secret", cel.IntType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(`secret / 0`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	lp, _ := cel.NewLabelProgram()
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}
	_, evalErr := cel.EvalLabeled(prg,
		map[string]any{"secret": int64(1)},
		cel.LabelInput{"secret": []string{"c"}})
	if evalErr == nil || !strings.Contains(evalErr.Error(), "division") {
		t.Fatalf("expected division error semantics preserved, got %v", evalErr)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLabelsUndeclaredExtensionPropagatesByDefault(t *testing.T) {
	var calls int
	env, err := cel.NewEnv(
		cel.Variable("secret", cel.StringType),
		cel.Function("echo",
			cel.Overload("echo_string", []*cel.Type{cel.StringType}, cel.StringType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					calls++
					return v
				}),
			),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(`echo(secret)`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	// Non-strict mode: no declaration needed, argument labels propagate and the body runs.
	lp, _ := cel.NewLabelProgram()
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cel.EvalLabeled(prg,
		map[string]any{"secret": "s"},
		cel.LabelInput{"secret": []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("body calls = %d, want 1", calls)
	}
	if got := res.Labels.Slice(); !equalStringSlices(got, []string{"c"}) {
		t.Fatalf("labels = %v, want [c]", got)
	}
}

func TestLabelsSinkSkippedByShortCircuit(t *testing.T) {
	var calls int
	env, err := cel.NewEnv(
		cel.Variable("secret", cel.StringType),
		cel.Function("send",
			cel.Overload("send_string", []*cel.Type{cel.StringType}, cel.BoolType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					calls++
					return types.True
				}),
			),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	// false && send(secret): send is never evaluated, so the sink policy and body never run.
	ast, iss := env.Compile(`false && send(secret)`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	policyRuns := 0
	lp, err := cel.NewLabelProgram(
		cel.SinkFunction(func(c cel.SinkCall) error {
			policyRuns++
			return errors.New("should not run")
		}, "send_string", "send"),
	)
	if err != nil {
		t.Fatal(err)
	}
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cel.EvalLabeled(prg,
		map[string]any{"secret": "s"},
		cel.LabelInput{"secret": []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Value.Value() != false {
		t.Fatalf("value = %v, want false", res.Value)
	}
	if calls != 0 || policyRuns != 0 {
		t.Fatalf("sink body=%d policy=%d, want 0/0", calls, policyRuns)
	}
}

func TestLabelsDisabledByDefault(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("a", cel.StringType))
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(`a + "!"`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	// No LabelTracking option: ordinary Eval behaves exactly as without the feature.
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	val, _, err := prg.Eval(map[string]any{"a": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if val.Value() != "hi!" {
		t.Fatalf("value = %v", val)
	}
}

func TestLabelsComprehensionPerIterationPrecision(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("items", cel.ListType(cel.StringType)),
		cel.Variable("marker", cel.StringType),
		cel.Variable("other", cel.StringType),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Filter keeps an element only when it equals `other`; the filter condition is a control
	// dependency. `marker` is unrelated and must not appear.
	ast, iss := env.Compile(`items.filter(x, x == other)`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	lp, _ := cel.NewLabelProgram()
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cel.EvalLabeled(prg,
		map[string]any{"items": []string{"a", "b", "c"}, "other": "b", "marker": "m"},
		cel.LabelInput{
			"other":  []string{"selector"},
			"marker": []string{"unrelated"},
		})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Labels.Slice()
	if !equalStringSlices(got, []string{"selector"}) {
		t.Fatalf("labels = %v, want [selector]", got)
	}
}

func TestLabelsIndexPropagation(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("rec", cel.MapType(cel.StringType, cel.StringType)),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(`rec["email"]`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	lp, _ := cel.NewLabelProgram()
	prg, err := env.Program(ast, cel.LabelTracking(lp))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cel.EvalLabeled(prg,
		map[string]any{"rec": map[string]string{"email": "a@b.c"}},
		cel.LabelInput{"rec.email": []string{"pii"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Labels.Slice(); !equalStringSlices(got, []string{"pii"}) {
		t.Fatalf("labels = %v, want [pii]", got)
	}
}
