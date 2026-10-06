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

package deprecation_test

import (
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/checker/deprecation"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"

	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// compileEnv builds an environment with a pair of same-name overloads, one deprecated and one
// current, plus a namespaced function.
func compileEnv(t *testing.T, opts ...cel.EnvOption) *cel.Env {
	t.Helper()
	base := []cel.EnvOption{
		cel.Variable("s", cel.StringType),
		cel.Variable("l", cel.ListType(cel.StringType)),
		cel.Function("convert",
			cel.Overload("convert_string", []*cel.Type{cel.StringType}, cel.IntType),
			cel.Overload("convert_int", []*cel.Type{cel.IntType}, cel.IntType),
		),
		cel.Function("ns.convert",
			cel.Overload("ns_convert_string", []*cel.Type{cel.StringType}, cel.StringType),
		),
	}
	env, err := cel.NewEnv(append(base, opts...)...)
	if err != nil {
		t.Fatalf("cel.NewEnv() failed: %v", err)
	}
	return env
}

func mustCheck(t *testing.T, env *cel.Env, expr string) *ast.AST {
	t.Helper()
	parsed, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(%q) failed: %v", expr, iss.Err())
	}
	return parsed.NativeRep()
}

func newChecker(t *testing.T, env *cel.Env, rules ...deprecation.Rule) *deprecation.Checker {
	t.Helper()
	c, err := deprecation.NewChecker(
		deprecation.Rules(rules...),
		deprecation.Functions(env.Functions()),
	)
	if err != nil {
		t.Fatalf("deprecation.NewChecker() failed: %v", err)
	}
	return c
}

func TestSameNameDifferentOverloads(t *testing.T) {
	env := compileEnv(t)
	// The deprecated overload matches the string argument, the sibling int overload is current.
	checked := mustCheck(t, env, "convert(s) + convert(1)")
	checker := newChecker(t, env, deprecation.Rule{
		OverloadID:  "convert_string",
		Severity:    deprecation.ErrorSeverity,
		Alternative: "convertInt()",
	})

	findings := checker.Check(checked)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, wanted 1: %v", len(findings), findings)
	}
	f := findings[0]
	if f.OverloadID != "convert_string" {
		t.Errorf("OverloadID = %q, wanted convert_string", f.OverloadID)
	}
	if f.Function != "convert" {
		t.Errorf("Function = %q, wanted convert", f.Function)
	}
	if f.Severity != deprecation.ErrorSeverity {
		t.Errorf("Severity = %v, wanted error", f.Severity)
	}
	if f.Alternative != "convertInt()" {
		t.Errorf("Alternative = %q, wanted convertInt()", f.Alternative)
	}
	if f.Ambiguous {
		t.Errorf("Ambiguous = true, wanted false for a uniquely resolved call")
	}
	if len(f.ResolvedOverloads) != 1 || f.ResolvedOverloads[0].OverloadID != "convert_string" {
		t.Errorf("ResolvedOverloads = %v, wanted only convert_string", f.ResolvedOverloads)
	}
	if f.ResolvedOverloads[0].Signature == "" {
		t.Errorf("resolved signature was empty")
	}
	if f.Range == nil || f.MissingSourceInfo {
		t.Fatalf("Range = %v MissingSourceInfo = %v, wanted a concrete range", f.Range, f.MissingSourceInfo)
	}
	// The Pratt parser pins the call node to its opening parenthesis; the range must still sit on
	// the single source line and span a non-empty slice of the expression text.
	if f.Range.Start.Line() != 1 || f.Range.Stop.Line() != 1 {
		t.Errorf("range lines = %d..%d, wanted 1..1", f.Range.Start.Line(), f.Range.Stop.Line())
	}
	if f.Range.Start.Column() < 0 || f.Range.Stop.Column() <= f.Range.Start.Column() {
		t.Errorf("range columns = %d..%d, wanted a positive span", f.Range.Start.Column(), f.Range.Stop.Column())
	}
	if f.FromMacroExpansion {
		t.Errorf("FromMacroExpansion = true for a plain call")
	}
}

func TestReceiverStyleCall(t *testing.T) {
	env := compileEnv(t)
	// string.size() resolves to the member overload; the global size_string is not selected.
	checked := mustCheck(t, env, "s.size()")
	checker := newChecker(t, env, deprecation.Rule{
		OverloadID:  "string_size",
		Severity:    deprecation.WarningSeverity,
		Alternative: "len(s)",
	})

	findings := checker.Check(checked)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, wanted 1", len(findings))
	}
	f := findings[0]
	if f.OverloadID != "string_size" {
		t.Errorf("OverloadID = %q, wanted string_size", f.OverloadID)
	}
	if f.Severity != deprecation.WarningSeverity {
		t.Errorf("Severity = %v, wanted warning", f.Severity)
	}
	if got := f.ResolvedOverloads[0].Signature; got == "" || got[0] != 's' {
		t.Errorf("Signature = %q, wanted a receiver-style signature", got)
	}
}

func TestNamespacedFunction(t *testing.T) {
	env := compileEnv(t, cel.Container("ns"))
	checked := mustCheck(t, env, "convert(s)")
	checker := newChecker(t, env, deprecation.Rule{
		OverloadID:  "ns_convert_string",
		Severity:    deprecation.WarningSeverity,
		Alternative: "ns.convertV2(s)",
	})

	findings := checker.Check(checked)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, wanted 1", len(findings))
	}
	if findings[0].Function != "ns.convert" {
		t.Errorf("Function = %q, wanted ns.convert", findings[0].Function)
	}
	if findings[0].ResolvedOverloads[0].OverloadID != "ns_convert_string" {
		t.Errorf("unexpected resolved overloads %v", findings[0].ResolvedOverloads)
	}
}

func TestAmbiguousOverloadResolution(t *testing.T) {
	// When the type checker records several candidate overloads for a single call, the finding
	// must surface the ambiguity and list every selected candidate while flagging only the
	// deprecated one.
	env, err := cel.NewEnv(
		cel.Variable("s", cel.StringType),
		cel.Function("pick",
			cel.Overload("pick_string", []*cel.Type{cel.StringType}, cel.StringType),
		),
	)
	if err != nil {
		t.Fatalf("cel.NewEnv() failed: %v", err)
	}

	// Start from a valid, uniquely-resolving checked call, then emulate an ambiguous resolution by
	// recording an additional candidate overload id, as the checker does when more than one
	// declaration matches (a situation that can arise for parameterized declarations).
	checked := mustCheck(t, env, "pick(s)")
	ambiguousRef := ast.NewFunctionReference("pick_string")
	ambiguousRef.AddOverload("pick_runtime_fallback")
	checked.SetReference(checked.Expr().ID(), ambiguousRef)

	checker := newChecker(t, env, deprecation.Rule{
		OverloadID:  "pick_string",
		Severity:    deprecation.WarningSeverity,
		Alternative: "pickV2(s)",
	})

	findings := checker.Check(checked)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, wanted 1 for the single deprecated candidate", len(findings))
	}
	f := findings[0]
	if !f.Ambiguous {
		t.Errorf("Ambiguous = false, wanted true when multiple overloads match")
	}
	if len(f.ResolvedOverloads) != 2 {
		t.Fatalf("resolved %d overloads, wanted both candidates", len(f.ResolvedOverloads))
	}
	if f.ResolvedOverloads[0].OverloadID != "pick_string" ||
		f.ResolvedOverloads[1].OverloadID != "pick_runtime_fallback" {
		t.Errorf("resolved overloads = %v", f.ResolvedOverloads)
	}
	// The non-declared candidate is still reported, but without a declared signature.
	if f.ResolvedOverloads[1].Signature != "" {
		t.Errorf("unexpected signature for undeclared candidate: %q", f.ResolvedOverloads[1].Signature)
	}
}

func TestDeduplicateAndStableOrder(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("s", cel.StringType),
		cel.Function("convert",
			cel.Overload("convert_string", []*cel.Type{cel.StringType}, cel.IntType),
		),
	)
	if err != nil {
		t.Fatalf("cel.NewEnv() failed: %v", err)
	}
	// Two distinct call sites both resolve to the deprecated overload.
	checked := mustCheck(t, env, "convert(s) == convert(s)")
	checker := newChecker(t, env, deprecation.Rule{
		OverloadID: "convert_string",
		Severity:   deprecation.WarningSeverity,
	})

	first := checker.Check(checked)
	second := checker.Check(checked)
	if len(first) != 2 {
		t.Fatalf("got %d findings, wanted two distinct call sites", len(first))
	}
	if first[0].ExprID == first[1].ExprID {
		t.Fatalf("expected findings at distinct expression ids")
	}
	// Stable ordering across runs, ordered by expression id.
	if first[0].ExprID > first[1].ExprID {
		t.Fatalf("findings not ordered by expression id: %d > %d", first[0].ExprID, first[1].ExprID)
	}
	for i := range first {
		if first[i].ExprID != second[i].ExprID || first[i].OverloadID != second[i].OverloadID {
			t.Fatalf("non-deterministic ordering between runs")
		}
	}
}

// legacyMacro expands "<list>.legacy(x, x)" into a synthesized global call to the deprecated
// legacyFn. The synthesized call reuses the user argument but is otherwise macro-generated.
func legacyMacro() cel.Macro {
	return cel.NewReceiverMacro("legacy", 2,
		func(meh cel.MacroExprHelper, target *exprpb.Expr, args []*exprpb.Expr) (*exprpb.Expr, *cel.Error) {
			// Ignore the identifier and predicate; emit legacyFn(<target>) with fresh macro ids.
			return meh.GlobalCall("legacyFn", meh.Copy(target)), nil
		})
}

func TestMacroGeneratedCallAttribution(t *testing.T) {
	env, err := cel.NewEnv(
		cel.EnableMacroCallTracking(),
		cel.Variable("l", cel.ListType(cel.StringType)),
		cel.Macros(legacyMacro()),
		cel.Function("legacyFn",
			cel.Overload("legacyFn_list", []*cel.Type{cel.ListType(cel.StringType)}, cel.IntType),
		),
	)
	if err != nil {
		t.Fatalf("cel.NewEnv() failed: %v", err)
	}
	const expr = "l.legacy(x, x)"
	checked := mustCheck(t, env, expr)
	checker := newChecker(t, env, deprecation.Rule{
		OverloadID:  "legacyFn_list",
		Severity:    deprecation.WarningSeverity,
		Alternative: "newFn(l)",
	})

	findings := checker.Check(checked)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, wanted 1", len(findings))
	}
	f := findings[0]
	if !f.FromMacroExpansion {
		t.Errorf("FromMacroExpansion = false, wanted true for a synthesized macro call")
	}
	if f.MissingSourceInfo || f.Range == nil {
		t.Fatalf("MissingSourceInfo = %v Range = %v, wanted attribution to the macro call",
			f.MissingSourceInfo, f.Range)
	}
	// The range must point at the user-written macro token "l.legacy", not an invented line.
	if f.Range.Start.Line() != 1 || f.Range.Start.Column() != 0 {
		t.Errorf("range start = %d:%d, wanted 1:0 at the macro token",
			f.Range.Start.Line(), f.Range.Start.Column())
	}
}

func TestNestedMacroCallAttribution(t *testing.T) {
	env, err := cel.NewEnv(
		cel.EnableMacroCallTracking(),
		cel.Variable("l", cel.ListType(cel.StringType)),
		cel.Macros(
			cel.MapMacro,
			legacyMacro(),
		),
		cel.Function("legacyFn",
			cel.Overload("legacyFn_string", []*cel.Type{cel.StringType}, cel.IntType),
		),
	)
	if err != nil {
		t.Fatalf("cel.NewEnv() failed: %v", err)
	}
	// The inner legacy macro is nested inside the map transform; its synthesized call must
	// attribute to the inner user-written "x.legacy(y, y)" token.
	const expr = "l.map(x, x.legacy(y, y))"
	checked := mustCheck(t, env, expr)
	checker := newChecker(t, env, deprecation.Rule{
		OverloadID:  "legacyFn_string",
		Severity:    deprecation.WarningSeverity,
		Alternative: "newFn(x)",
	})

	findings := checker.Check(checked)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, wanted 1 for the nested macro, got %v", len(findings), findings)
	}
	f := findings[0]
	if !f.FromMacroExpansion {
		t.Errorf("FromMacroExpansion = false for nested macro call")
	}
	if f.Range == nil || f.Range.Start.Line() != 1 {
		t.Fatalf("range = %v, wanted a concrete single-line macro location", f.Range)
	}
	// "x.legacy" begins after "l.map(x, " which is 9 characters.
	if f.Range.Start.Column() != 9 {
		t.Errorf("range start column = %d, wanted 9 (inner macro token)", f.Range.Start.Column())
	}
}

func TestMissingSourceInfo(t *testing.T) {
	// Construct a checked AST programmatically with no SourceInfo, simulating a checked AST
	// produced outside the parser.
	fac := ast.NewExprFactory()
	call := fac.NewCall(1, "convert", fac.NewIdent(2, "s"))
	checked := ast.NewCheckedAST(
		ast.NewAST(call, nil),
		map[int64]*types.Type{
			1: types.IntType,
			2: types.StringType,
		},
		map[int64]*ast.ReferenceInfo{
			1: ast.NewFunctionReference("convert_string"),
		},
	)
	c, err := deprecation.NewChecker(deprecation.Rules(deprecation.Rule{
		OverloadID: "convert_string",
		Severity:   deprecation.ErrorSeverity,
	}))
	if err != nil {
		t.Fatalf("deprecation.NewChecker() failed: %v", err)
	}

	findings := c.Check(checked)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, wanted 1", len(findings))
	}
	f := findings[0]
	if !f.MissingSourceInfo {
		t.Errorf("MissingSourceInfo = false, wanted true")
	}
	if f.Range != nil {
		t.Errorf("Range = %v, wanted nil when source info is absent", f.Range)
	}
	if f.ExprID != 1 {
		t.Errorf("ExprID = %d, wanted 1", f.ExprID)
	}
}

func TestInvalidRuleConfiguration(t *testing.T) {
	if _, err := deprecation.NewChecker(deprecation.Rules(deprecation.Rule{
		OverloadID: "",
		Severity:   deprecation.WarningSeverity,
	})); err == nil {
		t.Fatal("expected error for empty overload id")
	}
	if _, err := deprecation.NewChecker(deprecation.Rules(deprecation.Rule{
		OverloadID: "x",
		Severity:   0,
	})); err == nil {
		t.Fatal("expected error for invalid severity")
	}
}
