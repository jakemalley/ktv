package ktv

import "testing"

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args          []string
		wantNS, wantP string
		wantErr       bool
	}{
		{nil, "", "", false},
		{[]string{"kube-system"}, "kube-system", "", false},
		{[]string{"kube-system/coredns"}, "kube-system", "coredns", false},
		{[]string{"kube-system", "coredns"}, "kube-system", "coredns", false},
		{[]string{"a", "b", "c"}, "", "", true},
	}
	for _, tt := range tests {
		ns, pod, err := parseArgs(tt.args)
		if (err != nil) != tt.wantErr || ns != tt.wantNS || pod != tt.wantP {
			t.Errorf("parseArgs(%q) = %q, %q, %v", tt.args, ns, pod, err)
		}
	}
}

func TestAutoMatch(t *testing.T) {
	items := []string{"default", "kube-system", "kube-public", "monitoring"}
	tests := []struct {
		query, want string
		ok          bool
	}{
		{"default", "default", true},  // exact
		{"MONIT", "monitoring", true}, // unique case-insensitive substring
		{"system", "kube-system", true},
		{"kube", "", false}, // ambiguous
		{"nope", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := autoMatch(tt.query, items)
		if got != tt.want || ok != tt.ok {
			t.Errorf("autoMatch(%q) = %q, %v; want %q, %v", tt.query, got, ok, tt.want, tt.ok)
		}
	}
}
