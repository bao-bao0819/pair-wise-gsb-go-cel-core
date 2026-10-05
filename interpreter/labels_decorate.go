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

package interpreter

import (
	"fmt"

	"cel.dev/cel-go/common/stdlib"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// LabelTracking enables per-evaluation data label tracking using the supplied function
// registry. The returned option is passed to NewInterpretable / cel.Program.
//
// The registry is read-only during evaluation; all tracking state lives on the evaluation
// frame. Functions already registered on the dispatcher at planning time are treated as
// built-ins for strict-mode purposes, so later user extension functions must be declared.
func LabelTracking(prog *LabelProgram) PlannerOption {
	return func(p *planner) (*planner, error) {
		p.lateDecorators = append(p.lateDecorators, decTrackLabels(prog.withBuiltinBaseline(stdlibBaseline())))
		p.rootWrapper = func(root InterpretableV2) InterpretableV2 {
			return &labeledRoot{InterpretableV2: root, prog: prog}
		}
		return p, nil
	}
}

// stdlibBaseline returns the function names and overload ids declared by the CEL standard
// library, which are built-ins requiring no declaration in strict label mode.
func stdlibBaseline() map[string]struct{} {
	baseline := map[string]struct{}{}
	for _, fn := range stdlib.Functions() {
		baseline[fn.Name()] = struct{}{}
		for _, ov := range fn.OverloadDecls() {
			baseline[ov.ID()] = struct{}{}
		}
	}
	return baseline
}

// decTrackLabels returns the outermost decorator which instruments the final planned nodes
// with label propagation.
func decTrackLabels(prog *LabelProgram) InterpretableDecoratorV2 {
	return func(i InterpretableV2) (InterpretableV2, error) {
		switch node := i.(type) {
		case InterpretableConst:
			return &labeledConst{InterpretableConst: node, prog: prog}, nil
		case *evalFold:
			return newLabeledFold(node, prog), nil
		case InterpretableConstructor:
			return &labeledConstructor{InterpretableConstructor: node, prog: prog}, nil
		case InterpretableCall:
			return decorateLabeledCall(node, prog)
		case InterpretableAttribute:
			if cond, isCond := node.Attr().(*conditionalAttribute); isCond {
				if evalA, ok := node.(*evalAttr); ok {
					evalA.attr = &trackingConditional{conditionalAttribute: cond}
				}
			}
			return &labeledAttr{InterpretableAttribute: node, prog: prog}, nil
		// evalOr / evalAnd and their exhaustive counterparts implement neither InterpretableCall
		// nor InterpretableConst; instrument them explicitly for short-circuit control labels.
		case *evalOr:
			return &labeledLogical{id: node.id, terms: node.terms, exhaustive: false, isOr: true, prog: prog}, nil
		case *evalAnd:
			return &labeledLogical{id: node.id, terms: node.terms, exhaustive: false, isOr: false, prog: prog}, nil
		case *evalExhaustiveOr:
			return &labeledLogical{id: node.id, terms: node.terms, exhaustive: true, isOr: true, prog: prog}, nil
		case *evalExhaustiveAnd:
			return &labeledLogical{id: node.id, terms: node.terms, exhaustive: true, isOr: false, prog: prog}, nil
		}
		return i, nil
	}
}

// preCallFor builds the pre-body hook for a call node which enforces sink policy and strict
// mode before the function implementation runs. Result labels are recorded by the wrapping
// node after the call completes.
func preCallFor(prog *LabelProgram, function, overload string, argLabels func() labelSet) func(*ExecutionFrame, string, string, []ref.Val) ref.Val {
	return func(frame *ExecutionFrame, fn, ov string, args []ref.Val) ref.Val {
		st := frame.labelState()
		if st == nil {
			return nil
		}
		class, declared := prog.classFor(fn, ov)
		if class == FunctionSink {
			labels := argLabels()
			policy := prog.policies[ov]
			if policy == nil {
				policy = prog.policies[fn]
			}
			if policy != nil {
				if err := policy(SinkCall{Function: fn, OverloadID: ov, Args: args, Labels: labels}); err != nil {
					return types.NewErrWithNodeID(0, "sink %s rejected: %v", fn, err)
				}
			}
			return nil
		}
		if !declared && prog.strict && !prog.isBuiltin(fn, ov) {
			return types.NewErrWithNodeID(0, "undeclared extension function %q rejected by strict label mode", fn)
		}
		return nil
	}
}

// resultLabelsFor computes the result labels for a call according to its declared class.
func resultLabelsFor(prog *LabelProgram, function, overload string, argLabels labelSet) labelSet {
	class, declared := prog.classFor(function, overload)
	if class == FunctionSanitizes {
		return nil
	}
	if !declared && prog.strict && !prog.isBuiltin(function, overload) {
		return nil
	}
	return argLabels
}

// labeledConst wraps a constant node; constants carry no labels.
type labeledConst struct {
	InterpretableConst
	prog *LabelProgram
}

func (c *labeledConst) Eval(vars Activation) ref.Val {
	return EvalActivation(vars, c.Exec)
}

func (c *labeledConst) Exec(frame *ExecutionFrame) ref.Val {
	if st := frame.labelState(); st != nil {
		st.setLabels(c.ID(), nil)
	}
	return c.InterpretableConst.Exec(frame)
}

// labeledConstructor tracks list/map/struct construction: the constructed value carries the
// union of all its element/key/value labels.
type labeledConstructor struct {
	InterpretableConstructor
	prog *LabelProgram
}

func (c *labeledConstructor) Eval(vars Activation) ref.Val {
	return EvalActivation(vars, c.Exec)
}

func (c *labeledConstructor) Exec(frame *ExecutionFrame) ref.Val {
	val := c.InterpretableConstructor.Exec(frame)
	if st := frame.labelState(); st != nil {
		st.setLabels(c.ID(), st.argLabels(c.InitVals()))
	}
	return val
}

// labeledLogical re-implements && / || evaluation while recording the labels of the terms that
// actually influenced the result. In short-circuit mode unevaluated terms contribute nothing;
// in exhaustive mode every evaluated term contributes. The recorded labels include both data
// and control dependence, since a term's value selects the logical result.
type labeledLogical struct {
	id         int64
	terms      []InterpretableV2
	exhaustive bool
	isOr       bool
	prog       *LabelProgram
}

func (l *labeledLogical) ID() int64 { return l.id }

func (l *labeledLogical) Eval(ctx Activation) ref.Val {
	return EvalActivation(ctx, l.Exec)
}

func (l *labeledLogical) Exec(frame *ExecutionFrame) ref.Val {
	var labels labelSet
	var err ref.Val
	var unk *types.Unknown
	shortCircuit := false
	for _, term := range l.terms {
		if shortCircuit && !l.exhaustive {
			break
		}
		val := term.Exec(frame)
		if st := frame.labelState(); st != nil {
			labels = labels.add(st.labelsOf(term.ID()))
		}
		boolVal, ok := val.(types.Bool)
		if ok {
			if (l.isOr && boolVal == types.True) || (!l.isOr && boolVal == types.False) {
				shortCircuit = true
				continue
			}
			continue
		}
		if unkVal, isUnk := val.(*types.Unknown); isUnk {
			if unk == nil {
				unk = unkVal
			} else {
				unk = types.MergeUnknowns(unk, unkVal)
			}
		} else if err == nil {
			if isError(val) {
				err = val
			} else {
				err = types.MaybeNoSuchOverloadErr(val)
			}
			err = types.LabelErrNode(l.id, err)
		}
	}
	if st := frame.labelState(); st != nil {
		st.setLabels(l.id, labels)
	}
	if shortCircuit {
		if l.isOr {
			return types.True
		}
		return types.False
	}
	if unk != nil {
		return unk
	}
	if err != nil {
		return err
	}
	if l.isOr {
		return types.False
	}
	return types.True
}

// labeledAttr tracks variable and field selection nodes. Root variables and static field/index
// paths resolve labels from the per-evaluation input; relative selections inherit their
// operand's labels plus any static path continuation. Control-dependent ternary attributes are
// handled from their evaluated branches.
type labeledAttr struct {
	InterpretableAttribute
	prog *LabelProgram
}

func (a *labeledAttr) Eval(vars Activation) ref.Val {
	return EvalActivation(vars, a.Exec)
}

func (a *labeledAttr) Exec(frame *ExecutionFrame) ref.Val {
	val := a.InterpretableAttribute.Exec(frame)
	if st := frame.labelState(); st != nil {
		st.setLabels(a.ID(), a.computeLabels(frame, st))
	}
	return val
}

func (a *labeledAttr) computeLabels(frame *ExecutionFrame, st *labelEvalState) labelSet {
	switch attr := a.Attr().(type) {
	case *trackingConditional:
		labels := st.labelsOf(attr.expr.ID())
		if attr.selected == condSelectedTrue {
			labels = labels.add(attrBranchLabels(attr.truthy, st))
		} else if attr.selected == condSelectedFalse {
			labels = labels.add(attrBranchLabels(attr.falsy, st))
		}
		return labels
	case NamespacedAttribute:
		return namespacedAttrLabels(attr, st)
	case *relativeAttribute:
		return relativeAttrLabels(attr, st)
	}
	return nil
}

// condSelection records which branch a ternary selected during a single Resolve.
type condSelection int

const (
	condSelectedNone condSelection = iota
	condSelectedTrue
	condSelectedFalse
)

// trackingConditional wraps a conditionalAttribute to remember the selected branch during a
// single resolution without re-evaluating the condition. It embeds the attribute and shares
// its qualifiers; selection is reset at the start of every Resolve.
type trackingConditional struct {
	*conditionalAttribute
	selected condSelection
}

func (t *trackingConditional) Resolve(vars Activation) (any, error) {
	t.selected = condSelectedNone
	val := t.conditionalAttribute.expr.Eval(vars)
	if val == types.True {
		t.selected = condSelectedTrue
		return t.conditionalAttribute.truthy.Resolve(vars)
	}
	if val == types.False {
		t.selected = condSelectedFalse
		return t.conditionalAttribute.falsy.Resolve(vars)
	}
	if types.IsUnknown(val) {
		return val, nil
	}
	return nil, types.MaybeNoSuchOverloadErr(val).(*types.Err)
}

// attrBranchLabels computes the data labels of a conditional branch attribute. Branch
// attributes are constructed privately by the planner (often relative attributes) and are not
// themselves decorated, so compute their labels directly.
func attrBranchLabels(attr Attribute, st *labelEvalState) labelSet {
	switch br := attr.(type) {
	case *relativeAttribute:
		// The branch relative attribute is independently wrapped as a labeled attr node (see
		// planBuilder.relativeAttr), so read its recorded labels directly by node id. Fall back
		// to the operand labels if the node id was not recorded.
		if labels := st.labelsOf(br.ID()); labels != nil {
			return labels
		}
		return relativeAttrLabels(br, st)
	case NamespacedAttribute:
		return namespacedAttrLabels(br, st)
	default:
		return nil
	}
}

// staticQualifierString appends the qualifier to the path if it is a static field/key/index;
// ok is false for dynamically evaluated qualifiers.
func staticQualifierString(q Qualifier) (seg string, ok bool) {
	cq, isConst := q.(ConstantQualifier)
	if !isConst {
		return "", false
	}
	switch v := cq.Value().(type) {
	case types.String:
		return string(v), true
	case types.Int:
		return fmt.Sprintf("%d", int64(v)), true
	case types.Uint:
		return fmt.Sprintf("%d", uint64(v)), true
	case types.Bool:
		return fmt.Sprintf("%t", bool(v)), true
	default:
		return "", false
	}
}

func namespacedAttrLabels(attr NamespacedAttribute, st *labelEvalState) labelSet {
	names := attr.CandidateVariableNames()
	quals := attr.Qualifiers()
	var labels labelSet
	for _, root := range names {
		path := root
		// A comprehension iteration/accumulator variable that shadows an input name resolves
		// the root from the fold scope; otherwise the root comes from the input labels.
		var rootLabels labelSet
		if len(names) == 1 {
			rootLabels = st.localLabelsFor(root)
		}
		if rootLabels == nil {
			rootLabels = st.resolveInputLabels(root)
		}
		labels = labels.add(rootLabels)
		allStatic := true
		for _, q := range quals {
			seg, ok := staticQualifierString(q)
			if !ok {
				allStatic = false
				break
			}
			path = path + "." + seg
			labels = labels.add(st.resolveInputLabels(path))
		}
		// A dynamically indexed selection could land on any element: conservatively retain the
		// root variable labels even though the exact child path is unknown.
		if !allStatic {
			labels = labels.add(rootLabels)
		}
	}
	return labels
}

func relativeAttrLabels(attr *relativeAttribute, st *labelEvalState) labelSet {
	// The operand carries the base data labels; statically qualified field/index selections on
	// top of a relative operand keep those labels. A dynamic qualifier cannot refine labels
	// further, so the operand labels are the conservative result.
	return st.labelsOf(attr.operand.ID())
}
