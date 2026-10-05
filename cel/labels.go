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
	"cel.dev/cel-go/common/stdlib"
	"cel.dev/cel-go/interpreter"
)

// Label identifies a security classification tag attached to an input variable or to a field
// beneath one. See interpreter.Label for the path semantics.
type Label = interpreter.Label

// LabelKind classifies how a registered function overload interacts with labels.
type LabelKind = interpreter.LabelKind

// Function overload label propagation rules.
const (
	// LabelPropagate propagates the union of argument labels to the function result.
	LabelPropagate = interpreter.LabelPropagate
	// LabelSink marks an overload as a restricted sink gated by the host policy.
	LabelSink = interpreter.LabelSink
	// LabelSanitize marks an overload as an explicit de-classification function.
	LabelSanitize = interpreter.LabelSanitize
)

// SinkCall describes a sink invocation at the point of the host policy decision.
type SinkCall = interpreter.SinkCall

// LabelSinkPolicy decides whether a restricted sink call is permitted. The policy runs before
// the sink body; a non-nil error rejects the call without invoking the body.
type LabelSinkPolicy = interpreter.LabelSinkPolicy

// DataLabelOption configures explicit data label tracking for a program.
type DataLabelOption func(*dataLabelConfig)

// dataLabelConfig is the cel-facing mirror of interpreter.LabelConfig.
type dataLabelConfig struct {
	inputLabels map[string][]Label
	strict      bool
	sinkPolicy  LabelSinkPolicy
	overloads   map[string]LabelKind
}

// DataLabels enables explicit data label (taint) tracking.
//
// In this mode every evaluated value retains CEL's ordinary value and error semantics and, in
// addition, carries a set of labels derived from the labeled inputs that influenced it either
// through data flow or through a control condition that selected it (e.g. a ternary branch).
// Labels are read from EvalDetails.Labels() after evaluation.
//
// Label state is scoped to a single Eval call: it is never stored on the shared Program or on
// input objects, so the same Program may be evaluated concurrently against differently labeled
// inputs.
//
// Scope and limitations: tracking is defined for the supported CEL expression surface
// (scalar/arithmetic/string operators, list/map/object construction, field selection and
// indexing, conditionals, logical operators, and comprehensions including cel.bind blocks).
// Label propagation is a value-level data/control dependency analysis; it does not model, and
// makes no claim about, timing channels, resource-usage side channels, or other non-value
// channels by which labeled inputs might influence an observer.
func DataLabels(opts ...DataLabelOption) ProgramOption {
	return func(p *prog) (*prog, error) {
		cfg := &dataLabelConfig{
			inputLabels: map[string][]Label{},
			overloads:   map[string]LabelKind{},
		}
		for _, opt := range opts {
			opt(cfg)
		}
		internal := &interpreter.LabelConfig{
			InputLabels: cfg.inputLabels,
			Strict:      cfg.strict,
			SinkPolicy:  cfg.sinkPolicy,
			Overloads:   cfg.overloads,
		}
		p.labelConfig = internal
		return p, nil
	}
}

// LabeledInput declares labels for a root input variable and optional field paths beneath it.
//
// Labels supplied for an ancestor path automatically taint descendant field selections. Call
// DataLabels repeatedly with LabeledInput values to label multiple variables.
func LabeledInput(variable string, labels ...Label) DataLabelOption {
	return func(cfg *dataLabelConfig) {
		cfg.inputLabels[variable] = append(cfg.inputLabels[variable], labels...)
	}
}

// StrictLabelMode rejects extension function overloads with no declared label propagation rule
// at program construction time. Built-in CEL operators always propagate labels.
func StrictLabelMode() DataLabelOption {
	return func(cfg *dataLabelConfig) { cfg.strict = true }
}

// SinkPolicy installs the host policy consulted before every LabelSink overload runs.
func SinkPolicy(policy LabelSinkPolicy) DataLabelOption {
	return func(cfg *dataLabelConfig) { cfg.sinkPolicy = policy }
}

// SinkOverload registers an extension overload id as a restricted sink.
func SinkOverload(overloadID string) DataLabelOption {
	return func(cfg *dataLabelConfig) { cfg.overloads[overloadID] = LabelSink }
}

// SanitizeOverload registers an extension overload id as an explicit de-classification
// function; its results carry no labels.
func SanitizeOverload(overloadID string) DataLabelOption {
	return func(cfg *dataLabelConfig) { cfg.overloads[overloadID] = LabelSanitize }
}

// PropagateOverload explicitly registers an extension overload id as propagating argument
// labels. This is primarily useful in StrictLabelMode.
func PropagateOverload(overloadID string) DataLabelOption {
	return func(cfg *dataLabelConfig) { cfg.overloads[overloadID] = LabelPropagate }
}

// SinkFunction registers all overloads of a function name as restricted sinks.
func SinkFunction(function string) DataLabelOption {
	return func(cfg *dataLabelConfig) { cfg.overloads[function] = LabelSink }
}

// SanitizeFunction registers all overloads of a function name as de-classification functions.
func SanitizeFunction(function string) DataLabelOption {
	return func(cfg *dataLabelConfig) { cfg.overloads[function] = LabelSanitize }
}

// PropagateFunction explicitly registers a function name as propagating argument labels.
func PropagateFunction(function string) DataLabelOption {
	return func(cfg *dataLabelConfig) { cfg.overloads[function] = LabelPropagate }
}

// builtinOverloadIDs returns the overload ids implemented by the CEL standard library.
func builtinOverloadIDs() map[string]bool {
	out := map[string]bool{}
	for _, fn := range stdlib.Functions() {
		bindings, err := fn.Bindings()
		if err != nil {
			continue
		}
		for _, b := range bindings {
			out[b.Operator] = true
		}
	}
	return out
}
