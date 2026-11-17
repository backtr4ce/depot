package main

import (
	"strings"
	"testing"
)

func TestConditions(t *testing.T) {
	vars := map[string]any{"host_cpu": "x64", "old": false, "label": "a'b"}

	for _, tt := range []struct {
		expression string
		want       bool
		wantError  string
	}{
		{"", true, ""},
		{"host_cpu == 'x64'", true, ""},
		{`host_cpu != "arm64" and old == false`, true, ""},
		{"not (old or host_cpu == 'arm64')", true, ""},
		{"((host_cpu == 'x64' and (not old or false)) and not (old and (true or false)))", true, ""},
		{"not ((host_cpu == 'x64' or old) and (true and not old))", false, ""},
		{"true or false and false", true, ""},
		{"not host_cpu == 'arm64'", true, ""},
		{"not not false", false, ""},
		{`label == 'a\'b'`, true, ""},
		{"host_cpu == 'arm64'", false, ""},
		{"missing == false", false, "undefined variable"},
		{"false and missing", false, "undefined variable"},
		{"host_cpu == true", false, "cannot compare"},
		{"not host_cpu", false, "must be a boolean"},
		{"host_cpu", false, "must evaluate"},
		{"true and host_cpu", false, "must be booleans"},
		{"(true", false, "closing parenthesis"},
		{"true)", false, "unexpected token"},
		{"host_cpu = 'x64'", false, "expected == or !="},
		{"host_cpu == 'x64", false, "unterminated string"},
		{"host_cpu == 'x64' == true", false, "unexpected token"},
		{"exec('ls')", false, "unexpected token '('"},
		{"host_cpu = = 'x64'", false, "expected == or !="},
		{"host_cpu ! = 'x64'", false, "expected == or !="},
		{"old /* comment */", false, "unexpected character '/'"},
		{"old == 1", false, "unexpected character '1'"},
		{"host_cpu == 'x\ny'", false, "unterminated string"},
		{"(old", false, "offset 4: expected closing parenthesis for '(' at offset 0"},
		{"not not not not true", true, ""},
		{strings.Repeat("(", 64) + "true" + strings.Repeat(")", 64), true, ""},
		{strings.Repeat("(", 65) + "true" + strings.Repeat(")", 65), false, "nests deeper than 64"},
		{strings.Repeat("not ", 65) + "true", false, "nests deeper than 64"},
		{strings.Repeat("(", 1_000_000), false, "exceeds 4096 bytes"},
	} {
		t.Run(tt.expression, func(t *testing.T) {
			got, err := evaluate(tt.expression, vars)

			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("got %v, want error containing %q", err, tt.wantError)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Fatalf("got (%v, %v), want %v", got, err, tt.want)
			}
		})
	}
}

func TestVariables(t *testing.T) {
	vars, err := variables(map[string]string{"target_cpu": "custom", "old": "false", "version": "17.0.0"})
	if err != nil {
		t.Fatal(err)
	}

	if vars["old"] != false || vars["target_cpu"] != "custom" || vars["version"] != "17.0.0" || vars["host_cpu"] == "" {
		t.Fatalf("unexpected variables: %v", vars)
	}

	for _, name := range []string{"host_cpu", "host_os", "host_custom", "and", "true", "bad-name", "1abc", "é", ""} {
		if _, err := variables(map[string]string{name: "x"}); err == nil {
			t.Errorf("accepted variable %q", name)
		}
	}
}

func TestNewPlatformUsesGNNames(t *testing.T) {
	tests := []struct {
		goos, goarch string
		want         platform
	}{
		{"darwin", "arm64", platform{os: "mac", cpu: "arm64"}},
		{"windows", "amd64", platform{os: "win", cpu: "x64"}},
		{"linux", "386", platform{os: "linux", cpu: "x86"}},
		{"linux", "mips64le", platform{os: "linux", cpu: "mips64el"}},
		{"futureos", "futurecpu", platform{os: "futureos", cpu: "futurecpu"}},
	}

	for _, tt := range tests {
		if got := newPlatform(tt.goos, tt.goarch); got != tt.want {
			t.Errorf("newPlatform(%q, %q) = %+v, want %+v", tt.goos, tt.goarch, got, tt.want)
		}
	}
}
