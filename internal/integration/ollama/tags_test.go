// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ollama

import (
	"reflect"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"Go", "go", "  GO  "}, []string{"go"}},
		{[]string{"machine learning", "Machine-Learning"}, []string{"machine-learning"}},
		{[]string{"", "  ", "kept"}, []string{"kept"}},
		{nil, []string{}},
	}
	for _, tc := range cases {
		got := normalizeTags(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("normalizeTags(%v) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"```json\n{\"tags\":[\"a\"]}\n```", `{"tags":["a"]}`},
		{"prefix {\"score\": 0.7} suffix", `{"score": 0.7}`},
		{"no json here", ""},
		{"{}", "{}"},
	}
	for _, tc := range cases {
		if got := extractJSON(tc.in); got != tc.want {
			t.Errorf("extractJSON(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 3); got != "hel" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("hi", 10); got != "hi" {
		t.Errorf("truncate long = %q", got)
	}
}

func TestClamp01(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{-0.5, 0}, {0, 0}, {0.5, 0.5}, {1, 1}, {1.7, 1},
	}
	for _, tc := range cases {
		if got := clamp01(tc.in); got != tc.want {
			t.Errorf("clamp01(%v) = %v; want %v", tc.in, got, tc.want)
		}
	}
}
