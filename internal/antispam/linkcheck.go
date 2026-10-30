// Package antispam implements the synchronous checks every public submission
// passes before it is stored: link/HTML rejection, rate limits and Akismet.
package antispam

import (
	"regexp"
	"strings"
)

var linkPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bhttps?://`),
	regexp.MustCompile(`(?i)\bftp://`),
	regexp.MustCompile(`(?i)\bwww\.[a-z0-9-]`),
	regexp.MustCompile(`\[[^\]]*\]\s*\(\s*[^)]*[./:#][^)]*\)`), // [text](url-ish target)
	regexp.MustCompile(`<[a-zA-Z][a-zA-Z0-9+.-]*:[^>]*>`),      // <scheme:...> autolinks
	regexp.MustCompile(`</?[a-zA-Z][^>]*>`),                    // HTML tags
	regexp.MustCompile(`(?i)\bmailto:`),
}

// FindLinks returns up to limit offending snippets found in text. An empty
// result means the text is acceptable.
func FindLinks(text string, limit int) []string {
	var found []string
	seen := map[string]bool{}
	for _, re := range linkPatterns {
		for _, loc := range re.FindAllStringIndex(text, -1) {
			start, end := loc[0], loc[1]
			// Show a little context around the match.
			s := start
			for s > 0 && end-s < 40 && text[s-1] != '\n' {
				s--
			}
			e := end
			for e < len(text) && e-start < 40 && text[e] != '\n' && text[e] != ' ' {
				e++
			}
			snippet := strings.TrimSpace(text[s:e])
			if snippet == "" || seen[snippet] {
				continue
			}
			seen[snippet] = true
			found = append(found, snippet)
			if len(found) >= limit {
				return found
			}
		}
	}
	return found
}
