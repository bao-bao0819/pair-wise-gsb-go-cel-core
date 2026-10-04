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
	"fmt"
	"strings"
	"testing"

	"cel.dev/cel-go/common"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
)

func ExampleSubstitution() {
	env, err := NewEnv(
		Variable("items", ListType(IntType)),
		Variable("threshold", IntType),
		Variable("x", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		panic(err)
	}
	rule, iss := env.Compile(`items.exists(x, x > 10)`)
	if iss != nil && iss.Err() != nil {
		panic(iss.Err())
	}
	fragment, fragIss := env.Compile(`x < threshold`)
	if fragIss != nil && fragIss.Err() != nil {
		panic(fragIss.Err())
	}

	// Find the comparison "x > 10" and replace it with the checked fragment. The target's iter
	// variable "x" is alpha-renamed because it is free in the fragment context.
	compareID := findExampleCompareID(rule.NativeRep())
	merged, err := NewSubstitution(env, rule).Replace(compareID, fragment).Substitute()
	if err != nil {
		panic(err)
	}
	prg, err := env.Program(merged)
	if err != nil {
		panic(err)
	}
	out, _, err := prg.Eval(map[string]any{
		"items":     []int64{100, 200},
		"threshold": int64(5),
		"x":         int64(3),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(out.Value())
	// Output: true
}

func findExampleCompareID(a *celast.AST) int64 {
	matches := celast.MatchDescendants(celast.NavigateAST(a), func(e celast.NavigableExpr) bool {
		return e.Kind() == celast.CallKind && e.AsCall().FunctionName() == "_>_"
	})
	return matches[0].ID()
}

func TestSubstitutionNestedAllMapCapture(t *testing.T) {
	// Rule with nested all() and map() comprehensions whose iteration variables shadow the free
	// variables of the incoming fragment. The outer all() returns bool; the inner map() returns
	// a list whose sole element is directly observed through an equality comparison, making the
	// distinction between capture and alpha-renaming observable in the program result.
	rule := `
groups.all(group,
  group.members.all(member,
    member.tags.map(tag, tag.score + member.bonus)[0] == 21
  )
)`
	env, err := NewEnv(
		Variable("groups", ListType(MapType(StringType, DynType))),
		Variable("group", MapType(StringType, DynType)),
		Variable("member", MapType(StringType, DynType)),
		Variable("tag", MapType(StringType, DynType)),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}

	target, iss := env.Compile(rule)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(rule) failed: %v", iss.Err())
	}

	// Fragment whose free variables collide with the comprehensions at the insertion sites:
	// 'tag', 'member', and 'group' must continue to refer to the outer declared variables rather
	// than to the iter vars of the target's all()/map() macros. Correct binding yields
	// 10 + 7 + 4 == 21; captured binding would yield 3 + 2 + 100 == 105.
	fragment := `tag.score + member.bonus + group.weight`
	frag, fragIss := env.Compile(fragment)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(fragment) failed: %v", fragIss.Err())
	}

	// Locate the target id: the inner map() transform "tag.score + member.bonus".
	targetID := findExprID(t, target.NativeRep(), func(e celast.NavigableExpr) bool {
		if e.Kind() != celast.CallKind {
			return false
		}
		if e.AsCall().FunctionName() != "_+_" {
			return false
		}
		args := e.AsCall().Args()
		if len(args) != 2 {
			return false
		}
		// The inner add(tag.score, member.bonus): left operand is a select of field "score".
		if args[0].Kind() != celast.SelectKind || args[0].AsSelect().FieldName() != "score" {
			return false
		}
		return true
	})

	result, subErr := NewSubstitution(env, target).Replace(targetID, frag).Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}

	// The result must be re-checkable against the target environment.
	checked, checkIss := env.Check(result)
	if checkIss != nil && checkIss.Err() != nil {
		t.Fatalf("Check(result) failed: %v", checkIss.Err())
	}

	// Inputs chosen so captured (wrong) and alpha-renamed (correct) bindings produce different
	// numeric results and therefore flip the == 21 comparison in the rule.
	prg, err := env.Program(checked)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	input := map[string]any{
		"groups": []map[string]any{
			{
				"weight": int64(100),
				"members": []map[string]any{
					{
						"bonus": int64(2),
						"tags": []map[string]any{
							{"score": int64(3)},
						},
					},
				},
			},
		},
		// Declared outer variables with the same names as the comprehension binders. The
		// substituted fragment must read these values.
		"tag":    map[string]any{"score": int64(10)},
		"member": map[string]any{"bonus": int64(7)},
		"group":  map[string]any{"weight": int64(4)},
	}
	out, _, err := prg.Eval(input)
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	// With alpha renaming the fragment reads the outer declared variables (10 + 7 + 4 == 21),
	// so the inner comparison and both all() predicates are true. Without renaming the iter
	// vars would be captured (3 + 2 + 100 == 105 != 21) and the result would be false.
	if got := out.Value(); got != true {
		t.Fatalf("Eval() got %v, wanted true (fragment free vars must not be captured)", got)
	}
}

func TestSubstitutionMultipleDisjointTargets(t *testing.T) {
	env, err := NewEnv(
		Variable("items", ListType(StringType)),
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	rule := `items.exists(x, x.startsWith("a")) && n > 10`
	target, iss := env.Compile(rule)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile() failed: %v", iss.Err())
	}

	frag1, iss1 := env.Compile(`n == 3`)
	if iss1 != nil && iss1.Err() != nil {
		t.Fatalf("Compile(frag1) failed: %v", iss1.Err())
	}
	frag2, iss2 := env.Compile(`n + 5`)
	if iss2 != nil && iss2.Err() != nil {
		t.Fatalf("Compile(frag2) failed: %v", iss2.Err())
	}

	// Targets: the startsWith() predicate within the exists macro (bool) and the literal 10
	// on the right side of "n > 10" (int).
	startsWithID := findMemberCallID(t, target.NativeRep(), "startsWith")
	compareLitID := findLiteralID(t, target.NativeRep(), int64(10))

	result, subErr := NewSubstitution(env, target).
		Replace(startsWithID, frag1).
		Replace(compareLitID, frag2).
		Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}

	prg, err := env.Program(result)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, _, err := prg.Eval(map[string]any{
		"items": []string{"a"},
		"n":     int64(3),
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	// Predicate becomes n == 3 -> true, so exists(...) is true; n + 5 > 10 -> 8 > 10 -> false
	// => true && false == false.
	if out.Value() != false {
		t.Fatalf("Eval() got %v, wanted false", out.Value())
	}
}

func TestSubstitutionSameFragmentAtMultipleSites(t *testing.T) {
	env, err := NewEnv(
		Variable("a", IntType),
		Variable("b", IntType),
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`a + b > 0 && a - b < 100`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile() failed: %v", iss.Err())
	}
	frag, fragIss := env.Compile(`n * 2`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}

	// Replace both literal-free leaf identifiers "a" and "b" with the same fragment instance.
	bIDs := findAllIdentIDs(target.NativeRep(), "b")
	aIDs := findAllIdentIDs(target.NativeRep(), "a")

	result, subErr := NewSubstitution(env, target).
		Replace(aIDs[0], frag).
		Replace(bIDs[0], frag).
		Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}
	prg, err := env.Program(result)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, _, err := prg.Eval(map[string]any{
		"a": int64(0),
		"b": int64(0),
		"n": int64(3),
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	// (3*2)+(3*2) > 0 == true && (3*2)-(3*2) < 100 == true.
	if out.Value() != true {
		t.Fatalf("Eval() got %v, wanted true", out.Value())
	}

	// Both inserted copies must have distinct ids: walk the tree and count duplicates.
	idCounts := map[int64]int{}
	celast.PostOrderVisit(result.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		idCounts[e.ID()]++
	}))
	for id, count := range idCounts {
		if count > 1 {
			t.Fatalf("id %d appears on %d expression nodes", id, count)
		}
	}
}

func TestSubstitutionRejectsOverlap(t *testing.T) {
	env, err := NewEnv(
		Variable("items", ListType(IntType)),
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`items.exists(x, x > n)`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile() failed: %v", iss.Err())
	}
	fragBool, _ := env.Compile(`n == 1`)
	fragInt, _ := env.Compile(`n == 1`)

	compreID := findComprehensionID(t, target.NativeRep())
	compareID := findBinaryCompareID(t, target.NativeRep(), nil)
	// Both sites are bool-typed, so rejection is specifically about overlap rather than type.

	_, subErr := NewSubstitution(env, target).
		Replace(compreID, fragBool).
		Replace(compareID, fragInt). // nested inside the comprehension
		Substitute()
	if subErr == nil {
		t.Fatal("Substitute() succeeded, expected overlap rejection")
	}
	if !strings.Contains(subErr.Error(), "overlap") {
		t.Fatalf("expected overlap error, got %v", subErr)
	}
}

func TestSubstitutionRejectsTypeMismatch(t *testing.T) {
	env, err := NewEnv(
		Variable("items", ListType(IntType)),
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`items.exists(x, x > n)`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile() failed: %v", iss.Err())
	}
	fragInt, _ := env.Compile(`n + 1`)

	compreID := findComprehensionID(t, target.NativeRep())
	_, subErr := NewSubstitution(env, target).
		Replace(compreID, fragInt). // bool site, int fragment
		Substitute()
	if subErr == nil {
		t.Fatal("Substitute() succeeded, expected type mismatch rejection")
	}
	if !strings.Contains(subErr.Error(), "type") {
		t.Fatalf("expected type error, got %v", subErr)
	}
}

func TestSubstitutionRejectsEnvMismatch(t *testing.T) {
	env, err := NewEnv(
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	otherEnv, err := NewEnv(
		Variable("n", StringType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv(other) failed: %v", err)
	}
	target, iss := env.Compile(`n + 1`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(target) failed: %v", iss.Err())
	}
	frag, fragIss := otherEnv.Compile(`n`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}

	rootID := target.NativeRep().Expr().ID()
	_, subErr := NewSubstitution(env, target).
		ReplaceWithEnv(otherEnv, rootID, frag).
		Substitute()
	if subErr == nil {
		t.Fatal("Substitute() succeeded, expected environment mismatch rejection")
	}
	if !strings.Contains(subErr.Error(), "environment") {
		t.Fatalf("expected environment error, got %v", subErr)
	}
}

func TestSubstitutionAcceptsExtendedEnv(t *testing.T) {
	// A fragment checked in an Env produced by Extend from the target environment shares the
	// same underlying checker declarations and must be accepted.
	env, err := NewEnv(
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	extended, err := env.Extend()
	if err != nil {
		t.Fatalf("Extend() failed: %v", err)
	}
	target, iss := env.Compile(`n > 0`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(target) failed: %v", iss.Err())
	}
	frag, fragIss := extended.Compile(`n + 1`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}

	litID := findLiteralID(t, target.NativeRep(), int64(0))
	result, subErr := NewSubstitution(env, target).
		ReplaceWithEnv(extended, litID, frag).
		Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}
	prg, err := env.Program(result)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, _, err := prg.Eval(map[string]any{"n": int64(2)})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	// n > (n + 1) == 2 > 3 == false.
	if out.Value() != false {
		t.Fatalf("Eval() got %v, wanted false", out.Value())
	}
}

func TestSubstitutionReplacesRoot(t *testing.T) {
	env, err := NewEnv(
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`n + 1`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(target) failed: %v", iss.Err())
	}
	frag, fragIss := env.Compile(`n * 2`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}
	result, subErr := NewSubstitution(env, target).
		Replace(target.NativeRep().Expr().ID(), frag).
		Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}
	prg, err := env.Program(result)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, _, err := prg.Eval(map[string]any{"n": int64(21)})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	if out.Value() != int64(42) {
		t.Fatalf("Eval() got %v, wanted 42", out.Value())
	}
	if result.OutputType().String() != "int" {
		t.Fatalf("output type got %s, wanted int", result.OutputType())
	}
}

func TestSubstitutionDoesNotMutateInputs(t *testing.T) {
	env, err := NewEnv(
		Variable("items", ListType(IntType)),
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`items.exists(x, x > n)`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile() failed: %v", iss.Err())
	}
	frag, fragIss := env.Compile(`n == 2`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}

	targetBefore, _ := AstToString(target)
	fragBefore, _ := AstToString(frag)

	compreID := findComprehensionID(t, target.NativeRep())
	_, err = NewSubstitution(env, target).Replace(compreID, frag).Substitute()
	if err != nil {
		t.Fatalf("Substitute() failed: %v", err)
	}

	targetAfter, _ := AstToString(target)
	fragAfter, _ := AstToString(frag)
	if targetBefore != targetAfter {
		t.Fatalf("target AST mutated: before=%q after=%q", targetBefore, targetAfter)
	}
	if fragBefore != fragAfter {
		t.Fatalf("fragment AST mutated: before=%q after=%q", fragBefore, fragAfter)
	}
}

func TestSubstitutionErrorAttribution(t *testing.T) {
	env, err := NewEnv(
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`n + 1 > 0`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(target) failed: %v", iss.Err())
	}

	// Unknown target id: error is reported against the target source.
	_, subErr := NewSubstitution(env, target).Replace(99999, target).Substitute()
	if subErr == nil {
		t.Fatal("expected error for unknown target id")
	}
	se, ok := subErr.(*SubstitutionError)
	if !ok {
		t.Fatalf("expected *SubstitutionError, got %T", subErr)
	}
	if len(se.TargetErrors()) == 0 {
		t.Fatalf("expected target error, got %v", se)
	}

	// Unchecked fragment: error is reported against the fragment source description.
	parsedFrag, pIss := env.Parse(`n + 1`)
	if pIss != nil && pIss.Err() != nil {
		t.Fatalf("Parse failed: %v", pIss.Err())
	}
	rootID := target.NativeRep().Expr().ID()
	_, subErr = NewSubstitution(env, target).Replace(rootID, parsedFrag).Substitute()
	if subErr == nil {
		t.Fatal("expected error for unchecked fragment")
	}
	se, ok = subErr.(*SubstitutionError)
	if !ok {
		t.Fatalf("expected *SubstitutionError, got %T", subErr)
	}
	if len(se.FragmentErrors()) == 0 {
		t.Fatalf("expected fragment error, got %v", se)
	}
	if !strings.Contains(se.Error(), "type-checked") {
		t.Fatalf("expected message about type-checked fragment, got %v", se)
	}
}

func TestSubstitutionUniqueIDsAndMetadata(t *testing.T) {
	env, err := NewEnv(
		Variable("items", ListType(IntType)),
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`items.exists(x, x > n) && n > 1`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(target) failed: %v", iss.Err())
	}
	frag, fragIss := env.Compile(`n == 2`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}

	compreID := findComprehensionID(t, target.NativeRep())
	result, subErr := NewSubstitution(env, target).Replace(compreID, frag).Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}

	// All node ids in the resulting AST (including macro calls) must be unique.
	ids := result.NativeRep().IDs()
	seen := map[int64]struct{}{}
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d in result", id)
		}
		seen[id] = struct{}{}
	}

	// Every expression node must carry a type (the result is checked) and function calls within
	// the migrated fragment must carry overload selections.
	missingTypes := 0
	missingRefs := 0
	celast.PostOrderVisit(result.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.ID() == 0 {
			return
		}
		if t := result.NativeRep().GetType(e.ID()); t == nil {
			missingTypes++
		}
		if e.Kind() == celast.CallKind {
			if len(result.NativeRep().GetOverloadIDs(e.ID())) == 0 {
				missingRefs++
			}
		}
	}))
	if missingTypes != 0 {
		t.Fatalf("%d nodes missing types", missingTypes)
	}
	if missingRefs != 0 {
		t.Fatalf("%d call nodes missing overload references", missingRefs)
	}

	// The result must still be a valid executable program.
	prg, err := env.Program(result)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, _, err := prg.Eval(map[string]any{
		"items": []int64{1, 2, 3},
		"n":     int64(2),
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	if out.Value() != true {
		t.Fatalf("Eval() got %v, wanted true", out.Value())
	}
}

func TestSubstitutionFragmentWithoutCaptureRisk(t *testing.T) {
	// When the fragment has no free variable matching a binder at the insertion site, no alpha
	// renaming is required and the original binder names are preserved.
	env, err := NewEnv(
		Variable("items", ListType(IntType)),
		Variable("n", IntType),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`items.exists(x, x > 0)`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(target) failed: %v", iss.Err())
	}
	frag, fragIss := env.Compile(`n + 1 > 0`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}

	compareID := findBinaryCompareID(t, target.NativeRep(), nil)
	result, subErr := NewSubstitution(env, target).Replace(compareID, frag).Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}
	// The iter var "x" must be unchanged since the fragment only references n.
	preservedX := false
	celast.PostOrderVisit(result.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() == celast.ComprehensionKind && e.AsComprehension().IterVar() == "x" {
			preservedX = true
		}
	}))
	if !preservedX {
		t.Fatal("expected original iter var 'x' to be preserved")
	}
	prg, err := env.Program(result)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, _, err := prg.Eval(map[string]any{
		"items": []int64{1},
		"n":     int64(0),
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	if out.Value() != true {
		t.Fatalf("Eval() got %v, wanted true", out.Value())
	}
}

func TestSubstitutionTwoVarComprehension(t *testing.T) {
	// A custom two-variable macro exercises iterVar2 alpha-renaming: tables.allPair(k, v, ...)
	// iterates a map with both key and value binders live in the predicate.
	allPair := ReceiverMacro("allPair", 3, func(eh MacroExprFactory, target celast.Expr, args []celast.Expr) (celast.Expr, *common.Error) {
		k := args[0].AsIdent()
		v := args[1].AsIdent()
		predicate := args[2]
		return eh.NewComprehensionTwoVar(
			eh.Copy(target),
			k, v, eh.AccuIdentName(),
			eh.NewLiteral(types.True),
			eh.NewCall("@not_strictly_false", eh.NewAccuIdent()),
			eh.NewCall("_&&_", eh.NewAccuIdent(), predicate),
			eh.NewAccuIdent(),
		), nil
	})

	env, err := NewEnv(
		Variable("tables", MapType(StringType, IntType)),
		Variable("k", StringType),
		Variable("v", IntType),
		Macros(allPair),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`tables.allPair(k, v, v > 0)`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(target) failed: %v", iss.Err())
	}
	// Fragment free vars k/v must not be captured by the two iter binders.
	frag, fragIss := env.Compile(`v > k.size()`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}

	compareID := findBinaryCompareID(t, target.NativeRep(), nil)
	result, subErr := NewSubstitution(env, target).Replace(compareID, frag).Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}
	prg, err := env.Program(result)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, _, err := prg.Eval(map[string]any{
		"tables": map[string]int64{"a": 1},
		// Outer vars: v=100 and k="long" => 100 > 4 = true.
		"k": "long",
		"v": int64(100),
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	// Captured iter vars would give v=1 > k.size()("a"=1) = false.
	if out.Value() != true {
		t.Fatalf("Eval() got %v, wanted true", out.Value())
	}
}

func TestSubstitutionAlphaRenamesTargetBinders(t *testing.T) {
	env, err := NewEnv(
		Variable("groups", ListType(MapType(StringType, DynType))),
		Variable("group", MapType(StringType, DynType)),
		Variable("member", MapType(StringType, DynType)),
		Variable("tag", MapType(StringType, DynType)),
		EnableMacroCallTracking(),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	target, iss := env.Compile(`groups.all(group,
		group.members.all(member,
			member.tags.map(tag, tag.score)[0] > 0))`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(target) failed: %v", iss.Err())
	}
	frag, fragIss := env.Compile(`tag.score + member.bonus + group.weight`)
	if fragIss != nil && fragIss.Err() != nil {
		t.Fatalf("Compile(frag) failed: %v", fragIss.Err())
	}

	// The select "tag.score" inside the map transform is the capture site.
	scoreSelectID := findSelectFieldID(t, target.NativeRep(), "score")
	result, subErr := NewSubstitution(env, target).Replace(scoreSelectID, frag).Substitute()
	if subErr != nil {
		t.Fatalf("Substitute() failed: %v", subErr)
	}

	// Walk the resulting AST: the fragment's free identifiers must keep their original names,
	// and at least one comprehension binder on the target path must have been renamed away from
	// "tag", "member", or "group".
	renamedBinders := map[string]bool{}
	freeNamesFound := map[string]bool{}
	celast.PostOrderVisit(result.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() == celast.ComprehensionKind {
			compre := e.AsComprehension()
			for _, name := range []string{compre.IterVar(), compre.IterVar2(), compre.AccuVar()} {
				if name == "tag" || name == "member" || name == "group" {
					renamedBinders[name] = false
				}
				if strings.HasPrefix(name, "__cel_subst_") {
					renamedBinders[name] = true
				}
			}
		}
		if e.Kind() == celast.IdentKind {
			switch e.AsIdent() {
			case "tag", "member", "group":
				freeNamesFound[e.AsIdent()] = true
			}
		}
	}))

	foundRenamed := false
	for _, renamed := range renamedBinders {
		if renamed {
			foundRenamed = true
		}
	}
	if !foundRenamed {
		t.Fatalf("expected at least one target binder to be alpha-renamed, got binders %v", renamedBinders)
	}
	for _, expected := range []string{"tag", "member", "group"} {
		if !freeNamesFound[expected] {
			t.Fatalf("expected fragment free identifier %q to be preserved", expected)
		}
	}
}

// findSelectFieldID returns the id of the first select expression on the given field.
func findSelectFieldID(t *testing.T, a *celast.AST, field string) int64 {
	t.Helper()
	matches := celast.MatchDescendants(celast.NavigateAST(a), func(e celast.NavigableExpr) bool {
		return e.Kind() == celast.SelectKind && e.AsSelect().FieldName() == field
	})
	if len(matches) == 0 {
		t.Fatalf("no select on field %q found", field)
	}
	return matches[0].ID()
}

// findExprID returns the id of the first post-order node matching the predicate.
func findExprID(t *testing.T, a *celast.AST, match func(celast.NavigableExpr) bool) int64 {
	t.Helper()
	matches := celast.MatchDescendants(celast.NavigateAST(a), match)
	if len(matches) == 0 {
		t.Fatal("no matching expression found")
	}
	return matches[0].ID()
}

// findIdentID returns the id of the first identifier with the given name.
func findIdentID(t *testing.T, a *celast.AST, name string) int64 {
	t.Helper()
	return findExprID(t, a, func(e celast.NavigableExpr) bool {
		return e.Kind() == celast.IdentKind && e.AsIdent() == name
	})
}

// findAllIdentIDs returns the ids of all identifier nodes with the given name.
func findAllIdentIDs(a *celast.AST, name string) []int64 {
	matches := celast.MatchDescendants(celast.NavigateAST(a), func(e celast.NavigableExpr) bool {
		return e.Kind() == celast.IdentKind && e.AsIdent() == name
	})
	ids := make([]int64, len(matches))
	for i, m := range matches {
		ids[i] = m.ID()
	}
	return ids
}

// findComprehensionID returns the id of the sole comprehension in the AST.
func findComprehensionID(t *testing.T, a *celast.AST) int64 {
	t.Helper()
	return findExprID(t, a, func(e celast.NavigableExpr) bool {
		return e.Kind() == celast.ComprehensionKind
	})
}

// findBinaryCompareID returns the id of the first "_>_" binary call. If literal is non-nil the
// right-hand side must equal that literal value.
func findBinaryCompareID(t *testing.T, a *celast.AST, literal any) int64 {
	t.Helper()
	matches := celast.MatchDescendants(celast.NavigateAST(a), func(e celast.NavigableExpr) bool {
		if e.Kind() != celast.CallKind {
			return false
		}
		c := e.AsCall()
		if c.FunctionName() != "_>_" || len(c.Args()) != 2 {
			return false
		}
		if literal == nil {
			return true
		}
		rhs := c.Args()[1]
		return rhs.Kind() == celast.LiteralKind && rhs.AsLiteral().Value() == literal
	})
	if len(matches) == 0 {
		t.Fatal("no matching compare found")
	}
	return matches[0].ID()
}

// findMemberCallID returns the id of the first member call with the given function name.
func findMemberCallID(t *testing.T, a *celast.AST, name string) int64 {
	t.Helper()
	matches := celast.MatchDescendants(celast.NavigateAST(a), func(e celast.NavigableExpr) bool {
		if e.Kind() != celast.CallKind {
			return false
		}
		c := e.AsCall()
		return c.IsMemberFunction() && c.FunctionName() == name
	})
	if len(matches) == 0 {
		t.Fatalf("no member call %q found", name)
	}
	return matches[0].ID()
}

// findLiteralID returns the id of the first literal node with the given value.
func findLiteralID(t *testing.T, a *celast.AST, value any) int64 {
	t.Helper()
	matches := celast.MatchDescendants(celast.NavigateAST(a), func(e celast.NavigableExpr) bool {
		return e.Kind() == celast.LiteralKind && e.AsLiteral().Value() == value
	})
	if len(matches) == 0 {
		t.Fatalf("no literal %v found", value)
	}
	return matches[0].ID()
}
