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
	"fmt"
	"sort"

	"cel.dev/cel-go/common/ast"
)

// scopeBinding records a comprehension binder that is in scope at a point in the target tree.
type scopeBinding struct {
	comprehensionID int64
	name            string
	kind            binderKind
}

type binderKind int

const (
	binderIter binderKind = iota
	binderIter2
	binderAccu
)

// planRenames walks from the target root to each substitution target and records the target
// comprehension binders whose names clash with a free identifier of the fragment being inserted.
// Only binders that are actually in scope at the splice point are considered.
func (b *substituteBuilder) planRenames(prepared []*preparedSubstitution, targetSet map[int64]int) map[int64]*binderRename {
	renamed := map[int64]*binderRename{}

	// freeAtTarget maps a target id to the union of free identifiers of the fragment inserted at
	// that point.
	freeAtTarget := map[int64]map[string]struct{}{}
	allFree := map[string]struct{}{}
	for _, p := range prepared {
		set := map[string]struct{}{}
		for name := range p.freeIdents {
			set[name] = struct{}{}
			allFree[name] = struct{}{}
		}
		freeAtTarget[p.sub.TargetID] = set
	}

	var walk func(e ast.Expr, scope []scopeBinding)
	walk = func(e ast.Expr, scope []scopeBinding) {
		if e == nil {
			return
		}
		if freeNames, isTarget := freeAtTarget[e.ID()]; isTarget {
			for _, binding := range scope {
				if _, clashes := freeNames[binding.name]; !clashes {
					continue
				}
				rn := renamed[binding.comprehensionID]
				if rn == nil {
					rn = &binderRename{}
					renamed[binding.comprehensionID] = rn
				}
				switch binding.kind {
				case binderIter:
					rn.iterOld = binding.name
				case binderIter2:
					rn.iter2Old = binding.name
				case binderAccu:
					rn.accuOld = binding.name
				}
			}
		}
		switch e.Kind() {
		case ast.SelectKind:
			walk(e.AsSelect().Operand(), scope)
		case ast.CallKind:
			c := e.AsCall()
			if c.IsMemberFunction() {
				walk(c.Target(), scope)
			}
			for _, arg := range c.Args() {
				walk(arg, scope)
			}
		case ast.ListKind:
			for _, el := range e.AsList().Elements() {
				walk(el, scope)
			}
		case ast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				me := entry.AsMapEntry()
				walk(me.Key(), scope)
				walk(me.Value(), scope)
			}
		case ast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				walk(field.AsStructField().Value(), scope)
			}
		case ast.ComprehensionKind:
			c := e.AsComprehension()
			walk(c.IterRange(), scope)
			walk(c.AccuInit(), scope)
			accuScope := append(scope, scopeBinding{
				comprehensionID: e.ID(),
				name:            c.AccuVar(),
				kind:            binderAccu,
			})
			iterScope := accuScope
			if c.IterVar() != "" {
				iterScope = append(iterScope, scopeBinding{
					comprehensionID: e.ID(),
					name:            c.IterVar(),
					kind:            binderIter,
				})
			}
			if c.HasIterVar2() {
				iterScope = append(iterScope, scopeBinding{
					comprehensionID: e.ID(),
					name:            c.IterVar2(),
					kind:            binderIter2,
				})
			}
			walk(c.LoopCondition(), iterScope)
			walk(c.LoopStep(), iterScope)
			// Iter variables are out of scope for the result.
			walk(c.Result(), accuScope)
		}
	}
	walk(b.target.Expr(), nil)

	// Resolve fresh names deterministically. A fresh name must not be used by any fragment free
	// identifier and must not collide with another fresh name.
	forbidden := map[string]struct{}{}
	for name := range allFree {
		forbidden[name] = struct{}{}
	}
	comprehensionIDs := make([]int64, 0, len(renamed))
	for id := range renamed {
		comprehensionIDs = append(comprehensionIDs, id)
	}
	sort.Slice(comprehensionIDs, func(i, j int) bool { return comprehensionIDs[i] < comprehensionIDs[j] })
	counter := 0
	freshName := func(original string) string {
		for {
			candidate := fmt.Sprintf("%s%d_%s", renamedNamePrefix, counter, sanitizeIdent(original))
			counter++
			if _, taken := forbidden[candidate]; taken {
				continue
			}
			forbidden[candidate] = struct{}{}
			return candidate
		}
	}
	for _, id := range comprehensionIDs {
		rn := renamed[id]
		if rn.iterOld != "" {
			rn.iterNew = freshName(rn.iterOld)
		}
		if rn.iter2Old != "" {
			rn.iter2New = freshName(rn.iter2Old)
		}
		if rn.accuOld != "" {
			rn.accuNew = freshName(rn.accuOld)
		}
		if rn.isEmpty() {
			delete(renamed, id)
		}
	}
	return renamed
}

func sanitizeIdent(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r == '@' || r == '#' {
			out = append(out, '_')
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "v"
	}
	return string(out)
}

// renameEntry is an applied renaming from an original binder name to a fresh name.
type renameEntry struct {
	from, to string
}

// bindingScope describes the variables bound by one comprehension as seen by its subexpressions.
type bindingScope struct {
	accu []renameEntry
	iter []renameEntry
}

// applyRenames rewrites binder declarations and references within the copied target AST,
// including identifier expressions, reference metadata, and tracked macro call expressions.
func (b *substituteBuilder) applyRenames(a *ast.AST) {
	if len(b.renamed) == 0 {
		return
	}
	walker := &renameWalker{
		factory: b.factory,
		renamed: b.renamed,
		refMap:  a.ReferenceMap(),
	}
	walker.walk(a.Expr(), nil)

	info := a.SourceInfo()
	for id, rn := range b.renamed {
		call, found := info.GetMacroCall(id)
		if !found {
			continue
		}
		renameMacroCallBinders(call, rn)
	}
}

type renameWalker struct {
	factory ast.ExprFactory
	renamed map[int64]*binderRename
	refMap  map[int64]*ast.ReferenceInfo
}

// resolve maps an identifier name through the active scopes, honoring binder shadowing.
func (w *renameWalker) resolve(name string, scopes []bindingScope) string {
	for i := len(scopes) - 1; i >= 0; i-- {
		for _, e := range scopes[i].iter {
			if e.from == name {
				return e.to
			}
		}
		for _, e := range scopes[i].accu {
			if e.from == name {
				return e.to
			}
		}
	}
	return name
}

func (w *renameWalker) walk(e ast.Expr, scopes []bindingScope) {
	if e == nil {
		return
	}
	if e.Kind() == ast.IdentKind {
		name := e.AsIdent()
		if resolved := w.resolve(name, scopes); resolved != name {
			e.SetKindCase(w.factory.NewIdent(e.ID(), resolved))
			if ref, found := w.refMap[e.ID()]; found {
				w.refMap[e.ID()] = ast.NewIdentReference(resolved, ref.Value)
			}
		}
		return
	}
	switch e.Kind() {
	case ast.SelectKind:
		w.walk(e.AsSelect().Operand(), scopes)
	case ast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			w.walk(c.Target(), scopes)
		}
		for _, arg := range c.Args() {
			w.walk(arg, scopes)
		}
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			w.walk(el, scopes)
		}
	case ast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			w.walk(me.Key(), scopes)
			w.walk(me.Value(), scopes)
		}
	case ast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			w.walk(field.AsStructField().Value(), scopes)
		}
	case ast.ComprehensionKind:
		c := e.AsComprehension()
		w.walk(c.IterRange(), scopes)
		w.walk(c.AccuInit(), scopes)

		rn := w.renamed[e.ID()]
		accuName := c.AccuVar()
		iterName := c.IterVar()
		iterName2 := c.IterVar2()
		binding := bindingScope{}
		if rn != nil {
			if rn.accuNew != "" {
				accuName = rn.accuNew
				binding.accu = append(binding.accu, renameEntry{from: rn.accuOld, to: rn.accuNew})
			}
			if rn.iterNew != "" {
				iterName = rn.iterNew
				binding.iter = append(binding.iter, renameEntry{from: rn.iterOld, to: rn.iterNew})
			}
			if rn.iter2New != "" {
				iterName2 = rn.iter2New
				binding.iter = append(binding.iter, renameEntry{from: rn.iter2Old, to: rn.iter2New})
			}
			e.SetKindCase(w.factory.NewComprehensionTwoVar(e.ID(),
				c.IterRange(), iterName, iterName2, accuName,
				c.AccuInit(), c.LoopCondition(), c.LoopStep(), c.Result()))
			c = e.AsComprehension()
		}
		// Unrenamed binders still shadow outer bindings of the same name; record identity entries.
		if binding.accu == nil {
			binding.accu = []renameEntry{{from: c.AccuVar(), to: c.AccuVar()}}
		}
		if binding.iter == nil {
			if c.IterVar() != "" {
				binding.iter = append(binding.iter, renameEntry{from: c.IterVar(), to: c.IterVar()})
			}
			if c.HasIterVar2() {
				binding.iter = append(binding.iter, renameEntry{from: c.IterVar2(), to: c.IterVar2()})
			}
		}
		nextScopes := append(scopes, binding)
		w.walk(c.LoopCondition(), nextScopes)
		w.walk(c.LoopStep(), nextScopes)
		// Iter variables leave scope for the result; the accumulator stays bound.
		resultScopes := append(scopes, bindingScope{accu: binding.accu})
		w.walk(c.Result(), resultScopes)
	}
}

// renameMacroCallBinders rewrites the unexpanded macro call signature so it stays consistent
// with the alpha-renamed expanded comprehension. Standard comprehension macros never mention the
// accumulator in source, and the iteration variables appear both as the binder declaration and as
// references in the macro body; both are renamed. Nested macros are represented as unspecified
// placeholder nodes and are left untouched.
func renameMacroCallBinders(call ast.Expr, rn *binderRename) {
	if call.Kind() != ast.CallKind {
		return
	}
	renames := map[string]string{}
	if rn.iterNew != "" {
		renames[rn.iterOld] = rn.iterNew
	}
	if rn.iter2New != "" {
		renames[rn.iter2Old] = rn.iter2New
	}
	if len(renames) == 0 {
		return
	}
	renameMacroIdents(call, renames)
}

func renameMacroIdents(e ast.Expr, renames map[string]string) {
	if e == nil || e.Kind() == ast.UnspecifiedExprKind {
		return
	}
	if e.Kind() == ast.IdentKind {
		if to, found := renames[e.AsIdent()]; found {
			e.SetKindCase(ast.NewExprFactory().NewIdent(e.ID(), to))
		}
		return
	}
	switch e.Kind() {
	case ast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			renameMacroIdents(c.Target(), renames)
		}
		for _, arg := range c.Args() {
			renameMacroIdents(arg, renames)
		}
	case ast.SelectKind:
		renameMacroIdents(e.AsSelect().Operand(), renames)
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			renameMacroIdents(el, renames)
		}
	case ast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			renameMacroIdents(me.Key(), renames)
			renameMacroIdents(me.Value(), renames)
		}
	case ast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			renameMacroIdents(field.AsStructField().Value(), renames)
		}
	}
}
