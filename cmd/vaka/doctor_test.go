package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseComposeMajorVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "v2.27.0", want: 2},
		{in: "2.23.3", want: 2},
		{in: "1.29.0", want: 1},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseComposeMajorVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunDoctorChecksTimeout(t *testing.T) {
	results := runDoctorChecks(context.Background(), []doctorCheck{
		{
			name:     "times out",
			required: true,
			timeout:  25 * time.Millisecond,
			run: func(ctx context.Context) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
		},
	})
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].ok {
		t.Fatalf("expected failed result")
	}
	if !strings.Contains(results[0].errText, "timed out after") {
		t.Fatalf("unexpected err text: %q", results[0].errText)
	}
}

func TestRunDoctorChecksInformationalResult(t *testing.T) {
	results := runDoctorChecks(context.Background(), []doctorCheck{
		{
			name:     "info check",
			required: false,
			run: func(context.Context) (string, error) {
				return "", errors.New("probe unavailable")
			},
		},
	})
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].required {
		t.Fatalf("expected informational (required=false) result")
	}
	if results[0].ok {
		t.Fatalf("expected failed informational probe")
	}
}
