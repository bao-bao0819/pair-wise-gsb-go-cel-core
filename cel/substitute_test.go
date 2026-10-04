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

	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/parser"
)

func substituteTestEnv(t *testing.T) *Env {
	t.Helper()
	env, err := NewEnv(
		Variable("items", ListType(StringType)),
		Variable("x", StringType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func compileForSubstitute(t *testing.T, env *Env, desc, text string) (*Ast, Source) {
	t.Helper()
	src := common.NewStringSource(text, desc)
	ast, iss := env.CompileSource(src)
	if iss.Err() != nil {
		t.Fatalf("compile %q failed: %v", text, iss.Err())
	}
	return ast, src
}

func callIDIn(t *testing.T, a *Ast, function string) int64 {
	t.Helper()
	var found int64
	celast.PostOrderVisit(a.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if found == 0 && e.Kind() == celast.CallKind && e.AsCall().FunctionName() == function {
			found = e.ID()
		}
	}))
	if found == 0 {
		t.Fatalf("no call with function %q", function)
	}
	return found
}

// TestSubstituteExprsProgramEvaluation verifies the full loop: substitution with alpha-renaming,
// rechecking the result through the checker, and executing it as a Program.
func TestSubstituteExprsProgramEvaluation(t *testing.T) {
	env := substituteTestEnv(t)

	// Target binds a local x inside all(); the predicate is a bool over the list element.
	targetAst, targetSrc := compileForSubstitute(t, env, "rule",
		`items.all(x, x.startsWith("a"))`)

	// Fragment free variable x means the global string x.
	fragmentAst, fragmentSrc := compileForSubstitute(t, env, "fragment",
		`x == "global"`)

	predicateID := callIDIn(t, targetAst, "startsWith")

	chk, err := env.initChecker()
	if err != nil {
		t.Fatal(err)
	}
	res, issues := checker.SubstituteExprs(chk, targetSrc, targetAst.NativeRep(),
		[]checker.Substitution{{
			TargetID:       predicateID,
			Fragment:       fragmentAst.NativeRep(),
			FragmentSource: fragmentSrc,
		}})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}

	// The local iter x must have been renamed while the fragment free x stays global.
	out, err := parser.Unparse(res.AST.Expr(), res.AST.SourceInfo())
	if err != nil {
		celast.PostOrderVisit(res.AST.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
			_, hasMacro := res.AST.SourceInfo().GetMacroCall(e.ID())
			t.Logf("node id=%d kind=%v macro=%v", e.ID(), e.Kind(), hasMacro)
		}))
		t.Fatal(err)
	}
	if strings.Contains(out, "all(x,") {
		t.Fatalf("expected captured iter var to be renamed, got %q", out)
	}
	if !strings.Contains(out, `x == "global"`) {
		t.Fatalf("expected fragment spliced, got %q", out)
	}

	// Recheck the substituted AST through the public checker path.
	rewrapped := &Ast{source: res.Source, impl: res.AST}
	rechecked, iss := env.Check(rewrapped)
	if iss.Err() != nil {
		t.Fatalf("recheck failed: %v", iss.Err())
	}

	// Execute the rechecked result.
	prg, err := env.Program(rechecked)
	if err != nil {
		t.Fatalf("program planning failed: %v", err)
	}
	out2, _, err := prg.Eval(map[string]any{
		"items": []string{"nope", "global"},
		"x":     "global",
	})
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	// The fragment compares the GLOBAL x against "global" for every list element, so the result
	// is true regardless of the list contents. If the local iter x had captured the fragment,
	// the first element "nope" would make it false.
	if out2.Value() != true {
		t.Errorf("eval = %v, want true (fragment free x must bind globally)", out2.Value())
	}
}

// TestSubstituteExprsNestedMacrosProgramEvaluation exercises nested all/map macros where the
// splice point sits under two same-named binders and the fragment also contains its own macros.
func TestSubstituteExprsNestedMacrosProgramEvaluation(t *testing.T) {
	env := substituteTestEnv(t)

	targetAst, targetSrc := compileForSubstitute(t, env, "rule",
		`items.all(x, items.map(x, x.startsWith("a")).size() > 0)`)
	// Fragment itself uses a map macro and refers to the global free variable x.
	fragmentAst, fragmentSrc := compileForSubstitute(t, env, "fragment",
		`[x].exists(v, v == "global")`)

	predicateID := callIDIn(t, targetAst, "startsWith")

	chk, err := env.initChecker()
	if err != nil {
		t.Fatal(err)
	}
	res, issues := checker.SubstituteExprs(chk, targetSrc, targetAst.NativeRep(),
		[]checker.Substitution{{
			TargetID:       predicateID,
			Fragment:       fragmentAst.NativeRep(),
			FragmentSource: fragmentSrc,
		}})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}

	// All ids must be unique and every node must carry migrated type metadata; the target macros
	// (all, map) and the fragment macro (exists) must all survive.
	seenIDs := map[int64]bool{}
	celast.PostOrderVisit(res.AST.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if seenIDs[e.ID()] {
			t.Errorf("duplicate id %d after substitution", e.ID())
		}
		seenIDs[e.ID()] = true
		if got := res.AST.GetType(e.ID()); got == nil {
			t.Errorf("missing migrated type for id %d", e.ID())
		}
	}))
	if got := len(res.AST.SourceInfo().MacroCalls()); got < 3 {
		t.Errorf("expected >=3 macro calls migrated, got %d", got)
	}

	rewrapped := &Ast{source: res.Source, impl: res.AST}
	rechecked, iss := env.Check(rewrapped)
	if iss.Err() != nil {
		t.Fatalf("recheck failed: %v", iss.Err())
	}

	prg, err := env.Program(rechecked)
	if err != nil {
		t.Fatalf("program planning failed: %v", err)
	}
	val, _, err := prg.Eval(map[string]any{
		"items": []string{"anything"},
		"x":     "global",
	})
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if val.Value() != true {
		t.Errorf("eval = %v, want true", val.Value())
	}
}

// TestSubstituteExprsTypeMismatchRejected ensures an incompatible fragment result type is
// rejected before any AST is produced.
func TestSubstituteExprsTypeMismatchRejected(t *testing.T) {
	env := substituteTestEnv(t)
	targetAst, targetSrc := compileForSubstitute(t, env, "rule", `items.all(x, x.startsWith("a"))`)
	fragmentAst, fragmentSrc := compileForSubstitute(t, env, "fragment", `items`)

	predicateID := callIDIn(t, targetAst, "startsWith")
	chk, err := env.initChecker()
	if err != nil {
		t.Fatal(err)
	}
	res, issues := checker.SubstituteExprs(chk, targetSrc, targetAst.NativeRep(),
		[]checker.Substitution{{
			TargetID:       predicateID,
			Fragment:       fragmentAst.NativeRep(),
			FragmentSource: fragmentSrc,
		}})
	if res != nil {
		t.Fatal("expected nil result on rejection")
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Origin != checker.FragmentOrigin {
		t.Errorf("origin = %v, want fragment", issues[0].Origin)
	}
	if !strings.Contains(issues[0].Message, "not assignable") {
		t.Errorf("message = %q", issues[0].Message)
	}
}

// TestSubstituteExprsIssueSourceLocation verifies fragment issues carry a location within the
// fragment source rather than the target.
func TestSubstituteExprsIssueSourceLocation(t *testing.T) {
	env, err := NewEnv(
		Variable("items", ListType(StringType)),
		Variable("x", StringType),
		Variable("z", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatal(err)
	}
	// A second environment without z models an incompatible environment for the fragment.
	envNoZ, err := NewEnv(
		Variable("items", ListType(StringType)),
		Variable("x", StringType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatal(err)
	}

	targetAst, targetSrc := compileForSubstitute(t, env, "rule", `x == "a"`)

	// Compile the fragment against the env that declares z, then substitute into the env without z.
	fragmentAst, fragmentSrc := compileForSubstitute(t, env, "fragment-src", `z + 1`)

	eqID := callIDIn(t, targetAst, "_==_")
	chk, err := envNoZ.initChecker()
	if err != nil {
		t.Fatal(err)
	}
	_, issues := checker.SubstituteExprs(chk, targetSrc, targetAst.NativeRep(),
		[]checker.Substitution{{
			TargetID:       eqID,
			Fragment:       fragmentAst.NativeRep(),
			FragmentSource: fragmentSrc,
		}})
	if len(issues) == 0 {
		t.Fatal("expected rejection")
	}
	var undeclared *checker.SubstitutionIssue
	for _, iss := range issues {
		if strings.Contains(iss.Message, "'z'") {
			undeclared = iss
		}
	}
	if undeclared == nil {
		t.Fatalf("expected undeclared z issue, got %v", issues)
	}
	if undeclared.Origin != checker.FragmentOrigin {
		t.Errorf("origin = %v, want fragment", undeclared.Origin)
	}
	// The undeclared-variable location points into the fragment source (z is column 0).
	if undeclared.Location.Column() != 0 || undeclared.Location.Line() != 1 {
		t.Errorf("location = %d:%d, want 1:0", undeclared.Location.Line(), undeclared.Location.Column())
	}
}
