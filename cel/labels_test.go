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
	"fmt"
	"strings"
	"sync"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

func labelVars(secret any) map[string]any {
	return map[string]any{"secret": secret, "publicA": "a", "publicB": "b"}
}

func secretLabel() cel.Label {
	return cel.Label{Variable: "secret"}
}

func labelStrings(labels []cel.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.String())
	}
	return out
}

func TestLabels_TernaryControlDependency(t *testing.T) {
	env := newLabelEnv(t)
	ast := compileExpr(t, env, "secret ? publicA : publicB")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secret", secretLabel())))
	if err != nil {
		t.Fatalf("program: %v", err)
	}

	for _, secret := range []bool{true, false} {
		out, det, err := prg.Eval(labelVars(secret))
		if err != nil {
			t.Fatalf("eval secret=%v: %v", secret, err)
		}
		labels := labelStrings(det.Labels())
		if len(labels) != 1 || labels[0] != "secret" {
			t.Fatalf("secret=%v result %v labels=%v, want [secret]", secret, out, labels)
		}
	}
}

func TestLabels_TernaryNoControlLabelWhenConditionPublic(t *testing.T) {
	env := newLabelEnv(t)
	ast := compileExpr(t, env, "true ? publicA : publicB")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secret", secretLabel())))
	if err != nil {
		t.Fatal(err)
	}
	out, det, err := prg.Eval(labelVars(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 0 {
		t.Fatalf("public constant condition selected publicA, got labels %v value %v", got, out)
	}
}

func TestLabels_ScalarPropagation(t *testing.T) {
	env := newLabelEnv(t)
	cases := []string{
		"secret == true",
		"secret && (publicA == '')",
		"!secret",
		"secret ? 1 : 2",
	}
	for _, expr := range cases {
		ast := compileExpr(t, env, expr)
		prg, err := env.Program(ast, cel.DataLabels(
			cel.LabeledInput("secret", secretLabel())))
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		_, det, err := prg.Eval(labelVars(true))
		if err != nil {
			t.Fatalf("%s eval: %v", expr, err)
		}
		if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "secret" {
			t.Fatalf("%s labels=%v, want [secret]", expr, got)
		}
	}
}

func TestLabels_ListMapFieldPropagation(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("payload", cel.MapType(cel.StringType, cel.StringType)))
	ast := compileExpr(t, env,
		"[payload.a, {\"k\": publicA}].filter(x, x != '').size() > 0 && payload.b == 'y'")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("payload", cel.Label{Variable: "payload"}),
	))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{
		"publicA": "a",
		"payload": map[string]string{"a": "x", "b": "y"},
	}
	_, det, err := prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) == 0 {
		t.Fatalf("expected payload labels on comprehension/result, got none")
	}
}

func TestLabels_FieldPathNarrowing(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("payload", cel.MapType(cel.StringType, cel.StringType)))
	ast := compileExpr(t, env, "payload.a")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("payload", cel.Label{Variable: "payload", Path: []any{"b"}}),
	))
	if err != nil {
		t.Fatal(err)
	}
	_, det, err := prg.Eval(map[string]any{
		"payload": map[string]string{"a": "x", "b": "y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 0 {
		t.Fatalf("sibling field label must not propagate, got %v", got)
	}

	ast = compileExpr(t, env, "payload.b")
	prg, _ = env.Program(ast, cel.DataLabels(
		cel.LabeledInput("payload", cel.Label{Variable: "payload", Path: []any{"b"}}),
	))
	_, det, err = prg.Eval(map[string]any{
		"payload": map[string]string{"a": "x", "b": "y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "payload.b" {
		t.Fatalf("exact field path labels=%v, want [payload.b]", got)
	}
}

func TestLabels_SinkPolicyRunsBeforeBody(t *testing.T) {
	var bodyCalls int
	env := newLabelEnv(t,
		cel.Function("send",
			cel.Overload("send_string", []*cel.Type{cel.StringType}, cel.StringType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					bodyCalls++
					return v
				})),
			cel.Overload("send_bool", []*cel.Type{cel.BoolType}, cel.BoolType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					bodyCalls++
					return v
				}))))

	// Public input: policy allows, body runs exactly once.
	astPublic := compileExpr(t, env, "send(publicA)")
	prgPublic, err := env.Program(astPublic,
		cel.DataLabels(cel.SinkOverload("send_string"), cel.SinkPolicy(func(call cel.SinkCall) error {
			if len(call.CombinedLabels()) > 0 {
				return fmt.Errorf("unexpected labels: %v", call.CombinedLabels())
			}
			return nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := prgPublic.Eval(labelVars(true)); err != nil {
		t.Fatalf("public input should pass sink: %v", err)
	}
	if bodyCalls != 1 {
		t.Fatalf("public sink body should run once, got %d", bodyCalls)
	}

	// Labeled input: policy rejects before the body runs; the body count stays at one.
	var seen []string
	astSecret := compileExpr(t, env, "send(secret)")
	prgSecret, err := env.Program(astSecret,
		cel.DataLabels(
			cel.LabeledInput("secret", secretLabel()),
			cel.SinkOverload("send_bool"),
			cel.SinkPolicy(func(call cel.SinkCall) error {
				for _, l := range call.CombinedLabels() {
					seen = append(seen, l.String())
				}
				return fmt.Errorf("denied")
			}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = prgSecret.Eval(labelVars(true))
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected sink denial error, got %v", err)
	}
	if bodyCalls != 1 {
		t.Fatalf("sink body must not run on denial; calls=%d, want 1", bodyCalls)
	}
	if len(seen) != 1 || seen[0] != "secret" {
		t.Fatalf("policy saw labels %v, want [secret]", seen)
	}
}
func TestLabels_SanitizeClearsLabels(t *testing.T) {
	env := newLabelEnv(t,
		cel.Function("redact",
			cel.Overload("redact_bool", []*cel.Type{cel.BoolType}, cel.StringType,
				cel.UnaryBinding(func(v ref.Val) ref.Val { return types.String("redacted") }))))
	ast := compileExpr(t, env, "redact(secret)")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secret", secretLabel()),
		cel.SanitizeOverload("redact_bool"),
	))
	if err != nil {
		t.Fatal(err)
	}
	out, det, err := prg.Eval(labelVars(true))
	if err != nil {
		t.Fatal(err)
	}
	if out.Value() != "redacted" {
		t.Fatalf("unexpected value %v", out)
	}
	if got := labelStrings(det.Labels()); len(got) != 0 {
		t.Fatalf("sanitized result must carry no labels, got %v", got)
	}
}

func TestLabels_ShortCircuitControlLabels(t *testing.T) {
	env := newLabelEnv(t)
	ast := compileExpr(t, env, "secret || publicA == ''")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secret", secretLabel())))
	if err != nil {
		t.Fatal(err)
	}
	out, det, err := prg.Eval(labelVars(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "secret" {
		t.Fatalf("short-circuit OR result %v labels=%v, want [secret]", out, got)
	}
}

func TestLabels_ComprehensionShadowing(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("outer", cel.ListType(cel.StringType)),
		cel.Variable("inner", cel.ListType(cel.StringType)))
	// The outer comprehension variable is labeled via the labeled range; the nested
	// comprehension reuses the same loop variable name and ranges over a public list, which
	// must shadow the outer binding within the nested body.
	ast := compileExpr(t, env,
		"outer.exists(x, inner.exists(x, x == 'pub'))")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("outer", cel.Label{Variable: "outer"}),
	))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{
		"outer": []string{"sec"},
		"inner": []string{"pub"},
	}
	out, det, err := prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	_ = out
	// The outer range label legitimately influences the outer exists outcome.
	if got := labelStrings(det.Labels()); len(got) == 0 {
		t.Fatalf("outer labeled exists should carry the outer range label, got none (out=%v)", out)
	}

	// With only the inner range labeled, the nested exists (whose loop variable shadows the
	// outer one) contributes the inner label to the result.
	astInner := compileExpr(t, env,
		"outer.exists(x, inner.exists(x, x == 'pub'))")
	prgInner, err := env.Program(astInner, cel.DataLabels(
		cel.LabeledInput("inner", cel.Label{Variable: "inner"}),
	))
	if err != nil {
		t.Fatal(err)
	}
	_, det, err = prgInner.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) == 0 {
		t.Fatalf("inner labeled exists should carry the inner range label, got none")
	}
}

func TestLabels_TwoEvaluationsDifferentInputs(t *testing.T) {
	env := newLabelEnv(t)
	ast := compileExpr(t, env, "secret ? publicA : publicB")
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}

	// First evaluation without labels: no label state.
	out, det, err := prg.Eval(labelVars(true))
	if err != nil {
		t.Fatal(err)
	}
	if det != nil && len(det.Labels()) != 0 {
		t.Fatalf("unlabeled program must not report labels: %v", det.Labels())
	}
	_ = out

	// A separately constructed labeled program evaluates against two differently labeled
	// inputs repeatedly with no state carryover.
	prgLabeled, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secret", secretLabel())))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, det, err := prgLabeled.Eval(labelVars(i%2 == 0))
		if err != nil {
			t.Fatal(err)
		}
		if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "secret" {
			t.Fatalf("iteration %d labels=%v", i, got)
		}
	}
}

func TestLabels_StrictRejectsUndeclaredExtension(t *testing.T) {
	env := newLabelEnv(t,
		cel.Function("ext",
			cel.Overload("ext_string", []*cel.Type{cel.StringType}, cel.StringType,
				cel.UnaryBinding(func(v ref.Val) ref.Val { return v }))))
	ast := compileExpr(t, env, "ext(publicA)")
	_, err := env.Program(ast, cel.DataLabels(cel.StrictLabelMode()))
	if err == nil || !strings.Contains(err.Error(), "no declared label propagation rule") {
		t.Fatalf("expected strict rejection, got %v", err)
	}

	prg, err := env.Program(ast, cel.DataLabels(cel.StrictLabelMode(),
		cel.PropagateOverload("ext_string")))
	if err != nil {
		t.Fatalf("declared propagation should be accepted: %v", err)
	}
	_, det, err := prg.Eval(labelVars(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 0 {
		t.Fatalf("public arg labels=%v, want none", got)
	}
}

func newLabelEnv(t *testing.T, opts ...cel.EnvOption) *cel.Env {
	t.Helper()
	defaults := []cel.EnvOption{
		cel.Variable("secret", cel.BoolType),
		cel.Variable("publicA", cel.StringType),
		cel.Variable("publicB", cel.StringType),
	}
	env, err := cel.NewEnv(append(defaults, opts...)...)
	if err != nil {
		t.Fatalf("new env: %v", err)
	}
	return env
}

func compileExpr(t *testing.T, env *cel.Env, expr string) *cel.Ast {
	t.Helper()
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("compile %q: %v", expr, iss.Err())
	}
	return ast
}

func TestLabels_TernarySelectedBranchData(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("flag", cel.BoolType),
		cel.Variable("secretA", cel.StringType),
		cel.Variable("secretB", cel.StringType))
	ast := compileExpr(t, env, "flag ? secretA : secretB")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secretA", cel.Label{Variable: "secretA"}),
		cel.LabeledInput("secretB", cel.Label{Variable: "secretB"}),
	))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"flag": true, "secretA": "a", "secretB": "b"}
	_, det, err := prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "secretA" {
		t.Fatalf("true branch labels=%v, want [secretA]", got)
	}
	vars["flag"] = false
	_, det, err = prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "secretB" {
		t.Fatalf("false branch labels=%v, want [secretB]", got)
	}
}

func TestLabels_RelativeAttributeFieldPropagation(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("flag", cel.BoolType),
		cel.Variable("a", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("b", cel.MapType(cel.StringType, cel.StringType)))
	ast := compileExpr(t, env, "(flag ? a : b).k")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("a", cel.Label{Variable: "a"}),
	))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{
		"flag": true,
		"a":    map[string]string{"k": "v"},
		"b":    map[string]string{"k": "w"},
	}
	_, det, err := prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "a" {
		t.Fatalf("relative field labels=%v, want [a]", got)
	}

	vars["flag"] = false
	_, det, err = prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	// b is unlabeled and flag is public.
	if got := labelStrings(det.Labels()); len(got) != 0 {
		t.Fatalf("unlabeled false branch labels=%v, want none", got)
	}
}

func TestLabels_SinkPerArgumentLabels(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("secret2", cel.StringType),
		cel.Function("log2",
			cel.Overload("log2_string_string",
				[]*cel.Type{cel.StringType, cel.StringType}, cel.StringType,
				cel.BinaryBinding(func(a, b ref.Val) ref.Val { return a }))))
	ast := compileExpr(t, env, "log2(publicA, secret2)")
	var got [][]string
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secret2", cel.Label{Variable: "secret2"}),
		cel.SinkOverload("log2_string_string"),
		cel.SinkPolicy(func(call cel.SinkCall) error {
			got = nil
			for _, set := range call.ArgumentLabels {
				var ls []string
				for _, l := range set {
					ls = append(ls, l.String())
				}
				got = append(got, ls)
			}
			return nil
		}),
	))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"secret": true, "publicA": "a", "secret2": "s"}
	if _, _, err := prg.Eval(vars); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[0]) != 0 || len(got[1]) != 1 || got[1][0] != "secret2" {
		t.Fatalf("per-argument labels=%v, want [[], [secret2]]", got)
	}
}

func TestLabels_ErrorSemanticsPreserved(t *testing.T) {
	env := newLabelEnv(t)
	ast := compileExpr(t, env, "1 / 0")
	prg, err := env.Program(ast, cel.DataLabels())
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := prg.Eval(labelVars(true))
	if err == nil {
		t.Fatalf("expected division by zero error, got %v", out)
	}
}

func TestLabels_DynamicIndexInheritsBase(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("m", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("k", cel.StringType))
	ast := compileExpr(t, env, "m[k]")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("m", cel.Label{Variable: "m"}),
	))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"m": map[string]string{"x": "v"}, "k": "x"}
	_, det, err := prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "m" {
		t.Fatalf("dynamic index labels=%v, want [m] (over-approx via base container)", got)
	}
}

func TestLabels_ListConstructAndIndex(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("s", cel.StringType))
	ast := compileExpr(t, env, "[publicA, s][1]")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("s", cel.Label{Variable: "s"}),
	))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"secret": true, "publicA": "a", "s": "secret-val"}
	out, det, err := prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	if out.Value() != "secret-val" {
		t.Fatalf("unexpected %v", out)
	}
	if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "s" {
		t.Fatalf("indexed labeled element labels=%v, want [s]", got)
	}
}

func TestLabels_TernaryThenFieldSelection(t *testing.T) {
	env := newLabelEnv(t,
		cel.Variable("flag", cel.BoolType),
		cel.Variable("a", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("b", cel.MapType(cel.StringType, cel.StringType)))
	// Label the nested field a.k specifically; the ternary control condition is public but the
	// selected branch value is labeled.
	ast := compileExpr(t, env, "(flag ? a : b)['k']")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("a", cel.Label{Variable: "a", Path: []any{"k"}}),
	))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{
		"flag": true,
		"a":    map[string]string{"k": "v"},
		"b":    map[string]string{"k": "w"},
	}
	_, det, err := prg.Eval(vars)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "a.k" {
		t.Fatalf("ternary+field labels=%v, want [a.k]", got)
	}
}

func TestLabels_SinkFunctionNameRegistration(t *testing.T) {
	calls := 0
	env := newLabelEnv(t,
		cel.Function("emit",
			cel.Overload("emit_bool", []*cel.Type{cel.BoolType}, cel.BoolType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					calls++
					return v
				}))))
	ast := compileExpr(t, env, "emit(secret)")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secret", secretLabel()),
		cel.SinkFunction("emit"),
		cel.SinkPolicy(func(call cel.SinkCall) error {
			return fmt.Errorf("blocked by function-name rule")
		}),
	))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = prg.Eval(labelVars(true))
	if err == nil || !strings.Contains(err.Error(), "blocked by function-name rule") {
		t.Fatalf("expected function-name sink denial, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("sink body invoked %d times after denial", calls)
	}
}

func TestLabels_ConcurrentEvaluationsIsolated(t *testing.T) {
	env := newLabelEnv(t)
	ast := compileExpr(t, env, "secret ? publicA : publicB")
	prg, err := env.Program(ast, cel.DataLabels(
		cel.LabeledInput("secret", secretLabel())))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, det, err := prg.Eval(labelVars(i%2 == 0))
			if err != nil {
				t.Errorf("eval %d: %v", i, err)
				return
			}
			if got := labelStrings(det.Labels()); len(got) != 1 || got[0] != "secret" {
				t.Errorf("eval %d labels=%v", i, got)
			}
		}(i)
	}
	wg.Wait()
}
