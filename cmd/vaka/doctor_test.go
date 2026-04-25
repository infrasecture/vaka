package main

import "testing"

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
