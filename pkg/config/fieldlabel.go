// Copyright © 2026 Nik Ogura <nik.ogura@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"fmt"
	"sort"
	"strings"
)

// fieldLabelPrefix is put in front of every field label's name, so it obeys
// the rule every object label does: an xp_ label came from the exporter, an
// unprefixed one came from scrape configuration, and the two never collide.
const fieldLabelPrefix = "xp_"

// FieldLabel names one string field of one kind to carry as a label on every
// per-object series of that kind.
//
// It is written exactly like a FieldMetric, group/Kind:dotted.path, with the
// same exact-group matching and the same dotted path. The label is named
// after the path's last segment, in snake case under the xp_ prefix:
// spec.forProvider.region becomes xp_region, and
// status.atProvider.availabilityZone becomes xp_availability_zone.
type FieldLabel struct {
	// Group is the API group of the kind.
	Group string

	// Kind is the kind, matched exactly.
	Kind string

	// Path is the field's location within the object, one segment per element.
	Path []string
}

// ParseFieldLabel parses one group/Kind:dotted.path entry.
func ParseFieldLabel(entry string) (label FieldLabel, err error) {
	label.Group, label.Kind, label.Path, err = parseFieldEntry("field label", entry)
	return label, err
}

// ParseFieldLabels parses a list value of entries, accepting commas and
// newlines between them.
func ParseFieldLabels(value string) (labels []FieldLabel, err error) {
	for _, entry := range ParseList(value) {
		var label FieldLabel

		label, err = ParseFieldLabel(entry)
		if err != nil {
			return labels, err
		}

		labels = append(labels, label)
	}

	return labels, err
}

// Name returns the label name the field is carried under.
func (f FieldLabel) Name() (name string) {
	name = fieldLabelPrefix + snakeCase(f.Path[len(f.Path)-1])
	return name
}

// PathString returns the path as written.
func (f FieldLabel) PathString() (path string) {
	path = strings.Join(f.Path, ".")
	return path
}

// Matches reports whether the label applies to objects of the given kind.
func (f FieldLabel) Matches(group string, kind string) (matches bool) {
	matches = f.Group == group && f.Kind == kind
	return matches
}

// FieldLabelNames returns the distinct label names the configured field
// labels produce, sorted. A Prometheus metric has one fixed label set, so
// every per-object series carries all of them, and a series whose kind has
// no value for one carries it empty, which Prometheus treats as absent.
func FieldLabelNames(labels []FieldLabel) (names []string) {
	seen := make(map[string]struct{}, len(labels))

	for _, label := range labels {
		name := label.Name()
		if _, dup := seen[name]; dup {
			continue
		}

		seen[name] = struct{}{}
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// reservedLabelNames are the xp_ labels every per-object series already
// carries. A field label that would produce one of them is rejected rather
// than silently doubling it.
//
//nolint:gochecknoglobals // an immutable lookup table
var reservedLabelNames = map[string]struct{}{
	"xp_group":           {},
	"xp_version":         {},
	"xp_kind":            {},
	"xp_name":            {},
	"xp_namespace":       {},
	"xp_external_name":   {},
	"xp_composite":       {},
	"xp_claim":           {},
	"xp_claim_namespace": {},
}

// ReservedLabelName reports whether a label name is one the exporter already
// uses for object identity or ownership.
func ReservedLabelName(name string) (reserved bool) {
	_, reserved = reservedLabelNames[name]
	return reserved
}

// validateFieldLabels rejects a field label that would collide with a fixed
// label, or with another field label on the same kind. The same name on two
// different kinds is fine, and is the point: xp_region on every kind that has
// a region.
func validateFieldLabels(labels []FieldLabel) (err error) {
	type kindName struct {
		group string
		kind  string
		name  string
	}

	taken := make(map[kindName]FieldLabel, len(labels))

	for _, label := range labels {
		name := label.Name()

		if ReservedLabelName(name) {
			err = fmt.Errorf("field label %s/%s:%s would produce the reserved label %s", label.Group, label.Kind, label.PathString(), name)
			return err
		}

		key := kindName{group: label.Group, kind: label.Kind, name: name}

		if earlier, dup := taken[key]; dup {
			err = fmt.Errorf("field labels %s/%s:%s and %s/%s:%s both produce the label %s",
				earlier.Group, earlier.Kind, earlier.PathString(), label.Group, label.Kind, label.PathString(), name)
			return err
		}

		taken[key] = label
	}

	return err
}

// snakeCase rewrites a JSON key as a Prometheus label segment: ASCII letters
// and digits in lower case, an underscore at each camel-case boundary, and an
// underscore in place of anything else.
func snakeCase(key string) (name string) {
	var out strings.Builder

	for index := range len(key) {
		char := key[index]

		switch {
		case char >= 'A' && char <= 'Z':
			if camelBoundary(key, index) {
				out.WriteByte('_')
			}

			out.WriteByte(char - 'A' + 'a')
		case (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9'):
			out.WriteByte(char)
		default:
			out.WriteByte('_')
		}
	}

	name = out.String()

	return name
}

// camelBoundary reports whether the upper-case byte at index starts a new
// word: it follows a lower-case letter or digit, or it ends an upper-case run
// that a lower-case letter continues, as the Z in availabilityZone or the I
// in instanceID.
func camelBoundary(key string, index int) (boundary bool) {
	if index == 0 {
		return boundary
	}

	prev := key[index-1]
	prevLower := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
	prevUpper := prev >= 'A' && prev <= 'Z'
	nextLower := index+1 < len(key) && key[index+1] >= 'a' && key[index+1] <= 'z'

	boundary = prevLower || (prevUpper && nextLower)

	return boundary
}
