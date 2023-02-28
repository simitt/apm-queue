// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package queuecontext provides convenient wrappers for storing and
// accessing a stored metadata.
package queuecontext

import "context"

type metadataKey struct{}

// WithMetadata enriches a context with metadata.
func WithMetadata(ctx context.Context, metadata map[string]string) context.Context {
	return context.WithValue(ctx, metadataKey{}, metadata)
}

// MetadataFromContext returns the metadata from the passed context and a bool
// indicating whether the value is present or not.
func MetadataFromContext(ctx context.Context) (map[string]string, bool) {
	if v := ctx.Value(metadataKey{}); v != nil {
		metadata, ok := v.(map[string]string)
		return metadata, ok
	}
	return nil, false
}
