package localterm

import "strings"

// firstLine is the first line a failed step printed, or its error.
func firstLine(output []byte, err error) string {
	text := strings.TrimSpace(string(output))
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if text == "" {
		return err.Error()
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}
