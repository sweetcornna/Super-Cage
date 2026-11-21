package attach

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	_ "golang.org/x/image/webp"

	"github.com/sylvinus/agent-vm/internal/localterm"
)

// Most terminal applications paste a dropped file as its local path, which
// tells the environment the owner's user name and is useless there. cage
// attach does what Cage's web terminal does with a dropped or pasted image:
// it uploads the image and pastes the environment's path instead. Only
// images the web terminal accepts qualify (PNG, JPEG, WebP, GIF, at most
// 20 MB and 40 megapixels, decided by decoding the content), and only when a
// bracketed paste holds nothing but absolute paths in a form an application
// inserts for a drop (DroppedPaths). Typed text never uploads. Anything
// else, a key file's path for instance, is pasted unchanged and never read
// beyond its header.

const (
	attachPasteStart = "\x1b[200~"
	attachPasteEnd   = "\x1b[201~"
	// attachPasteHold bounds a paste kept while its end has not arrived;
	// anything longer is not a list of dropped files.
	attachPasteHold = 64 << 10
	// attachPastePaths bounds the files of one drop.
	attachPastePaths = 20
)

// attachPasteWait is how long the start of a paste waits for its end.
var attachPasteWait = 300 * time.Millisecond

type attachPaste struct {
	a       *attacher
	pending []byte
	t       *time.Timer
}

// feed sends typed input on at once and holds a bracketed paste until it is
// complete.
func (p *attachPaste) feed(ctx context.Context, data []byte) error {
	for len(data) > 0 {
		if p.pending == nil {
			i := bytes.Index(data, []byte(attachPasteStart))
			if i < 0 {
				return p.a.sendInput(data)
			}
			if err := p.a.sendInput(data[:i]); err != nil {
				return err
			}
			p.pending = append([]byte(nil), data[i:]...)
			p.t = time.NewTimer(attachPasteWait)
		} else {
			p.pending = append(p.pending, data...)
		}
		data = nil
		end := bytes.Index(p.pending[len(attachPasteStart):], []byte(attachPasteEnd))
		if end < 0 {
			if len(p.pending) > attachPasteHold {
				return p.flush()
			}
			return nil
		}
		cut := len(attachPasteStart) + end + len(attachPasteEnd)
		paste, rest := p.pending[:cut], append([]byte(nil), p.pending[cut:]...)
		p.stop()
		if err := p.deliver(ctx, paste); err != nil {
			return err
		}
		data = rest
	}
	return nil
}

func (p *attachPaste) stop() {
	p.pending = nil
	if p.t != nil {
		p.t.Stop()
		p.t = nil
	}
}

// timer fires when a held paste waited too long for its end.
func (p *attachPaste) timer() <-chan time.Time {
	if p.t == nil {
		return nil
	}
	return p.t.C
}

// flush sends a held paste as it is.
func (p *attachPaste) flush() error {
	data := p.pending
	p.stop()
	return p.a.sendInput(data)
}

func (p *attachPaste) deliver(ctx context.Context, paste []byte) error {
	content := string(paste[len(attachPasteStart) : len(paste)-len(attachPasteEnd)])
	paths, ok := DroppedPaths(content)
	if !ok {
		return p.a.sendInput(paste)
	}
	// The switch is read for each paste: turning it off in Super Cage
	// applies to windows already open. An unreadable file keeps the choice
	// read at the start.
	upload := p.a.settings.ImageUpload
	if settings, err := localterm.LoadSettings(filepath.Dir(p.a.cfg.ConfigPath)); err == nil {
		upload = settings.ImageUpload
	}
	if !upload {
		return p.a.sendInput(paste)
	}
	images := make([][]byte, 0, len(paths))
	for _, path := range paths {
		data, err := readImage(path)
		if err == nil {
			data, err = acceptImage(data)
		}
		if err != nil {
			return p.a.sendInput(paste) // not an image: an ordinary paste
		}
		images = append(images, data)
	}
	guest := make([]string, 0, len(images))
	for _, data := range images {
		path, err := p.a.uploadImage(ctx, data)
		if err != nil {
			// The paste is not sent: it would give the environment this
			// computer's path. The bell says it did not arrive.
			_, _ = p.a.tty.Out.Write([]byte("\a"))
			return nil
		}
		guest = append(guest, path)
	}
	return p.a.sendInput([]byte(attachPasteStart + attachShellPaths(guest) + attachPasteEnd))
}

// attachShellPaths quotes paths as the web terminal does (file-transfer.js).
func attachShellPaths(paths []string) string {
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ") + " "
}

// DroppedPaths reads a paste as the paths a terminal application inserts
// for files dropped on it, in the forms those applications use on macOS
// (paste_test.go has one row per application, with its source):
//
//   - backslash escapes, joined by spaces, perhaps a trailing space:
//     Terminal, iTerm2, Ghostty, WezTerm (quote_dropped_files = SpacesOnly);
//   - shlex quoting ('...', with '"'"' for a quote), joined by spaces:
//     kitty at a shell prompt;
//   - unescaped, one path per line, or one path and a trailing space:
//     kitty elsewhere, Alacritty (one paste per file).
//
// ok is false unless every path is absolute. No application inserts ~/ or
// a file:// URL for a drop, or uses double quotes on macOS: a paste holding
// them was typed or copied, and nothing is read.
func DroppedPaths(text string) (paths []string, ok bool) {
	if paths, ok = escapedPaths(text); ok {
		return paths, true
	}
	return literalPaths(text)
}

func escapedPaths(text string) (paths []string, ok bool) {
	var word strings.Builder
	inWord := false
	end := func() bool {
		if !inWord {
			return true
		}
		inWord = false
		path, valid := attachLocalPath(word.String())
		word.Reset()
		if !valid || len(paths) == attachPastePaths {
			return false
		}
		paths = append(paths, path)
		return true
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == ' ' || c == '\n':
			if !end() {
				return nil, false
			}
		case c == '\\':
			if i+1 == len(text) || text[i+1] < 0x20 || text[i+1] == 0x7f {
				return nil, false
			}
			i++
			word.WriteByte(text[i])
			inWord = true
		case c == '\'':
			j := strings.IndexByte(text[i+1:], '\'')
			if j < 0 || strings.ContainsFunc(text[i+1:i+1+j], unicode.IsControl) {
				return nil, false
			}
			word.WriteString(text[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			// Only shlex's way to write a quote inside quotes: "'".
			if !strings.HasPrefix(text[i:], "\"'\"") {
				return nil, false
			}
			word.WriteByte('\'')
			i += 2
			inWord = true
		case c < 0x20 || c == 0x7f:
			return nil, false
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if !end() || len(paths) == 0 {
		return nil, false
	}
	return paths, true
}

// literalPaths reads unescaped paths: one per line, or one followed by the
// space Alacritty adds.
func literalPaths(text string) (paths []string, ok bool) {
	text = strings.TrimSuffix(text, " ")
	for _, line := range strings.Split(text, "\n") {
		path, valid := attachLocalPath(line)
		if !valid || strings.ContainsFunc(line, unicode.IsControl) || len(paths) == attachPastePaths {
			return nil, false
		}
		paths = append(paths, path)
	}
	return paths, len(paths) > 0
}

func attachLocalPath(word string) (string, bool) {
	if !filepath.IsAbs(word) {
		return "", false
	}
	return filepath.Clean(word), true
}

// imageReadTimeout bounds reading one dropped file, so a stalled network
// volume cannot hold up the window. Decoding is not under it: it works on
// the bytes read, bounded by the size and pixel limits.
var imageReadTimeout = 5 * time.Second

// loadImage is readImageFile; tests replace it.
var loadImage = readImageFile

// readImage reads one dropped file off the input loop and gives up at the
// deadline.
func readImage(path string) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := loadImage(path)
		done <- result{data, err}
	}()
	timer := time.NewTimer(imageReadTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.data, r.err
	case <-timer.C:
		return nil, errors.New("reading the file took too long")
	}
}

var errNotImage = errors.New("not an accepted image file")

// readImageFile reads a dropped file whole. Only a regular file is read,
// not through a symbolic link, never a FIFO or device (opened without
// blocking); its size is checked before anything is read and again on the
// file actually opened, and at most MaxImageBytes are read.
func readImageFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > MaxImageBytes {
		return nil, errNotImage
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() == 0 || opened.Size() > MaxImageBytes {
		return nil, errNotImage
	}
	data, err := io.ReadAll(io.NewSectionReader(f, 0, MaxImageBytes+1))
	if err != nil || len(data) > MaxImageBytes {
		return nil, errNotImage
	}
	return data, nil
}

// acceptImage keeps an image the web terminal would accept and returns it
// as PNG, which is what the workbench takes: the content decides, not the
// name. A decoder that panics on a crafted file makes it unreadable, not
// the attach dead.
func acceptImage(data []byte) (out []byte, err error) {
	defer func() {
		if recover() != nil {
			out, err = nil, errNotImage
		}
	}()
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 40_000_000 {
		return nil, errNotImage
	}
	switch format {
	case "png":
		return data, nil
	case "jpeg", "gif", "webp":
	default:
		return nil, errNotImage
	}
	// Like the browser, keep one frame of an animated image.
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var converted bytes.Buffer
	if err := png.Encode(&converted, decoded); err != nil || converted.Len() > MaxImageBytes {
		return nil, errors.New("image too large")
	}
	return converted.Bytes(), nil
}

// attachImageFile reads and checks one dropped image, as a paste does.
func attachImageFile(path string) ([]byte, error) {
	data, err := readImageFile(path)
	if err != nil {
		return nil, err
	}
	return acceptImage(data)
}

// uploadImage sends one image the way the web terminal does
// (image-transfer.js) and returns its path in the environment.
func (a *attacher) uploadImage(ctx context.Context, data []byte) (string, error) {
	var binding struct {
		Available bool   `json:"available"`
		Context   string `json:"context"`
	}
	destination := map[string]any{"target": "terminal", "id": a.terminal}
	if err := a.request(ctx, "POST", "/api/input/paste/start", destination, &binding); err != nil || !binding.Available {
		return "", errors.New("paste not accepted")
	}
	var result struct {
		Path string `json:"path"`
	}
	body := map[string]any{"png": base64.StdEncoding.EncodeToString(data), "target": "terminal", "id": a.terminal, "context": binding.Context}
	if err := a.request(ctx, "POST", "/api/input/image", body, &result); err != nil {
		return "", err
	}
	if result.Path == "" || strings.ContainsAny(result.Path, "\x00\r\n") {
		return "", errors.New("invalid path")
	}
	return result.Path, nil
}
