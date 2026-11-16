package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
)

// maxSignInBytes bounds a harness's login file.
const maxSignInBytes = 1 << 20

// signInPaths are the harness's login file in the runtime's sign-in
// directory, its login store, and in its native home (ADR 0047).
func signInPaths(c config) (stored, native string, ok bool) {
	h, err := harness.Managed(c.Harness)
	if c.SignInDirectory == "" || err != nil || h.SignInFile == "" {
		return "", "", false
	}
	return filepath.Join(c.SignInDirectory, h.SignInFile),
		filepath.Join(harness.NativeSessionRoot, h.NativeDirectory, h.SignInFile), true
}

// loginSync keeps a harness's login file and its login store the same while
// the harness runs (ADR 0049). Every session of one login shares the store,
// so a sign-in, refresh or sign-out in one reaches the others, as it would
// between processes on one machine. The harness writes its own file; the
// supervisor copies a change to the store, and a change another session made
// to the store back into the file. A store this supervisor can't write, such
// as a Secret, only flows in. Empty and missing both mean "not signed in".
//
// Only a whole login reaches the store: a harness killed while rewriting its
// file can leave it cut short, or empty, and the store is every session's.
// A file that is not a whole login is never copied out. Once a stop has
// begun, an empty or missing file is not a sign-out; and once the supervisor
// has killed the harness, or the deadline has passed, nothing is copied out.
type loginSync struct {
	stored, native string
	mu             sync.Mutex
	// agreed is the content both sides last held, once known.
	agreed []byte
	known  bool
	// level is loginRunning, loginStopping or loginForced; it only rises.
	level atomic.Int32
	// deadline is the launch's; once it passes the harness may have been
	// killed at it, as by loginForced.
	deadline time.Time
	// quit ends run, which closes stopped; once makes stop idempotent.
	quit, stopped chan struct{}
	once          sync.Once
	// diagnostics receives diagnostic lines; nil is standard error.
	diagnostics io.Writer
	// wait replaces loginSaveWait when set.
	wait time.Duration
	// nativeOut and storeOut count consecutive syncs that saw the harness's
	// file, or the store, newly signed out.
	nativeOut, storeOut int
}

// signOutSyncs is how many consecutive syncs must see a login file or the
// store signed out before the sign-out is shared: Codex and Claude Code
// rewrite their login by truncating it first, so one sync can catch it empty
// while the harness runs normally, and the store can be caught the same way.
const signOutSyncs = 2

// loginInterval is how often a running harness's login is compared with its
// store.
var loginInterval = time.Second

// loginSaveWait bounds each sync's wait for the store's lock, and stop's
// whole save. A peer that holds the lock, as on a stalled volume, can then
// never hold an activation, a stop or the supervisor's exit.
var loginSaveWait = 5 * time.Second

// loginLockPoll is how often a sync tries the store's lock again.
const loginLockPoll = 20 * time.Millisecond

// What the harness's login file may be trusted for.
const (
	// loginRunning: the harness runs, and an empty or missing file is its
	// sign-out.
	loginRunning int32 = iota
	// loginStopping: a stop has begun, so an empty or missing file may be a
	// rewrite the coming kill cuts short; a whole login is still copied out.
	loginStopping
	// loginForced: the supervisor kills or killed the harness, which can
	// land while it rewrites its file; nothing is copied out.
	loginForced
)

// stopping records that a stop has begun. A nil sync does nothing.
func (l *loginSync) stopping() { l.raise(loginStopping) }

// forced records, before the supervisor kills the harness, that its login
// file may be left half written. A nil sync does nothing.
func (l *loginSync) forced() { l.raise(loginForced) }

func (l *loginSync) raise(level int32) {
	if l == nil {
		return
	}
	for {
		current := l.level.Load()
		if current >= level || l.level.CompareAndSwap(current, level) {
			return
		}
	}
}

// trust is what the login file read just before may be trusted for. It is
// read after the file, so a kill recorded before it landed is seen.
func (l *loginSync) trust() int32 {
	if deadline.At(l.deadline).Passed(time.Now()) {
		return loginForced
	}
	return l.level.Load()
}

// wholeLogin reports whether data is a complete login file. Every managed
// harness keeps its login as one JSON object: Codex's auth.json, Claude
// Code's .credentials.json and Copilot's config.json, after the header of
// comment lines Copilot writes. A file cut short, or one rewritten in place
// over a longer one, does not parse as one.
func wholeLogin(data []byte) bool {
	body := bytes.TrimSpace(withoutCommentLines(data))
	return len(body) > 0 && body[0] == '{' && json.Valid(body)
}

// withoutCommentLines drops the whole-line // comments Copilot writes at the
// top of its config.json.
func withoutCommentLines(data []byte) []byte {
	var body []byte
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("//")) {
			body = append(body, line...)
			body = append(body, '\n')
		}
	}
	return body
}

// startLoginSync copies the stored login into the native home and keeps
// the two in step until stop. It returns nil when the harness has no login
// store.
func startLoginSync(c config) (*loginSync, error) {
	stored, native, ok := signInPaths(c)
	if !ok {
		return nil, nil
	}
	l := &loginSync{stored: stored, native: native, deadline: c.Deadline,
		quit: make(chan struct{}), stopped: make(chan struct{})}
	if err := l.begin(); err != nil {
		return nil, err
	}
	return l, nil
}

// errStoreNotWhole is a store that holds no whole login: a writer stopped
// part way, or it holds something else.
var errStoreNotWhole = errors.New("the login store is not a whole login")

// begin runs the first sync and starts the periodic one. A store that holds
// no whole login starts the harness without one, rather than failing the
// launch or giving it the store's bytes; its own sign-in then replaces it.
func (l *loginSync) begin() error {
	err := l.sync()
	if errors.Is(err, errStoreNotWhole) {
		l.diagnose("Started the harness without a sign-in: %v", err)
		l.mu.Lock()
		l.known = true
		l.mu.Unlock()
	} else if err != nil {
		return err
	}
	go l.run()
	return nil
}

func (l *loginSync) run() {
	defer close(l.stopped)
	// Stopping ends a periodic sync's wait for the lock at once.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-l.quit
		cancel()
	}()
	ticker := time.NewTicker(loginInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.quit:
			return
		case <-ticker.C:
			if err := l.syncWithin(ctx); err != nil {
				l.diagnose("Failed to sync the harness's sign-in with its login store: %v", err)
			}
		}
	}
}

// stop ends the periodic sync and saves the harness's last change, once,
// before the supervisor exits. The save waits at most loginSaveWait: the
// harness is stopped and its checkpoint matters more, so a store that stays
// locked or unavailable keeps what it had, and the missed save is logged. A
// nil sync does nothing.
func (l *loginSync) stop() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.stopping()
		close(l.quit)
		ctx, cancel := context.WithTimeout(context.Background(), l.saveWait())
		defer cancel()
		saved := make(chan error, 1)
		go func() {
			<-l.stopped
			saved <- l.syncWithin(ctx)
		}()
		select {
		case err := <-saved:
			if err != nil {
				l.diagnose("Skipped saving the harness's sign-in to its login store: %v", err)
			}
		case <-ctx.Done():
			l.diagnose("Skipped saving the harness's sign-in after its login store was unavailable for %s",
				l.saveWait())
		}
	})
}

// saveWait is the bound on this sync's wait for its store.
func (l *loginSync) saveWait() time.Duration {
	if l.wait > 0 {
		return l.wait
	}
	return loginSaveWait
}

// diagnose writes one line to diagnostics, or to standard error.
func (l *loginSync) diagnose(format string, args ...any) {
	out := l.diagnostics
	if out == nil {
		out = os.Stderr
	}
	_, _ = fmt.Fprintf(out, format+"\n", args...)
}

// sync is syncWithin with no context but its own bound.
func (l *loginSync) sync() error {
	return l.syncWithin(context.Background())
}

// syncWithin compares both sides under the store's lock, waiting for the
// lock at most loginSaveWait and never after ctx ends. A change in the
// native file wins, as the latest write on one machine would; otherwise a
// change in the store is copied in. A change that may be a cut-short write
// is not copied out (loginSync); the store keeps what it had.
func (l *loginSync) syncWithin(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, l.saveWait())
	defer cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	native, err := readLogin(l.native)
	if err != nil {
		return err
	}
	trust := l.trust()
	if !l.known && native != nil && !wholeLogin(native) {
		// Left half written before this launch: the store's login replaces it.
		native = nil
	}
	changed := l.known && !bytes.Equal(native, l.agreed)
	if changed && native == nil {
		l.nativeOut++
	} else {
		l.nativeOut = 0
	}
	switch {
	case changed && trust == loginForced:
		return errors.New("the harness was killed, which can cut short a rewrite of its login file")
	case trust == loginForced:
		return nil
	case changed && native != nil && !wholeLogin(native):
		return fmt.Errorf("sign-in file %s is not a whole login", l.native)
	case changed && native == nil && trust != loginRunning:
		// Not a sign-out made while the harness ran: the stop may cut a
		// rewrite short. The store keeps the login.
		return nil
	case changed && native == nil && l.nativeOut < signOutSyncs:
		// Perhaps a rewrite caught after its truncate; the next sync decides.
		return nil
	}
	store, writable, err := openStore(ctx, l.stored)
	if err != nil {
		return err
	}
	if store != nil {
		defer func() { _ = store.Close() }()
	}
	stored, err := readStore(store)
	if err != nil {
		return err
	}
	unfinished := stored != nil && !wholeLogin(stored)
	if !changed && l.known && stored == nil && l.agreed != nil {
		l.storeOut++
	} else {
		l.storeOut = 0
	}
	switch {
	case l.storeOut > 0 && l.storeOut < signOutSyncs:
		// Another session's sign-out, or its rewrite caught after a
		// truncate; the next sync decides.
		return nil
	case !l.known && (stored == nil || unfinished) && native != nil, changed:
		// The harness signed in, refreshed or signed out. Its whole login
		// also replaces a store another writer left unfinished.
		if writable {
			if err = writeStore(ctx, store, native); err != nil {
				return err
			}
		}
		l.agreed = native
	case unfinished:
		// Another writer stopped part way, or the store holds something
		// else: the harness keeps its own login.
		return fmt.Errorf("%w: %s", errStoreNotWhole, l.stored)
	case !l.known || !bytes.Equal(stored, l.agreed):
		// The first sync, or another session changed the store.
		if err = writeLogin(l.native, stored); err != nil {
			return err
		}
		l.agreed = stored
	}
	l.known = true
	return nil
}

// openStore opens and locks the stored login: exclusively when this
// supervisor may write it, creating it in a writable claim, and shared when
// it may only read it, as for a Secret. A store that is missing and can't be
// created is nil. A link may only resolve inside the sign-in directory, as a
// Secret's keys do. The wait for the lock ends with ctx.
func openStore(ctx context.Context, name string) (*os.File, bool, error) {
	if err := storeInside(name); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	writable := err == nil
	if errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission) {
		f, err = os.Open(name)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_SH
	if writable {
		how = syscall.LOCK_EX
	}
	if err = lockStore(ctx, f, how); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSignInBytes {
		_ = f.Close()
		return nil, false, fmt.Errorf("sign-in file %s is not a bounded regular file", name)
	}
	return f, writable, nil
}

// lockStore takes the store's lock, trying again until ctx ends rather than
// blocking: a peer that holds it on a stalled volume would otherwise hold
// this supervisor for ever. A lock is never taken once ctx has ended, as
// when an open returned only after the save's bound.
func lockStore(ctx context.Context, f *os.File, how int) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sign-in file %s was not locked before the save's bound: %w", f.Name(), err)
		}
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sign-in file %s stayed locked by another session: %w", f.Name(), ctx.Err())
		case <-time.After(loginLockPoll):
		}
	}
}

// storeInside refuses a stored login that is a link leading out of its
// directory.
func storeInside(name string) error {
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) || err == nil && info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(name))
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil || !strings.HasPrefix(resolved, directory+string(filepath.Separator)) {
		return fmt.Errorf("sign-in file %s links outside its directory", name)
	}
	return nil
}

func readStore(f *os.File) ([]byte, error) {
	if f == nil {
		return nil, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSignInBytes+1))
	if err != nil || len(data) > maxSignInBytes {
		return nil, errors.Join(err, errors.New("stored sign-in exceeds its limit"))
	}
	return nonEmpty(data), nil
}

// storeFile is the store's open file as writeStore uses it.
type storeFile interface {
	WriteAt(data []byte, offset int64) (int, error)
	Truncate(size int64) error
	Sync() error
}

// storeMarker overwrites a store's first byte while it is rewritten. No
// whole login starts with it, so a store left part way through is never a
// login, and never empty.
const storeMarker = '#'

// writeStore replaces the store's content in place, under its lock. It
// cannot rename a new file into place: the lock is the store file's own
// flock, which a rename would leave on the old file while other sessions
// opened the new one unlocked, and a Docker volume's directory belongs to
// root. Truncating first would leave the store empty, a sign-out to every
// other session, if the write then stopped: the supervisor exits once a stop
// stops waiting for its save. So the first byte becomes storeMarker, the rest
// is written and the file cut to length, and the first byte is written last;
// the marker is synced before and the rest before its replacement. A write
// that stops at any point leaves the old login, the new one, or a file that
// is not a whole login, which readers leave alone. Nothing is written once
// ctx has ended, when the save has been reported skipped. Empty data is a
// sign-out, one truncate.
func writeStore(ctx context.Context, f storeFile, data []byte) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("the save was given up before writing the login store: %w", err)
	}
	if len(data) == 0 {
		return errors.Join(f.Truncate(0), f.Sync())
	}
	steps := []func() error{
		func() error { _, err := f.WriteAt([]byte{storeMarker}, 0); return err },
		f.Sync,
		func() error { _, err := f.WriteAt(data[1:], 1); return err },
		func() error { return f.Truncate(int64(len(data))) },
		f.Sync,
		func() error { _, err := f.WriteAt(data[:1], 0); return err },
		f.Sync,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// readLogin reads the harness's own login file; missing or empty is nil.
func readLogin(name string) ([]byte, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSignInBytes {
		return nil, fmt.Errorf("sign-in file %s is not a bounded regular file", name)
	}
	data, err := os.ReadFile(name)
	return nonEmpty(data), err
}

// writeLogin replaces the harness's login file, or removes it for no
// sign-in, through a temporary file so the harness never reads half of it.
func writeLogin(name string, data []byte) error {
	if data == nil {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	return writePrivate(name, data)
}

// nonEmpty is nil for a login file that says "not signed in": empty, or an
// object with nothing in it, after Copilot's comment header. Every other
// whole login has at least one member; which members a client writes is
// left to the client, so a renamed field never stops the store updating.
func nonEmpty(data []byte) []byte {
	body := bytes.TrimSpace(withoutCommentLines(data))
	var object map[string]json.RawMessage
	if len(body) == 0 || body[0] == '{' && json.Unmarshal(body, &object) == nil && len(object) == 0 {
		return nil
	}
	return data
}
