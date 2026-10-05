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

// gateLabels is invoked by a core call node immediately after its arguments have been
// evaluated and immediately before the function body is invoked. It enforces the host sink
// policy. A non-nil return is a CEL error that replaces the call result; in that case the
// function body is never executed.
//
// argLabelNodes are the argument expression nodes in argument order and are used to read the
// per-argument label sets recorded during this evaluation.
func gateLabels(frame *ExecutionFrame, id int64, function, overload string,
	argVals []ref.Val, argLabelNodes []InterpretableV2) ref.Val {

	tracker := trackerFromFrame(frame)
	if tracker == nil {
		return nil
	}
	kind, declared := tracker.cfg.overloadKind(function, overload)
	if !declared || kind != LabelSink {
		return nil
	}
	policy := tracker.cfg.SinkPolicy
	if policy == nil {
		return nil
	}
	argLabels := make([][]Label, len(argVals))
	for i := range argVals {
		var set labelSet
		if i < len(argLabelNodes) && argLabelNodes[i] != nil {
			set = tracker.node(argLabelNodes[i].ID())
		}
		argLabels[i] = set
	}
	if callErr := policy(SinkCall{
		Function:       function,
		OverloadID:     overload,
		ArgumentLabels: argLabels,
	}); callErr != nil {
		return types.NewErrWithNodeID(id, "%v", callErr)
	}
	return nil
}
