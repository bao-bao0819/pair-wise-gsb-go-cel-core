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
	"sort"

	"cel.dev/cel-go/common/ast"
)

// stableIDGen allocates fresh, monotonic ids.
type stableIDGen struct {
	seed int64
}

func newStableIDGen(seed int64) *stableIDGen {
	return &stableIDGen{seed: seed}
}

func (g *stableIDGen) next() int64 {
	g.seed++
	return g.seed
}

// renumberWithMacros renumbers the expression tree and the tracked macro call expressions with a
// single stable mapping so ids shared between the expanded tree and macro signatures stay
// consistent.
func renumberWithMacros(expr ast.Expr, info *ast.SourceInfo, idGen ast.IDGenerator) {
	expr.RenumberIDs(idGen)

	if len(info.MacroCalls()) == 0 {
		return
	}
	macroIDs := make([]int64, 0, len(info.MacroCalls()))
	for id := range info.MacroCalls() {
		macroIDs = append(macroIDs, id)
	}
	sort.Slice(macroIDs, func(i, j int) bool { return macroIDs[i] < macroIDs[j] })

	type macroUpdate struct {
		id   int64
		call ast.Expr
	}
	updates := make([]macroUpdate, 0, len(macroIDs))
	for _, oldID := range macroIDs {
		call, found := info.GetMacroCall(oldID)
		if !found {
			continue
		}
		call.RenumberIDs(idGen)
		newID := idGen(oldID)
		info.ClearMacroCall(oldID)
		updates = append(updates, macroUpdate{id: newID, call: call})
	}
	for _, u := range updates {
		info.SetMacroCall(u.id, u.call)
	}
}

// cloneReference returns an independent copy of a ReferenceInfo so that renaming a fragment
// identifier after splicing never mutates the input fragment metadata.
func cloneReference(r *ast.ReferenceInfo) *ast.ReferenceInfo {
	if r == nil {
		return nil
	}
	c := &ast.ReferenceInfo{Name: r.Name, Value: r.Value}
	if len(r.OverloadIDs) > 0 {
		c.OverloadIDs = append([]string{}, r.OverloadIDs...)
	}
	return c
}

// spliceInfo is the concrete description of a committed replacement.
type spliceInfo struct {
	targetID  int64
	fragment  ast.Expr
	rootNewID int64
}

// rebuildWithSplices returns a copy of the target tree in which each splice target node is
// replaced by the corresponding renumbered fragment root. The splice point keeps the target id;
// fragment subexpressions retain their freshly allocated ids.
func rebuildWithSplices(factory ast.ExprFactory, root ast.Expr, splices map[int64]*spliceInfo) ast.Expr {
	var rebuild func(ast.Expr) ast.Expr
	rebuild = func(e ast.Expr) ast.Expr {
		if e == nil {
			return nil
		}
		if s, found := splices[e.ID()]; found {
			// Take the fragment's kind case but preserve the target node id at the splice point.
			// SetKindCase takes ownership of the fragment subexpressions, which already carry
			// fresh ids unique within the merged tree.
			replaced := factory.NewUnspecifiedExpr(e.ID())
			replaced.SetKindCase(s.fragment)
			return replaced
		}
		switch e.Kind() {
		case ast.CallKind:
			c := e.AsCall()
			args := make([]ast.Expr, len(c.Args()))
			for i, arg := range c.Args() {
				args[i] = rebuild(arg)
			}
			if c.IsMemberFunction() {
				return factory.NewMemberCall(e.ID(), c.FunctionName(), rebuild(c.Target()), args...)
			}
			return factory.NewCall(e.ID(), c.FunctionName(), args...)
		case ast.SelectKind:
			s := e.AsSelect()
			operand := rebuild(s.Operand())
			if s.IsTestOnly() {
				return factory.NewPresenceTest(e.ID(), operand, s.FieldName())
			}
			return factory.NewSelect(e.ID(), operand, s.FieldName())
		case ast.ListKind:
			l := e.AsList()
			elems := make([]ast.Expr, l.Size())
			for i, el := range l.Elements() {
				elems[i] = rebuild(el)
			}
			return factory.NewList(e.ID(), elems, l.OptionalIndices())
		case ast.MapKind:
			m := e.AsMap()
			entries := make([]ast.EntryExpr, m.Size())
			for i, entry := range m.Entries() {
				me := entry.AsMapEntry()
				entries[i] = factory.NewMapEntry(entry.ID(),
					rebuild(me.Key()), rebuild(me.Value()), me.IsOptional())
			}
			return factory.NewMap(e.ID(), entries)
		case ast.StructKind:
			st := e.AsStruct()
			fields := make([]ast.EntryExpr, len(st.Fields()))
			for i, field := range st.Fields() {
				sf := field.AsStructField()
				fields[i] = factory.NewStructField(field.ID(),
					sf.Name(), rebuild(sf.Value()), sf.IsOptional())
			}
			return factory.NewStruct(e.ID(), st.TypeName(), fields)
		case ast.ComprehensionKind:
			c := e.AsComprehension()
			return factory.NewComprehensionTwoVar(e.ID(),
				rebuild(c.IterRange()), c.IterVar(), c.IterVar2(), c.AccuVar(),
				rebuild(c.AccuInit()), rebuild(c.LoopCondition()), rebuild(c.LoopStep()), rebuild(c.Result()))
		default:
			return factory.CopyExpr(e)
		}
	}
	return rebuild(root)
}

// rewriteMacroRef updates references to a spliced target within retained enclosing macro call
// expressions. A concrete reference is replaced by an independent copy of the renumbered
// fragment root; a nested-macro placeholder (an unspecified node) is renumbered to the splice
// point id so the enclosing macro keeps expanding through its macro call entry.
func rewriteMacroRef(call ast.Expr, s *spliceInfo) {
	factory := ast.NewExprFactory()
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if e == nil {
			return
		}
		if e.Kind() == ast.UnspecifiedExprKind && e.ID() == s.targetID {
			// The placeholder only carries an id; keep the splice point id which is live in the
			// resulting expression tree and has its own macro/metadata entry.
			return
		}
		if e.Kind() == ast.UnspecifiedExprKind {
			return
		}
		switch e.Kind() {
		case ast.CallKind:
			c := e.AsCall()
			if c.IsMemberFunction() {
				target := c.Target()
				if target.ID() == s.targetID {
					target.SetKindCase(factory.CopyExpr(s.fragment))
				} else {
					walk(target)
				}
			}
			for _, arg := range c.Args() {
				if arg.ID() == s.targetID {
					arg.SetKindCase(factory.CopyExpr(s.fragment))
					continue
				}
				walk(arg)
			}
		case ast.SelectKind:
			operand := e.AsSelect().Operand()
			if operand.ID() == s.targetID {
				operand.SetKindCase(factory.CopyExpr(s.fragment))
			} else {
				walk(operand)
			}
		case ast.ListKind:
			for _, el := range e.AsList().Elements() {
				if el.ID() == s.targetID {
					el.SetKindCase(factory.CopyExpr(s.fragment))
					continue
				}
				walk(el)
			}
		case ast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				me := entry.AsMapEntry()
				if me.Key().ID() == s.targetID {
					me.Key().SetKindCase(factory.CopyExpr(s.fragment))
				} else {
					walk(me.Key())
				}
				if me.Value().ID() == s.targetID {
					me.Value().SetKindCase(factory.CopyExpr(s.fragment))
				} else {
					walk(me.Value())
				}
			}
		case ast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				value := field.AsStructField().Value()
				if value.ID() == s.targetID {
					value.SetKindCase(factory.CopyExpr(s.fragment))
				} else {
					walk(value)
				}
			}
		}
	}
	walk(call)
}

// cleanupDanglingMacros removes macro call entries whose id no longer denotes a comprehension
// node in the resulting expression tree.
func cleanupDanglingMacros(root ast.Expr, info *ast.SourceInfo) {
	if len(info.MacroCalls()) == 0 {
		return
	}
	live := collectExprIDs(root)
	for id := range info.MacroCalls() {
		if _, found := live[id]; !found {
			info.ClearMacroCall(id)
		}
	}
}
