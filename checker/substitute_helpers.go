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
	"cel.dev/cel-go/common/ast"
)

// renamedNamePrefix is used when generating fresh comprehension binder names. The underscores
// keep the identifier legal CEL source while making collisions with user variables effectively
// impossible.
const renamedNamePrefix = "__cel_renamed_"

// ------------------------------------------------------------------------------------------------
// Free identifier analysis
// ------------------------------------------------------------------------------------------------

// freeIdentifiers returns the set of free identifier names referenced by the fragment, following
// CEL comprehension scoping. Comprehension iter and accumulator variables are bound and are not
// reported. Names are returned exactly as they appear in the checked AST.
func freeIdentifiers(a *ast.AST) map[string]struct{} {
	fv := &freeVarWalker{
		scope: map[string]int{},
		found: map[string]struct{}{},
	}
	fv.walk(a.Expr())
	return fv.found
}

type freeVarWalker struct {
	scope map[string]int
	found map[string]struct{}
}

func (w *freeVarWalker) push(names ...string) {
	for _, n := range names {
		if n == "" {
			continue
		}
		w.scope[n]++
	}
}

func (w *freeVarWalker) pop(names ...string) {
	for _, n := range names {
		if n == "" {
			continue
		}
		if w.scope[n] <= 1 {
			delete(w.scope, n)
		} else {
			w.scope[n]--
		}
	}
}

func (w *freeVarWalker) bind(names ...string) func() {
	w.push(names...)
	return func() { w.pop(names...) }
}

func (w *freeVarWalker) walk(e ast.Expr) {
	if e == nil {
		return
	}
	switch e.Kind() {
	case ast.IdentKind:
		name := e.AsIdent()
		if _, bound := w.scope[name]; !bound {
			w.found[name] = struct{}{}
		}
	case ast.SelectKind:
		s := e.AsSelect()
		// Selects are left alone even though qualified identifiers are sometimes rewritten into
		// chained selects before checking; a checked AST resolves globals to Ident nodes.
		w.walk(s.Operand())
	case ast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			w.walk(c.Target())
		}
		for _, arg := range c.Args() {
			w.walk(arg)
		}
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			w.walk(el)
		}
	case ast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			w.walk(me.Key())
			w.walk(me.Value())
		}
	case ast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			w.walk(field.AsStructField().Value())
		}
	case ast.ComprehensionKind:
		c := e.AsComprehension()
		w.walk(c.IterRange())
		w.walk(c.AccuInit())
		// The accumulator is in scope for the whole comprehension body.
		popAccu := w.bind(c.AccuVar())
		// The iter variables are in scope for the condition and step, but not the result.
		w.walk(c.LoopCondition())
		popIter := w.bind(c.IterVar(), c.IterVar2())
		w.walk(c.LoopStep())
		popIter()
		w.walk(c.Result())
		popAccu()
	}
}

// ------------------------------------------------------------------------------------------------
// Fragment validation
// ------------------------------------------------------------------------------------------------

func (b *substituteBuilder) validateFragment(p *preparedSubstitution) {
	loc := p.sub.Fragment.SourceInfo().GetStartLocation(p.sub.Fragment.Expr().ID())

	// Every free identifier must resolve in the target environment to an equivalent declared
	// type. The checked fragment stores the expected type at the identifier node.
	for name := range p.freeIdents {
		fragLoc := p.sub.Fragment.SourceInfo().GetStartLocation(findIdentID(p.sub.Fragment, name))
		resolution := b.env.resolveSimpleIdent(name)
		if resolution == nil {
			b.report(FragmentOrigin, p.index, fragLoc,
				"fragment free variable '%s' is not declared in the target environment", name)
			continue
		}
		expected := p.sub.Fragment.GetType(findIdentID(p.sub.Fragment, name))
		if !resolution.Type().IsEquivalentType(expected) {
			b.report(FragmentOrigin, p.index, fragLoc,
				"fragment free variable '%s' has type '%s' in the fragment but '%s' in the target environment",
				name, FormatCELType(expected), FormatCELType(resolution.Type()))
		}
	}

	// Every selected overload must exist in the target environment declarations.
	callByName := indexCallFunctions(p.sub.Fragment)
	for id, ref := range p.sub.Fragment.ReferenceMap() {
		if len(ref.OverloadIDs) == 0 {
			continue
		}
		fnName := callByName[id]
		if fnName == "" {
			// Some overload selections (e.g. optional field selection) attach to non-call nodes.
			fnName = ref.Name
		}
		if fnName == "select_optional_field" {
			// Optional field selection is a checker-built pseudo-function present in any
			// environment that supports optional types; it requires no declaration lookup.
			continue
		}
		fn := b.env.lookupFunction(fnName)
		if fn == nil {
			b.report(FragmentOrigin, p.index, p.sub.Fragment.SourceInfo().GetStartLocation(id),
				"fragment uses an undeclared function '%s'", fnName)
			continue
		}
		available := map[string]struct{}{}
		for _, ov := range fn.OverloadDecls() {
			available[ov.ID()] = struct{}{}
		}
		for _, overloadID := range ref.OverloadIDs {
			if _, found := available[overloadID]; !found {
				b.report(FragmentOrigin, p.index, p.sub.Fragment.SourceInfo().GetStartLocation(id),
					"fragment overload '%s' is not declared in the target environment", overloadID)
			}
		}
	}

	// The fragment result type must fit the target position.
	if p.targetType != nil && p.fragType != nil && !p.targetType.IsAssignableType(p.fragType) {
		b.report(FragmentOrigin, p.index, loc,
			"fragment result type '%s' is not assignable to target type '%s'",
			FormatCELType(p.fragType), FormatCELType(p.targetType))
	}
}

// findIdentID returns an expression id for a free identifier so its fragment type can be looked
// up. The first matching identifier node is sufficient as free identifiers sharing a name share a
// declaration.
func findIdentID(a *ast.AST, name string) int64 {
	var found int64
	ast.PostOrderVisit(a.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if found == 0 && e.Kind() == ast.IdentKind && e.AsIdent() == name {
			found = e.ID()
		}
	}))
	return found
}

// indexCallFunctions returns a map from call expression id to function name.
func indexCallFunctions(a *ast.AST) map[int64]string {
	names := map[int64]string{}
	ast.PostOrderVisit(a.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() == ast.CallKind {
			names[e.ID()] = e.AsCall().FunctionName()
		}
	}))
	return names
}

// ------------------------------------------------------------------------------------------------
// Target indexing and overlap detection
// ------------------------------------------------------------------------------------------------

// collectExprIDs returns the set of expression and entry ids reachable from the expression tree.
func collectExprIDs(root ast.Expr) map[int64]struct{} {
	ids := map[int64]struct{}{}
	collectExprIDsInto(root, ids)
	return ids
}

func collectExprIDsInto(e ast.Expr, ids map[int64]struct{}) {
	if e == nil {
		return
	}
	ids[e.ID()] = struct{}{}
	switch e.Kind() {
	case ast.SelectKind:
		collectExprIDsInto(e.AsSelect().Operand(), ids)
	case ast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			collectExprIDsInto(c.Target(), ids)
		}
		for _, arg := range c.Args() {
			collectExprIDsInto(arg, ids)
		}
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			collectExprIDsInto(el, ids)
		}
	case ast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			ids[entry.ID()] = struct{}{}
			me := entry.AsMapEntry()
			collectExprIDsInto(me.Key(), ids)
			collectExprIDsInto(me.Value(), ids)
		}
	case ast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			ids[field.ID()] = struct{}{}
			collectExprIDsInto(field.AsStructField().Value(), ids)
		}
	case ast.ComprehensionKind:
		c := e.AsComprehension()
		collectExprIDsInto(c.IterRange(), ids)
		collectExprIDsInto(c.AccuInit(), ids)
		collectExprIDsInto(c.LoopCondition(), ids)
		collectExprIDsInto(c.LoopStep(), ids)
		collectExprIDsInto(c.Result(), ids)
	}
}

// indexExprs returns a map from expression id to the expression node.
func indexExprs(root ast.Expr) map[int64]ast.Expr {
	nodes := map[int64]ast.Expr{}
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if e == nil {
			return
		}
		nodes[e.ID()] = e
		switch e.Kind() {
		case ast.SelectKind:
			walk(e.AsSelect().Operand())
		case ast.CallKind:
			c := e.AsCall()
			if c.IsMemberFunction() {
				walk(c.Target())
			}
			for _, arg := range c.Args() {
				walk(arg)
			}
		case ast.ListKind:
			for _, el := range e.AsList().Elements() {
				walk(el)
			}
		case ast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				me := entry.AsMapEntry()
				walk(me.Key())
				walk(me.Value())
			}
		case ast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				walk(field.AsStructField().Value())
			}
		case ast.ComprehensionKind:
			c := e.AsComprehension()
			walk(c.IterRange())
			walk(c.AccuInit())
			walk(c.LoopCondition())
			walk(c.LoopStep())
			walk(c.Result())
		}
	}
	walk(root)
	return nodes
}

// isAncestor reports whether ancestorID is ancestorID itself, the descendant, or a strict
// ancestor of descendantID within the tree rooted at root.
func isAncestor(root ast.Expr, nodes map[int64]ast.Expr, ancestorID, descendantID int64) bool {
	if ancestorID == descendantID {
		return true
	}
	ancestor, found := nodes[ancestorID]
	if !found {
		return false
	}
	foundDescendant := false
	switch ancestor.Kind() {
	case ast.SelectKind:
		foundDescendant = subtreeContains(ancestor.AsSelect().Operand(), descendantID)
	case ast.CallKind:
		c := ancestor.AsCall()
		if c.IsMemberFunction() && subtreeContains(c.Target(), descendantID) {
			foundDescendant = true
		}
		if !foundDescendant {
			for _, arg := range c.Args() {
				if subtreeContains(arg, descendantID) {
					foundDescendant = true
					break
				}
			}
		}
	case ast.ListKind:
		for _, el := range ancestor.AsList().Elements() {
			if subtreeContains(el, descendantID) {
				foundDescendant = true
				break
			}
		}
	case ast.MapKind:
		for _, entry := range ancestor.AsMap().Entries() {
			me := entry.AsMapEntry()
			if subtreeContains(me.Key(), descendantID) || subtreeContains(me.Value(), descendantID) {
				foundDescendant = true
				break
			}
		}
	case ast.StructKind:
		for _, field := range ancestor.AsStruct().Fields() {
			if subtreeContains(field.AsStructField().Value(), descendantID) {
				foundDescendant = true
				break
			}
		}
	case ast.ComprehensionKind:
		c := ancestor.AsComprehension()
		for _, child := range []ast.Expr{c.IterRange(), c.AccuInit(), c.LoopCondition(), c.LoopStep(), c.Result()} {
			if subtreeContains(child, descendantID) {
				foundDescendant = true
				break
			}
		}
	}
	return foundDescendant
}

func subtreeContains(root ast.Expr, id int64) bool {
	if root == nil {
		return false
	}
	found := false
	ast.PreOrderVisit(root, ast.NewExprVisitor(func(e ast.Expr) {
		if e.ID() == id {
			found = true
		}
	}))
	return found
}
