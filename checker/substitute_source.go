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

package checker

import (
	"strings"

	"cel.dev/cel-go/common"
)

// combinedSource concatenates the target source content with each fragment source content so
// that target positions are unchanged and fragment positions remain addressable after a fixed
// offset. A newline separates the segments so line numbers remain human readable.
type combinedSource struct {
	content      string
	source       common.Source
	descriptions []string
}

func newCombinedSource(target common.Source) *combinedSource {
	return &combinedSource{
		content:      target.Content(),
		source:       target,
		descriptions: []string{target.Description()},
	}
}

// append adds a fragment source to the combined content and returns the code point offset to add
// to fragment positions.
func (c *combinedSource) append(fragment common.Source) int32 {
	fragmentContent := fragment.Content()

	// The existing content plus one newline is the base offset for the fragment.
	bias := int32(len([]rune(c.content))) + 1

	var b strings.Builder
	b.WriteString(c.content)
	b.WriteByte('\n')
	b.WriteString(fragmentContent)
	c.content = b.String()

	c.descriptions = append(c.descriptions, fragment.Description())
	c.source = common.NewStringSource(c.content, combinedDescription(c.descriptions))
	return bias
}

func (c *combinedSource) lineOffsets() []int32 {
	return append([]int32{}, c.source.LineOffsets()...)
}

func combinedDescription(descriptions []string) string {
	return strings.Join(descriptions, "+")
}
