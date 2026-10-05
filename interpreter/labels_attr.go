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
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// labelAttribute handles labels for variable and field-selection attribute nodes.
//
// For a plain absolute/ident attribute (a variable plus constant qualifiers) the label set is
// taken from the declared input labels at the selected path, or from the current
// comprehension/bind scope for a bare local variable. A relative attribute (a qualifier applied
// to an expression result) inherits the operand's labels.
type labelAttribute struct {
	InterpretableAttribute
	cfg *LabelConfig
}

func newLabelAttribute(node InterpretableAttribute, cfg *LabelConfig) (InterpretableV2, error) {
	if cond, isCond := node.Attr().(*conditionalAttribute); isCond {
		return &labelConditionalAttribute{
			InterpretableAttribute: node,
			cond:                   cond,
		}, nil
	}
	return &labelAttribute{InterpretableAttribute: node, cfg: cfg}, nil
}

func (a *labelAttribute) Exec(frame *ExecutionFrame) ref.Val {
	val := a.InterpretableAttribute.Exec(frame)
	tracker := trackerFromFrame(frame)
	if tracker != nil {
		if types.IsUnknownOrError(val) {
			tracker.setNodeValue(a.ID(), val, nil)
		} else {
			tracker.setNodeValue(a.ID(), val, a.computeLabels(tracker))
		}
	}
	return val
}

func (a *labelAttribute) Eval(ctx Activation) ref.Val {
	return EvalActivation(ctx, a.Exec)
}

// computeLabels resolves labels for an attribute node.
func (a *labelAttribute) computeLabels(tracker *labelTracker) labelSet {
	if r := resolveAttribute(a.Attr()); r.root != "" {
		if r.dynamic {
			return tracker.labelsForPath(r.root, nil)
		}
		if len(r.path) == 0 {
			if labels, isLocal := tracker.local(r.root); isLocal {
				return labels
			}
		}
		return tracker.labelsForPath(r.root, r.path)
	}
	if rel, isRel := a.Attr().(*relativeAttribute); isRel {
		return tracker.node(rel.operand.ID())
	}
	return nil
}

// labelConditionalAttribute tracks (cond ? truthy : falsy).
//
// The condition is evaluated once here and memoized for the underlying conditional attribute,
// which then resolves exactly one branch. The result carries the selected branch's data labels
// plus the condition labels, since the condition controls which value is chosen. Field
// qualifications added after the ternary are applied to the conditional attribute and inherit
// the resulting label set via the outer attribute node.
type labelConditionalAttribute struct {
	InterpretableAttribute
	cond *conditionalAttribute
}

func (a *labelConditionalAttribute) Exec(frame *ExecutionFrame) ref.Val {
	tracker := trackerFromFrame(frame)
	if tracker == nil {
		return a.InterpretableAttribute.Exec(frame)
	}

	condVal := a.cond.expr.Exec(frame)
	condLabels := tracker.node(a.cond.expr.ID())
	tracker.setCondValue(a.cond.expr.ID(), condVal)

	val := a.InterpretableAttribute.Exec(frame)

	record := func(labels labelSet) {
		tracker.setNodeValue(a.ID(), val, labels)
		tracker.setNodeValue(a.cond.id, val, labels)
	}

	if types.IsUnknownOrError(val) || types.IsUnknownOrError(condVal) {
		record(nil)
		return val
	}

	var branchLabels labelSet
	switch condVal {
	case types.True:
		branchLabels = a.branchLabels(tracker, a.cond.truthy)
	case types.False:
		branchLabels = a.branchLabels(tracker, a.cond.falsy)
	default:
		record(nil)
		return val
	}
	record(unionLabelSets(branchLabels, condLabels))
	return val
}

func (a *labelConditionalAttribute) Eval(ctx Activation) ref.Val {
	return EvalActivation(ctx, a.Exec)
}

// branchLabels computes the labels of a resolved conditional branch. Absolute/ident branch
// attributes carry declared input (or local scope) labels; relative branch attributes inherit
// the labels of the expression they wrap.
func (a *labelConditionalAttribute) branchLabels(tracker *labelTracker, attr Attribute) labelSet {
	if r := resolveAttribute(attr); r.root != "" {
		if r.dynamic {
			return tracker.labelsForPath(r.root, nil)
		}
		if len(r.path) == 0 {
			if labels, isLocal := tracker.local(r.root); isLocal {
				return labels
			}
		}
		return tracker.labelsForPath(r.root, r.path)
	}
	if rel, isRel := attr.(*relativeAttribute); isRel {
		return tracker.node(rel.operand.ID())
	}
	return nil
}
