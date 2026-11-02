package smarthttp

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

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

// errTooLarge is returned by limitReader past its limit.
var errTooLarge = errors.New("request body too large")

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

	protocol := gitProtocol(r)
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	noCache(w.Header())
	out := &streamWriter{w: w, rc: http.NewResponseController(w)}
	// Protocol v0 starts with a line naming the service; v2 does not
	// (as in git http-backend).
	if !isV2(protocol) {
		out.prefix = append(pktLine("# service="+service+"\n"), "0000"...)
	}

	err := s.opts.Git.Stream(r.Context(), gitcmd.StreamOptions{Env: protocolEnv(protocol), Stdout: out},
		s.gitArgs(service, "--stateless-rpc", "--advertise-refs", s.opts.Layout.RepoPath(id))...)
	s.finish(w, r, out, err, false)
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
		body, err := requestBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
			return
		}
		var limited *limitReader
		if service == serviceUploadPack {
			// A push is bounded by git itself (receive.maxInputSize).
			limited = &limitReader{r: body, n: s.opts.MaxFetchRequestSize}
			body = limited
		}

		rc := http.NewResponseController(w)
		// Git may answer before it has read the whole request, for example
		// to reject a push that is too large.
		_ = rc.EnableFullDuplex()
		w.Header().Set("Content-Type", "application/x-"+service+"-result")
		noCache(w.Header())
		out := &streamWriter{w: w, rc: rc}
		opts := gitcmd.StreamOptions{Env: protocolEnv(gitProtocol(r)), Stdin: body, Stdout: out}

		ctx := r.Context()
		var refsFile string
		if service == serviceReceivePack {
			ctx = s.pushCtx
			if refsFile, err = s.newRefsFile(); err != nil {
				s.finish(w, r, out, err, false)
				return
			}
			defer s.removeRefsFile(refsFile)
			opts.Env = append(opts.Env, envPushedRefs+"="+refsFile)
		}
		ctx, cancel := context.WithTimeout(ctx, maxGitDuration)
		defer cancel()

		err = s.opts.Git.Stream(ctx, opts, s.gitArgs(service, "--stateless-rpc", s.opts.Layout.RepoPath(id))...)
		s.finish(w, r, out, err, limited != nil && limited.exceeded.Load())

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

// finish ends a response after git returned. Once git's output has started
// the status is sent, so a failure can only cut the response short; the
// client then reports a protocol error.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, out *streamWriter, err error, tooLarge bool) {
	switch {
	case err == nil:
		out.flushPrefix()
	case out.started():
		noteError(w, err)
	case r.Context().Err() != nil:
		noteError(w, err) // the client went away; nobody reads an answer
	case tooLarge:
		noteError(w, err)
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
	default:
		noteError(w, err)
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
func requestBody(r *http.Request) (io.Reader, error) {
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
		return r.Body, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, errors.New("the request body is not valid gzip")
		}
		return zr, nil
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", enc)
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
// A write error means the client went away. The writer then drops the
// rest of the output without failing, so git still runs to the end: a
// push that git accepted must complete and be recorded.
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
	r io.Reader
	n int64 // bytes left
	// exceeded is read by the handler while the copy to git may still run.
	exceeded atomic.Bool
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.exceeded.Load() {
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
	l.exceeded.Store(true)
	return n, errTooLarge
}
