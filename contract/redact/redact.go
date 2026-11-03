// Package redact masks well-known credential formats in free text.
package redact

import "regexp"

const mask = "****"

// ponytail: eigene Regex-Liste, gitleaks-Regeln wenn Trefferquote nicht reicht
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`gh[ps]_[A-Za-z0-9]{36}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{82}`),
	regexp.MustCompile(`lin_api_[A-Za-z0-9]{40}`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

// String replaces every secret match in s with "****".
func String(s string) string {
	for _, p := range patterns {
		s = p.ReplaceAllString(s, mask)
	}
	return s
}

// Contains reports whether s holds at least one secret match.
func Contains(s string) bool {
	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}
