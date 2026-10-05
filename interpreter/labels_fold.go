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
	"cel.dev/cel-go/common/types/traits"
)

// labelFold drives a comprehension (or cel.bind block) with label tracking.
//
// The underlying evalFold and pooled folder retain CEL value and error semantics; this node
// establishes a per-fold tracker scope that the folder populates with the per-iteration labels
// of the range element(s) and accumulator. Iteration variables therefore propagate actual data
// dependencies and correctly shadow outer bindings for the duration of the fold. The scope is
// popped before the result expression is computed, so the result only sees the accumulator as
// recorded by the last step.
type labelFold struct {
	*evalFold
	cfg *LabelConfig
}

func (lf *labelFold) Exec(frame *ExecutionFrame) ref.Val {
	tracker := trackerFromFrame(frame)
	if tracker == nil {
		return lf.evalFold.Exec(frame)
	}

	f := newFolder(lf.evalFold, frame)
	f.labels = tracker
	tracker.pushScope()
	defer tracker.popScope()

	foldRange := lf.iterRange.Exec(frame)
	rangeLabels := tracker.node(lf.iterRange.ID())

	// Early return if the folder range is an error or unknown.
	if types.IsUnknownOrError(foldRange) {
		releaseFolder(f)
		tracker.setNodeValue(lf.id, foldRange, nil)
		return foldRange
	}

	// Short-circuit the iteration if the range is empty.
	if sizer, ok := foldRange.(traits.Sizer); ok && sizer.Size() == types.IntZero {
		res := f.evalResult()
		lf.finish(tracker, f, res, rangeLabels)
		return res
	}

	var foldable traits.Foldable
	switch r := foldRange.(type) {
	case traits.Foldable:
		foldable = r
		if lf.iterVar2 == "" {
			_, f.listV1Fold = foldRange.(traits.Lister)
		}
	case traits.Mapper:
		foldable = types.ToFoldableMap(r)
	case traits.Lister:
		if lf.iterVar2 == "" {
			f.listV1Fold = true
		}
		foldable = types.ToFoldableList(r)
	default:
		if lf.iterVar2 != "" {
			res := types.NewErrWithNodeID(lf.ID(), "unsupported comprehension range type: %T", foldRange)
			lf.finish(tracker, f, res, rangeLabels)
			return res
		}
	}

	var res ref.Val
	if foldable != nil {
		foldable.Fold(f)
		res = f.evalResult()
	} else {
		if !foldRange.Type().HasTrait(traits.IterableType) {
			res = types.ValOrErr(foldRange, "got '%T', expected iterable type", foldRange)
			lf.finish(tracker, f, res, rangeLabels)
			return res
		}
		res = f.foldIterable(foldRange.(traits.Iterable))
	}

	lf.finish(tracker, f, res, rangeLabels)
	return res
}

// finish tears down the iteration scope, records the fold result labels and releases the folder.
func (lf *labelFold) finish(tracker *labelTracker, f *folder, res ref.Val, rangeLabels labelSet) {
	labels := tracker.node(lf.result.ID())
	releaseFolder(f)
	if types.IsUnknownOrError(res) {
		labels = nil
	} else {
		// Whether the comprehension body executes at all, and how many iterations contribute
		// to the result, is controlled by the range value; the range labels are therefore a
		// control dependency of the result in addition to the data labels propagated through
		// the accumulator and result expressions.
		labels = unionLabelSets(labels, rangeLabels)
	}
	tracker.setNodeValue(lf.id, res, labels)
}
