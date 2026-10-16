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

package collector

import (
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nikogura/crossplane-state-metrics/pkg/config"
	"github.com/nikogura/crossplane-state-metrics/pkg/watch"
)

// fieldLabelsByKind indexes the configured field labels by the kind they
// apply to, so the per-object lookup is one map read.
func fieldLabelsByKind(labels []config.FieldLabel) (index map[schema.GroupKind][]config.FieldLabel) {
	index = make(map[schema.GroupKind][]config.FieldLabel, len(labels))

	for _, label := range labels {
		key := schema.GroupKind{Group: label.Group, Kind: label.Kind}
		index[key] = append(index[key], label)
	}

	return index
}

// fieldLabelValues resolves the configured field labels for one object, in
// the order the descriptors declare them.
//
// A descriptor's label set is fixed, so every object carries every field
// label name. A label the object's kind is not configured for, or whose field
// is absent or not a string, is carried empty, and Prometheus treats an empty
// label as no label at all: the series matches {xp_region=""} and nothing
// else, and no series churns when a value comes and goes.
func (c *Collector) fieldLabelValues(snapshot watch.Snapshot, object *unstructured.Unstructured) (values []string) {
	if len(c.fieldLabelNames) == 0 {
		return values
	}

	resolved := make(map[string]string, len(c.fieldLabelNames))

	for _, label := range c.fieldLabels[snapshot.Kind.GVK.GroupKind()] {
		raw, found, lookupErr := unstructured.NestedFieldNoCopy(object.Object, label.Path...)
		if lookupErr != nil || !found {
			continue
		}

		value, isString := raw.(string)
		if !isString {
			c.logger.Debug("field label skipped: the field is not a string",
				slog.String("kind", snapshot.Kind.GVK.Kind),
				slog.String("name", object.GetName()),
				slog.String("path", label.PathString()))

			continue
		}

		resolved[label.Name()] = value
	}

	values = make([]string, 0, len(c.fieldLabelNames))
	for _, name := range c.fieldLabelNames {
		values = append(values, resolved[name])
	}

	return values
}
