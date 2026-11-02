package smarthttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// These tests run real git on both sides over real HTTP. Repositories and
// pushes are recorded by fakes; the tests with PostgreSQL are in
// smarthttp_integration_test.go.

const zeroID = "0000000000000000000000000000000000000000"

func TestCloneAndPush(t *testing.T) {
	for _, version := range []string{"0", "2"} {
		t.Run("protocol v"+version, func(t *testing.T) {
			env := newTestEnv(t)
			id := env.createRepo(t)
			work := filepath.Join(t.TempDir(), "w")

			env.gitOK(t, "", "-c", "protocol.version="+version, "clone", "--quiet", env.repoURL(id), work)
			sha := env.commit(t, work, "README.md", "hello\n")
			env.gitOK(t, work, "-c", "protocol.version="+version, "-c", env.userHeader(),
				"push", "--quiet", "origin", "HEAD:refs/heads/main")

			env.wantPushes(t, repo.Push{RepositoryID: id, PusherID: env.user, RefUpdates: []repo.RefUpdate{
				{Ref: "refs/heads/main", OldSHA: zeroID, NewSHA: sha},
			}})

			again := filepath.Join(t.TempDir(), "a")
			env.gitOK(t, "", "-c", "protocol.version="+version, "clone", "--quiet", env.repoURL(id), again)
			if got := env.gitOK(t, again, "rev-parse", "HEAD"); got != sha {
				t.Errorf("cloned HEAD = %s, want %s", got, sha)
			}

			// The Git-Protocol header reached the server as git sent it.
			wantProtocol := map[string]string{"0": "", "2": "version=2"}[version]
			for _, req := range env.requests.all() {
				if strings.HasSuffix(req.path, "/"+serviceUploadPack) && req.protocol != wantProtocol {
					t.Errorf("upload-pack request with Git-Protocol %q, want %q", req.protocol, wantProtocol)
				}
			}
		})
	}
}

func TestPushRefUpdates(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	work := env.clone(t, id)
	push := func(args ...string) {
		t.Helper()
		env.gitOK(t, work, append([]string{"-c", env.userHeader(), "push", "--quiet", "origin"}, args...)...)
	}

	first := env.commit(t, work, "a.txt", "1\n")
	push("HEAD:refs/heads/main")
	second := env.commit(t, work, "a.txt", "2\n")
	push("HEAD:refs/heads/main")

	// Rewrite history: back to the first commit, then a different second one.
	env.gitOK(t, work, "reset", "--quiet", "--hard", first)
	rewritten := env.commit(t, work, "a.txt", "2 again\n")
	push("--force", "HEAD:refs/heads/main")

	// Two refs in one push.
	env.gitOK(t, work, "tag", "v1")
	push("HEAD:refs/heads/feature", "refs/tags/v1")
	push(":refs/heads/feature") // delete

	got := env.pushes.all()
	want := [][]repo.RefUpdate{
		{{Ref: "refs/heads/main", OldSHA: zeroID, NewSHA: first}},
		{{Ref: "refs/heads/main", OldSHA: first, NewSHA: second}},
		{{Ref: "refs/heads/main", OldSHA: second, NewSHA: rewritten, Forced: true}},
		{{Ref: "refs/heads/feature", OldSHA: zeroID, NewSHA: rewritten}, {Ref: "refs/tags/v1", OldSHA: zeroID, NewSHA: rewritten}},
		{{Ref: "refs/heads/feature", OldSHA: rewritten, NewSHA: zeroID}},
	}
	if len(got) != len(want) {
		t.Fatalf("%d pushes recorded, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		updates := slices.Clone(got[i].RefUpdates)
		slices.SortFunc(updates, func(a, b repo.RefUpdate) int { return strings.Compare(a.Ref, b.Ref) })
		if !slices.Equal(updates, want[i]) {
			t.Errorf("push %d: ref updates = %+v, want %+v", i+1, updates, want[i])
		}
	}
}

func TestRejectedRefsAreNotRecorded(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	alice, bob := env.clone(t, id), env.clone(t, id)

	base := env.commit(t, alice, "a.txt", "alice\n")
	env.gitOK(t, alice, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")

	// Make the server refuse rewrites, then let Bob force one: the server
	// rejects his main and accepts his new branch in the same push.
	env.gitOK(t, env.layout.RepoPath(id), "config", "receive.denyNonFastForwards", "true")
	mine := env.commit(t, bob, "b.txt", "bob\n")
	err := env.gitFails(t, bob, "-c", env.userHeader(), "push", "--force", "origin",
		"HEAD:refs/heads/main", "HEAD:refs/heads/bob")
	if !strings.Contains(err.Stderr, "[remote rejected]") {
		t.Errorf("push stderr = %q, want a ref rejected by the server", err.Stderr)
	}

	env.wantPushes(t,
		repo.Push{RepositoryID: id, PusherID: env.user, RefUpdates: []repo.RefUpdate{
			{Ref: "refs/heads/main", OldSHA: zeroID, NewSHA: base},
		}},
		repo.Push{RepositoryID: id, PusherID: env.user, RefUpdates: []repo.RefUpdate{
			{Ref: "refs/heads/bob", OldSHA: zeroID, NewSHA: mine},
		}},
	)
}

func TestPushNeedsUser(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	work := env.clone(t, id) // fetching needs no user
	env.commit(t, work, "a.txt", "1\n")

	for _, header := range []string{"", HeaderUserID + ": not-a-uuid"} {
		args := []string{"push", "--quiet", "origin", "HEAD:refs/heads/main"}
		if header != "" {
			args = append([]string{"-c", "http.extraHeader=" + header}, args...)
		}
		err := env.gitFails(t, work, args...)
		if !strings.Contains(err.Stderr, "pushing needs a signed-in user") {
			t.Errorf("header %q: push stderr = %q, want the server's message", header, err.Stderr)
		}
	}

	env.wantPushes(t)
	if refs := env.gitOK(t, env.layout.RepoPath(id), "for-each-ref"); refs != "" {
		t.Errorf("refs after refused pushes = %q, want none", refs)
	}
}

func TestUnknownRepository(t *testing.T) {
	env := newTestEnv(t)
	deleted := env.createRepo(t)
	env.repos.remove(deleted)

	for _, id := range []uuid.UUID{uuid.New(), deleted} {
		err := env.gitFails(t, "", "clone", "--quiet", env.repoURL(id), filepath.Join(t.TempDir(), "w"))
		if !strings.Contains(err.Stderr, "repository not found") {
			t.Errorf("clone of %s: stderr = %q, want 'repository not found'", id, err.Stderr)
		}
	}
	// The directory of the deleted repository is still on disk (it moves to
	// the trash only after the database says so), but it is not served.
	if _, err := os.Stat(env.layout.RepoPath(deleted)); err != nil {
		t.Fatalf("test setup: %v", err)
	}
}

func TestHTTPErrors(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	repoPath := "/" + id.String() + ".git"
	fetchRequest := "application/x-git-upload-pack-request"

	tests := []struct {
		name       string
		method     string
		path       string
		header     map[string]string
		body       string
		wantStatus int
	}{
		{"dumb HTTP", http.MethodGet, repoPath + "/info/refs", nil, "", http.StatusForbidden},
		{"unknown service", http.MethodGet, repoPath + "/info/refs?service=git-upload-archive", nil, "", http.StatusForbidden},
		{"malformed ID", http.MethodGet, "/not-a-uuid.git/info/refs?service=git-upload-pack", nil, "", http.StatusNotFound},
		{"ID without .git", http.MethodGet, "/" + id.String() + "/info/refs?service=git-upload-pack", nil, "", http.StatusNotFound},
		{"unknown repository", http.MethodPost, "/" + uuid.NewString() + ".git/git-upload-pack",
			map[string]string{"Content-Type": fetchRequest}, "0000", http.StatusNotFound},
		{"wrong content type", http.MethodPost, repoPath + "/git-upload-pack",
			map[string]string{"Content-Type": "text/plain"}, "0000", http.StatusUnsupportedMediaType},
		{"unsupported encoding", http.MethodPost, repoPath + "/git-upload-pack",
			map[string]string{"Content-Type": fetchRequest, "Content-Encoding": "br"}, "0000", http.StatusUnsupportedMediaType},
		{"broken gzip", http.MethodPost, repoPath + "/git-upload-pack",
			map[string]string{"Content-Type": fetchRequest, "Content-Encoding": "gzip"}, "not gzip", http.StatusUnsupportedMediaType},
		{"push without user", http.MethodPost, repoPath + "/git-receive-pack",
			map[string]string{"Content-Type": "application/x-git-receive-pack-request"}, "0000", http.StatusForbidden},
		{"GET on an RPC", http.MethodGet, repoPath + "/git-upload-pack", nil, "", http.StatusMethodNotAllowed},
		{"other path", http.MethodGet, "/" + id.String() + ".git/objects/info/packs", nil, "", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := env.do(t, tt.method, tt.path, tt.header, strings.NewReader(tt.body))
			if resp.status != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", resp.status, tt.wantStatus, resp.body)
			}
			if ct := resp.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain, which git shows to the user", ct)
			}
		})
	}
}

func TestLookupFailureIsInternal(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	env.repos.setErr(errors.New("database at /secret/path is down"))

	resp := env.do(t, http.MethodGet, "/"+id.String()+".git/info/refs?service=git-upload-pack", nil, nil)
	if resp.status != http.StatusInternalServerError || strings.Contains(resp.body, "secret") {
		t.Errorf("status %d, body %q; want 500 without internal details", resp.status, resp.body)
	}
}

func TestAdvertisementHeaders(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)

	for _, tt := range []struct {
		service, protocol string
		wantPrefix        string
	}{
		// v0 names the service first, then git's own advertisement.
		{serviceUploadPack, "", "001e# service=git-upload-pack\n0000"},
		{serviceReceivePack, "", "001f# service=git-receive-pack\n0000"},
		// v2 starts with the capability advertisement.
		{serviceUploadPack, "version=2", "000eversion 2\n"},
	} {
		header := map[string]string{HeaderUserID: env.user.String()}
		if tt.protocol != "" {
			header["Git-Protocol"] = tt.protocol
		}
		resp := env.do(t, http.MethodGet, "/"+id.String()+".git/info/refs?service="+tt.service, header, nil)
		if resp.status != http.StatusOK || !strings.HasPrefix(resp.body, tt.wantPrefix) {
			t.Errorf("%s %s: status %d, body %q; want 200 starting with %q", tt.service, tt.protocol, resp.status, resp.body, tt.wantPrefix)
		}
		if got, want := resp.header.Get("Content-Type"), "application/x-"+tt.service+"-advertisement"; got != want {
			t.Errorf("%s: Content-Type = %q, want %q", tt.service, got, want)
		}
		if got := resp.header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
			t.Errorf("%s: Cache-Control = %q, want no-cache", tt.service, got)
		}
	}
}

func TestGzipFetchRequest(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	work := env.clone(t, id)

	// 40 branches on 40 different commits: a clone then wants 40 objects,
	// and git compresses a request of more than 1 KiB.
	const branches = 40
	for i := range branches {
		env.commit(t, work, "n.txt", fmt.Sprintf("%d\n", i))
		env.gitOK(t, work, "branch", fmt.Sprintf("b%02d", i))
	}
	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "refs/heads/*:refs/heads/*")

	again := filepath.Join(t.TempDir(), "a")
	env.gitOK(t, "", "clone", "--quiet", env.repoURL(id), again)
	remote := env.gitOK(t, again, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/b*")
	if n := len(strings.Fields(remote)); n != branches {
		t.Errorf("cloned %d branches, want %d", n, branches)
	}
	gzipped := slices.ContainsFunc(env.requests.all(), func(r loggedRequest) bool {
		return strings.HasSuffix(r.path, "/"+serviceUploadPack) && r.encoding == "gzip"
	})
	if !gzipped {
		t.Error("no gzip-compressed upload-pack request reached the server; the test no longer covers gzip")
	}
}

func TestFetchRequestTooLarge(t *testing.T) {
	env := newTestEnv(t, func(o *Options) { o.MaxFetchRequestSize = 100 })
	id := env.createRepo(t)

	// A protocol v2 ls-refs request: git reads all its arguments before it
	// looks anything up, so the limit is what cuts the request off.
	var body bytes.Buffer
	body.Write(pktLine("command=ls-refs\n"))
	body.WriteString("0001")
	for range 10 {
		body.Write(pktLine("ref-prefix refs/heads/" + strings.Repeat("x", 30) + "\n"))
	}
	body.WriteString("0000")
	resp := env.do(t, http.MethodPost, "/"+id.String()+".git/"+serviceUploadPack, map[string]string{
		"Content-Type": "application/x-git-upload-pack-request",
		"Git-Protocol": "version=2",
	}, &body)
	if resp.status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d (body %q), want 413", resp.status, resp.body)
	}
}

func TestPushTooLarge(t *testing.T) {
	env := newTestEnv(t, func(o *Options) { o.MaxPushSize = 4 << 10 })
	id := env.createRepo(t)
	work := env.clone(t, id)
	env.commit(t, work, "random.bin", string(randomBytes(t, 64<<10))) // does not compress

	err := env.gitFails(t, work, "-c", env.userHeader(), "push", "origin", "HEAD:refs/heads/main")
	if !strings.Contains(err.Stderr, "maximum allowed size") {
		t.Errorf("push stderr = %q, want git's size message", err.Stderr)
	}
	env.wantPushes(t)
}

// git-svc's hooks replace the repository's own: a hook inside a repository
// (which nobody should be able to put there) never runs.
func TestRepositoryHooksDoNotRun(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	hook := filepath.Join(env.layout.RepoPath(id), "hooks", "pre-receive")
	//nolint:gosec // G306: the hook must be executable to prove it is ignored.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho blocked by the repository hook >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	work := env.clone(t, id)
	env.commit(t, work, "a.txt", "1\n")
	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")
	if n := len(env.pushes.all()); n != 1 {
		t.Errorf("%d pushes recorded, want 1", n)
	}
}

func TestConcurrentPushesAreRecordedSeparately(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	const n = 4
	works := make([]string, n)
	shas := make([]string, n)
	for i := range n {
		works[i] = env.clone(t, id)
		shas[i] = env.commit(t, works[i], "a.txt", fmt.Sprintf("%d\n", i))
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, err := env.git.Run(t.Context(), works[i], "-c", env.userHeader(),
				"push", "--quiet", "origin", fmt.Sprintf("HEAD:refs/heads/b%d", i))
			if err != nil {
				t.Errorf("push %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	var got []repo.RefUpdate
	for _, p := range env.pushes.all() {
		if len(p.RefUpdates) != 1 {
			t.Errorf("a push recorded %d ref updates, want 1: %+v", len(p.RefUpdates), p.RefUpdates)
		}
		got = append(got, p.RefUpdates...)
	}
	slices.SortFunc(got, func(a, b repo.RefUpdate) int { return strings.Compare(a.Ref, b.Ref) })
	var want []repo.RefUpdate
	for i := range n {
		want = append(want, repo.RefUpdate{Ref: fmt.Sprintf("refs/heads/b%d", i), OldSHA: zeroID, NewSHA: shas[i]})
	}
	if !slices.Equal(got, want) {
		t.Errorf("recorded %+v, want %+v", got, want)
	}
}

// A push that git accepted stays accepted when its event cannot be
// written; git-svc logs the loss.
func TestPushSucceedsWhenRecordingFails(t *testing.T) {
	var logs syncBuffer
	env := newTestEnv(t, func(o *Options) { o.Log = slog.New(slog.NewTextHandler(&logs, nil)) })
	env.pushes.setErr(errors.New("database is down"))
	id := env.createRepo(t)
	work := env.clone(t, id)
	sha := env.commit(t, work, "a.txt", "1\n")

	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")

	if got := env.gitOK(t, env.layout.RepoPath(id), "rev-parse", "refs/heads/main"); got != sha {
		t.Errorf("main = %s, want %s", got, sha)
	}
	if !strings.Contains(logs.String(), "RepositoryPushed is lost") {
		t.Errorf("logs do not report the lost event:\n%s", logs.String())
	}
}

func TestHealthEndpoints(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	env := newTestEnv(t, func(o *Options) {
		o.Ready = func(context.Context) error {
			if down.Load() {
				return errors.New("database unreachable")
			}
			return nil
		}
	})

	if resp := env.do(t, http.MethodGet, "/healthz", nil, nil); resp.status != http.StatusOK || resp.body != "ok\n" {
		t.Errorf("/healthz = %d %q, want 200 ok", resp.status, resp.body)
	}
	if resp := env.do(t, http.MethodGet, "/readyz", nil, nil); resp.status != http.StatusServiceUnavailable {
		t.Errorf("/readyz with a failing check = %d, want 503", resp.status)
	}
	down.Store(false)
	if resp := env.do(t, http.MethodGet, "/readyz", nil, nil); resp.status != http.StatusOK || resp.body != "ok\n" {
		t.Errorf("/readyz = %d %q, want 200 ok", resp.status, resp.body)
	}
}

func TestInstallHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "post-receive")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := installHooks(dir); err != nil {
		t.Fatalf("installHooks() error = %v", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temp file.
	if err != nil || string(got) != postReceiveHook {
		t.Errorf("hook = %q (error %v), want the current hook", got, err)
	}
	if fi, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0) {
		t.Errorf("hook mode = %v (error %v), want executable", fi.Mode(), err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporary file left behind (stat error %v)", err)
	}
}

func TestParsePushedRefs(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	tests := []struct {
		name    string
		data    string
		want    []repo.RefUpdate
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"one", zeroID + " " + sha1 + " refs/heads/main\n", []repo.RefUpdate{{Ref: "refs/heads/main", OldSHA: zeroID, NewSHA: sha1}}, false},
		{"two, no final newline", sha1 + " " + zeroID + " refs/heads/x\n" + zeroID + " " + sha1 + " refs/tags/v1",
			[]repo.RefUpdate{{Ref: "refs/heads/x", OldSHA: sha1, NewSHA: zeroID}, {Ref: "refs/tags/v1", OldSHA: zeroID, NewSHA: sha1}}, false},
		{"SHA-256", strings.Repeat("0", 64) + " " + sha256 + " refs/heads/main\n",
			[]repo.RefUpdate{{Ref: "refs/heads/main", OldSHA: strings.Repeat("0", 64), NewSHA: sha256}}, false},
		{"missing ref", zeroID + " " + sha1 + "\n", nil, true},
		{"short ID", "abc " + sha1 + " refs/heads/main\n", nil, true},
		{"uppercase ID", strings.ToUpper(sha1) + " " + sha1 + " refs/heads/main\n", nil, true},
		{"not a ref", zeroID + " " + sha1 + " HEAD\n", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePushedRefs(tt.data)
			if (err != nil) != tt.wantErr || !slices.Equal(got, tt.want) {
				t.Errorf("parsePushedRefs() = %+v, %v; want %+v, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestServeReturnsNilAfterShutdown(t *testing.T) {
	env := newTestEnv(t)
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- env.srv.Serve(lis) }()

	resp := env.doURL(t, http.MethodGet, "http://"+lis.Addr().String()+"/healthz", nil, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", resp.status)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	env.srv.Shutdown(ctx)
	if err := <-served; err != nil {
		t.Errorf("Serve() error = %v, want nil after Shutdown", err)
	}
}

// testEnv is a Server on a test HTTP server, with fakes for the database
// and a git client of its own.
type testEnv struct {
	url      string
	srv      *Server
	layout   *storage.Layout
	store    *storage.Store
	repos    *fakeRepos
	pushes   *fakePushes
	requests *requestLog
	git      *gitcmd.Runner // the client's git, with a HOME of its own
	user     uuid.UUID      // the signed-in user
}

func newTestEnv(t *testing.T, configure ...func(*Options)) *testEnv {
	t.Helper()
	layout := storage.NewLayout(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	serverGit, err := gitcmd.New(layout.HomeDir())
	if err != nil {
		t.Fatal(err)
	}
	clientGit, err := gitcmd.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	env := &testEnv{
		layout:   layout,
		store:    storage.NewStore(layout, serverGit),
		repos:    &fakeRepos{live: map[uuid.UUID]bool{}},
		pushes:   &fakePushes{},
		requests: &requestLog{},
		git:      clientGit,
		user:     uuid.New(),
	}
	opts := Options{
		Log:    slog.New(slog.NewTextHandler(t.Output(), nil)),
		Layout: layout,
		Git:    serverGit,
		Repos:  env.repos,
		Pushes: env.pushes,
	}
	for _, c := range configure {
		c(&opts)
	}
	if env.srv, err = New(opts); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.requests.add(r)
		env.srv.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	env.url = ts.URL
	return env
}

// createRepo creates an empty repository with default branch main.
func (e *testEnv) createRepo(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := e.store.Init(t.Context(), id, "main"); err != nil {
		t.Fatal(err)
	}
	e.repos.add(id)
	return id
}

func (e *testEnv) repoURL(id uuid.UUID) string {
	return e.url + "/" + id.String() + ".git"
}

// userHeader is the -c value that makes git send the gateway's user header.
func (e *testEnv) userHeader() string {
	return "http.extraHeader=" + HeaderUserID + ": " + e.user.String()
}

// clone clones a repository into a new directory and returns it.
func (e *testEnv) clone(t *testing.T, id uuid.UUID) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "w")
	e.gitOK(t, "", "clone", "--quiet", e.repoURL(id), dir)
	return dir
}

// commit writes a file, commits it and returns the commit's ID.
func (e *testEnv) commit(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	e.gitOK(t, dir, "add", file)
	e.gitOK(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", file)
	return e.gitOK(t, dir, "rev-parse", "HEAD")
}

// gitOK runs the client's git and returns its trimmed output.
func (e *testEnv) gitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := e.git.Run(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// gitFails runs the client's git, which must fail.
func (e *testEnv) gitFails(t *testing.T, dir string, args ...string) *gitcmd.Error {
	t.Helper()
	_, err := e.git.Run(t.Context(), dir, args...)
	var gitErr *gitcmd.Error
	if !errors.As(err, &gitErr) || gitErr.ExitCode <= 0 {
		t.Fatalf("git %v: error = %v, want git to fail", args, err)
	}
	return gitErr
}

// wantPushes checks the pushes recorded so far.
func (e *testEnv) wantPushes(t *testing.T, want ...repo.Push) {
	t.Helper()
	got := e.pushes.all()
	equal := slices.EqualFunc(got, want, func(a, b repo.Push) bool {
		return a.RepositoryID == b.RepositoryID && a.PusherID == b.PusherID && slices.Equal(a.RefUpdates, b.RefUpdates)
	})
	if !equal {
		t.Errorf("recorded pushes = %+v, want %+v", got, want)
	}
}

type response struct {
	status int
	header http.Header
	body   string
}

func (e *testEnv) do(t *testing.T, method, path string, header map[string]string, body io.Reader) response {
	t.Helper()
	return e.doURL(t, method, e.url+path, header, body)
}

func (e *testEnv) doURL(t *testing.T, method, url string, header map[string]string, body io.Reader) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeRepos knows which repositories are live.
type fakeRepos struct {
	mu   sync.Mutex
	live map[uuid.UUID]bool
	err  error
}

func (f *fakeRepos) Get(_ context.Context, id uuid.UUID) (repo.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return repo.Repository{}, f.err
	}
	if !f.live[id] {
		return repo.Repository{}, repo.ErrNotFound
	}
	return repo.Repository{ID: id}, nil
}

func (f *fakeRepos) add(id uuid.UUID)    { f.mu.Lock(); f.live[id] = true; f.mu.Unlock() }
func (f *fakeRepos) remove(id uuid.UUID) { f.mu.Lock(); delete(f.live, id); f.mu.Unlock() }
func (f *fakeRepos) setErr(err error)    { f.mu.Lock(); f.err = err; f.mu.Unlock() }

// fakePushes keeps the pushes it records.
type fakePushes struct {
	mu     sync.Mutex
	pushes []repo.Push
	err    error
}

func (f *fakePushes) RecordPush(_ context.Context, p repo.Push) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.pushes = append(f.pushes, p)
	return nil
}

func (f *fakePushes) all() []repo.Push {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushes)
}

func (f *fakePushes) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

// requestLog keeps what the server received.
type requestLog struct {
	mu   sync.Mutex
	reqs []loggedRequest
}

type loggedRequest struct {
	method, path, protocol, encoding string
}

func (l *requestLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, loggedRequest{
		method: r.Method, path: r.URL.Path,
		protocol: r.Header.Get("Git-Protocol"), encoding: r.Header.Get("Content-Encoding"),
	})
}

func (l *requestLog) all() []loggedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reqs)
}

// syncBuffer is a bytes.Buffer safe for the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
