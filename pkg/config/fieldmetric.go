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
	"strings"
)

// FieldMetric names one numeric field of one kind to export as a gauge.
//
// It is written as group/Kind:dotted.path, for example
// ecs.aws.m.upbound.io/Service:status.atProvider.desiredCount. The group is
// exact, so the namespaced *.m.upbound.io kind and its cluster-scoped
// *.upbound.io counterpart are configured separately. The path is split on
// dots, which means a key containing a dot cannot be addressed; nothing in a
// forProvider or atProvider block is named that way.
type FieldMetric struct {
	// Group is the API group of the kind.
	Group string

	// Kind is the kind, matched exactly.
	Kind string

	// Path is the field's location within the object, one segment per element.
	Path []string
}

// ParseFieldMetric parses one group/Kind:dotted.path entry.
func ParseFieldMetric(entry string) (metric FieldMetric, err error) {
	kindPart, pathPart, hasPath := strings.Cut(entry, ":")
	groupPart, kindName, hasKind := strings.Cut(kindPart, "/")

	if !hasPath || !hasKind || strings.Contains(pathPart, "/") {
		err = fmt.Errorf("field metric %q must be written as group/Kind:dotted.path", entry)
		return metric, err
	}

	metric.Group = strings.TrimSpace(groupPart)
	metric.Kind = strings.TrimSpace(kindName)

	if metric.Group == "" {
		err = fmt.Errorf("field metric %q has an empty group", entry)
		return metric, err
	}

	if metric.Kind == "" {
		err = fmt.Errorf("field metric %q has an empty kind", entry)
		return metric, err
	}

	for _, segment := range strings.Split(strings.TrimSpace(pathPart), ".") {
		if segment == "" {
			err = fmt.Errorf("field metric %q has an empty path segment", entry)
			return metric, err
		}

		metric.Path = append(metric.Path, segment)
	}

	return metric, err
}

// ParseFieldMetrics parses a list value of entries, accepting commas and
// newlines between them.
func ParseFieldMetrics(value string) (metrics []FieldMetric, err error) {
	for _, entry := range ParseList(value) {
		var metric FieldMetric

		metric, err = ParseFieldMetric(entry)
		if err != nil {
			return metrics, err
		}

		metrics = append(metrics, metric)
	}

	return metrics, err
}

// PathString returns the path as written, which is the value of the field
// label.
func (f FieldMetric) PathString() (path string) {
	path = strings.Join(f.Path, ".")
	return path
}

// Matches reports whether the metric applies to objects of the given kind.
func (f FieldMetric) Matches(group string, kind string) (matches bool) {
	matches = f.Group == group && f.Kind == kind
	return matches
}
