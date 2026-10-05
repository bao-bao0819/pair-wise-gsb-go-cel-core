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

import "cel.dev/cel-go/common/types/ref"

// decorateLabeledCall installs the pre-body policy hook on the concrete call node and returns
// a wrapper which records the result labels after the call. The hook runs strictly after the
// arguments have been evaluated (preserving short-circuit and error semantics) and strictly
// before the function body, so a rejected sink observes zero body invocations.
func decorateLabeledCall(call InterpretableCall, prog *LabelProgram) (InterpretableV2, error) {
	function := call.Function()
	overload := call.OverloadID()
	argNodes := call.Args()
	argLabels := func(frame *ExecutionFrame) labelSet {
		st := frame.labelState()
		if st == nil {
			return nil
		}
		return st.argLabels(argNodes)
	}
	hook := func(frame *ExecutionFrame, fn, ov string, args []ref.Val) ref.Val {
		return preCallFor(prog, function, overload, func() labelSet { return argLabels(frame) })(frame, fn, ov, args)
	}
	switch c := call.(type) {
	case *evalEq:
		return &labeledEq{evalEq: c, prog: prog}, nil
	case *evalNe:
		return &labeledNe{evalNe: c, prog: prog}, nil
	case *evalZeroArity:
		c.preCall = hook
	case *evalUnary:
		c.preCall = hook
	case *evalBinary:
		c.preCall = hook
	case *evalVarArgs:
		c.preCall = hook
	case *evalLateBoundFunc:
		// Late-bound overloads resolve at evaluation time; the hook receives the concrete
		// overload id from the call itself.
		c.preCall = hook
	}
	return &labeledCall{InterpretableCall: call, prog: prog, function: function, overload: overload}, nil
}

// labeledCall records result labels after an ordinary call completes. The value and error
// semantics are produced by the wrapped node unchanged.
type labeledCall struct {
	InterpretableCall
	prog     *LabelProgram
	function string
	overload string
}

func (c *labeledCall) Exec(frame *ExecutionFrame) ref.Val {
	val := c.InterpretableCall.Exec(frame)
	if st := frame.labelState(); st != nil {
		labels := resultLabelsFor(c.prog, c.function, c.overload, st.argLabels(c.Args()))
		st.setLabels(c.ID(), labels)
	}
	return val
}

func (c *labeledCall) Eval(vars Activation) ref.Val {
	return EvalActivation(vars, c.Exec)
}

// labeledEq / labeledNe handle the specialized equality nodes which have no preCall field.
// Equality is a built-in propagator, so only result labels need recording.
type labeledEq struct {
	*evalEq
	prog *LabelProgram
}

func (e *labeledEq) Eval(vars Activation) ref.Val {
	return EvalActivation(vars, e.Exec)
}

func (e *labeledEq) Exec(frame *ExecutionFrame) ref.Val {
	val := e.evalEq.Exec(frame)
	if st := frame.labelState(); st != nil {
		st.setLabels(e.ID(), st.labelsOf(e.lhs.ID()).add(st.labelsOf(e.rhs.ID())))
	}
	return val
}

type labeledNe struct {
	*evalNe
	prog *LabelProgram
}

func (e *labeledNe) Eval(vars Activation) ref.Val {
	return EvalActivation(vars, e.Exec)
}

func (e *labeledNe) Exec(frame *ExecutionFrame) ref.Val {
	val := e.evalNe.Exec(frame)
	if st := frame.labelState(); st != nil {
		st.setLabels(e.ID(), st.labelsOf(e.lhs.ID()).add(st.labelsOf(e.rhs.ID())))
	}
	return val
}
