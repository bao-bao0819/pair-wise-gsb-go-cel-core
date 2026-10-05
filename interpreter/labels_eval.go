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

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// LabelTracking configures an interpreter plan to evaluate with data label tracking enabled.
//
// The supplied configuration is shared across evaluations of the returned plan, but all label
// state itself is allocated fresh for each evaluation and is discarded when evaluation ends.
//
// builtinOverloads is the set of overload ids implemented by the CEL runtime itself; these
// overloads always propagate argument labels and are never subject to strict-mode rejection.
func LabelTracking(cfg *LabelConfig, builtinOverloads map[string]bool) PlannerOption {
	return func(p *planner) (*planner, error) {
		if cfg == nil {
			return nil, fmt.Errorf("label tracking requires a non-nil LabelConfig")
		}
		p.decorators = append(p.decorators, decLabelTracking(cfg, p.disp, builtinOverloads))
		return p, nil
	}
}

// decLabelTracking wraps every planned node so that node results carry a label set alongside
// their ordinary CEL value and error semantics.
func decLabelTracking(cfg *LabelConfig, disp Dispatcher, builtinOverloads map[string]bool) InterpretableDecoratorV2 {
	extOverloads := extensionOverloadSet(disp, builtinOverloads)
	return func(i InterpretableV2) (InterpretableV2, error) {
		switch node := i.(type) {
		case InterpretableConst:
			return &labelConst{InterpretableConst: node}, nil
		case *evalOr:
			return &labelLogical{id: node.id, terms: node.terms, isOr: true}, nil
		case *evalAnd:
			return &labelLogical{id: node.id, terms: node.terms, isOr: false}, nil
		case *evalFold:
			return &labelFold{evalFold: node, cfg: cfg}, nil
		case InterpretableCall:
			if err := checkExtensionOverload(cfg, node, extOverloads); err != nil {
				return nil, err
			}
			return &labelCall{InterpretableCall: node, cfg: cfg}, nil
		case InterpretableConstructor:
			return &labelConstructor{InterpretableConstructor: node}, nil
		case InterpretableAttribute:
			return newLabelAttribute(node, cfg)
		default:
			return &labelGeneric{InterpretableV2: node}, nil
		}
	}
}

// extensionOverloadSet returns the overload ids introduced by extension dispatchers, i.e.
// those not present in the built-in set.
func extensionOverloadSet(disp Dispatcher, builtin map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, id := range disp.OverloadIds() {
		if !builtin[id] {
			out[id] = true
		}
	}
	return out
}

func checkExtensionOverload(cfg *LabelConfig, call InterpretableCall, ext map[string]bool) error {
	if !cfg.Strict {
		return nil
	}
	overload := call.OverloadID()
	if overload == "" || !ext[overload] {
		return nil
	}
	if _, ok := cfg.overloadKind(call.Function(), overload); ok {
		return nil
	}
	return fmt.Errorf("strict label tracking: extension overload %q (function %q) has no declared label propagation rule",
		overload, call.Function())
}

// trackerFromFrame returns the per-evaluation label tracker, or nil if tracking is not active.
func trackerFromFrame(frame *ExecutionFrame) *labelTracker {
	if frame == nil || frame.ctx == nil || frame.ctx.labels == nil {
		return nil
	}
	return frame.ctx.labels
}

// labelConst produces an empty label set; literals carry no input labels.
type labelConst struct {
	InterpretableConst
}

func (c *labelConst) Exec(frame *ExecutionFrame) ref.Val {
	val := c.InterpretableConst.Exec(frame)
	if tracker := trackerFromFrame(frame); tracker != nil {
		tracker.setNodeValue(c.ID(), val, nil)
	}
	return val
}

// labelGeneric covers nodes without specialized label handling.
type labelGeneric struct {
	InterpretableV2
}

func (g *labelGeneric) Exec(frame *ExecutionFrame) ref.Val {
	val := g.InterpretableV2.Exec(frame)
	if tracker := trackerFromFrame(frame); tracker != nil {
		tracker.setNodeValue(g.ID(), val, collectChildLabels(tracker, g.InterpretableV2))
	}
	return val
}

// labelConstructor unions the labels of all constructed elements, keys and field values.
type labelConstructor struct {
	InterpretableConstructor
}

func (c *labelConstructor) Exec(frame *ExecutionFrame) ref.Val {
	val := c.InterpretableConstructor.Exec(frame)
	if tracker := trackerFromFrame(frame); tracker != nil {
		var sets []labelSet
		for _, init := range c.InitVals() {
			if labels := tracker.node(init.ID()); labels != nil {
				sets = append(sets, labels)
			}
		}
		tracker.setNodeValue(c.ID(), val, unionLabelSets(sets...))
	}
	return val
}

// labelLogical drives non-strict logical and/or evaluation, recording the control-condition
// labels that select the short-circuited or final result.
type labelLogical struct {
	id    int64
	terms []InterpretableV2
	isOr  bool
}

func (l *labelLogical) ID() int64 { return l.id }

func (l *labelLogical) Eval(ctx Activation) ref.Val {
	return EvalActivation(ctx, l.Exec)
}

func (l *labelLogical) Exec(frame *ExecutionFrame) ref.Val {
	tracker := trackerFromFrame(frame)
	if tracker == nil {
		if l.isOr {
			return (&evalOr{id: l.id, terms: l.terms}).Exec(frame)
		}
		return (&evalAnd{id: l.id, terms: l.terms}).Exec(frame)
	}

	var conditionSets []labelSet
	var err ref.Val
	var unk *types.Unknown

	for _, term := range l.terms {
		val := term.Exec(frame)
		labels := tracker.node(term.ID())
		boolVal, ok := val.(types.Bool)
		if ok {
			shortCircuit := (l.isOr && boolVal == types.True) || (!l.isOr && boolVal == types.False)
			if shortCircuit {
				tracker.setNodeValue(l.id, boolVal, unionLabelSets(append(conditionSets, labels)...))
				return boolVal
			}
			conditionSets = append(conditionSets, labels)
			continue
		}
		if unkVal, isUnk := val.(*types.Unknown); isUnk {
			if unk == nil {
				unk = unkVal
			} else {
				unk = types.MergeUnknowns(unk, unkVal)
			}
			continue
		}
		if err == nil {
			if isError(val) {
				err = val
			} else {
				err = types.MaybeNoSuchOverloadErr(val)
			}
			err = types.LabelErrNode(l.id, err)
		}
	}
	if unk != nil {
		tracker.setNodeValue(l.id, unk, nil)
		return unk
	}
	if err != nil {
		tracker.setNodeValue(l.id, err, nil)
		return err
	}
	result := types.False
	if !l.isOr {
		result = types.True
	}
	tracker.setNodeValue(l.id, result, unionLabelSets(conditionSets...))
	return result
}

// labelCall wraps an ordinary function or operator call. The wrapped node evaluates the
// arguments exactly once; this wrapper records the resulting label set and enforces the sink
// gate that the core node raises before invoking the body.
type labelCall struct {
	InterpretableCall
	cfg *LabelConfig
}

func (c *labelCall) Exec(frame *ExecutionFrame) ref.Val {
	tracker := trackerFromFrame(frame)
	if tracker == nil {
		return c.InterpretableCall.Exec(frame)
	}
	function := c.Function()
	overload := c.OverloadID()

	val := c.InterpretableCall.Exec(frame)
	if types.IsUnknownOrError(val) {
		tracker.setNodeValue(c.ID(), val, nil)
		return val
	}

	kind, declared := c.cfg.overloadKind(function, overload)
	if declared && kind == LabelSanitize {
		tracker.setNodeValue(c.ID(), val, nil)
		return val
	}

	var sets []labelSet
	for _, arg := range c.Args() {
		if labels := tracker.node(arg.ID()); labels != nil {
			sets = append(sets, labels)
		}
	}
	tracker.setNodeValue(c.ID(), val, unionLabelSets(sets...))
	return val
}

// Eval preserves the Activation-based entry point.
func (c *labelCall) Eval(ctx Activation) ref.Val {
	return EvalActivation(ctx, c.Exec)
}

// collectChildLabels unions the labels recorded for the direct children of a generic node.
func collectChildLabels(tracker *labelTracker, node InterpretableV2) labelSet {
	switch n := node.(type) {
	case InterpretableCall:
		var sets []labelSet
		for _, arg := range n.Args() {
			if labels := tracker.node(arg.ID()); labels != nil {
				sets = append(sets, labels)
			}
		}
		return unionLabelSets(sets...)
	case InterpretableConstructor:
		var sets []labelSet
		for _, init := range n.InitVals() {
			if labels := tracker.node(init.ID()); labels != nil {
				sets = append(sets, labels)
			}
		}
		return unionLabelSets(sets...)
	default:
		return nil
	}
}
