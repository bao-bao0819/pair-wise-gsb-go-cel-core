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
	"sort"

	"cel.dev/cel-go/common/types/ref"
)

// This file implements an explicitly opt-in data-label (taint) evaluation mode.
//
// Input labels are supplied per evaluation, keyed by variable or dotted field path. During a
// labeled evaluation every expression node carries, in addition to its ordinary CEL value and
// error semantics, the set of labels that influenced it:
//
//   - Data dependence: labels flow through scalar operations, list/map construction, field
//     selection, ternary conditions, and comprehensions from the operands actually used.
//   - Control dependence: the labels of any condition whose outcome selected the current result
//     are also attached (e.g. the condition of a ternary, a short-circuiting logical operator,
//     or a comprehension filter).
//
// Functions are registered by overload identity as ordinary propagators, restricted sinks, or
// explicit declassification functions. Sink policies are consulted before the function body
// runs, so a rejected sink is never invoked. Extension functions without a declared propagation
// rule are rejected in strict mode.
//
// Tracking state is allocated per evaluation on the execution frame and is never stored on the
// shared Program, its plan, or the caller's input objects. Two evaluations of the same Program
// with different labels are fully independent.
//
// The tracking covers the data and control dependencies expressible within supported CEL
// expressions; it does not attempt to cover timing or other side channels.

// Labels is an immutable set of data labels carried by an evaluated result.
type Labels interface {
	// Contains reports whether the label set contains the given label.
	Contains(label string) bool

	// Slice returns a sorted slice of the labels in the set.
	Slice() []string

	// Size returns the number of distinct labels.
	Size() int
}

// labelSet is the internal mutable label set. The zero value is an empty set.
type labelSet map[string]struct{}

func newLabelSet(labels ...string) labelSet {
	if len(labels) == 0 {
		return nil
	}
	ls := make(labelSet, len(labels))
	for _, l := range labels {
		ls[l] = struct{}{}
	}
	return ls
}

func (ls labelSet) add(other labelSet) labelSet {
	if len(other) == 0 {
		return ls
	}
	if ls == nil {
		ls = make(labelSet, len(other))
	}
	for l := range other {
		ls[l] = struct{}{}
	}
	return ls
}

func (ls labelSet) addLabels(labels ...string) labelSet {
	if len(labels) == 0 {
		return ls
	}
	if ls == nil {
		ls = make(labelSet, len(labels))
	}
	for _, l := range labels {
		ls[l] = struct{}{}
	}
	return ls
}

// Contains implements the Labels interface method.
func (ls labelSet) Contains(label string) bool {
	_, ok := ls[label]
	return ok
}

// Size implements the Labels interface method.
func (ls labelSet) Size() int {
	return len(ls)
}

// Slice implements the Labels interface method.
func (ls labelSet) Slice() []string {
	if len(ls) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(ls))
	for l := range ls {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// LabelInput declares the labels attached to a single evaluation's inputs.
//
// Keys are either a bare variable name or a dotted field/index path rooted at a variable,
// e.g. "user" or "user.profile.email". A label on a parent path labels every selection beneath
// it; a label on a child path labels only that exact selection.
type LabelInput map[string][]string

// AsLabels returns the labels declared for the given path.
func (in LabelInput) AsLabels(path string) labelSet {
	return newLabelSet(in[path]...)
}

// labelSource is implemented by Activations which supply per-evaluation input labels.
type labelSource interface {
	AsLabels(path string) labelSet
}

// findLabelSource walks the activation chain (hierarchical activations and frame scopes) to
// locate the per-evaluation label input.
func findLabelSource(a Activation) labelSource {
	for a != nil {
		if src, ok := a.(labelSource); ok {
			return src
		}
		if w, ok := a.(activationWrapper); ok {
			a = w.Unwrap()
			continue
		}
		a = a.Parent()
	}
	return nil
}

// FunctionClass declares how a registered function participates in label propagation.
type FunctionClass int

const (
	// FunctionPropagates is the default: the result carries the union of its arguments' labels.
	FunctionPropagates FunctionClass = iota
	// FunctionSanitizes explicitly removes labels: the result is unlabeled regardless of input.
	FunctionSanitizes
	// FunctionSink sends data outside the evaluation; its policy runs before the body and a
	// rejected policy prevents the body from being invoked.
	FunctionSink
)

// SinkCall describes a function invocation presented to a SinkPolicy immediately before the
// function body would run.
type SinkCall struct {
	// Function is the function name as it appears at the call site.
	Function string
	// OverloadID is the resolved overload identity for the invocation.
	OverloadID string
	// Args are the evaluated argument values.
	Args []ref.Val
	// Labels are the labels carried by the evaluated arguments.
	Labels Labels
}

// SinkPolicy decides whether a restricted sink call may proceed. Returning a non-nil error
// rejects the call: the error becomes the node's CEL evaluation error and the function body is
// not invoked.
type SinkPolicy func(call SinkCall) error

// LabelProgram is a Program-level function registry and policy configuration for labeled
// evaluation. It is safe to build once and reuse across Programs and evaluations; it never
// stores per-evaluation state.
type LabelProgram struct {
	// classByOverload maps overload id, or "<function>" as a function-wide default, to class.
	classByOverload map[string]FunctionClass
	// builtins names function/overload ids that are CEL standard-library functions and
	// therefore need no declaration even in strict mode.
	builtins map[string]struct{}
	policies map[string]SinkPolicy
	strict   bool
}

// LabelProgramOption configures a LabelProgram.
type LabelProgramOption func(*LabelProgram) error

// StrictLabelMode rejects calls to extension functions which have no declared propagation
// rule. Without it, undeclared extension functions default to propagating argument labels.
func StrictLabelMode() LabelProgramOption {
	return func(p *LabelProgram) error {
		p.strict = true
		return nil
	}
}

// PropagatingFunction declares functions, or specific overloads, as ordinary label
// propagators (their result carries the union of their arguments' labels).
func PropagatingFunction(overloadOrFunction string, more ...string) LabelProgramOption {
	return registerClass(FunctionPropagates, append([]string{overloadOrFunction}, more...)...)
}

// SanitizingFunction declares functions, or specific overloads, as explicit declassification
// points whose result carries no labels.
func SanitizingFunction(overloadOrFunction string, more ...string) LabelProgramOption {
	return registerClass(FunctionSanitizes, append([]string{overloadOrFunction}, more...)...)
}

// SinkFunction declares functions, or specific overloads, as restricted sinks guarded by the
// given policy. The policy is consulted before the function body executes.
func SinkFunction(policy SinkPolicy, overloadOrFunction string, more ...string) LabelProgramOption {
	names := append([]string{overloadOrFunction}, more...)
	return func(p *LabelProgram) error {
		if policy == nil {
			return fmt.Errorf("sink function %v requires a non-nil policy", names)
		}
		for _, name := range names {
			p.classByOverload[name] = FunctionSink
			p.policies[name] = policy
		}
		return nil
	}
}

// BuiltinFunction marks functions, or specific overloads, as CEL built-ins that require no
// propagation declaration even in strict mode. Standard-library functions are recognized
// automatically; this is an escape hatch for libraries vendored as extensions.
func BuiltinFunction(overloadOrFunction string, more ...string) LabelProgramOption {
	names := append([]string{overloadOrFunction}, more...)
	return func(p *LabelProgram) error {
		for _, name := range names {
			p.builtins[name] = struct{}{}
		}
		return nil
	}
}

func registerClass(class FunctionClass, names ...string) LabelProgramOption {
	return func(p *LabelProgram) error {
		for _, name := range names {
			p.classByOverload[name] = class
		}
		return nil
	}
}

// NewLabelProgram creates a function registry for labeled evaluation from the supplied options.
func NewLabelProgram(opts ...LabelProgramOption) (*LabelProgram, error) {
	p := &LabelProgram{
		classByOverload: map[string]FunctionClass{},
		builtins:        map[string]struct{}{},
		policies:        map[string]SinkPolicy{},
	}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// withBuiltinBaseline seeds the builtin set from the CEL standard library's function names and
// overload ids, so standard-library calls require no propagation declaration in strict mode.
func (p *LabelProgram) withBuiltinBaseline(builtinNames map[string]struct{}) *LabelProgram {
	for name := range builtinNames {
		p.builtins[name] = struct{}{}
	}
	return p
}

// classFor resolves the declared class for a call, preferring the overload id and falling back
// to the function-wide default.
func (p *LabelProgram) classFor(function, overload string) (FunctionClass, bool) {
	if p == nil {
		return FunctionPropagates, false
	}
	if class, found := p.classByOverload[overload]; found {
		return class, true
	}
	class, found := p.classByOverload[function]
	return class, found
}

func (p *LabelProgram) isBuiltin(function, overload string) bool {
	if _, found := p.builtins[overload]; found {
		return true
	}
	_, found := p.builtins[function]
	return found
}

// labelEvalState holds all mutable tracking state for a single evaluation.
type labelEvalState struct {
	prog   *LabelProgram
	source labelSource
	// nodeLabels records the result labels for nodes executed during the pass. Within
	// comprehensions a node can execute many times; unionInto accumulates across iterations.
	nodeLabels map[int64]labelSet
	// out holds the labels produced by the most recent execution of each node. It is
	// overwritten rather than accumulated, so a parent always reads the exact labels of the
	// child invocation it just evaluated, even inside a comprehension.
	out map[int64]labelSet
	// localLabels holds the labels of comprehension iteration/accumulator variables for the
	// currently active fold scopes, as a stack keyed by variable name.
	localScopes []map[string]labelSet
	// resultLabels receives the root node's labels at the end of the pass.
	resultLabels labelSet
}

func newLabelEvalState(prog *LabelProgram, source labelSource) *labelEvalState {
	return &labelEvalState{
		prog:       prog,
		source:     source,
		nodeLabels: map[int64]labelSet{},
		out:        map[int64]labelSet{},
	}
}

// labelsOf returns the labels currently recorded for a child node.
func (st *labelEvalState) labelsOf(id int64) labelSet {
	return st.out[id]
}

// setLabels replaces the recorded labels for a node.
func (st *labelEvalState) setLabels(id int64, labels labelSet) {
	st.out[id] = labels
	if len(labels) == 0 {
		delete(st.nodeLabels, id)
		return
	}
	st.nodeLabels[id] = labels
}

// unionInto adds labels to a node's recorded set; used across comprehension iterations so that
// the surviving result reflects every iteration which affected it.
func (st *labelEvalState) unionInto(id int64, labels labelSet) {
	if len(labels) == 0 {
		return
	}
	st.nodeLabels[id] = st.nodeLabels[id].add(labels)
}

// pushLocalScope enters a comprehension scope.
func (st *labelEvalState) pushLocalScope() {
	st.localScopes = append(st.localScopes, map[string]labelSet{})
}

// popLocalScope leaves a comprehension scope, dropping its iteration variables.
func (st *labelEvalState) popLocalScope() {
	if len(st.localScopes) > 0 {
		st.localScopes = st.localScopes[:len(st.localScopes)-1]
	}
}

// setLocal records a local variable's labels in the innermost scope.
func (st *labelEvalState) setLocal(name string, labels labelSet) {
	if len(st.localScopes) == 0 || name == "" {
		return
	}
	st.localScopes[len(st.localScopes)-1][name] = labels
}

// localLabelsFor resolves a local variable's labels from the innermost scope outward,
// implementing comprehension variable shadowing.
func (st *labelEvalState) localLabelsFor(name string) labelSet {
	for i := len(st.localScopes) - 1; i >= 0; i-- {
		if labels, found := st.localScopes[i][name]; found {
			return labels
		}
	}
	return nil
}

// resolveInputLabels finds labels declared for an input variable or dotted path.
func (st *labelEvalState) resolveInputLabels(path string) labelSet {
	if st.source == nil {
		return nil
	}
	return st.source.AsLabels(path)
}

// argLabels unions the labels of the given argument nodes.
func (st *labelEvalState) argLabels(args []InterpretableV2) labelSet {
	var ls labelSet
	for _, a := range args {
		ls = ls.add(st.labelsOf(a.ID()))
	}
	return ls
}
