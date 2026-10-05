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
	"cel.dev/cel-go/common/types/ref"
)

// labeledRoot wraps the top-level interpretable to allocate and tear down the tracking state
// for a single evaluation. Because a fresh state is attached to the execution frame on every
// Exec call, evaluating the same Program twice with different inputs/labels is fully
// independent: no state survives on the Program, its plan, or the caller's input objects.
type labeledRoot struct {
	InterpretableV2
	prog *LabelProgram
}

func (r *labeledRoot) Exec(frame *ExecutionFrame) ref.Val {
	source := findLabelSource(frame)
	st := newLabelEvalState(r.prog, source)
	frame.labels = st
	val := r.InterpretableV2.Exec(frame)
	st.resultLabels = st.labelsOf(r.InterpretableV2.ID())
	if sink, ok := source.(labelResultSink); ok {
		sink.setResultLabels(st.resultLabels)
	}
	frame.labels = nil
	return val
}

func (r *labeledRoot) Eval(activation Activation) ref.Val {
	return EvalActivation(activation, r.Exec)
}

// LabeledActivation wraps an input activation with per-evaluation input labels.
type LabeledActivation struct {
	Activation
	labels LabelInput
	// result is overwritten once per evaluation and never escapes into the shared Program.
	result labelSet
}

// AsLabels implements the labelSource interface.
func (a *LabeledActivation) AsLabels(path string) labelSet {
	return a.labels.AsLabels(path)
}

func (a *LabeledActivation) setResultLabels(labels labelSet) {
	a.result = labels
}

// labelResultSink is implemented by per-evaluation activations which capture the root labels.
type labelResultSink interface {
	setResultLabels(labelSet)
}

// NewLabeledActivation binds input labels to a map or Activation input.
func NewLabeledActivation(input any, labels LabelInput) (*LabeledActivation, error) {
	a, err := NewActivation(input)
	if err != nil {
		return nil, err
	}
	return &LabeledActivation{Activation: a, labels: labels}, nil
}

// ResultLabels returns the labels carried by the result of the most recent evaluation run
// against the activation, or an empty set if the program did not track labels.
func ResultLabels(activation *LabeledActivation) Labels {
	if activation == nil {
		return labelSet(nil)
	}
	return activation.result
}
