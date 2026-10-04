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
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"cel.dev/cel-go/common"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
)

// Substitution performs a controlled replacement of one or more checked expression nodes within a
// target rule AST with checked expression fragments.
//
// Unlike string concatenation followed by re-parsing, the substitution operates directly on the
// typed AST graph: expression ids are renumbered to remain unique, type and reference metadata
// (including overload selections and constant references) is migrated together with the nodes, and
// macro call source information is preserved. Binder variables in the target are alpha-renamed as
// needed so that free variables in a fragment keep their original meaning when inserted under
// nested comprehensions (e.g. all, exists, map).
//
// Replacements are validated atomically: overlapping targets, environments which do not declare
// the same container, variables, functions, and type-checking features, and result types which are
// not assignable to the target positions are all rejected before any result is produced. Neither
// the target nor fragment Ast values are modified.
//
// The returned error, if non-nil, is a *SubstitutionError with source positions attributed to the
// original target or fragment expression sources.
type Substitution struct {
	targetEnv *Env
	target    *Ast
	fragEnv   *Env
	edits     []substitutionEdit
}

// NewSubstitution starts building a substitution against a target rule within a given environment.
//
// The target Ast must be type-checked.
func NewSubstitution(env *Env, target *Ast) *Substitution {
	if env == nil || target == nil {
		return &Substitution{}
	}
	return &Substitution{targetEnv: env, target: target, fragEnv: env}
}

// Replace records a request to replace the target expression node identified by targetID with the
// fragment expression.
//
// The fragment must be type-checked in the same environment (or a structurally compatible
// environment) as the target. Multiple non-overlapping target ids may be replaced in a single
// Substitute call. Replacements are not applied until Substitute runs, so the input Ast values
// remain unchanged.
func (s *Substitution) Replace(targetID int64, fragment *Ast) *Substitution {
	return s.ReplaceWithEnv(nil, targetID, fragment)
}

// ReplaceWithEnv behaves like Replace but allows the fragment to have been checked against a
// separate environment.
//
// The fragment environment must declare the same container, variables, functions, and
// type-checking features as the target environment; otherwise the substitution is rejected.
func (s *Substitution) ReplaceWithEnv(fragmentEnv *Env, targetID int64, fragment *Ast) *Substitution {
	if s == nil {
		return s
	}
	s.edits = append(s.edits, substitutionEdit{
		targetID: targetID,
		fragment: fragment,
		fragEnv:  fragmentEnv,
	})
	return s
}

type substitutionEdit struct {
	targetID int64
	fragment *Ast
	fragEnv  *Env
}

// SubstitutionError describes a rejected substitution.
//
// Errors are attributed to the target rule source or to a fragment source so callers can locate the
// offending node in the original inputs.
type SubstitutionError struct {
	targetErrs   []*common.Error
	fragmentErrs map[string][]*common.Error
	reasons      []string
	targetSrc    common.Source
	fragmentSrc  map[string]common.Source
}

// TargetErrors returns the validation errors located in the target rule expression.
func (e *SubstitutionError) TargetErrors() []*common.Error {
	if e == nil {
		return nil
	}
	return e.targetErrs
}

// FragmentErrors returns validation errors keyed by fragment source description.
func (e *SubstitutionError) FragmentErrors() map[string][]*common.Error {
	if e == nil {
		return nil
	}
	return e.fragmentErrs
}

// Error renders the errors with their originating source descriptions and locations.
func (e *SubstitutionError) Error() string {
	var sb strings.Builder
	for _, r := range e.reasons {
		fmt.Fprintf(&sb, "ERROR: %s\n", r)
	}
	targetSrc := e.targetSrc
	if targetSrc == nil {
		targetSrc = common.NewTextSource("")
	}
	for _, err := range e.targetErrs {
		fmt.Fprintf(&sb, "%s\n", err.ToDisplayString(targetSrc))
	}
	descs := make([]string, 0, len(e.fragmentErrs))
	for desc := range e.fragmentErrs {
		descs = append(descs, desc)
	}
	sort.Strings(descs)
	for _, desc := range descs {
		fragSrc := e.fragmentSrc[desc]
		if fragSrc == nil {
			fragSrc = common.NewTextSource("")
		}
		for _, err := range e.fragmentErrs[desc] {
			fmt.Fprintf(&sb, "%s\n", err.ToDisplayString(fragSrc))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

type substitutionBuilder struct {
	target    *Ast
	fragments map[*Ast]*fragmentPlan
	edits     []resolvedEdit
	sections  []*fragmentSection

	errs *SubstitutionError
}

type fragmentPlan struct {
	env     *Env
	rootID  int64
	freeVar map[string]struct{}
}

type resolvedEdit struct {
	targetID int64
	fragment *Ast
	plan     *fragmentPlan
	target   celast.NavigableExpr
}

// migratedFragment holds the copied, renumbered contents of a fragment Ast.
type migratedFragment struct {
	frag  *Ast
	expr  celast.Expr
	info  *celast.SourceInfo
	idMap map[int64]int64
}

// Substitute validates and applies all recorded replacements, returning a new checked Ast.
func (s *Substitution) Substitute() (*Ast, error) {
	if s == nil || s.target == nil || s.targetEnv == nil {
		err := &SubstitutionError{
			reasons:      []string{"substitution requires a target environment and target expression"},
			fragmentErrs: map[string][]*common.Error{},
			fragmentSrc:  map[string]common.Source{},
		}
		return nil, err
	}
	b := &substitutionBuilder{
		target:    s.target,
		fragments: map[*Ast]*fragmentPlan{},
		errs: &SubstitutionError{
			fragmentErrs: map[string][]*common.Error{},
			fragmentSrc:  map[string]common.Source{},
			targetSrc:    s.target.Source(),
		},
	}
	if !s.target.IsChecked() {
		b.errs.reasons = append(b.errs.reasons, "substitution target must be a type-checked expression")
		return nil, b.errs
	}

	if !b.validate(s) {
		return nil, b.errs
	}
	return b.build(s)
}

func (b *substitutionBuilder) validate(s *Substitution) bool {
	targetNA := celast.NavigateAST(b.target.NativeRep())
	targetByID := map[int64]celast.NavigableExpr{}
	for _, n := range celast.MatchDescendants(targetNA, celast.AllMatcher()) {
		targetByID[n.ID()] = n
	}

	seenTargets := map[int64]struct{}{}
	for _, edit := range s.edits {
		if edit.fragment == nil {
			b.errs.reasons = append(b.errs.reasons,
				fmt.Sprintf("replacement for target id %d requires a fragment expression", edit.targetID))
			continue
		}
		if _, dup := seenTargets[edit.targetID]; dup {
			b.errs.reasons = append(b.errs.reasons,
				fmt.Sprintf("duplicate replacement target id %d", edit.targetID))
			continue
		}
		seenTargets[edit.targetID] = struct{}{}

		targetNode, found := targetByID[edit.targetID]
		if !found {
			b.reportTarget(edit.targetID,
				"substitution target id %d does not exist in the target expression", edit.targetID)
			continue
		}
		if !edit.fragment.IsChecked() {
			b.reportFragment(edit.fragment,
				"fragment used to replace target id %d must be type-checked", edit.targetID)
			continue
		}
		fragEnv := edit.fragEnv
		if fragEnv == nil {
			fragEnv = s.fragEnv
		}
		if fragEnv == nil {
			fragEnv = s.targetEnv
		}
		if reason, ok := environmentsCompatible(s.targetEnv, fragEnv); !ok {
			b.reportFragment(edit.fragment,
				"fragment environment is not compatible with the target environment: %s", reason)
			continue
		}

		plan := b.planFragment(edit.fragment, fragEnv)
		targetType := b.target.NativeRep().GetType(edit.targetID)
		fragType := edit.fragment.NativeRep().GetType(plan.rootID)
		if !targetType.IsAssignableType(fragType) {
			b.reportTarget(edit.targetID,
				"cannot replace expression of type '%s' with fragment of type '%s'",
				FormatCELType(targetType), FormatCELType(fragType))
			continue
		}
		b.edits = append(b.edits, resolvedEdit{
			targetID: edit.targetID,
			fragment: edit.fragment,
			plan:     plan,
			target:   targetNode,
		})
	}
	if len(b.errs.reasons) > 0 || len(b.errs.targetErrs) > 0 || len(b.errs.fragmentErrs) > 0 {
		return false
	}

	// Reject overlapping target sites: pairs of target nodes must not be nested within each other.
	for i := 0; i < len(b.edits); i++ {
		for j := i + 1; j < len(b.edits); j++ {
			if editsOverlap(b.edits[i].target, b.edits[j].target) {
				b.reportTarget(b.edits[i].targetID,
					"substitution targets %d and %d overlap; nested replacements are not permitted",
					b.edits[i].targetID, b.edits[j].targetID)
			}
		}
	}
	return len(b.errs.targetErrs) == 0
}

func editsOverlap(a, node celast.NavigableExpr) bool {
	if a.ID() == node.ID() {
		return true
	}
	for _, child := range a.Children() {
		if editsOverlap(child, node) {
			return true
		}
	}
	return false
}

func (b *substitutionBuilder) reportTarget(id int64, format string, args ...any) {
	loc := b.target.NativeRep().SourceInfo().GetStartLocation(id)
	b.errs.targetErrs = append(b.errs.targetErrs,
		common.NewError(id, fmt.Sprintf(format, args...), loc))
}

func (b *substitutionBuilder) reportFragment(frag *Ast, format string, args ...any) {
	desc := fragmentDescription(frag)
	id := frag.NativeRep().Expr().ID()
	loc := frag.NativeRep().SourceInfo().GetStartLocation(id)
	b.errs.fragmentErrs[desc] = append(b.errs.fragmentErrs[desc],
		common.NewError(id, fmt.Sprintf(format, args...), loc))
	if frag.Source() != nil {
		b.errs.fragmentSrc[desc] = frag.Source()
	}
}

func fragmentDescription(a *Ast) string {
	if desc := a.NativeRep().SourceInfo().Description(); desc != "" {
		return desc
	}
	if a.Source() != nil && a.Source().Description() != "" {
		return a.Source().Description()
	}
	return "<fragment>"
}

// planFragment computes the fragment root id and its set of free, simple identifier names.
//
// Free names are computed using the same binder scoping the checker applies to comprehensions.
func (b *substitutionBuilder) planFragment(a *Ast, env *Env) *fragmentPlan {
	if plan, found := b.fragments[a]; found {
		return plan
	}
	fv := newFreeVarCollector()
	fv.collect(a.NativeRep().Expr())
	plan := &fragmentPlan{env: env, rootID: a.NativeRep().Expr().ID(), freeVar: fv.free}
	b.fragments[a] = plan
	return plan
}

// freeVarCollector computes the free simple identifier names of an expression following the
// checker's comprehension scoping: the iteration range and accumulator initializer are evaluated
// in the outer scope; the accumulator is bound within the condition, step, and result; the
// iteration variables are additionally bound within the condition and step only.
type freeVarCollector struct {
	bound map[string]struct{}
	free  map[string]struct{}
}

func newFreeVarCollector() *freeVarCollector {
	return &freeVarCollector{bound: map[string]struct{}{}, free: map[string]struct{}{}}
}

func (v *freeVarCollector) push(names ...string) func() {
	previouslyBound := map[string]struct{}{}
	for _, n := range names {
		if n == "" {
			continue
		}
		if _, existed := v.bound[n]; !existed {
			previouslyBound[n] = struct{}{}
		}
		v.bound[n] = struct{}{}
	}
	return func() {
		for _, n := range names {
			if n == "" {
				continue
			}
			if _, wasBound := previouslyBound[n]; wasBound {
				v.bound[n] = struct{}{}
			} else {
				delete(v.bound, n)
			}
		}
	}
}

func (v *freeVarCollector) noteIdent(name string) {
	if strings.Contains(name, ".") {
		return
	}
	if _, isBound := v.bound[name]; !isBound {
		v.free[name] = struct{}{}
	}
}

func (v *freeVarCollector) collect(e celast.Expr) {
	switch e.Kind() {
	case celast.IdentKind:
		v.noteIdent(e.AsIdent())
	case celast.LiteralKind:
		return
	case celast.SelectKind:
		v.collect(e.AsSelect().Operand())
	case celast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			v.collect(c.Target())
		}
		for _, arg := range c.Args() {
			v.collect(arg)
		}
	case celast.ListKind:
		for _, elem := range e.AsList().Elements() {
			v.collect(elem)
		}
	case celast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			v.collect(me.Key())
			v.collect(me.Value())
		}
	case celast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			v.collect(field.AsStructField().Value())
		}
	case celast.ComprehensionKind:
		compre := e.AsComprehension()
		// Range and accumulator initializer are evaluated in the outer scope.
		v.collect(compre.IterRange())
		v.collect(compre.AccuInit())
		iterNames := []string{compre.IterVar()}
		if compre.HasIterVar2() {
			iterNames = append(iterNames, compre.IterVar2())
		}
		// Condition and step see the accumulator and iteration variables.
		popAccu := v.push(compre.AccuVar())
		popIter := v.push(iterNames...)
		v.collect(compre.LoopCondition())
		v.collect(compre.LoopStep())
		popIter()
		// Result only sees the accumulator.
		v.collect(compre.Result())
		popAccu()
	}
}

// siteBinder is a comprehension binder captured on the path from the target root to a
// replacement site. The frame records the binder name and which sub-expressions of the
// comprehension the name is in scope for.
type siteBinder struct {
	compre celast.Expr
	name   string
}

// sitePath records the path taken through the expression tree from the root to a replacement
// target, along with the comprehension binders in scope at the target.
type sitePath struct {
	targetID int64
	binders  []siteBinder
}

func (b *substitutionBuilder) build(s *Substitution) (*Ast, error) {
	// 1. Determine the binders which must be alpha-renamed so each fragment's free variables
	// continue to resolve against the outer environment rather than against a comprehension
	// local at the insertion site.
	paths := map[int64]*sitePath{}
	targetIDs := map[int64]struct{}{}
	for _, edit := range b.edits {
		targetIDs[edit.targetID] = struct{}{}
	}
	findSitePaths(b.target.NativeRep().Expr(), nil, targetIDs, paths)

	renameNames := map[int64]map[string]string{}
	nameCounter := 0
	usedNames := collectAllBinderNames(b.target.NativeRep())
	for _, edit := range b.edits {
		path := paths[edit.targetID]
		if path == nil {
			return nil, &SubstitutionError{reasons: []string{
				fmt.Sprintf("internal error: no binder path recorded for target id %d", edit.targetID)}}
		}
		for i := len(path.binders) - 1; i >= 0; i-- {
			binder := path.binders[i]
			freeInFragment := false
			for freeName := range edit.plan.freeVar {
				if freeName == binder.name {
					freeInFragment = true
					break
				}
			}
			if !freeInFragment {
				continue
			}
			if renameNames[binder.compre.ID()] == nil {
				renameNames[binder.compre.ID()] = map[string]string{}
			}
			if _, alreadyRenamed := renameNames[binder.compre.ID()][binder.name]; alreadyRenamed {
				continue
			}
			fresh := freshBinderName(binder.name, nameCounter)
			nameCounter++
			for isNameUsed(fresh, usedNames) {
				fresh = freshBinderName(binder.name, nameCounter)
				nameCounter++
			}
			usedNames[fresh] = struct{}{}
			renameNames[binder.compre.ID()][binder.name] = fresh
		}
	}

	// 2. Deep copy the target expression and apply the alpha renaming while copying. The copy
	// keeps the target ids.
	fac := celast.NewExprFactory()
	renamedExpr := cloneExprRenamed(fac, b.target.NativeRep().Expr(), renameNames, nil)

	// 3. Allocate fresh ids for every fragment node. Fragment ids come from a shared id
	// generator seeded above the target id range, so all new nodes are globally unique.
	nextID := celast.MaxID(b.target.NativeRep())
	for frag := range b.fragments {
		if max := celast.MaxID(frag.NativeRep()); max > nextID {
			nextID = max
		}
	}

	// 4. Copy, renumber, and migrate a separate fragment instance for every edit. A fragment Ast
	// used at multiple sites must be copied once per site so the spliced trees do not share
	// nodes or collide on ids.
	migratedEdits := make([]*migratedFragment, len(b.edits))
	for i, edit := range b.edits {
		idGen := newFragmentIDGenerator(nextID)
		exprCopy := fac.CopyExpr(edit.fragment.NativeRep().Expr())
		exprCopy.RenumberIDs(idGen.generator)
		infoCopy := celast.CopySourceInfo(edit.fragment.NativeRep().SourceInfo())
		infoCopy.RenumberIDs(idGen.generator)
		renumberMacroCallIDs(infoCopy, idGen)
		nextID = idGen.seed
		migratedEdits[i] = &migratedFragment{
			frag:  edit.fragment,
			expr:  exprCopy,
			info:  infoCopy,
			idMap: idGen.idMap,
		}
	}

	// 5. Splice the fragment roots into the target copy. A target node keeps its id but adopts
	// the fragment root's kind case; the fragment root id metadata is migrated onto the target id.
	targetCopyIndex := indexExprs(renamedExpr)
	// Keep track of target subtree ids that were replaced so stale metadata can be removed.
	// Target sites are disjoint, but collect the subtree ids before splicing any fragment so
	// traversals cannot accidentally include nodes from a previously inserted fragment.
	removedIDs := map[int64]struct{}{}
	for _, edit := range b.edits {
		targetNode, found := targetCopyIndex[edit.targetID]
		if !found {
			return nil, &SubstitutionError{reasons: []string{
				fmt.Sprintf("internal error: target id %d missing after copy", edit.targetID)}}
		}
		collectSubtreeIDs(targetNode, removedIDs)
	}
	for i, edit := range b.edits {
		targetNode, found := targetCopyIndex[edit.targetID]
		if !found {
			return nil, &SubstitutionError{reasons: []string{
				fmt.Sprintf("internal error: target id %d missing after copy", edit.targetID)}}
		}
		mf := migratedEdits[i]
		targetNode.SetKindCase(mf.expr)
	}

	// 6. Assemble the merged SourceInfo in a shared coordinate system: target source first, then
	// each fragment source on its own line. Offset ranges are shifted accordingly so errors can
	// locate nodes in the original target and fragment text.
	combinedSrc, mergedInfo, sectionList := b.mergeSourceInfo(migratedEdits, removedIDs)
	b.sections = sectionList

	// 7. Migrate type and reference metadata from target and fragment ASTs using the id maps.
	typeMap := map[int64]*types.Type{}
	refMap := map[int64]*celast.ReferenceInfo{}
	for id, t := range b.target.NativeRep().TypeMap() {
		if _, removed := removedIDs[id]; removed {
			continue
		}
		typeMap[id] = t
	}
	for id, r := range b.target.NativeRep().ReferenceMap() {
		if _, removed := removedIDs[id]; removed {
			continue
		}
		refMap[id] = r
	}
	for i, edit := range b.edits {
		mf := migratedEdits[i]
		for oldID, t := range edit.fragment.NativeRep().TypeMap() {
			newID, found := mf.idMap[oldID]
			if !found {
				continue
			}
			if newID == mf.idMap[edit.plan.rootID] {
				newID = edit.targetID
			}
			typeMap[newID] = t
		}
		for oldID, r := range edit.fragment.NativeRep().ReferenceMap() {
			newID, found := mf.idMap[oldID]
			if !found {
				continue
			}
			if newID == mf.idMap[edit.plan.rootID] {
				newID = edit.targetID
			}
			refMap[newID] = r
		}
	}

	merged := celast.NewCheckedAST(celast.NewAST(renamedExpr, mergedInfo), typeMap, refMap)

	// 8. Re-check the merged AST against the target environment to confirm that the migrated
	// metadata is internally consistent (overloads resolve, references resolve, and types flow
	// through the splice boundaries). Errors at fragment nodes retain fragment source positions.
	checked, iss := s.targetEnv.Check(&Ast{source: combinedSrc, impl: merged})
	if iss != nil && iss.Err() != nil {
		return nil, translateSubstitutionIssues(b, iss)
	}

	return &Ast{source: combinedSrc, impl: checked.NativeRep()}, nil
}

// findSitePaths walks the target tree and, for every id in targets, records the comprehension
// binders that would be in scope at that position.
//
// binders is ordered from outermost to innermost. The scoping matches the checker: iterRange and
// accuInit see no binder from the current comprehension; loopCondition and loopStep see all
// binders; result sees only the accumulator.
func findSitePaths(e celast.Expr, binders []siteBinder, targets map[int64]struct{}, out map[int64]*sitePath) {
	if _, isTarget := targets[e.ID()]; isTarget {
		path := &sitePath{targetID: e.ID(), binders: append([]siteBinder(nil), binders...)}
		out[e.ID()] = path
		// Targets are disjoint, but continue so children are not needed; stop descending since
		// nested targets are rejected.
		return
	}
	switch e.Kind() {
	case celast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			findSitePaths(c.Target(), binders, targets, out)
		}
		for _, arg := range c.Args() {
			findSitePaths(arg, binders, targets, out)
		}
	case celast.SelectKind:
		findSitePaths(e.AsSelect().Operand(), binders, targets, out)
	case celast.ListKind:
		for _, elem := range e.AsList().Elements() {
			findSitePaths(elem, binders, targets, out)
		}
	case celast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			findSitePaths(me.Key(), binders, targets, out)
			findSitePaths(me.Value(), binders, targets, out)
		}
	case celast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			findSitePaths(field.AsStructField().Value(), binders, targets, out)
		}
	case celast.ComprehensionKind:
		compre := e.AsComprehension()
		// Range and accu init are outside the comprehension's scopes.
		findSitePaths(compre.IterRange(), binders, targets, out)
		findSitePaths(compre.AccuInit(), binders, targets, out)

		accuBinder := siteBinder{compre: e, name: compre.AccuVar()}
		iterBinder := siteBinder{compre: e, name: compre.IterVar()}
		iterBinder2 := siteBinder{compre: e, name: compre.IterVar2()}

		accuScope := append(binders, accuBinder)
		loopScope := append(append([]siteBinder(nil), accuScope...), iterBinder)
		if compre.HasIterVar2() {
			loopScope = append(loopScope, iterBinder2)
		}
		findSitePaths(compre.LoopCondition(), loopScope, targets, out)
		findSitePaths(compre.LoopStep(), loopScope, targets, out)
		findSitePaths(compre.Result(), accuScope, targets, out)
	}
}

func collectAllBinderNames(a *celast.AST) map[string]struct{} {
	names := map[string]struct{}{}
	celast.PostOrderVisit(a.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() != celast.ComprehensionKind {
			return
		}
		compre := e.AsComprehension()
		names[compre.IterVar()] = struct{}{}
		names[compre.AccuVar()] = struct{}{}
		if compre.HasIterVar2() {
			names[compre.IterVar2()] = struct{}{}
		}
	}))
	return names
}

func isNameUsed(name string, names map[string]struct{}) bool {
	_, found := names[name]
	return found
}

// freshBinderName generates a legal CEL identifier which cannot collide with an existing
// comprehension binder or a user variable: the "__cel_subst_" prefix is not a valid prefix for
// declared identifiers in typical environments and is also distinct from the parser accumulator
// names.
func freshBinderName(original string, counter int) string {
	sanitized := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, original)
	return fmt.Sprintf("__cel_subst_%d_%s", counter, sanitized)
}

// cloneExprRenamed deep-copies an expression while renaming comprehension binders according to
// the rename map keyed by comprehension expression id.
//
// The traversal mirrors the checker's scoping so that references inside each comprehension part
// are only renamed for binders live in that part. Renames for enclosing comprehensions are
// carried via the rename stack: the stack passed to each sub-expression contains exactly the
// binders in scope at that position.
func cloneExprRenamed(fac celast.ExprFactory, e celast.Expr, renames map[int64]map[string]string, active []activeRename) celast.Expr {
	switch e.Kind() {
	case celast.IdentKind:
		return fac.NewIdent(e.ID(), applyRenames(e.AsIdent(), active))
	case celast.LiteralKind:
		return fac.NewLiteral(e.ID(), e.AsLiteral())
	case celast.SelectKind:
		s := e.AsSelect()
		operand := cloneExprRenamed(fac, s.Operand(), renames, active)
		if s.IsTestOnly() {
			return fac.NewPresenceTest(e.ID(), operand, s.FieldName())
		}
		return fac.NewSelect(e.ID(), operand, s.FieldName())
	case celast.CallKind:
		c := e.AsCall()
		args := make([]celast.Expr, len(c.Args()))
		for i, arg := range c.Args() {
			args[i] = cloneExprRenamed(fac, arg, renames, active)
		}
		if c.IsMemberFunction() {
			target := cloneExprRenamed(fac, c.Target(), renames, active)
			return fac.NewMemberCall(e.ID(), c.FunctionName(), target, args...)
		}
		return fac.NewCall(e.ID(), c.FunctionName(), args...)
	case celast.ListKind:
		l := e.AsList()
		elems := make([]celast.Expr, l.Size())
		for i, elem := range l.Elements() {
			elems[i] = cloneExprRenamed(fac, elem, renames, active)
		}
		return fac.NewList(e.ID(), elems, l.OptionalIndices())
	case celast.MapKind:
		m := e.AsMap()
		entries := make([]celast.EntryExpr, m.Size())
		for i, entry := range m.Entries() {
			me := entry.AsMapEntry()
			key := cloneExprRenamed(fac, me.Key(), renames, active)
			value := cloneExprRenamed(fac, me.Value(), renames, active)
			entries[i] = fac.NewMapEntry(entry.ID(), key, value, me.IsOptional())
		}
		return fac.NewMap(e.ID(), entries)
	case celast.StructKind:
		s := e.AsStruct()
		fields := make([]celast.EntryExpr, len(s.Fields()))
		for i, field := range s.Fields() {
			sf := field.AsStructField()
			value := cloneExprRenamed(fac, sf.Value(), renames, active)
			fields[i] = fac.NewStructField(field.ID(), sf.Name(), value, sf.IsOptional())
		}
		return fac.NewStruct(e.ID(), s.TypeName(), fields)
	case celast.ComprehensionKind:
		compre := e.AsComprehension()
		// Range and accuInit are evaluated in the outer scope, so the comprehension's own
		// renames do not apply within them.
		iterRange := cloneExprRenamed(fac, compre.IterRange(), renames, active)
		accuInit := cloneExprRenamed(fac, compre.AccuInit(), renames, active)

		localRenames := renames[e.ID()]
		iterVar := compre.IterVar()
		iterVar2 := compre.IterVar2()
		accuVar := compre.AccuVar()
		// The loop scope stack extends the outer binders with the (possibly renamed) accumulator
		// and iteration variables.
		loopActive := append([]activeRename(nil), active...)
		if renamed, found := localRenames[iterVar]; found {
			loopActive = append(loopActive, activeRename{from: iterVar, to: renamed})
			iterVar = renamed
		}
		if compre.HasIterVar2() {
			if renamed, found := localRenames[iterVar2]; found {
				loopActive = append(loopActive, activeRename{from: iterVar2, to: renamed})
				iterVar2 = renamed
			}
		}
		if renamed, found := localRenames[compre.AccuVar()]; found {
			loopActive = append(loopActive, activeRename{from: compre.AccuVar(), to: renamed})
			accuVar = renamed
		}

		loopCond := cloneExprRenamed(fac, compre.LoopCondition(), renames, loopActive)
		loopStep := cloneExprRenamed(fac, compre.LoopStep(), renames, loopActive)

		// The result sees the outer binders plus the accumulator only.
		resultActive := append([]activeRename(nil), active...)
		if renamed, found := localRenames[compre.AccuVar()]; found {
			resultActive = append(resultActive, activeRename{from: compre.AccuVar(), to: renamed})
		}
		result := cloneExprRenamed(fac, compre.Result(), renames, resultActive)

		return fac.NewComprehensionTwoVar(e.ID(), iterRange, iterVar, iterVar2, accuVar,
			accuInit, loopCond, loopStep, result)
	default:
		return fac.NewUnspecifiedExpr(e.ID())
	}
}

// activeRename records a single in-scope binder rename: references to "from" are rewritten to
// "to" while the stack entry is present.
type activeRename struct {
	from string
	to   string
}

func applyRenames(name string, active []activeRename) string {
	// Innermost binding wins, so walk in reverse.
	for i := len(active) - 1; i >= 0; i-- {
		if active[i].from == name {
			return active[i].to
		}
	}
	return name
}

// fragmentIDGenerator allocates fresh, globally unique ids for fragment nodes while remembering
// the mapping from old fragment ids so type, reference, and source metadata can be migrated.
type fragmentIDGenerator struct {
	seed  int64
	idMap map[int64]int64
}

func newFragmentIDGenerator(seed int64) *fragmentIDGenerator {
	return &fragmentIDGenerator{seed: seed, idMap: map[int64]int64{}}
}

func (g *fragmentIDGenerator) generator(oldID int64) int64 {
	if oldID == 0 {
		return 0
	}
	if newID, found := g.idMap[oldID]; found {
		return newID
	}
	g.seed++
	g.idMap[oldID] = g.seed
	return g.seed
}

// renumberMacroCallIDs renumbers the ids embedded within the macro call expressions themselves
// (the original, unexpanded macro signatures). The map keys have already been renumbered by
// SourceInfo.RenumberIDs.
func renumberMacroCallIDs(info *celast.SourceInfo, gen *fragmentIDGenerator) {
	macroIDs := make([]int64, 0, len(info.MacroCalls()))
	for id := range info.MacroCalls() {
		macroIDs = append(macroIDs, id)
	}
	sort.Slice(macroIDs, func(i, j int) bool { return macroIDs[i] < macroIDs[j] })
	type pendingMacro struct {
		id   int64
		call celast.Expr
	}
	pending := make([]pendingMacro, 0, len(macroIDs))
	for _, id := range macroIDs {
		call := info.MacroCalls()[id]
		info.ClearMacroCall(id)
		call.RenumberIDs(gen.generator)
		pending = append(pending, pendingMacro{id: id, call: call})
	}
	for _, p := range pending {
		info.SetMacroCall(p.id, p.call)
	}
}

// indexExprs returns a map from expr id to the expr node within a copied target tree. The copy
// produced for substitution keeps the original target ids, so the target nodes can be looked up
// directly.
func indexExprs(root celast.Expr) map[int64]celast.Expr {
	index := map[int64]celast.Expr{}
	indexExprsInto(root, index)
	return index
}

func indexExprsInto(e celast.Expr, index map[int64]celast.Expr) {
	if e == nil {
		return
	}
	index[e.ID()] = e
	switch e.Kind() {
	case celast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			indexExprsInto(c.Target(), index)
		}
		for _, arg := range c.Args() {
			indexExprsInto(arg, index)
		}
	case celast.SelectKind:
		indexExprsInto(e.AsSelect().Operand(), index)
	case celast.ListKind:
		for _, elem := range e.AsList().Elements() {
			indexExprsInto(elem, index)
		}
	case celast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			indexExprsInto(me.Key(), index)
			indexExprsInto(me.Value(), index)
		}
	case celast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			indexExprsInto(field.AsStructField().Value(), index)
		}
	case celast.ComprehensionKind:
		compre := e.AsComprehension()
		indexExprsInto(compre.IterRange(), index)
		indexExprsInto(compre.AccuInit(), index)
		indexExprsInto(compre.LoopCondition(), index)
		indexExprsInto(compre.LoopStep(), index)
		indexExprsInto(compre.Result(), index)
	}
}

func collectSubtreeIDs(root celast.Expr, out map[int64]struct{}) {
	celast.PostOrderVisit(root, celast.NewExprVisitor(func(e celast.Expr) {
		out[e.ID()] = struct{}{}
	}))
}

// fragmentSection records where a fragment's content was placed within the combined source text
// so checker errors can be translated back to the fragment's own source coordinates.
type fragmentSection struct {
	frag         *Ast
	offsetShift  int32 // offset in the combined content where fragment text begins
	lineShift    int32 // number of lines preceding the fragment text
	idMap        map[int64]int64
	rootTargetID int64
}

// mergeSourceInfo builds a combined textual source and merged SourceInfo for the spliced AST.
//
// Layout: target source text first, followed for each distinct fragment by a blank line and a
// labeled marker line ("// fragment: <desc>") and then the fragment's own source text. Target
// nodes keep their original offsets; fragment node offsets are shifted by the section offset.
func (b *substitutionBuilder) mergeSourceInfo(
	migratedEdits []*migratedFragment,
	removedIDs map[int64]struct{},
) (common.Source, *celast.SourceInfo, []*fragmentSection) {
	targetInfo := b.target.NativeRep().SourceInfo()
	targetContent := ""
	if b.target.Source() != nil {
		targetContent = b.target.Source().Content()
	}

	var sb strings.Builder
	sb.WriteString(targetContent)

	sectionList := []*fragmentSection{}
	for i, mf := range migratedEdits {
		frag := mf.frag
		targetID := b.edits[i].targetID
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		lineShift := int32(strings.Count(sb.String(), "\n"))
		offsetBeforeHeader := int32(sb.Len())
		header := fmt.Sprintf("// fragment: %s\n", fragmentDescription(frag))
		sb.WriteString(header)
		fragContent := ""
		if frag.Source() != nil {
			fragContent = frag.Source().Content()
		}
		sb.WriteString(fragContent)

		section := &fragmentSection{
			frag:         frag,
			offsetShift:  offsetBeforeHeader + int32(len(header)),
			lineShift:    lineShift + 2,
			idMap:        mf.idMap,
			rootTargetID: targetID,
		}
		sectionList = append(sectionList, section)
	}

	content := sb.String()
	combinedSrc := common.NewStringSource(content, targetInfo.Description())
	mergedInfo := celast.NewMergedSourceInfo(
		targetInfo.Description(),
		combinedSrc.LineOffsets(),
		0, 0)

	fac := celast.NewExprFactory()

	// Target offset ranges and macro calls, excluding replaced subtrees.
	for id, r := range targetInfo.OffsetRanges() {
		if _, removed := removedIDs[id]; removed {
			continue
		}
		mergedInfo.SetOffsetRange(id, r)
	}
	for id, call := range targetInfo.MacroCalls() {
		if _, removed := removedIDs[id]; removed {
			continue
		}
		mergedInfo.SetMacroCall(id, fac.CopyExpr(call))
	}

	// Fragment offset ranges and macro calls, shifted into the combined coordinate system, with
	// fragment root metadata attached to the target node id.
	for i, section := range sectionList {
		mf := migratedEdits[i]
		frag := section.frag
		fragInfo := mf.info
		rootNewID := mf.idMap[frag.NativeRep().Expr().ID()]
		for id, r := range fragInfo.OffsetRanges() {
			newRange := celast.OffsetRange{
				Start: r.Start + section.offsetShift,
				Stop:  r.Stop + section.offsetShift,
			}
			if id == rootNewID {
				mergedInfo.SetOffsetRange(section.rootTargetID, newRange)
				continue
			}
			mergedInfo.SetOffsetRange(id, newRange)
		}
		for id, call := range fragInfo.MacroCalls() {
			if id == rootNewID {
				mergedInfo.SetMacroCall(section.rootTargetID, fac.CopyExpr(call))
				continue
			}
			mergedInfo.SetMacroCall(id, fac.CopyExpr(call))
		}
	}

	for _, ext := range targetInfo.Extensions() {
		mergedInfo.AddExtension(ext)
	}
	for i := range sectionList {
		for _, ext := range migratedEdits[i].info.Extensions() {
			mergedInfo.AddExtension(ext)
		}
	}

	return combinedSrc, mergedInfo, sectionList
}

// translateSubstitutionIssues converts re-check errors on the combined source into a
// SubstitutionError whose positions are expressed in the original target/fragment coordinates.
func translateSubstitutionIssues(b *substitutionBuilder, iss *Issues) error {
	out := &SubstitutionError{
		fragmentErrs: map[string][]*common.Error{},
		fragmentSrc:  map[string]common.Source{},
		targetSrc:    b.target.Source(),
	}

	// Build a map from new fragment node id to its section, and note the root special case.
	newIDToSection := map[int64]*fragmentSection{}
	for _, section := range b.sections {
		for _, newID := range section.idMap {
			newIDToSection[newID] = section
		}
		newIDToSection[section.rootTargetID] = section
	}

	for _, err := range iss.Errors() {
		if section, found := newIDToSection[err.ExprID]; found {
			loc := err.Location
			fragLoc := common.NewLocation(
				loc.Line()-int(section.lineShift)+1,
				loc.Column())
			desc := fragmentDescription(section.frag)
			out.fragmentErrs[desc] = append(out.fragmentErrs[desc],
				common.NewError(err.ExprID, err.Message, fragLoc))
			if section.frag.Source() != nil {
				out.fragmentSrc[desc] = section.frag.Source()
			}
			continue
		}
		// Target nodes keep their ids and line/columns, so the combined location is already the
		// target location (target content comes first).
		out.targetErrs = append(out.targetErrs,
			common.NewError(err.ExprID, err.Message, err.Location))
	}
	if len(out.targetErrs) == 0 && len(out.fragmentErrs) == 0 {
		// Fall back to the raw issue text if no per-node errors could be attributed.
		out.reasons = append(out.reasons, iss.Err().Error())
	}
	return out
}

// environmentsCompatible checks whether a fragment checked in fragmentEnv can safely be spliced
// into a rule checked against targetEnv.
//
// The fast path accepts the same Env instance (including shared checker environments produced by
// Env.Extend). Otherwise the serialized declaration sets (container, imports, variables, and
// function overloads), type-checking feature flags, and limits are compared structurally. The
// spliced AST is always re-checked against the target environment as a final guarantee.
func environmentsCompatible(targetEnv, fragmentEnv *Env) (string, bool) {
	if targetEnv == fragmentEnv {
		return "", true
	}
	if targetEnv == nil || fragmentEnv == nil {
		return "an environment is missing", false
	}
	targetChk, targetErr := targetEnv.initChecker()
	fragChk, fragErr := fragmentEnv.initChecker()
	if targetErr != nil || fragErr != nil {
		if targetErr != nil {
			return fmt.Sprintf("target environment initialization failed: %v", targetErr), false
		}
		return fmt.Sprintf("fragment environment initialization failed: %v", fragErr), false
	}
	if targetChk == fragChk {
		return "", true
	}

	targetSig, err1 := environmentSignature(targetEnv)
	fragSig, err2 := environmentSignature(fragmentEnv)
	if err1 != nil || err2 != nil {
		if err1 != nil {
			return fmt.Sprintf("could not inspect target environment: %v", err1), false
		}
		return fmt.Sprintf("could not inspect fragment environment: %v", err2), false
	}
	if targetSig != fragSig {
		return "environment declarations, features, or limits differ", false
	}
	return "", true
}

// environmentSignature produces a stable string describing the type-checking relevant parts of
// an environment: serialized container/variables/functions plus checker feature flags and limits.
func environmentSignature(e *Env) (string, error) {
	conf, err := e.ToConfig("")
	if err != nil {
		return "", err
	}
	conf.Name = ""
	conf.Description = ""

	// Include feature flags that affect type checking but are not serialized into the config.
	checkerFeatures := []int{
		featureCrossTypeNumericComparisons,
		featureEagerlyValidateDeclarations,
		featureDefaultUTCTimeZone,
		featureVariadicLogicalASTs,
		featureEnableErrorOnBadPresenceTest,
		featureIdentEscapeSyntax,
		featureEnableMacroCallTracking,
		featureJSONFieldNames,
	}
	featureParts := make([]string, 0, len(checkerFeatures))
	for _, flag := range checkerFeatures {
		featureParts = append(featureParts,
			fmt.Sprintf("%d=%t", flag, e.HasFeature(flag)))
	}

	yamlBytes, err := yaml.Marshal(conf)
	if err != nil {
		return "", err
	}
	return string(yamlBytes) + "|" + strings.Join(featureParts, ","), nil
}
