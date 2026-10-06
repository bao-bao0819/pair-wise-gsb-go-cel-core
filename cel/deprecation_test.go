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

package cel

import (
	"strings"
	"testing"

	"cel.dev/cel-go/common"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/parser"
)

func legacyEnv(t *testing.T, opts ...EnvOption) *Env {
	t.Helper()
	base := []EnvOption{
		Function("legacy",
			Overload("legacy_string", []*Type{types.StringType}, types.StringType,
				FunctionBinding(func(args ...ref.Val) ref.Val { return args[0] })),
			Overload("legacy_int", []*Type{types.IntType}, types.IntType,
				FunctionBinding(func(args ...ref.Val) ref.Val { return args[0] }))),
	}
	env, err := NewEnv(append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	return env
}

func mustCompile(t *testing.T, env *Env, expr string) *Ast {
	t.Helper()
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("Compile(%q) failed: %v", expr, iss.Err())
	}
	return ast
}

func mustChecker(t *testing.T, rules ...DeprecatedOverload) *DeprecationChecker {
	t.Helper()
	c, err := NewDeprecationChecker(rules...)
	if err != nil {
		t.Fatalf("NewDeprecationChecker() failed: %v", err)
	}
	return c
}

func TestDeprecationCheckerSameNameDistinctOverloads(t *testing.T) {
	env := legacyEnv(t)
	checker := mustChecker(t, DeprecatedOverload{
		OverloadID:  "legacy_string",
		Severity:    DeprecationWarning,
		Replacement: "use modern_string instead",
	})
	ast := mustCompile(t, env, `legacy('a') == 'x' && legacy(1) == 1`)
	diags, err := checker.Check(env, ast)
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("Check() got %d diagnostics, wanted 1: %v", len(diags), diags)
	}
	d := diags[0]
	if d.OverloadID != "legacy_string" {
		t.Errorf("diagnostic overload id got %q, wanted legacy_string", d.OverloadID)
	}
	if d.FunctionName != "legacy" {
		t.Errorf("diagnostic function name got %q, wanted legacy", d.FunctionName)
	}
	if d.Signature != "legacy(string) -> string" {
		t.Errorf("diagnostic signature got %q, wanted 'legacy(string) -> string'", d.Signature)
	}
	if d.Severity != DeprecationWarning {
		t.Errorf("diagnostic severity got %v, wanted warning", d.Severity)
	}
	if d.Ambiguous {
		t.Error("diagnostic marked ambiguous for a fully resolved call")
	}
	if d.Location == nil {
		t.Fatal("diagnostic missing source location")
	}
	// Call expression source ranges begin at the open paren of the call, which
	// matches the convention used by the CEL parser and type-checker errors.
	if d.Location.StartLine != 1 || d.Location.StartColumn != strings.Index(`legacy('a')`, "(") {
		t.Errorf("diagnostic location got %d:%d, wanted the call site of legacy('a')", d.Location.StartLine, d.Location.StartColumn)
	}
	if !strings.Contains(d.String(), "use modern_string instead") {
		t.Errorf("diagnostic message missing replacement text: %s", d.String())
	}
	// The active legacy_int overload of the same function must not be reported.
	for _, d := range diags {
		if d.OverloadID == "legacy_int" {
			t.Errorf("non-deprecated overload reported: %v", d)
		}
	}
}

func TestDeprecationCheckerReceiverStyleCall(t *testing.T) {
	env := legacyEnv(t,
		Function("legacy_size",
			MemberOverload("string_legacy_size", []*Type{types.StringType}, types.IntType)))
	checker := mustChecker(t, DeprecatedOverload{
		OverloadID: "string_legacy_size",
		Severity:   DeprecationWarning,
	})
	ast := mustCompile(t, env, `'abc'.legacy_size() == 3`)
	diags, err := checker.Check(env, ast)
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("Check() got %d diagnostics, wanted 1: %v", len(diags), diags)
	}
	d := diags[0]
	if d.Signature != "string.legacy_size() -> int" {
		t.Errorf("diagnostic signature got %q, wanted 'string.legacy_size() -> int'", d.Signature)
	}
	if d.Location == nil || d.Location.StartColumn != strings.Index(`'abc'.legacy_size() == 3`, "(") {
		t.Errorf("diagnostic location got %+v, wanted the call site of the receiver call", d.Location)
	}
}

func TestDeprecationCheckerNamespacedFunction(t *testing.T) {
	env := legacyEnv(t,
		Function("my.ns.legacy",
			Overload("my_ns_legacy_int", []*Type{types.IntType}, types.IntType)))
	checker := mustChecker(t, DeprecatedOverload{
		OverloadID: "my_ns_legacy_int",
		Severity:   DeprecationError,
	})
	ast := mustCompile(t, env, `my.ns.legacy(1) == 1`)
	diags, err := checker.Check(env, ast)
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("Check() got %d diagnostics, wanted 1: %v", len(diags), diags)
	}
	d := diags[0]
	if d.FunctionName != "my.ns.legacy" {
		t.Errorf("diagnostic function name got %q, wanted my.ns.legacy", d.FunctionName)
	}
	if d.Severity != DeprecationError {
		t.Errorf("diagnostic severity got %v, wanted error", d.Severity)
	}
	if !strings.HasPrefix(d.String(), "error:") {
		t.Errorf("diagnostic message got %q, wanted error prefix", d.String())
	}
}

func TestDeprecationCheckerNestedMacros(t *testing.T) {
	// The 'old' macro expands to a call of the deprecated legacy_string overload.
	oldMacro := parser.NewGlobalMacro("old", 1,
		func(eh parser.ExprHelper, target celast.Expr, args []celast.Expr) (celast.Expr, *common.Error) {
			return eh.NewCall("legacy", args[0]), nil
		})
	env := legacyEnv(t, Macros(oldMacro))
	checker := mustChecker(t, DeprecatedOverload{
		OverloadID: "legacy_string",
		Severity:   DeprecationWarning,
	})
	expr := `old(old('a')) == 'a'`
	ast := mustCompile(t, env, expr)
	diags, err := checker.Check(env, ast)
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}
	if len(diags) != 2 {
		t.Fatalf("Check() got %d diagnostics, wanted 2: %v", len(diags), diags)
	}
	// Both diagnostics must point back at the user-written macro call sites,
	// sorted by source position.
	outer := diags[0]
	inner := diags[1]
	outerCol := strings.Index(expr, "(")
	if outer.Location == nil || outer.Location.StartLine != 1 || outer.Location.StartColumn != outerCol {
		t.Errorf("outer macro call location got %+v, wanted line 1, column %d", outer.Location, outerCol)
	}
	innerCol := strings.Index(expr, "old('a')") + len("old")
	if inner.Location == nil || inner.Location.StartLine != 1 || inner.Location.StartColumn != innerCol {
		t.Errorf("inner macro call location got %+v, wanted line 1, column %d", inner.Location, innerCol)
	}
}

func TestDeprecationCheckerMacroGeneratedDuplicateCallSites(t *testing.T) {
	// The 'dup' macro expands to two copies of the same deprecated call, both
	// mapped to the macro call site. The duplicate position must be reported
	// only once per rule.
	dupMacro := parser.NewGlobalMacro("dup", 0,
		func(eh parser.ExprHelper, target celast.Expr, args []celast.Expr) (celast.Expr, *common.Error) {
			call := eh.NewCall("legacy", eh.NewLiteral(types.String("a")))
			return eh.NewCall("_+_", call, eh.Copy(call)), nil
		})
	env := legacyEnv(t, Macros(dupMacro))
	checker := mustChecker(t, DeprecatedOverload{
		OverloadID: "legacy_string",
		Severity:   DeprecationWarning,
	})
	ast := mustCompile(t, env, `dup() == 'aa'`)
	diags, err := checker.Check(env, ast)
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("Check() got %d diagnostics, wanted 1 deduplicated hit: %v", len(diags), diags)
	}
	if diags[0].Location == nil || diags[0].Location.StartColumn != strings.Index(`dup() == 'aa'`, "(") {
		t.Errorf("diagnostic location got %+v, wanted the dup() macro call site", diags[0].Location)
	}
}

func TestDeprecationCheckerNoSourceInfo(t *testing.T) {
	env := legacyEnv(t)
	checker := mustChecker(t, DeprecatedOverload{
		OverloadID: "legacy_string",
		Severity:   DeprecationWarning,
	})
	ast := mustCompile(t, env, `legacy('a')`)
	// Rebuild the checked AST without any source position metadata.
	impl := ast.NativeRep()
	stripped := &Ast{impl: celast.NewCheckedAST(
		celast.NewAST(impl.Expr(), celast.NewSourceInfo(nil)),
		impl.TypeMap(),
		impl.ReferenceMap())}
	diags, err := checker.Check(env, stripped)
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("Check() got %d diagnostics, wanted 1: %v", len(diags), diags)
	}
	d := diags[0]
	if d.Location != nil {
		t.Errorf("diagnostic location got %+v, wanted nil for AST without source info", d.Location)
	}
	if !strings.Contains(d.String(), "<unknown location>") {
		t.Errorf("diagnostic message got %q, wanted explicit unknown location marker", d.String())
	}
}

func TestDeprecationCheckerAmbiguousResolution(t *testing.T) {
	env := legacyEnv(t,
		Variable("v", types.DynType),
		Function("multi",
			Overload("multi_string", []*Type{types.StringType}, types.StringType),
			Overload("multi_int", []*Type{types.IntType}, types.IntType)))
	checker := mustChecker(t, DeprecatedOverload{
		OverloadID:  "multi_string",
		Severity:    DeprecationError,
		Replacement: "use multi_v2 instead",
	})
	ast := mustCompile(t, env, `multi(v)`)
	diags, err := checker.Check(env, ast)
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("Check() got %d diagnostics, wanted 1: %v", len(diags), diags)
	}
	d := diags[0]
	if !d.Ambiguous {
		t.Error("diagnostic not marked ambiguous for a call with multiple candidate overloads")
	}
	if d.Severity != DeprecationError {
		t.Errorf("diagnostic severity got %v, wanted error per rule config", d.Severity)
	}
	if d.OverloadID != "multi_string" {
		t.Errorf("diagnostic overload id got %q, wanted multi_string", d.OverloadID)
	}
	if !strings.Contains(d.String(), "ambiguous") {
		t.Errorf("diagnostic message got %q, wanted ambiguity note", d.String())
	}
	// The unselected multi_int declaration must not appear in the results.
	for _, d := range diags {
		if d.OverloadID == "multi_int" {
			t.Errorf("unselected overload reported: %v", d)
		}
	}
}

func TestDeprecationCheckerWarningDoesNotBlockProgram(t *testing.T) {
	env := legacyEnv(t)
	checker := mustChecker(t, DeprecatedOverload{
		OverloadID: "legacy_string",
		Severity:   DeprecationWarning,
	})
	ast := mustCompile(t, env, `legacy('a')`)
	diags, err := checker.Check(env, ast)
	if err != nil {
		t.Fatalf("Check() failed: %v", err)
	}
	if len(diags) != 1 || diags[0].Severity != DeprecationWarning {
		t.Fatalf("Check() got %v, wanted a single warning", diags)
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("Program() failed after deprecation warning: %v", err)
	}
	out, _, err := prg.Eval(NoVars())
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	if out != types.String("a") {
		t.Errorf("Eval() got %v, wanted 'a'", out)
	}
	// The replacement text is advisory only; the user expression is untouched.
	unparsed, err := AstToString(ast)
	if err != nil {
		t.Fatalf("AstToString() failed: %v", err)
	}
	if !strings.Contains(unparsed, "legacy") {
		t.Errorf("AstToString() got %q, wanted the original expression to remain unmodified", unparsed)
	}
}

func TestDeprecationCheckerRequiresCheckedAST(t *testing.T) {
	env := legacyEnv(t)
	checker := mustChecker(t, DeprecatedOverload{OverloadID: "legacy_string"})
	ast, iss := env.Parse(`legacy('a')`)
	if iss.Err() != nil {
		t.Fatalf("Parse() failed: %v", iss.Err())
	}
	if _, err := checker.Check(env, ast); err == nil {
		t.Error("Check() succeeded on a parse-only AST, wanted error")
	}
}

func TestNewDeprecationCheckerValidation(t *testing.T) {
	if _, err := NewDeprecationChecker(); err == nil {
		t.Error("NewDeprecationChecker() with no rules succeeded, wanted error")
	}
	if _, err := NewDeprecationChecker(DeprecatedOverload{}); err == nil {
		t.Error("NewDeprecationChecker() with empty overload id succeeded, wanted error")
	}
	if _, err := NewDeprecationChecker(
		DeprecatedOverload{OverloadID: "legacy_string"},
		DeprecatedOverload{OverloadID: "legacy_string"}); err == nil {
		t.Error("NewDeprecationChecker() with duplicate overload id succeeded, wanted error")
	}
}
