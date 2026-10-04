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

package checker

import (
	"strings"
	"testing"

	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/decls"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/parser"
)

// findCallID returns the id of the first call node whose function name matches.
func findCallID(t *testing.T, a *ast.AST, function string) int64 {
	t.Helper()
	var found int64
	ast.PostOrderVisit(a.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if found == 0 && e.Kind() == ast.CallKind && e.AsCall().FunctionName() == function {
			found = e.ID()
		}
	}))
	if found == 0 {
		t.Fatalf("no call with function %q", function)
	}
	return found
}

func unparse(t *testing.T, a *ast.AST) string {
	t.Helper()
	out, err := parser.Unparse(a.Expr(), a.SourceInfo())
	if err != nil {
		t.Fatalf("unparse failed: %v", err)
	}
	return out
}

func TestSubstituteAlphaRenamesCapturedIterVar(t *testing.T) {
	env := scratchEnv(t)
	target, targetSrc := scratchCheck(t, env, `items.all(x, x.startsWith("a"))`)
	fragment, fragSrc := scratchCheck(t, env, `x.endsWith("b")`)

	predicateID := findCallID(t, target, "startsWith")

	res, issues := SubstituteExprs(env, targetSrc, target, []Substitution{
		{TargetID: predicateID, Fragment: fragment, FragmentSource: fragSrc},
	})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}

	out := unparse(t, res.AST)
	if !strings.Contains(out, "x.endsWith") {
		t.Errorf("expected fragment free variable x to remain global, got %q", out)
	}
	if strings.Contains(out, "all(x,") {
		t.Errorf("expected captured iter var to be alpha-renamed, got %q", out)
	}

	// The fragment free variable must keep the global binding and the global type.
	ast.PostOrderVisit(res.AST.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() == ast.IdentKind && e.AsIdent() == "x" {
			if got := res.AST.GetType(e.ID()); !got.IsEquivalentType(types.StringType) {
				t.Errorf("global x type = %v, want string", got)
			}
		}
	}))

	// The result must remain a valid checked AST when rechecked against the same environment.
	rechecked, errs := Check(res.AST, res.Source, env)
	if len(errs.GetErrors()) != 0 {
		t.Fatalf("recheck failed: %s", errs.ToDisplayString())
	}
	if got := unparse(t, rechecked); !strings.Contains(got, "x.endsWith") {
		t.Errorf("rechecked expression lost fragment: %q", got)
	}
}

func TestSubstituteNestedAllMapMacroCapture(t *testing.T) {
	env := scratchEnv(t)
	// The target binds x in the outer all and y in the inner map. The splice point is inside the
	// inner map transform, where both x and y are bound. Both names are also global variables in
	// the environment; without alpha-renaming the fragment's free x and y would be captured.
	target, targetSrc := scratchCheck(t, env,
		`items.all(x, items.map(y, y.startsWith(x)).size() > 0)`)
	// The fragment is a boolean predicate over the global string x spliced into a position where
	// a local list-element x is in scope: the local x must be alpha-renamed to avoid capture.
	fragment, fragSrc := scratchCheck(t, env, `x.endsWith("b")`)

	concatID := findCallID(t, target, "startsWith")

	res, issues := SubstituteExprs(env, targetSrc, target, []Substitution{
		{TargetID: concatID, Fragment: fragment, FragmentSource: fragSrc},
	})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}

	out := unparse(t, res.AST)
	// The outer iter x must be renamed so the fragment free x resolves to the global variable.
	if strings.Contains(out, "all(x,") {
		t.Errorf("expected nested binders to be alpha-renamed, got %q", out)
	}
	if !strings.Contains(out, "x.endsWith") {
		t.Errorf("expected fragment reference to global x to be preserved, got %q", out)
	}

	// The result rechecks against the same environment with the fragment free x as a string, which
	// would be a type error if the local list-element x captured it.
	rechecked, errs := Check(res.AST, res.Source, env)
	if len(errs.GetErrors()) != 0 {
		t.Fatalf("recheck failed: %s\nunparsed: %s", errs.ToDisplayString(), out)
	}
	if got := rechecked.GetType(rechecked.Expr().ID()); !got.IsEquivalentType(types.BoolType) {
		t.Errorf("result type = %v, want bool", got)
	}
}

func TestSubstituteRejectsTypeMismatch(t *testing.T) {
	env := scratchEnv(t)
	target, targetSrc := scratchCheck(t, env, `items.all(x, x.startsWith("a"))`)
	fragment, fragSrc := scratchCheck(t, env, `items`)

	predicateID := findCallID(t, target, "startsWith")
	_, issues := SubstituteExprs(env, targetSrc, target, []Substitution{
		{TargetID: predicateID, Fragment: fragment, FragmentSource: fragSrc},
	})
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Origin != FragmentOrigin {
		t.Errorf("expected fragment origin, got %v", issues[0].Origin)
	}
	if !strings.Contains(issues[0].Message, "not assignable") {
		t.Errorf("expected type mismatch message, got %q", issues[0].Message)
	}
}

func TestSubstituteRejectsUnknownFreeVariable(t *testing.T) {
	env := scratchEnv(t)
	target, targetSrc := scratchCheck(t, env, `x == "a"`)

	// Build a fragment against a different environment which declares an extra free variable.
	other := scratchEnv(t)
	if err := other.AddIdents(decls.NewVariable("z", types.StringType)); err != nil {
		t.Fatal(err)
	}
	fragment, fragSrc := scratchCheck(t, other, `z == "b"`)

	eqID := findCallID(t, target, "_==_")
	_, issues := SubstituteExprs(env, targetSrc, target, []Substitution{
		{TargetID: eqID, Fragment: fragment, FragmentSource: fragSrc},
	})
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(issues), issues)
	}
	if !strings.Contains(issues[0].Message, "'z'") {
		t.Errorf("expected undeclared z message, got %q", issues[0].Message)
	}
}

func TestSubstituteRejectsOverlappingTargets(t *testing.T) {
	env := scratchEnv(t)
	target, targetSrc := scratchCheck(t, env, `items.all(x, x.startsWith("a"))`)
	fragment, fragSrc := scratchCheck(t, env, `x.endsWith("b")`)

	allID := findComprehensionID(t, target)
	predicateID := findCallID(t, target, "startsWith")

	_, issues := SubstituteExprs(env, targetSrc, target, []Substitution{
		{TargetID: allID, Fragment: fragment, FragmentSource: fragSrc},
		{TargetID: predicateID, Fragment: fragment, FragmentSource: fragSrc},
	})
	if len(issues) == 0 {
		t.Fatal("expected overlapping target rejection")
	}
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "overlap") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected overlap issue, got %v", issues)
	}
}

func TestSubstituteDoesNotMutateInputs(t *testing.T) {
	env := scratchEnv(t)
	target, targetSrc := scratchCheck(t, env, `items.all(x, x.startsWith("a"))`)
	fragment, fragSrc := scratchCheck(t, env, `x.endsWith("b")`)

	beforeTarget := unparse(t, target)
	beforeFragment := unparse(t, fragment)
	targetIDsBefore := len(target.TypeMap())
	fragIDsBefore := len(fragment.TypeMap())

	predicateID := findCallID(t, target, "startsWith")
	_, issues := SubstituteExprs(env, targetSrc, target, []Substitution{
		{TargetID: predicateID, Fragment: fragment, FragmentSource: fragSrc},
	})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}

	if got := unparse(t, target); got != beforeTarget {
		t.Errorf("target AST mutated: before %q after %q", beforeTarget, got)
	}
	if got := unparse(t, fragment); got != beforeFragment {
		t.Errorf("fragment AST mutated: before %q after %q", beforeFragment, got)
	}
	if len(target.TypeMap()) != targetIDsBefore {
		t.Errorf("target type map size changed: %d -> %d", targetIDsBefore, len(target.TypeMap()))
	}
	if len(fragment.TypeMap()) != fragIDsBefore {
		t.Errorf("fragment type map size changed: %d -> %d", fragIDsBefore, len(fragment.TypeMap()))
	}
}

func TestSubstituteMultipleNonOverlappingTargets(t *testing.T) {
	env := scratchEnv(t)
	// Two independent predicates.
	target, targetSrc := scratchCheck(t, env,
		`items.all(x, x.startsWith("a")) && items.exists(y, y.endsWith("z"))`)
	// Both fragments refer to the global x (string); the target binders x and y must not capture
	// the fragment free variables.
	fragmentA, fragSrcA := scratchCheck(t, env, `x == "a"`)
	fragmentB, fragSrcB := scratchCheck(t, env, `x == "z"`)

	startsWithID := findCallID(t, target, "startsWith")
	endsWithID := findCallID(t, target, "endsWith")

	res, issues := SubstituteExprs(env, targetSrc, target, []Substitution{
		{TargetID: startsWithID, Fragment: fragmentA, FragmentSource: fragSrcA},
		{TargetID: endsWithID, Fragment: fragmentB, FragmentSource: fragSrcB},
	})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	out := unparse(t, res.AST)
	if !strings.Contains(out, `x == "a"`) || !strings.Contains(out, `x == "z"`) {
		t.Errorf("expected both splices, got %q", out)
	}
	if strings.Contains(out, "all(x,") {
		t.Errorf("expected all() iter var to be renamed, got %q", out)
	}
	if !strings.Contains(out, "exists(y,") {
		t.Errorf("expected unrelated exists(y,...) binder to remain, got %q", out)
	}

	// Fresh ids must be unique across the whole merged tree.
	seen := map[int64]bool{}
	ast.PostOrderVisit(res.AST.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if seen[e.ID()] {
			t.Errorf("duplicate expression id %d", e.ID())
		}
		seen[e.ID()] = true
	}))

	rechecked, errs := Check(res.AST, res.Source, env)
	if len(errs.GetErrors()) != 0 {
		t.Fatalf("recheck failed: %s", errs.ToDisplayString())
	}
	_ = rechecked
}

func findComprehensionID(t *testing.T, a *ast.AST) int64 {
	t.Helper()
	var found int64
	ast.PostOrderVisit(a.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if found == 0 && e.Kind() == ast.ComprehensionKind {
			found = e.ID()
		}
	}))
	if found == 0 {
		t.Fatal("no comprehension")
	}
	return found
}

func TestSubstituteMacroSourceMigratesWithSplice(t *testing.T) {
	env := scratchEnv(t)
	target, targetSrc := scratchCheck(t, env, `items.all(x, x.startsWith("a"))`)
	fragment, fragSrc := scratchCheck(t, env, `x == "a"`)

	predicateID := findCallID(t, target, "startsWith")

	// Record ids present in the replaced target subtree (the predicate) before substitution.
	staleIDs := map[int64]bool{}
	predicateNode := indexExprs(target.Expr())[predicateID]
	for id := range collectExprIDs(predicateNode) {
		if id != predicateID {
			staleIDs[id] = true
		}
	}

	res, issues := SubstituteExprs(env, targetSrc, target, []Substitution{
		{TargetID: predicateID, Fragment: fragment, FragmentSource: fragSrc},
	})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	for id := range staleIDs {
		if _, found := res.AST.TypeMap()[id]; found {
			t.Errorf("stale type metadata retained for removed target id %d", id)
		}
		if _, found := res.AST.ReferenceMap()[id]; found {
			t.Errorf("stale reference metadata retained for removed target id %d", id)
		}
	}
	// The splice target is a non-macro subexpression (the startsWith predicate) that appears as a
	// concrete copy inside the enclosing all() macro call signature.

	// The enclosing all() macro entry must survive and reference the splice point so unparsing
	// renders the macro call rather than the expanded comprehension.
	if len(res.AST.SourceInfo().MacroCalls()) == 0 {
		t.Fatal("macro call metadata was lost after substitution")
	}
	out := unparse(t, res.AST)
	if !strings.HasPrefix(out, "items.all(") {
		t.Errorf("expected all() macro to unparse, got %q", out)
	}
	if !strings.Contains(out, `x == "a"`) {
		t.Errorf("expected spliced predicate in macro call, got %q", out)
	}
}
