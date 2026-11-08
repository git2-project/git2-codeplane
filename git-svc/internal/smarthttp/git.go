package smarthttp

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// The two Git services, as they appear in URLs and content types.
const (
	serviceUploadPack  = "git-upload-pack"  // clone and fetch
	serviceReceivePack = "git-receive-pack" // push
)

// protocolRE matches the Git-Protocol header that git sends, such as
// "version=2". It becomes GIT_PROTOCOL for git; anything else is ignored,
// and git falls back to protocol v0.
var protocolRE = regexp.MustCompile(`^[0-9A-Za-z=:._-]{1,256}$`)

// errTooLarge is returned by limitReader and commandsLimitReader past their
// limits.
var errTooLarge = errors.New("request body too large")

// errBodyStalled is returned by guardedBody when the client sent nothing
// for the stall timeout.
var errBodyStalled = errors.New("the client stopped sending the request body")

// infoRefs serves the ref advertisement that starts every clone, fetch and
// push.
func (s *Server) infoRefs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != serviceUploadPack && service != serviceReceivePack {
		// Without ?service= the client is asking for the old "dumb" HTTP
		// protocol, which reads repository files directly.
		http.Error(w, "only Git's smart HTTP protocol is supported", http.StatusForbidden)
		return
	}
	id, _, ok := s.prepare(w, r, service)
	if !ok {
		return
	}
	release, ok := s.acquireGitSlot(w)
	if !ok {
		return
	}
	defer release()

	protocol := gitProtocol(r)
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	noCache(w.Header())
	out := &streamWriter{w: w, rc: http.NewResponseController(w)}
	// Protocol v0 starts with a line naming the service; v2 does not
	// (as in git http-backend).
	if !isV2(protocol) {
		out.prefix = append(pktLine("# service="+service+"\n"), "0000"...)
	}

	ctx, cancel := context.WithTimeout(r.Context(), maxGitDuration)
	defer cancel()

	err := s.opts.Git.Stream(ctx, gitcmd.StreamOptions{Env: protocolEnv(protocol), Stdout: out},
		s.gitArgs(service, "--stateless-rpc", "--advertise-refs", s.opts.Layout.RepoPath(id))...)
	s.finish(w, r, out, err)
}

// rpc returns the handler of a service's POST request: the fetch
// negotiation and pack, or the push commands and pack.
func (s *Server) rpc(service string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, pusher, ok := s.prepare(w, r, service)
		if !ok {
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-"+service+"-request" {
			http.Error(w, "Content-Type must be application/x-"+service+"-request", http.StatusUnsupportedMediaType)
			return
		}
		rc := http.NewResponseController(w)
		guarded := &guardedBody{r: r.Body, rc: rc, timeout: s.opts.StallTimeout}
		defer guarded.close()
		body, err := requestBody(guarded, r.Header.Get("Content-Encoding"))
		switch {
		case errors.Is(err, errBodyStalled):
			noteError(w, err)
			answerStalled(w)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
			return
		}
		if service == serviceUploadPack {
			body = &limitReader{r: body, n: s.opts.MaxFetchRequestSize}
		} else {
			// Git bounds the pack itself (receive.maxInputSize), not the
			// commands before it.
			body = &commandsLimitReader{r: body, left: s.opts.MaxPushCommandsSize}
		}

		release, ok := s.acquireGitSlot(w)
		if !ok {
			return
		}
		defer release() // also covers the git calls that record a push

		// Git may answer before it has read the whole request, for example
		// to reject a push that is too large.
		_ = rc.EnableFullDuplex()
		w.Header().Set("Content-Type", "application/x-"+service+"-result")
		noCache(w.Header())
		out := &streamWriter{w: w, rc: rc}
		opts := gitcmd.StreamOptions{Env: protocolEnv(gitProtocol(r)), Stdin: body, StopStdin: guarded.stop, Stdout: out}

		ctx := r.Context()
		var refsFile string
		if service == serviceReceivePack {
			ctx = s.pushCtx
			if refsFile, err = s.newRefsFile(); err != nil {
				s.finish(w, r, out, err)
				return
			}
			defer s.removeRefsFile(refsFile)
			opts.Env = append(opts.Env, envPushedRefs+"="+refsFile)
		}
		ctx, cancel := context.WithTimeout(ctx, maxGitDuration)
		defer cancel()

		err = s.opts.Git.Stream(ctx, opts, s.gitArgs(service, "--stateless-rpc", s.opts.Layout.RepoPath(id))...)
		s.finish(w, r, out, err)

		if service == serviceReceivePack {
			// Even after an error: refs updated before it stay updated.
			s.recordPush(r, id, pusher, refsFile)
		}
	}
}

// prepare checks what every Git request needs: a repository ID in the
// path, a live repository and, for a push, the user. It returns the
// repository and the pusher (uuid.Nil for a fetch). When the request
// cannot go on, prepare has answered it and returns ok = false.
func (s *Server) prepare(w http.ResponseWriter, r *http.Request, service string) (id, pusher uuid.UUID, ok bool) {
	name, isGit := strings.CutSuffix(r.PathValue("repo"), ".git")
	id, err := repo.ParseID("id", name)
	if !isGit || err != nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return uuid.Nil, uuid.Nil, false
	}
	if service == serviceReceivePack {
		if pusher, err = repo.ParseID(HeaderUserID, r.Header.Get(HeaderUserID)); err != nil {
			http.Error(w, "pushing needs a signed-in user", http.StatusForbidden)
			return uuid.Nil, uuid.Nil, false
		}
	}
	switch _, err := s.opts.Repos.Get(r.Context(), id); {
	case errors.Is(err, repo.ErrNotFound):
		http.Error(w, "repository not found", http.StatusNotFound)
		return uuid.Nil, uuid.Nil, false
	case err != nil:
		noteError(w, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return uuid.Nil, uuid.Nil, false
	}
	return id, pusher, true
}

// acquireGitSlot takes one of the slots for Git operations and returns the
// function that gives it back. When none is free, it answers the request and
// returns ok = false: a client that waited here would hold its connection
// and a place in a queue that stalled clients could fill.
func (s *Server) acquireGitSlot(w http.ResponseWriter) (release func(), ok bool) {
	select {
	case s.gitSlots <- struct{}{}:
		return func() { <-s.gitSlots }, true
	default:
		noteError(w, errors.New("too many Git operations are running"))
		w.Header().Set("Retry-After", busyRetryAfter)
		http.Error(w, "too many Git operations are running; try again later", http.StatusServiceUnavailable)
		return nil, false
	}
}

// answerStalled tells a client that its request body stopped arriving. The
// connection is in the middle of that body, so it cannot serve another
// request.
func answerStalled(w http.ResponseWriter) {
	w.Header().Set("Connection", "close")
	http.Error(w, "timed out waiting for the request body", http.StatusRequestTimeout)
}

// finish ends a response after git returned. Once git's output has started
// the status is sent, so a failure can only cut the response short; the
// client then reports a protocol error.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, out *streamWriter, err error) {
	if err != nil {
		noteError(w, err)
	}
	if out.err != nil {
		noteError(w, out.err)
	}
	switch {
	case out.started():
	case errors.Is(err, errBodyStalled):
		answerStalled(w)
	case errors.Is(err, errTooLarge):
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
	case err == nil:
		out.flushPrefix()
	case r.Context().Err() != nil:
		// The client went away; nobody reads an answer.
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// gitArgs returns git's arguments for a service: the subcommand, and for a
// push the settings that git-svc enforces.
func (s *Server) gitArgs(service string, args ...string) []string {
	cmd := strings.TrimPrefix(service, "git-")
	if service != serviceReceivePack {
		return append([]string{cmd}, args...)
	}
	return append([]string{
		// git-svc's hooks replace the repository's own (ADR 0012).
		"-c", "core.hooksPath=" + s.opts.Layout.HooksDir(),
		"-c", "receive.maxInputSize=" + strconv.FormatInt(s.opts.MaxPushSize, 10),
		// Maintenance (gc) is not run inside a push; see ADR 0012.
		"-c", "receive.autogc=false",
		cmd,
	}, args...)
}

// requestBody returns the request body, decompressed if git compressed it.
// Reading the gzip header can fail with errBodyStalled.
func requestBody(body io.Reader, encoding string) (io.Reader, error) {
	switch encoding {
	case "", "identity":
		return body, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(body)
		if errors.Is(err, errBodyStalled) {
			return nil, err
		}
		if err != nil {
			return nil, errors.New("the request body is not valid gzip")
		}
		return zr, nil
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", encoding)
	}
}

// gitProtocol returns the request's Git-Protocol header if it is valid.
func gitProtocol(r *http.Request) string {
	if v := r.Header.Get("Git-Protocol"); protocolRE.MatchString(v) {
		return v
	}
	return ""
}

// protocolEnv passes the Git-Protocol header to git.
func protocolEnv(protocol string) []string {
	if protocol == "" {
		return nil
	}
	return []string{"GIT_PROTOCOL=" + protocol}
}

// isV2 reports whether the client asked for protocol version 2.
func isV2(protocol string) bool {
	return slices.Contains(strings.Split(protocol, ":"), "version=2")
}

// pktLine encodes s as one pkt-line: four hex digits of length, then s.
func pktLine(s string) []byte {
	return fmt.Appendf(nil, "%04x%s", len(s)+4, s)
}

// noCache keeps proxies and clients from caching a response, as git
// http-backend does: refs change with every push.
func noCache(h http.Header) {
	h.Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	h.Set("Pragma", "no-cache")
	h.Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}

// streamWriter sends git's output to the client and flushes after every
// write, so that packs and progress stream instead of piling up.
//
// A write error means the client went away or stopped reading for the stall
// timeout (recorder sets the deadline). The writer then drops the rest of the
// output without failing, so git still runs to the end: a push that git
// accepted must complete and be recorded. A clone or fetch ends sooner, as
// the failed write cancels the request's context.
type streamWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
	// prefix is written before git's first byte, so that the status can
	// still change if git fails before writing anything.
	prefix  []byte
	written int64
	err     error // the first write error
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if s.flushPrefix() {
		s.write(p)
	}
	return len(p), nil
}

// flushPrefix writes the prefix if it is still pending. It reports whether
// the client can still be written to.
func (s *streamWriter) flushPrefix() bool {
	if s.prefix != nil {
		p := s.prefix
		s.prefix = nil
		s.write(p)
	}
	return s.err == nil
}

func (s *streamWriter) write(p []byte) {
	if s.err != nil || len(p) == 0 {
		return
	}
	n, err := s.w.Write(p)
	s.written += int64(n)
	if err == nil {
		err = s.rc.Flush()
	}
	if err != nil {
		s.err = err
	}
}

// started reports whether the response has begun, after which its status
// cannot change.
func (s *streamWriter) started() bool {
	return s.written > 0 || s.err != nil
}

// limitReader reads at most n bytes; past that it fails with errTooLarge.
// Like http.MaxBytesReader, it reads one byte more to tell "exactly n"
// from "more than n".
type limitReader struct {
	r        io.Reader
	n        int64 // bytes left
	exceeded bool
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.exceeded {
		return 0, errTooLarge
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	if int64(n) <= l.n {
		l.n -= int64(n)
		return n, err
	}
	n = int(l.n)
	l.n = 0
	l.exceeded = true
	return n, errTooLarge
}

// commandsLimitReader bounds the first section of a push request: the
// pkt-lines up to the first flush-pkt, which hold the ref updates. Git keeps
// them all in memory, and receive.maxInputSize bounds only the pack that
// follows. Without this limit, a small compressed body could grow into
// gigabytes of commands inside receive-pack. The pack passes through
// uncounted.
type commandsLimitReader struct {
	r      io.Reader
	left   int64   // bytes of the section still allowed; below 0 past the limit
	header [4]byte // the length of the current pkt-line, as far as it was read
	filled int     // how much of header was read
	body   int64   // bytes of the current pkt-line still to come
	done   bool    // the section has ended
}

func (l *commandsLimitReader) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, errTooLarge
	}
	n, err := l.r.Read(p)
	if !l.done {
		l.left -= int64(l.scan(p[:n]))
		if l.left < 0 {
			return 0, errTooLarge // git never sees the bytes past the limit
		}
	}
	return n, err
}

// scan follows the pkt-lines in p and returns how many bytes of p belong to
// the section.
func (l *commandsLimitReader) scan(p []byte) int {
	i := 0
	for i < len(p) && !l.done {
		if l.body > 0 {
			step := min(l.body, int64(len(p)-i))
			l.body -= step
			i += int(step)
			continue
		}
		l.header[l.filled] = p[i]
		l.filled++
		i++
		if l.filled < len(l.header) {
			continue
		}
		l.filled = 0
		size, err := strconv.ParseUint(string(l.header[:]), 16, 16)
		if err != nil || size < 4 {
			// A flush-pkt, or one of the other special packets, ends the
			// section for git too. Anything else is no pkt-line, and git
			// stops there with a protocol error.
			l.done = true
			break
		}
		l.body = int64(size) - 4
	}
	return i
}

// guardedBody is the request body with a deadline on every read, so that a
// client that stops sending cannot hold a git process. A slow client is fine
// as long as data keeps arriving, and nothing limits the time git takes
// between two reads.
//
// net/http cancels the request's context when a read fails, so a timed-out
// read also stops a clone or fetch. A push runs on a context of its own: git
// sees the end of its input, finds the pack incomplete and changes no ref.
type guardedBody struct {
	r       io.ReadCloser
	rc      *http.ResponseController
	timeout time.Duration

	mu      sync.Mutex
	ended   bool // the body was read to its end
	stopped bool // stop was called
}

func (b *guardedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.ended {
		b.mu.Unlock()
		return 0, io.EOF
	}
	if !b.stopped {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.timeout))
	}
	b.mu.Unlock()

	n, err := b.r.Read(p)
	if err == nil {
		return n, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case errors.Is(err, io.EOF):
		// net/http now watches the connection in the background to notice a
		// client that goes away. A deadline left behind would look like that
		// and cancel a clone that is still running.
		b.ended = true
		_ = b.rc.SetReadDeadline(time.Time{})
	case !b.stopped && errors.Is(err, os.ErrDeadlineExceeded):
		err = errBodyStalled
	}
	return n, err
}

// stop makes a blocked Read return, once git does not need the rest of the
// body. After the end of the body it does nothing, as a deadline would then
// hit net/http's background read.
func (b *guardedBody) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ended {
		b.stopped = true
		_ = b.rc.SetReadDeadline(time.Now())
	}
}

// close ends the body before the handler returns, when no Read is running.
//
// It must happen here. If a full-duplex handler returns with part of the body
// unread, net/http closes the body after it stopped its background read. When
// that close reaches the end of the body, the background read starts again,
// and the server panics ("invalid concurrent Body.Read call") as soon as it
// turns to the connection's next request. Closing the body first avoids that:
// net/http stops the read that this close may start.
//
// Closing looks for the end of the body, so it gets a short deadline: long
// enough for a client that is still sending to see the answer before the
// connection closes, too short for one that stopped sending to delay it.
func (b *guardedBody) close() {
	b.mu.Lock()
	if !b.ended && !b.stopped {
		_ = b.rc.SetReadDeadline(time.Now().Add(bodyCloseGrace))
	}
	b.mu.Unlock()
	_ = b.r.Close()
}
