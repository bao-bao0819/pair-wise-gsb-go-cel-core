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
	"context"

	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"
)

// Data-label (taint) evaluation mode.
//
// The mode is explicitly opt-in via LabelTracking(). Input labels are supplied per evaluation
// as variable or dotted field paths. The ordinary CEL value and error semantics are unchanged;
// the result additionally carries the labels which influenced it through both data and control
// dependence. Functions are registered as ordinary propagators, restricted sinks (whose policy
// runs before the function body), or explicit declassification points. Tracking state belongs
// to a single evaluation and is never retained by the Program or the caller's input objects.
//
// The tracking covers the data/control dependencies expressible within supported CEL
// expressions; it does not model timing or other side channels.

// LabelProgram configures the function registry and sink policy for labeled evaluation.
type LabelProgram = interpreter.LabelProgram

// LabelProgramOption configures a LabelProgram.
type LabelProgramOption = interpreter.LabelProgramOption

// Labels is the set of labels carried by an evaluated result.
type Labels = interpreter.Labels

// SinkCall describes a sink invocation presented to a SinkPolicy.
type SinkCall = interpreter.SinkCall

// SinkPolicy decides whether a restricted sink call may proceed.
type SinkPolicy = interpreter.SinkPolicy

// NewLabelProgram builds the function registry used by LabelTracking.
func NewLabelProgram(opts ...LabelProgramOption) (*LabelProgram, error) {
	return interpreter.NewLabelProgram(opts...)
}

// StrictLabelMode rejects extension functions with no declared propagation rule.
func StrictLabelMode() LabelProgramOption {
	return interpreter.StrictLabelMode()
}

// PropagatingFunction registers ordinary label-propagating functions or overloads.
func PropagatingFunction(overloadOrFunction string, more ...string) LabelProgramOption {
	return interpreter.PropagatingFunction(overloadOrFunction, more...)
}

// SanitizingFunction registers explicit declassification functions or overloads.
func SanitizingFunction(overloadOrFunction string, more ...string) LabelProgramOption {
	return interpreter.SanitizingFunction(overloadOrFunction, more...)
}

// SinkFunction registers restricted sinks guarded by a policy evaluated before the body runs.
func SinkFunction(policy SinkPolicy, overloadOrFunction string, more ...string) LabelProgramOption {
	return interpreter.SinkFunction(policy, overloadOrFunction, more...)
}

// BuiltinFunction marks extension-provided functions as built-ins for strict-mode purposes.
func BuiltinFunction(overloadOrFunction string, more ...string) LabelProgramOption {
	return interpreter.BuiltinFunction(overloadOrFunction, more...)
}

// LabelTracking enables data label tracking for a Program using the supplied function registry.
func LabelTracking(labelProg *LabelProgram) ProgramOption {
	return func(p *prog) (*prog, error) {
		p.plannerOptions = append(p.plannerOptions, interpreter.LabelTracking(labelProg))
		return p, nil
	}
}

// LabelInput declares the labels attached to a single evaluation's inputs, keyed by variable
// or dotted field path.
type LabelInput = interpreter.LabelInput

// LabeledResult is the outcome of a labeled evaluation.
type LabeledResult struct {
	// Value is the ordinary CEL result.
	Value ref.Val
	// Details holds the ordinary evaluation details, if any.
	Details *EvalDetails
	// Labels are the labels carried by the result.
	Labels Labels
}

// EvalLabeled evaluates the program with per-evaluation input labels.
func EvalLabeled(prog Program, input any, labels LabelInput) (LabeledResult, error) {
	activation, err := interpreter.NewLabeledActivation(input, labels)
	if err != nil {
		return LabeledResult{}, err
	}
	val, details, err := prog.Eval(activation)
	return LabeledResult{
		Value:   val,
		Details: details,
		Labels:  interpreter.ResultLabels(activation),
	}, err
}

// ContextEvalLabeled evaluates the program with labels under the given context.
func ContextEvalLabeled(ctx context.Context, prog Program, input any, labels LabelInput) (LabeledResult, error) {
	activation, err := interpreter.NewLabeledActivation(input, labels)
	if err != nil {
		return LabeledResult{}, err
	}
	val, details, err := prog.ContextEval(ctx, activation)
	return LabeledResult{
		Value:   val,
		Details: details,
		Labels:  interpreter.ResultLabels(activation),
	}, err
}
