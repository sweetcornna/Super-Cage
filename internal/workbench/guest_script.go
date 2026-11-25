package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/klauspost/compress/gzip"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// guestScriptMissingExitCode is returned by guardedShellBody before Python
// starts when a cached hash-pinned guest helper file has disappeared from the
// guest (for example, after manual cleanup or an out-of-band VM reset).
const guestScriptMissingExitCode = 86

// guestScriptInstaller runs on the guest with the gzip-compressed script bundle
// on stdin, installing only missing hash-pinned files atomically. Because the
// payload travels over stdin, SSH argv stays small and constant regardless of
// script size, staying well below Windows CreateProcessW's 32,767 UTF-16 limit.
const guestScriptInstaller = `import gzip,os,pathlib,sys,tempfile
d=pathlib.Path.home()/sys.argv[1]
d.mkdir(parents=True,exist_ok=True,mode=0o700)
n,m=sys.argv[2],int(sys.argv[3],8)
raw=gzip.decompress(sys.stdin.buffer.read())
i=0
while i<len(raw):
 j=raw.index(10,i)
 s,k=raw[i:j].decode().split(" ",1)
 k=int(k)
 i=j+1
 v=raw[i:i+k]
 if len(v)!=k:sys.exit(2)
 i+=k
 p=d/(n+s)
 if not p.exists():
  t=None
  try:
   with tempfile.NamedTemporaryFile(dir=d,delete=False) as f:
    t=f.name
    f.write(v)
   os.chmod(t,m)
   os.replace(t,p)
  except Exception:
   if t:
    try:os.unlink(t)
    except OSError:pass
   raise
`

// stdinPythonBootstrap executes a Python script and its stdin payload framed on
// stdin ("<script_byte_len>\n<script_bytes><stdin_bytes>"). Used for privileged
// helpers (e.g. sudo -n cache maintenance) that must not be written into an
// unprivileged guest user's home directory.
const stdinPythonBootstrap = `import io,sys
b=sys.stdin.buffer
n=int(b.readline())
c=b.read(n)
if len(c)!=n:sys.exit(2)
sys.stdin=io.TextIOWrapper(io.BytesIO(b.read()),encoding="utf-8")
exec(compile(c,sys.argv[1],"exec"),{"__name__":"__main__"})`

func stdinPythonPayload(script string, input []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(script) + len(input) + 16)
	fmt.Fprintf(&buf, "%d\n", len(script))
	buf.WriteString(script)
	buf.Write(input)
	return buf.Bytes()
}

type guestScriptFile struct {
	suffix  string
	content []byte
}

type guestScriptSpec struct {
	relDir string
	hash   string
	mode   uint32
	entry  string
	files  []guestScriptFile

	compressOnce sync.Once
	compressed   []byte
}

func validGuestRelComponent(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-' || c == '/'
		if !ok {
			return false
		}
	}
	return true
}

func newGuestScriptSpec(relDir string, mode uint32, source string) *guestScriptSpec {
	sum := sha256.Sum256([]byte(source))
	return newGuestScriptBundle(relDir, fmt.Sprintf("%x", sum), mode, ".py", []guestScriptFile{
		{suffix: ".py", content: []byte(source)},
	})
}

func newGuestScriptBundle(relDir, hash string, mode uint32, entry string, files []guestScriptFile) *guestScriptSpec {
	if !validGuestRelComponent(relDir) || !validGuestRelComponent(hash) || strings.Contains(hash, "/") || len(files) == 0 {
		panic("invalid guestScriptSpec metadata")
	}
	foundEntry := false
	for _, f := range files {
		if !validGuestRelComponent(f.suffix) || strings.Contains(f.suffix, "/") {
			panic("invalid guestScriptFile suffix")
		}
		if f.suffix == entry {
			foundEntry = true
		}
	}
	if !foundEntry {
		panic("guestScriptSpec entry suffix missing from files")
	}
	return &guestScriptSpec{
		relDir: relDir,
		hash:   hash,
		mode:   mode,
		entry:  entry,
		files:  files,
	}
}

func (spec *guestScriptSpec) relPath(suffix string) string {
	return spec.relDir + "/" + spec.hash + suffix
}

func (spec *guestScriptSpec) entryRelPath() string {
	return spec.relPath(spec.entry)
}

func (spec *guestScriptSpec) cacheKey() string {
	return spec.relDir + "/" + spec.hash
}

// shellCommand returns a simple POSIX command that runs the hash-pinned entry
// script in Python's -I isolated mode. Because only "$HOME/<relPath>" is
// referenced, argv length is small and constant regardless of script size.
func (spec *guestScriptSpec) shellCommand(pythonBin string, args ...string) string {
	if pythonBin == "" {
		pythonBin = "python3"
	}
	cmd := shellescape.Quote(pythonBin) + ` -I "$HOME/` + spec.entryRelPath() + `"`
	if len(args) > 0 {
		cmd += " " + shellescape.QuoteCommand(args)
	}
	return cmd
}

// guardedShellBody returns a POSIX shell body that verifies every file of spec
// exists under $HOME, exiting with guestScriptMissingExitCode (86) before
// launching Python if any file is absent, and otherwise execs the entry script
// in -I isolated mode.
func (spec *guestScriptSpec) guardedShellBody(pythonBin string, args ...string) string {
	if pythonBin == "" {
		pythonBin = "python3"
	}
	if len(spec.files) == 1 {
		cmd := `p="$HOME/` + spec.entryRelPath() + `"; [ -f "$p" ] || exit 86; exec ` + shellescape.Quote(pythonBin) + ` -I "$p"`
		if len(args) > 0 {
			cmd += " " + shellescape.QuoteCommand(args)
		}
		return cmd
	}
	var checks []string
	for _, f := range spec.files {
		checks = append(checks, `[ -f "$d/`+spec.hash+f.suffix+`" ]`)
	}
	cmd := `d="$HOME/` + spec.relDir + `"; ` + strings.Join(checks, " && ") + ` || exit 86; exec ` +
		shellescape.Quote(pythonBin) + ` -I "$d/` + spec.hash + spec.entry + `"`
	if len(args) > 0 {
		cmd += " " + shellescape.QuoteCommand(args)
	}
	return cmd
}

func (spec *guestScriptSpec) installCommand(pythonBin string) string {
	if pythonBin == "" {
		pythonBin = "python3"
	}
	return shellescape.QuoteCommand([]string{
		pythonBin, "-I", "-c", guestScriptInstaller,
		spec.relDir, spec.hash, fmt.Sprintf("%o", spec.mode),
	})
}

func (spec *guestScriptSpec) installPayload() []byte {
	spec.compressOnce.Do(func() {
		var raw bytes.Buffer
		for _, f := range spec.files {
			fmt.Fprintf(&raw, "%s %d\n", f.suffix, len(f.content))
			raw.Write(f.content)
		}
		var compressed bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
		_, _ = writer.Write(raw.Bytes())
		_ = writer.Close()
		spec.compressed = compressed.Bytes()
	})
	return spec.compressed
}

type guestScriptFlight struct {
	done           chan struct{}
	err            error
	callerCanceled bool
}

// guestScriptCache remembers which hash-pinned helper scripts have been
// installed on the current guest boot so steady-state calls send zero runtime
// bytes and incur zero extra SSH round trips. It spawns no background
// goroutines or timers.
type guestScriptCache struct {
	mu        sync.Mutex
	gen       uint64
	installed map[string]bool
	inflight  map[string]*guestScriptFlight
}

func (c *guestScriptCache) reset() {
	c.mu.Lock()
	c.gen++
	c.installed = nil
	c.mu.Unlock()
}

func (c *guestScriptCache) has(key string) bool {
	c.mu.Lock()
	ok := c.installed[key]
	c.mu.Unlock()
	return ok
}

func (c *guestScriptCache) invalidate(key string) {
	c.mu.Lock()
	delete(c.installed, key)
	c.mu.Unlock()
}

func (c *guestScriptCache) ensure(ctx context.Context, key string, install func(context.Context) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if c.installed[key] {
			c.mu.Unlock()
			return nil
		}
		if flight := c.inflight[key]; flight != nil {
			c.mu.Unlock()
			select {
			case <-flight.done:
				if flight.err == nil {
					return nil
				}
				if !flight.callerCanceled {
					return flight.err
				}
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		gen := c.gen
		flight := &guestScriptFlight{done: make(chan struct{})}
		if c.inflight == nil {
			c.inflight = map[string]*guestScriptFlight{}
		}
		c.inflight[key] = flight
		c.mu.Unlock()

		err := install(ctx)
		flight.err = err
		flight.callerCanceled = err != nil && ctx.Err() != nil

		c.mu.Lock()
		if err == nil && c.gen == gen {
			if c.installed == nil {
				c.installed = map[string]bool{}
			}
			c.installed[key] = true
		}
		if c.inflight[key] == flight {
			delete(c.inflight, key)
		}
		c.mu.Unlock()
		close(flight.done)
		return err
	}
}

func (s *Server) guestShellScript(body string) string {
	if s.macosGuest != nil {
		return "exec /bin/zsh -c " + shellescape.Quote(body)
	}
	return "exec /bin/bash --noprofile --norc -c " + shellescape.Quote(body)
}

func (s *Server) guestScriptInstallSSHCommand(ctx context.Context, spec *guestScriptSpec, pythonBin string) *exec.Cmd {
	command := s.guestShellWrapper(spec.installCommand(pythonBin))
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	cmd.Stdin = bytes.NewReader(spec.installPayload())
	cmd.Stdout = io.Discard
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

type guestScriptEnsureError struct {
	msg string
	err error
}

func (e *guestScriptEnsureError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return e.err.Error()
}

func (e *guestScriptEnsureError) Unwrap() error { return e.err }

// guestScriptSharedInstaller decodes a base64+gzip bundle from stdin so
// ensureGuestScript can install hash-pinned scripts over the shared Linux
// guestSession without spawning an extra SSH handshake.
const guestScriptSharedInstaller = `import base64,gzip,os,pathlib,sys,tempfile
d=pathlib.Path.home()/sys.argv[1]
d.mkdir(parents=True,exist_ok=True,mode=0o700)
n,m=sys.argv[2],int(sys.argv[3],8)
raw=gzip.decompress(base64.b64decode(sys.stdin.read()))
i=0
while i<len(raw):
 j=raw.index(10,i)
 s,k=raw[i:j].decode().split(" ",1)
 k=int(k)
 i=j+1
 v=raw[i:i+k]
 if len(v)!=k:sys.exit(2)
 i+=k
 p=d/(n+s)
 if not p.exists():
  t=None
  try:
   with tempfile.NamedTemporaryFile(dir=d,delete=False) as f:
    t=f.name
    f.write(v)
   os.chmod(t,m)
   os.replace(t,p)
  except Exception:
   if t:
    try:os.unlink(t)
    except OSError:pass
   raise
`

const guestScriptSharedRunner = `import base64,io,runpy,sys
raw=base64.b64decode(sys.argv[1])
p=sys.argv[2]
sys.argv=[p]+sys.argv[3:]
sys.stdin=io.TextIOWrapper(io.BytesIO(raw),encoding="utf-8")
runpy.run_path(p,run_name="__main__")`

func (spec *guestScriptSpec) sharedInstallScript(pythonBin string) string {
	if pythonBin == "" {
		pythonBin = "python3"
	}
	cmd := shellescape.QuoteCommand([]string{
		pythonBin, "-I", "-c", guestScriptSharedInstaller,
		spec.relDir, spec.hash, fmt.Sprintf("%o", spec.mode),
	})
	return "exec " + cmd + " <<'__CAGE_GUEST_SCRIPT__'\n" +
		base64.StdEncoding.EncodeToString(spec.installPayload()) +
		"\n__CAGE_GUEST_SCRIPT__\n"
}

// guardedSharedScript builds a bash body for guestShared that checks every file
// of spec (exiting 86 if missing) and runs the entry script with input on stdin
// in one Python process, avoiding a dedicated cold SSH handshake.
func (spec *guestScriptSpec) guardedSharedScript(pythonBin string, input []byte, args ...string) string {
	if pythonBin == "" {
		pythonBin = "python3"
	}
	b64 := base64.StdEncoding.EncodeToString(input)
	var check, entryRef string
	if len(spec.files) == 1 {
		check = `p="$HOME/` + spec.entryRelPath() + `"; [ -f "$p" ] || exit 86; `
		entryRef = `"$p"`
	} else {
		var checks []string
		for _, f := range spec.files {
			checks = append(checks, `[ -f "$d/`+spec.hash+f.suffix+`" ]`)
		}
		check = `d="$HOME/` + spec.relDir + `"; ` + strings.Join(checks, " && ") + ` || exit 86; `
		entryRef = `"$d/` + spec.hash + spec.entry + `"`
	}
	cmd := check + "exec " + shellescape.QuoteCommand([]string{pythonBin, "-I", "-c", guestScriptSharedRunner, b64}) + " " + entryRef
	if len(args) > 0 {
		cmd += " " + shellescape.QuoteCommand(args)
	}
	return cmd
}

// Linux caps any single execve argv element at MAX_ARG_STRLEN (128 KiB).
// guest-session.py runs each shared script as one `/bin/bash -c <script>`
// argument, so keep the encoded shared install script below 120 KiB and fall
// through directly to dedicated stdin SSH upload for larger bundles.
const maxSharedScriptArgLen = 120 << 10

func (s *Server) ensureGuestScript(ctx context.Context, spec *guestScriptSpec, pythonBin string) error {
	return s.guestScripts.ensure(ctx, spec.cacheKey(), func(ctx context.Context) error {
		if s.windowsGuest == nil && s.macosGuest == nil && base64.StdEncoding.EncodedLen(len(spec.installPayload())) < maxSharedScriptArgLen {
			if script := spec.sharedInstallScript(pythonBin); len(script) <= maxSharedScriptArgLen {
				reply, ok, err := s.guestShared(ctx, script)
				if ok && err == nil && reply.code == 0 {
					return nil
				}
				if ctx.Err() != nil {
					return &guestScriptEnsureError{err: ctx.Err()}
				}
			}
		}
		cmd := s.guestScriptInstallSSHCommand(ctx, spec, pythonBin)
		var stderr fileControlStderr
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(diagnostics.Redact(string(stderr.data)))
			if len(msg) > 240 {
				msg = ""
			}
			return &guestScriptEnsureError{msg: msg, err: err}
		}
		return nil
	})
}

func (s *Server) runWithGuestScript(ctx context.Context, spec *guestScriptSpec, pythonBin string, run func() (int, error)) error {
	if err := s.ensureGuestScript(ctx, spec, pythonBin); err != nil {
		return err
	}
	exitCode, err := run()
	if err != nil && exitCode == guestScriptMissingExitCode {
		s.guestScripts.invalidate(spec.cacheKey())
		if ensureErr := s.ensureGuestScript(ctx, spec, pythonBin); ensureErr != nil {
			return ensureErr
		}
		_, err = run()
	}
	return err
}

func exitCodeOf(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
