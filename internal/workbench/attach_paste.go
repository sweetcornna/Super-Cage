package workbench

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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
)

// Most terminal applications paste a dropped file as its local path, which
// tells the environment the owner's user name and is useless there. cage
// attach does what Cage's web terminal does with a dropped or pasted image:
// it uploads the image and pastes the environment's path instead. Only
// images the web terminal accepts qualify (PNG, JPEG, WebP, GIF, at most
// 20 MB and 40 megapixels, decided by decoding the content), and only when a
// bracketed paste holds nothing but such paths. Anything else, a key file's
// path for instance, is pasted unchanged and never read beyond its header.

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
	if !p.a.settings.ImageUpload {
		return p.a.sendInput(paste)
	}
	paths, ok := attachDroppedPaths(content)
	if !ok {
		return p.a.sendInput(paste)
	}
	images := make([][]byte, 0, len(paths))
	for _, path := range paths {
		data, err := attachImageFile(path)
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
			_, _ = p.a.tty.out.Write([]byte("\a"))
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

// attachDroppedPaths reads a paste as the paths terminal applications insert
// for dropped files: separated by spaces or new lines, with backslash
// escapes, single or double quotes, or as file:// URLs. ok is false unless
// every word is an absolute path.
func attachDroppedPaths(text string) (paths []string, ok bool) {
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
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if !end() {
				return nil, false
			}
		case c == '\\':
			if i+1 == len(text) {
				return nil, false
			}
			i++
			word.WriteByte(text[i])
			inWord = true
		case c == '\'':
			j := strings.IndexByte(text[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			word.WriteString(text[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(text) && text[i] != '"'; i++ {
				if text[i] == '\\' && i+1 < len(text) && strings.IndexByte("\"\\$`", text[i+1]) >= 0 {
					i++
				}
				word.WriteByte(text[i])
			}
			if i == len(text) {
				return nil, false
			}
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

func attachLocalPath(word string) (string, bool) {
	if strings.HasPrefix(word, "file://") {
		u, err := url.Parse(word)
		if err != nil || u.Host != "" && u.Host != "localhost" || u.RawQuery != "" || u.Fragment != "" {
			return "", false
		}
		word = u.Path
	}
	if strings.HasPrefix(word, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		word = filepath.Join(home, word[2:])
	}
	if !filepath.IsAbs(word) {
		return "", false
	}
	return filepath.Clean(word), true
}

// attachImageFile reads an image the web terminal would accept and returns
// it as PNG, which is what the workbench takes. The size is checked before
// anything is read, and the content decides, not the name.
func attachImageFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxImageBytes {
		return nil, errors.New("not an image file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	config, format, err := image.DecodeConfig(file)
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 40_000_000 {
		return nil, errors.New("not an accepted image")
	}
	if format != "png" && format != "jpeg" && format != "gif" && format != "webp" {
		return nil, errors.New("not an accepted image")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if format == "png" {
		data, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
		if err != nil || len(data) > maxImageBytes {
			return nil, errors.New("image too large")
		}
		return data, nil
	}
	// Like the browser, keep one frame of an animated image.
	decoded, _, err := image.Decode(file)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, decoded); err != nil || out.Len() > maxImageBytes {
		return nil, errors.New("image too large")
	}
	return out.Bytes(), nil
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
