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

	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
)

// SourceOrigin identifies which input a substitution error or source position belongs to.
type SourceOrigin int

const (
	// TargetOrigin refers to the rule expression being modified.
	TargetOrigin SourceOrigin = iota + 1
	// FragmentOrigin refers to the checked expression fragment being spliced in.
	FragmentOrigin
)

func (o SourceOrigin) String() string {
	switch o {
	case TargetOrigin:
		return "target"
	case FragmentOrigin:
		return "fragment"
	default:
		return "unknown"
	}
}

// Substitution describes a single replacement: the target expression within the target AST is
// replaced by a fresh, renumbered copy of the fragment AST root.
type Substitution struct {
	// TargetID is the id of an expression node within the target AST.
	TargetID int64

	// Fragment is a checked AST whose root expression is spliced into the target.
	Fragment *ast.AST

	// FragmentSource is the source the fragment was parsed from. It is used to attribute
	// compatibility errors back to the fragment and to combine fragment positions into the
	// resulting source.
	FragmentSource common.Source
}

// SubstitutionIssue describes a single reason a substitution set could not be committed.
type SubstitutionIssue struct {
	// Origin identifies whether the issue refers to the target or a fragment.
	Origin SourceOrigin

	// FragmentIndex is the index of the fragment substitution when Origin is FragmentOrigin.
	FragmentIndex int

	// Location is the location of the issue within the original target or fragment source.
	Location common.Location

	// Message describes the incompatibility.
	Message string
}

// Error implements the error interface.
func (e *SubstitutionIssue) Error() string {
	if e.Location == nil || e.Location == common.NoLocation {
		return fmt.Sprintf("%s: %s", e.Origin, e.Message)
	}
	return fmt.Sprintf("%s:%d:%d: %s", e.Origin, e.Location.Line(), e.Location.Column()+1, e.Message)
}

// SubstitutionResult is the output of a successful SubstituteExprs call.
type SubstitutionResult struct {
	// AST is the new checked AST. The target and fragment inputs are left unmodified.
	AST *ast.AST

	// Source is the combined source against which the resulting AST positions are defined.
	// Target positions are unchanged; fragment positions live after the target content.
	Source common.Source
}

// SubstituteExprs produces a new checked AST in which each target expression node is replaced by
// the root expression of the corresponding fragment.
//
// The target and fragment ASTs must be type-checked against compatible environments. Environment
// compatibility is validated structurally: every free identifier in a fragment must resolve in
// the target environment to an equivalent type, and every selected overload must be declared in
// the target environment. In addition, the fragment result type must be assignable to the target
// expression type.
//
// Comprehension binders in the target which would capture free identifiers of an inserted
// fragment are alpha-renamed so that the meaning of the fragment's free variables is preserved.
// All spliced node ids are allocated fresh ids; the type map, reference map, overload selections,
// and macro call metadata are migrated consistently.
//
// Targets must be pairwise non-overlapping: neither target may be an ancestor of another. Neither
// input AST nor its source information is modified.
func SubstituteExprs(env *Env, targetSource common.Source, target *ast.AST, substitutions []Substitution) (*SubstitutionResult, []*SubstitutionIssue) {
	b := &substituteBuilder{
		env:           env,
		targetSource:  targetSource,
		target:        target,
		substitutions: substitutions,
		factory:       ast.NewExprFactory(),
		issues:        []*SubstitutionIssue{},
	}
	result, ok := b.build()
	if !ok {
		return nil, b.issues
	}
	return result, nil
}

type substituteBuilder struct {
	env           *Env
	targetSource  common.Source
	target        *ast.AST
	substitutions []Substitution
	factory       ast.ExprFactory
	issues        []*SubstitutionIssue

	// renamed maps a target comprehension node id to the fresh binder names for that node.
	renamed map[int64]*binderRename
}

// binderRename records the original and fresh names for binders of a single target comprehension
// that must be alpha-renamed. Empty pairs indicate the binder is unchanged.
type binderRename struct {
	iterOld  string
	iterNew  string
	iter2Old string
	iter2New string
	accuOld  string
	accuNew  string
}

func (r *binderRename) isEmpty() bool {
	return r.iterNew == "" && r.iter2New == "" && r.accuNew == ""
}

func (b *substituteBuilder) report(origin SourceOrigin, fragmentIndex int, loc common.Location, format string, args ...any) {
	b.issues = append(b.issues, &SubstitutionIssue{
		Origin:        origin,
		FragmentIndex: fragmentIndex,
		Location:      loc,
		Message:       fmt.Sprintf(format, args...),
	})
}

// preparedSubstitution holds the validated, pre-computed information for a single substitution.
type preparedSubstitution struct {
	sub        Substitution
	index      int
	freeIdents map[string]struct{}
	targetType *types.Type
	fragType   *types.Type
	offsetBias int32
}

func (b *substituteBuilder) build() (*SubstitutionResult, bool) {
	if b.target == nil || !b.target.IsChecked() {
		b.report(TargetOrigin, -1, common.NoLocation, "target AST must be a type-checked AST")
		return nil, false
	}
	if b.targetSource == nil {
		b.report(TargetOrigin, -1, common.NoLocation, "target source is required")
		return nil, false
	}
	if len(b.substitutions) == 0 {
		// Nothing to do, but return a copy so callers can rely on input immutability.
		return &SubstitutionResult{AST: ast.Copy(b.target), Source: b.targetSource}, true
	}

	prepared := make([]*preparedSubstitution, len(b.substitutions))
	targetIDs := collectExprIDs(b.target.Expr())
	for i, sub := range b.substitutions {
		p := &preparedSubstitution{sub: sub, index: i}
		prepared[i] = p
		if sub.Fragment == nil || !sub.Fragment.IsChecked() {
			b.report(FragmentOrigin, i, common.NoLocation, "fragment AST must be a type-checked AST")
			continue
		}
		if sub.FragmentSource == nil {
			b.report(FragmentOrigin, i, common.NoLocation, "fragment source is required")
			continue
		}
		if _, found := targetIDs[sub.TargetID]; !found {
			b.report(TargetOrigin, -1, b.target.SourceInfo().GetStartLocation(sub.TargetID),
				"substitution target id %d does not refer to an expression node in the target", sub.TargetID)
			continue
		}
		p.targetType = b.target.GetType(sub.TargetID)
		p.fragType = sub.Fragment.GetType(sub.Fragment.Expr().ID())
		p.freeIdents = freeIdentifiers(sub.Fragment)
		b.validateFragment(p)
	}

	// Pairwise validation of targets: duplicate ids and ancestor relationships are rejected.
	targetSet := map[int64]int{}
	for _, p := range prepared {
		if p == nil {
			continue
		}
		id := p.sub.TargetID
		if other, dup := targetSet[id]; dup {
			b.report(TargetOrigin, -1, b.target.SourceInfo().GetStartLocation(id),
				"duplicate substitution target id %d (substitutions %d and %d)", id, other, p.index)
		}
		targetSet[id] = p.index
	}
	targetNodeByID := indexExprs(b.target.Expr())
	for i := 0; i < len(prepared); i++ {
		for j := i + 1; j < len(prepared); j++ {
			pi, pj := prepared[i], prepared[j]
			if pi == nil || pj == nil {
				continue
			}
			a, c := pi.sub.TargetID, pj.sub.TargetID
			if isAncestor(b.target.Expr(), targetNodeByID, a, c) {
				b.report(TargetOrigin, -1, b.target.SourceInfo().GetStartLocation(a),
					"substitution targets %d and %d overlap", a, c)
			}
		}
	}
	if len(b.issues) > 0 {
		return nil, false
	}

	// Compute the alpha-renaming of target binders required to keep fragment free variables free.
	b.renamed = b.planRenames(prepared, targetSet)
	if len(b.issues) > 0 {
		return nil, false
	}

	// Deep copy the target before mutating anything.
	mergedAST := ast.Copy(b.target)
	b.applyRenames(mergedAST)

	// Combine the source contents and assign each fragment an offset bias.
	combined := newCombinedSource(b.targetSource)
	for _, p := range prepared {
		p.offsetBias = combined.append(p.sub.FragmentSource)
	}

	// Seed fresh id allocation strictly beyond any id used in any input, including macro calls.
	seed := ast.MaxID(b.target)
	for _, p := range prepared {
		if max := ast.MaxID(p.sub.Fragment); max > seed {
			seed = max
		}
	}
	idGen := newStableIDGen(seed)

	extraRanges := map[int64]ast.OffsetRange{}
	extraMacros := map[int64]ast.Expr{}
	splices := make([]*spliceInfo, 0, len(prepared))
	for _, p := range prepared {
		fragCopy := ast.Copy(p.sub.Fragment)
		fragIDMap := map[int64]int64{}
		fragGen := func(oldID int64) int64 {
			if oldID == 0 {
				return 0
			}
			if newID, found := fragIDMap[oldID]; found {
				return newID
			}
			newID := idGen.next()
			fragIDMap[oldID] = newID
			return newID
		}
		renumberWithMacros(fragCopy.Expr(), fragCopy.SourceInfo(), fragGen)

		// Migrate type and reference metadata under the fresh ids.
		for oldID, t := range p.sub.Fragment.TypeMap() {
			mergedAST.SetType(fragGen(oldID), t)
		}
		for oldID, r := range p.sub.Fragment.ReferenceMap() {
			mergedAST.SetReference(fragGen(oldID), cloneReference(r))
		}
		for oldID, r := range p.sub.Fragment.SourceInfo().OffsetRanges() {
			extraRanges[fragGen(oldID)] = ast.OffsetRange{
				Start: r.Start + p.offsetBias,
				Stop:  r.Stop + p.offsetBias,
			}
		}
		for oldID, call := range p.sub.Fragment.SourceInfo().MacroCalls() {
			extraMacros[fragGen(oldID)] = call
		}
		splices = append(splices, &spliceInfo{
			targetID:  p.sub.TargetID,
			fragment:  fragCopy.Expr(),
			rootNewID: fragGen(p.sub.Fragment.Expr().ID()),
		})
	}

	// Rebuild the target tree, substituting fragment roots at the target node positions. The
	// target node id is kept at the splice point; all fragment children carry fresh ids.
	spliceByTarget := map[int64]*spliceInfo{}
	for _, s := range splices {
		spliceByTarget[s.targetID] = s
	}
	// Collect the ids of the replaced target subtrees (excluding the splice point itself) so
	// their stale type and reference metadata can be removed after the splice is reconciled.
	removedTargetIDs := map[int64]struct{}{}
	for _, s := range splices {
		targetNode := indexExprs(mergedAST.Expr())[s.targetID]
		if targetNode == nil {
			continue
		}
		for id := range collectExprIDs(targetNode) {
			if id != s.targetID {
				removedTargetIDs[id] = struct{}{}
			}
		}
		// The fragment root's fresh id no longer exists in the final tree either; the splice
		// point adopts its metadata below.
		removedTargetIDs[s.rootNewID] = struct{}{}
	}
	rootExpr := rebuildWithSplices(b.factory, mergedAST.Expr(), spliceByTarget)

	mergedInfo := ast.NewMergedSourceInfo(mergedAST.SourceInfo(), combined.lineOffsets(), extraRanges, extraMacros)

	// Reconcile macro entries at the splice points. The splice point keeps the target id, so a
	// macro defined by the fragment root must be keyed by the target id; if the fragment root is
	// not a macro but the target node was one, the stale target macro entry is removed.
	for _, s := range splices {
		fragRootMacro, fragRootIsMacro := mergedInfo.GetMacroCall(s.rootNewID)
		if fragRootIsMacro {
			mergedInfo.ClearMacroCall(s.rootNewID)
			mergedInfo.SetMacroCall(s.targetID, fragRootMacro)
			continue
		}
		if _, targetWasMacro := mergedInfo.GetMacroCall(s.targetID); targetWasMacro {
			mergedInfo.ClearMacroCall(s.targetID)
		}
	}

	// Rewrite references to spliced nodes within retained enclosing macro call expressions so
	// unparsing and source navigation remain consistent.
	for _, s := range splices {
		for _, call := range mergedInfo.MacroCalls() {
			rewriteMacroRef(call, s)
		}
	}
	cleanupDanglingMacros(rootExpr, mergedInfo)

	// The splice point keeps the target id but adopts the fragment root kind case; its type and
	// reference metadata must reflect the fragment root.
	for _, s := range splices {
		if t, found := mergedAST.TypeMap()[s.rootNewID]; found {
			mergedAST.SetType(s.targetID, t)
		}
		if r, found := mergedAST.ReferenceMap()[s.rootNewID]; found {
			mergedAST.SetReference(s.targetID, r)
		}
		if r, found := extraRanges[s.rootNewID]; found {
			mergedInfo.SetOffsetRange(s.targetID, r)
		}
	}

	// Prune metadata referencing ids which no longer exist after splicing. Fragment metadata has
	// already been added and the splice points have been reconciled above.
	liveIDs := collectExprIDs(rootExpr)
	for id := range mergedInfo.MacroCalls() {
		if _, found := liveIDs[id]; !found {
			mergedInfo.ClearMacroCall(id)
		}
	}
	for id := range mergedInfo.OffsetRanges() {
		if _, found := liveIDs[id]; !found {
			mergedInfo.ClearOffsetRange(id)
		}
	}
	for id := range removedTargetIDs {
		delete(mergedAST.TypeMap(), id)
		delete(mergedAST.ReferenceMap(), id)
	}

	result := ast.NewCheckedAST(ast.NewAST(rootExpr, mergedInfo), mergedAST.TypeMap(), mergedAST.ReferenceMap())
	return &SubstitutionResult{AST: result, Source: combined.source}, true
}
