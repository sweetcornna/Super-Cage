package attach

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
)

// Each row is what one application inserts when files are dropped on it
// (macOS, default settings); only these forms upload.
func TestDroppedPathsPerApplication(t *testing.T) {
	two := []string{"/Users/o/My Pictures/a (1).png", "/tmp/b.png"}
	for _, row := range []struct {
		app, in string
		want    []string
	}{
		// iTerm2: backslash escapes, joined by spaces, a trailing space
		// (PTYTextView.m pasteValuesOnPasteboard).
		{"iTerm2", "/Users/o/My\\ Pictures/a\\ \\(1\\).png /tmp/b.png ", two},
		// Ghostty: Shell.escape, joined by spaces, no trailing space
		// (NSPasteboard+Extension.swift getOpinionatedStringContents).
		{"Ghostty", "/Users/o/My\\ Pictures/a\\ \\(1\\).png /tmp/b.png", two},
		// Terminal: backslash escapes, a trailing space. Unverified: no
		// source, and no application can be launched here. Warp has no row:
		// closed source, unverified.
		{"Terminal", "/Users/o/My\\ Pictures/a\\ \\(1\\).png /tmp/b.png ", two},
		// WezTerm: quote_dropped_files = SpacesOnly, a trailing space
		// (termwindow/mod.rs DroppedFile, config DroppedFileQuoting).
		{"WezTerm", "/Users/o/My\\ Pictures/a\\ (1).png /tmp/b.png ", two},
		// Alacritty: each file its own paste, unescaped, with a trailing space
		// (event.rs DroppedFile).
		{"Alacritty", "/tmp/b.png ", []string{"/tmp/b.png"}},
		// kitty: paths joined by new lines, or shlex.quote'd and joined by
		// spaces at a shell prompt (window.py on_drop, paste_with_actions).
		{"kitty", "/Users/o/My Pictures/a (1).png\n/tmp/b.png", two},
		{"kitty at a prompt", "'/Users/o/My Pictures/a (1).png' /tmp/b.png", two},
		{"kitty quote in a name", "'/tmp/it'\"'\"'s.png'", []string{"/tmp/it's.png"}},
	} {
		got, ok := DroppedPaths(row.in)
		if !ok || strings.Join(got, "|") != strings.Join(row.want, "|") {
			t.Errorf("%s: %q gave %q %v", row.app, row.in, got, ok)
		}
	}
	for _, in := range []string{
		"~/Desktop/a.png", "file:///tmp/a.png", "file://localhost/tmp/a.png", "\"/tmp/a b.png\"",
		"relative/a.png", "/tmp/a.png\x1b[31m", "/tmp/a.png\r/tmp/b.png", "", " ",
		"'/tmp/unterminated", "/tmp/a.png\n~/b.png", "/tmp/a.png\nfile:///b.png",
	} {
		if got, ok := DroppedPaths(in); ok {
			t.Errorf("%q uploads %q", in, got)
		}
	}
	// What is not an escaped list is read as one unescaped path, as kitty
	// and Alacritty insert it; no such file exists, so nothing uploads.
	for _, in := range []string{"/tmp/a.png and more", "/tmp/a.png\\"} {
		if got, ok := DroppedPaths(in); !ok || len(got) != 1 || got[0] != in {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
}

func writePNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
