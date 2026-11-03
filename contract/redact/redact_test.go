package redact

import (
	"strings"
	"testing"
)

func TestStringRedactsKnownTokens(t *testing.T) {
	alnum := strings.Repeat("aB3dE5gH7jK9mN1pQ", 6)
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEabc\ndef==\n-----END RSA PRIVATE KEY-----"
	tests := []struct {
		name, token string
	}{
		{"anthropic", "sk-ant-api03-" + alnum[:40]},
		{"github pat classic", "ghp_" + alnum[:36]},
		{"github server", "ghs_" + alnum[:36]},
		{"github fine grained", "github_pat_" + alnum[:82]},
		{"linear", "lin_api_" + alnum[:40]},
		{"private key", pem},
		{"aws", "AKIA" + strings.ToUpper(alnum[:16])},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := "vor " + tt.token + " nach"
			if got := String(in); got != "vor **** nach" {
				t.Errorf("String() = %q", got)
			}
			if !Contains(in) {
				t.Error("Contains() = false")
			}
		})
	}
}

func TestStringLeavesNormalTextUnchanged(t *testing.T) {
	in := "Bitte ändere README.md"
	if got := String(in); got != in {
		t.Errorf("String() = %q", got)
	}
	if Contains(in) {
		t.Error("Contains() = true")
	}
}
