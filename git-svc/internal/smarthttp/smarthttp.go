// Package smarthttp serves Git's smart HTTP protocol, so that a standard
// git client can clone, fetch and push, and the health endpoints of
// git-svc's HTTP port (ADR 0012):
//
//	GET  /<id>.git/info/refs?service=git-upload-pack   refs, for clone and fetch
//	POST /<id>.git/git-upload-pack                     clone and fetch
//	GET  /<id>.git/info/refs?service=git-receive-pack  refs, for push
//	POST /<id>.git/git-receive-pack                    push
//	GET  /healthz                                      the process is up
//	GET  /readyz                                       the database is reachable
//
// The protocol is git's own: every Git request runs git upload-pack or git
// receive-pack with --stateless-rpc and streams the request and the
// response through it.
//
// git-svc does not authenticate. The gateway signs the user in, turns
// /<owner>/<repo>.git into the repository ID and sets X-Git2-User-Id
// (gateway contract v0). A push without that header is refused.
package smarthttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// HeaderUserID carries the ID of the signed-in user. The gateway sets it
// and removes any value the client sent.
const HeaderUserID = "X-Git2-User-Id"

// headerRequestID is the gateway's request ID, logged with each request.
const headerRequestID = "X-Request-Id"

// Default limits; Options can change them.
const (
	// DefaultMaxPushSize bounds the pack of one push. Git enforces it
	// (receive.maxInputSize) and tells the client why the push failed.
	DefaultMaxPushSize = 2 << 30 // 2 GiB
	// DefaultMaxFetchRequestSize bounds the body of a clone or fetch
	// request after decompression. It holds the client's wants and haves,
	// never a pack; git's own http-backend allows 10 MB.
	DefaultMaxFetchRequestSize = 64 << 20 // 64 MiB
)

const (
	// maxGitDuration bounds one clone, fetch or push.
	maxGitDuration = 2 * time.Hour
	// readyTimeout bounds the readiness check.
	readyTimeout = 2 * time.Second
	// readHeaderTimeout and idleTimeout protect the server from clients
	// that hold connections open. Bodies have no timeout: a large push
	// can take long.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

// Repositories finds live repositories.
type Repositories interface {
	// Get returns repo.ErrNotFound for an unknown or deleted repository.
	Get(ctx context.Context, id uuid.UUID) (repo.Repository, error)
}

// Pushes records pushes that changed refs.
type Pushes interface {
	// RecordPush returns repo.ErrNotFound if the repository was deleted.
	RecordPush(ctx context.Context, p repo.Push) error
}

// Options configure a Server. Log, Layout, Git, Repos and Pushes are
// required.
type Options struct {
	Log    *slog.Logger
	Layout *storage.Layout
	Git    *gitcmd.Runner
	Repos  Repositories
	Pushes Pushes
	// Ready reports whether git-svc's dependencies are reachable; /readyz
	// calls it. Nil means always ready.
	Ready func(context.Context) error
	// MaxPushSize and MaxFetchRequestSize are in bytes; zero means the
	// default.
	MaxPushSize         int64
	MaxFetchRequestSize int64
}

// Server is git-svc's HTTP server.
type Server struct {
	opts Options
	mux  *http.ServeMux
	http *http.Server

	// pushCtx bounds pushes instead of the request's context: once git has
	// taken a push, the push completes and is recorded even if the client
	// goes away. It ends only when a shutdown runs out of time.
	pushCtx      context.Context
	cancelPushes context.CancelFunc
}

// New builds the server and installs the hook that reports pushed refs. It
// fails if the hook cannot run.
func New(opts Options) (*Server, error) {
	if opts.MaxPushSize <= 0 {
		opts.MaxPushSize = DefaultMaxPushSize
	}
	if opts.MaxFetchRequestSize <= 0 {
		opts.MaxFetchRequestSize = DefaultMaxFetchRequestSize
	}
	if err := installHooks(opts.Layout.HooksDir()); err != nil {
		return nil, err
	}
	if err := checkHooks(opts.Layout.HooksDir()); err != nil {
		return nil, err
	}

	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.pushCtx, s.cancelPushes = context.WithCancel(context.Background())

	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.HandleFunc("GET /{repo}/info/refs", s.infoRefs)
	s.mux.HandleFunc("POST /{repo}/"+serviceUploadPack, s.rpc(serviceUploadPack))
	s.mux.HandleFunc("POST /{repo}/"+serviceReceivePack, s.rpc(serviceReceivePack))

	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(opts.Log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Serve serves on lis until Shutdown is called; then it returns nil.
func (s *Server) Serve(lis net.Listener) error {
	if err := s.http.Serve(lis); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops accepting connections and waits for in-flight requests,
// pushes included, to finish. If ctx ends first, the remaining git
// processes are stopped and their connections closed.
func (s *Server) Shutdown(ctx context.Context) {
	if err := s.http.Shutdown(ctx); err != nil {
		s.cancelPushes()
		_ = s.http.Close()
	}
	s.cancelPushes()
}

// ServeHTTP routes a request and writes one log line for it.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w}
	body := &countingBody{ReadCloser: r.Body}
	r.Body = body

	s.mux.ServeHTTP(rec, r)

	status := rec.status
	if status == 0 {
		status = http.StatusOK // the handler wrote nothing
	}
	level := slog.LevelInfo
	switch {
	case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
		// Probes call them every few seconds.
		level = slog.LevelDebug
		if status >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
	case status >= http.StatusInternalServerError:
		level = slog.LevelError
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Int64("bytes_in", body.n),
		slog.Int64("bytes_out", rec.written),
		slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
	}
	if v := r.URL.Query().Get("service"); v != "" {
		attrs = append(attrs, slog.String("service", v))
	}
	if v := r.Header.Get(headerRequestID); v != "" {
		attrs = append(attrs, slog.String("request_id", v))
	}
	if v := r.Header.Get(HeaderUserID); v != "" {
		attrs = append(attrs, slog.String("user_id", v))
	}
	if rec.err != nil {
		attrs = append(attrs, slog.String("err", rec.err.Error()))
	}
	s.opts.Log.LogAttrs(r.Context(), level, "http request", attrs...)
}

// healthz reports that the process is up.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// readyz reports whether git-svc can serve requests.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.opts.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := s.opts.Ready(ctx); err != nil {
			noteError(w, err)
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// recorder notes a response's status and size, and an error to log with
// the request, for the access log.
type recorder struct {
	http.ResponseWriter
	status  int
	written int64
	err     error // logged, never sent to the client
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the connection, to flush.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// noteError attaches err to the request's log line. The first error wins.
func noteError(w http.ResponseWriter, err error) {
	if rec, ok := w.(*recorder); ok && rec.err == nil {
		rec.err = err
	}
}

// countingBody counts the bytes read from a request body.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}
