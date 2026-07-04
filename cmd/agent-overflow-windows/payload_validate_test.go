//go:build windows

package main

import "testing"

func TestHasELFMagic(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"real ELF header", "\x7fELF\x02\x01\x01\x00rest-of-binary", true},
		{"placeholder text", "placeholder: run task build:wsl", false},
		{"empty", "", false},
		{"shorter than magic", "\x7fEL", false},
		{"magic alone", "\x7fELF", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasELFMagic(tc.payload); got != tc.want {
				t.Errorf("hasELFMagic(%q) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}
