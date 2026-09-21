package main

import (
	"testing"
	"time"
)

func TestBackendIdleTimeout(t *testing.T) {
	tests := []struct {
		name  string
		input int
		want  time.Duration
	}{
		{name: "mosdns default sentinel", input: 0, want: 5 * time.Second},
		{name: "minimum valid explicit", input: 6, want: 1 * time.Second},
		{name: "normal", input: 30, want: 25 * time.Second},
		{name: "capped", input: 120, want: 90 * time.Second},
		{name: "large capped", input: 3600, want: 90 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backendIdleTimeout(tt.input); got != tt.want {
				t.Fatalf("backendIdleTimeout(%d) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}
