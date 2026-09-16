package review

import "strings"

const summaryMarker = "REVIEW SUMMARY:"

func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func extractSummary(text string) string {
	idx := strings.LastIndex(text, summaryMarker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(text[idx+len(summaryMarker):])
}
