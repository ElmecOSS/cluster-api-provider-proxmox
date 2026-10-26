/*
Copyright 2023-2026 IONOS Cloud.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package crdcheck holds assertions about the generated CRDs themselves,
// rather than about the Go types they are generated from.
package crdcheck

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestNumericBoundsFitTheirFormat walks every generated CRD and checks that a
// numeric bound is representable in the format declared next to it.
//
// A kubebuilder Maximum above what its field's type can hold looks harmless --
// unreachable, at worst -- but it makes the schema contradict itself, and an
// API server that checks for that rejects every object setting the field, not
// just one out of range. That is how the u32 maximum on the int32 vrfs[].table
// made VRFs unusable: valid objects came back with "Maximum boundary value
// must be of type integer with format int32".
//
// The check lives here rather than in an envtest because whether the API
// server enforces it depends on its version: Kubernetes 1.37 rejects the
// object, 1.34 accepts it. The contradiction is in the schema either way, so
// it is the schema this asserts against, and the guard holds whatever version
// the suite happens to run.
func TestNumericBoundsFitTheirFormat(t *testing.T) {
	crdDir := filepath.Join("..", "..", "config", "crd", "bases")
	entries, err := os.ReadDir(crdDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "no generated CRDs to check")

	checked := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}

		//nolint:gosec // the path is this repo's own generated CRD directory
		raw, err := os.ReadFile(filepath.Join(crdDir, entry.Name()))
		require.NoError(t, err)

		var crd apiextensionsv1.CustomResourceDefinition
		require.NoError(t, yaml.Unmarshal(raw, &crd), "%s", entry.Name())

		for _, version := range crd.Spec.Versions {
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			walk(t, entry.Name()+" "+version.Name, version.Schema.OpenAPIV3Schema)
			checked++
		}
	}
	require.NotZero(t, checked, "no schemas were walked")
}

// limits are the inclusive bounds of each integer format controller-gen emits.
var limits = map[string]struct{ min, max float64 }{
	"int32": {math.MinInt32, math.MaxInt32},
	"int64": {math.MinInt64, math.MaxInt64},
}

func walk(t *testing.T, path string, schema *apiextensionsv1.JSONSchemaProps) {
	t.Helper()

	if schema.Type == "integer" {
		if limit, known := limits[schema.Format]; known {
			if schema.Maximum != nil {
				require.LessOrEqual(t, *schema.Maximum, limit.max,
					"%s: maximum %v does not fit format %s", path, *schema.Maximum, schema.Format)
			}
			if schema.Minimum != nil {
				require.GreaterOrEqual(t, *schema.Minimum, limit.min,
					"%s: minimum %v does not fit format %s", path, *schema.Minimum, schema.Format)
			}
		}
	}

	for name, prop := range schema.Properties {
		walk(t, path+"."+name, &prop)
	}
	if schema.Items != nil && schema.Items.Schema != nil {
		walk(t, path+"[]", schema.Items.Schema)
	}
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil {
		walk(t, path+"{}", schema.AdditionalProperties.Schema)
	}
}
