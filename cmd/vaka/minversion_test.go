// cmd/vaka/minversion_test.go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckMinVakaVersion(t *testing.T) {
	t.Run("no requirement", func(t *testing.T) {
		if err := checkMinVakaVersion("", "0.0.2", &bytes.Buffer{}); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("satisfied (equal and newer)", func(t *testing.T) {
		if err := checkMinVakaVersion("0.0.2", "0.0.2", &bytes.Buffer{}); err != nil {
			t.Fatalf("equal: %v", err)
		}
		if err := checkMinVakaVersion("0.0.2", "0.1.0", &bytes.Buffer{}); err != nil {
			t.Fatalf("newer: %v", err)
		}
	})
	t.Run("too old is a hard block", func(t *testing.T) {
		err := checkMinVakaVersion("0.2.0", "0.1.0", &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "requires vaka >= 0.2.0") {
			t.Fatalf("err = %v, want hard block", err)
		}
	})
	t.Run("dev build skips with a warning", func(t *testing.T) {
		var buf bytes.Buffer
		if err := checkMinVakaVersion("0.2.0", "dev", &buf); err != nil {
			t.Fatalf("dev build blocked: %v", err)
		}
		if !strings.Contains(buf.String(), "not a release version") {
			t.Fatalf("no dev-skip warning: %q", buf.String())
		}
	})
	t.Run("unparseable minVakaVersion warns, does not block", func(t *testing.T) {
		var buf bytes.Buffer
		if err := checkMinVakaVersion("not-semver", "0.1.0", &buf); err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(buf.String(), "unparseable minVakaVersion") {
			t.Fatalf("no warning: %q", buf.String())
		}
	})
	t.Run("v-prefixed versions compare", func(t *testing.T) {
		if err := checkMinVakaVersion("0.0.2", "v0.1.0", &bytes.Buffer{}); err != nil {
			t.Fatalf("v-prefix: %v", err)
		}
	})
}
