// Copyright 2026 Google LLC
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

package testcert

import (
	"os"
	"path/filepath"
	"testing"
)

// ReplaceFile atomically replaces a test bundle, as projected TLS volumes do.
func ReplaceFile(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.CreateTemp(filepath.Dir(path), ".bundle-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(file.Name()); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove temporary bundle: %v", err)
		}
	})
	if err := os.WriteFile(file.Name(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		t.Fatal(err)
	}
}
