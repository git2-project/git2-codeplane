package smarthttp

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// These tests cover clients that real git never imitates, such as one that
// stops sending or reading halfway, so they speak HTTP over a raw connection.

// stallTimeoutForTests is short, so that stalled clients are noticed quickly.
const stallTimeoutForTests = 300 * time.Millisecond

// A client that stops reading must not hold a git process: nothing else ends
// a blocked write, and os/exec waits for it even after git was stopped.
func TestClientThatStopsReading(t *testing.T) {
	var lsRefs bytes.Buffer
	lsRefs.Write(pktLine("command=ls-refs\n"))
	lsRefs.WriteString("0001")
	lsRefs.WriteString("0000")

	tests := []struct {
		name    string
		request func(id uuid.UUID) string
	}{
		{"ref advertisement", func(id uuid.UUID) string {
			return "GET /" + id.String() + ".git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: git-svc\r\n\r\n"
		}},
		{"fetch", func(id uuid.UUID) string {
			return fmt.Sprintf("POST /%s.git/git-upload-pack HTTP/1.1\r\nHost: git-svc\r\n"+
				"Content-Type: application/x-git-upload-pack-request\r\nGit-Protocol: version=2\r\n"+
				"Content-Length: %d\r\n\r\n%s", id, lsRefs.Len(), lsRefs.String())
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, func(o *Options) {
				o.StallTimeout = stallTimeoutForTests
				o.MaxGitOperations = 1
			})
			id := env.createRepo(t)
			env.addManyRefs(t, id)

			conn := env.rawConn(t)
			// A small buffer here, so that the server's writes block early.
			if err := conn.(*net.TCPConn).SetReadBuffer(4 << 10); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, tt.request(id)); err != nil {
				t.Fatal(err)
			}
			// The answer started, so the operation holds the only slot. This
			// client reads no further, and the answer exceeds what the socket
			// buffers can hold.
			status, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil || !strings.Contains(status, "200") {
				t.Fatalf("status line = %q, error %v; want 200", status, err)
			}

			info := "/" + id.String() + ".git/info/refs?service=git-upload-pack"
			busy := env.do(t, http.MethodGet, info, nil, nil)
			if busy.status != http.StatusServiceUnavailable {
				t.Fatalf("second request: status = %d, want 503 while the first holds the only slot", busy.status)
			}
			if got := busy.header.Get("Retry-After"); got != busyRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, busyRetryAfter)
			}
			if !strings.HasPrefix(busy.header.Get("Content-Type"), "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain, which git shows to the user", busy.header.Get("Content-Type"))
			}

			// The stalled client is dropped and its slot comes back.
			deadline := time.Now().Add(30 * time.Second)
			for env.do(t, http.MethodGet, info, nil, nil).status != http.StatusOK {
				if time.Now().After(deadline) {
					t.Fatal("the git process of a client that stopped reading was not stopped")
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

// A client that stops sending must not hold a git process either.
func TestStalledRequestBody(t *testing.T) {
	// A gzip stream cut off after its first data, for the read that happens
	// while git is running.
	var partialGzip bytes.Buffer
	zw := gzip.NewWriter(&partialGzip)
	if _, err := zw.Write([]byte("00")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		service  string
		encoding string
		sent     string
	}{
		{"fetch", serviceUploadPack, "", "00"},
		{"push", serviceReceivePack, "", "00"},
		{"no data at all", serviceReceivePack, "", ""},
		{"gzip header", serviceReceivePack, "gzip", "\x1f"},
		{"gzip data, fetch", serviceUploadPack, "gzip", partialGzip.String()},
		{"gzip data, push", serviceReceivePack, "gzip", partialGzip.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, func(o *Options) {
				o.StallTimeout = stallTimeoutForTests
				o.MaxGitOperations = 1
			})
			id := env.createRepo(t)

			var header strings.Builder
			fmt.Fprintf(&header, "POST /%s.git/%s HTTP/1.1\r\nHost: git-svc\r\n", id, tt.service)
			fmt.Fprintf(&header, "Content-Type: application/x-%s-request\r\n", tt.service)
			fmt.Fprintf(&header, "%s: %s\r\n", HeaderUserID, env.user)
			header.WriteString("Content-Length: 100000\r\n")
			if tt.encoding != "" {
				fmt.Fprintf(&header, "Content-Encoding: %s\r\n", tt.encoding)
			}
			conn := env.rawConn(t)
			if _, err := io.WriteString(conn, header.String()+"\r\n"+tt.sent); err != nil {
				t.Fatal(err)
			}

			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("the server did not answer a stalled request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusRequestTimeout {
				t.Errorf("status = %d, want 408", resp.StatusCode)
			}
			if !resp.Close {
				t.Error("the answer does not close the connection, which is in the middle of a body")
			}

			// The slot was given back, so git has stopped; no ref changed.
			info := "/" + id.String() + ".git/info/refs?service=git-upload-pack"
			if got := env.do(t, http.MethodGet, info, nil, nil).status; got != http.StatusOK {
				t.Errorf("next request: status = %d, want 200", got)
			}
			env.wantPushes(t)
		})
	}
}

// The timeout is on silence, not on how long a request takes.
func TestSlowRequestBodyIsNotStalled(t *testing.T) {
	const stall = time.Second
	env := newTestEnv(t, func(o *Options) { o.StallTimeout = stall })
	id := env.createRepo(t)
	work := env.clone(t, id)
	env.commit(t, work, "a.txt", "1\n")
	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")

	pr, pw := io.Pipe()
	go func() {
		for _, part := range []string{string(pktLine("command=ls-refs\n")), "0001", "0000"} {
			time.Sleep(stall * 2 / 5)
			_, _ = io.WriteString(pw, part)
		}
		time.Sleep(stall * 2 / 5)
		_ = pw.Close()
	}()

	resp := env.do(t, http.MethodPost, "/"+id.String()+".git/"+serviceUploadPack, map[string]string{
		"Content-Type": "application/x-git-upload-pack-request",
		"Git-Protocol": "version=2",
	}, pr)
	if resp.status != http.StatusOK || !strings.Contains(resp.body, "refs/heads/") {
		t.Errorf("status = %d, %d bytes; want 200 with refs", resp.status, len(resp.body))
	}
}

// Once the request has arrived, git may work for longer than the stall timeout
// before it answers: the read deadline must not outlive the request body.
func TestCloneOutlivesTheStallTimeout(t *testing.T) {
	env := newTestEnv(t, func(o *Options) { o.StallTimeout = stallTimeoutForTests })
	id := env.createRepo(t)
	work := env.clone(t, id)

	// 40 branches on 40 different commits make git compress the request, so
	// this covers the plain and the gzip path.
	for i := range 40 {
		env.commit(t, work, "n.txt", fmt.Sprintf("%d\n", i))
		env.gitOK(t, work, "branch", fmt.Sprintf("b%02d", i))
	}
	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "refs/heads/*:refs/heads/*")

	// Git reads this setting only from the user's own config, and appends the
	// pack-objects command to it, which then runs after the delay.
	config := fmt.Sprintf("[uploadpack]\n\tpackObjectsHook = \"sleep %d;\"\n", int((5*stallTimeoutForTests).Seconds()+1))
	if err := os.WriteFile(filepath.Join(env.layout.HomeDir(), ".gitconfig"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	again := filepath.Join(t.TempDir(), "a")
	env.gitOK(t, "", "clone", "--quiet", env.repoURL(id), again)

	gzipped := slices.ContainsFunc(env.requests.all(), func(r loggedRequest) bool {
		return strings.HasSuffix(r.path, "/"+serviceUploadPack) && r.encoding == "gzip"
	})
	if !gzipped {
		t.Error("no gzip-compressed request reached the server; the test no longer covers gzip")
	}
}

// Git can answer without reading the end of the request. The handler then
// stops reading the body before it returns, because net/http does not allow
// reads after that, and the client still gets a complete answer.
func TestAnswerBeforeTheRequestEnds(t *testing.T) {
	env := newTestEnv(t, func(o *Options) { o.MaxGitOperations = 1 })
	id := env.createRepo(t)
	work := env.clone(t, id)
	env.commit(t, work, "a.txt", "1\n")
	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")

	request := string(pktLine("command=ls-refs\n")) + "0001" + "0000"
	conn := env.rawConn(t)
	_, err := fmt.Fprintf(conn, "POST /%s.git/git-upload-pack HTTP/1.1\r\nHost: git-svc\r\n"+
		"Content-Type: application/x-git-upload-pack-request\r\nGit-Protocol: version=2\r\n"+
		"Transfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", id, len(request), request)
	if err != nil {
		t.Fatal(err)
	}

	// The chunked body never ends: the client holds it open.
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read answer: %v (after %d bytes)", err, len(body))
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "refs/heads/main") {
		t.Errorf("status = %d, body %q; want 200 with the ref", resp.StatusCode, body)
	}

	info := "/" + id.String() + ".git/info/refs?service=git-upload-pack"
	if got := env.do(t, http.MethodGet, info, nil, nil).status; got != http.StatusOK {
		t.Errorf("next request: status = %d, want 200: the slot was not given back", got)
	}
}

// A small compressed body must not grow into gigabytes of commands inside
// receive-pack: receive.maxInputSize covers only the pack.
func TestPushCommandsTooLarge(t *testing.T) {
	var commands bytes.Buffer
	sha := strings.Repeat("a", 40)
	for i := range 1000 {
		line := fmt.Sprintf("%s %s refs/heads/b%d", zeroID, sha, i)
		if i == 0 {
			line += "\x00report-status"
		}
		commands.Write(pktLine(line + "\n"))
	}
	commands.WriteString("0000")

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(commands.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name     string
		encoding string
		body     []byte
	}{
		{"plain", "", commands.Bytes()},
		{"gzip", "gzip", compressed.Bytes()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, func(o *Options) { o.MaxPushCommandsSize = 4 << 10 })
			id := env.createRepo(t)

			header := map[string]string{
				"Content-Type": "application/x-git-receive-pack-request",
				HeaderUserID:   env.user.String(),
			}
			if tt.encoding != "" {
				header["Content-Encoding"] = tt.encoding
			}
			resp := env.do(t, http.MethodPost, "/"+id.String()+".git/"+serviceReceivePack, header, bytes.NewReader(tt.body))
			if resp.status != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d (body %q), want 413", resp.status, resp.body)
			}
			env.wantPushes(t)
			if refs := env.gitOK(t, env.layout.RepoPath(id), "for-each-ref"); refs != "" {
				t.Errorf("refs after a refused push = %q, want none", refs)
			}
		})
	}
}

// The limit is on the commands only: a pack far larger than it goes through,
// and pushing more refs than it allows fails with an error git can show.
func TestPushCommandsLimitWithRealGit(t *testing.T) {
	env := newTestEnv(t, func(o *Options) { o.MaxPushCommandsSize = 2 << 10 })
	id := env.createRepo(t)
	work := env.clone(t, id)
	sha := env.commit(t, work, "random.bin", string(randomBytes(t, 64<<10))) // does not compress

	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")
	env.wantPushes(t, repo.Push{RepositoryID: id, PusherID: env.user, RefUpdates: []repo.RefUpdate{
		{Ref: "refs/heads/main", OldSHA: zeroID, NewSHA: sha, Forced: repo.ForcedNo},
	}})

	for i := range 100 {
		env.gitOK(t, work, "branch", fmt.Sprintf("b%03d", i))
	}
	err := env.gitFails(t, work, "-c", env.userHeader(), "push", "origin", "refs/heads/*:refs/heads/*")
	if !strings.Contains(err.Stderr, "413") {
		t.Errorf("push stderr = %q, want HTTP 413", err.Stderr)
	}
	if len(env.pushes.all()) != 1 {
		t.Errorf("recorded pushes = %+v, want only the first", env.pushes.all())
	}
}

func TestCommandsLimitReader(t *testing.T) {
	section := string(pktLine("aaaa\n")) + string(pktLine("bbbb\n")) + "0000"
	pack := "PACK" + strings.Repeat("x", 1000)

	tests := []struct {
		name    string
		input   string
		limit   int64
		wantErr error
	}{
		{name: "section at the limit", input: section + pack, limit: int64(len(section))},
		{name: "section past the limit", input: section + pack, limit: int64(len(section)) - 1, wantErr: errTooLarge},
		{name: "pack is not counted", input: "0000" + pack, limit: 4},
		{name: "no commands at all", input: "0000", limit: 4},
		{name: "input ends without a flush", input: string(pktLine("aaaa\n")), limit: 100},
		{name: "empty pkt-line", input: "0004" + "0000" + pack, limit: 8},
		{name: "delimiter ends the section too", input: "0001" + pack, limit: 4},
		{name: "not a pkt-line: git stops there", input: "zzzz" + pack, limit: 4},
		{name: "one huge pkt-line", input: "ffff" + strings.Repeat("y", 200), limit: 100, wantErr: errTooLarge},
	}
	readers := map[string]func(io.Reader) io.Reader{
		"whole":        func(r io.Reader) io.Reader { return r },
		"byte by byte": iotest.OneByteReader,
	}
	for _, tt := range tests {
		for readerName, wrap := range readers {
			t.Run(tt.name+"/"+readerName, func(t *testing.T) {
				l := &commandsLimitReader{r: wrap(strings.NewReader(tt.input)), left: tt.limit}
				got, err := io.ReadAll(l)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				if tt.wantErr != nil {
					if int64(len(got)) > tt.limit {
						t.Errorf("git received %d bytes, past the limit of %d", len(got), tt.limit)
					}
					if _, err := l.Read(make([]byte, 1)); !errors.Is(err, errTooLarge) {
						t.Errorf("read after the limit: error = %v, want errTooLarge", err)
					}
					return
				}
				if string(got) != tt.input {
					t.Errorf("git received %q, want the input %q", got, tt.input)
				}
			})
		}
	}
}

// A push with many refs must keep its event when the check for forced updates
// runs out of time, and must not claim anything about the refs it skipped.
func TestForcedCheckOutOfTime(t *testing.T) {
	var logs syncBuffer
	env := newTestEnv(t, func(o *Options) {
		o.ForcedCheckTimeout = time.Nanosecond
		o.Log = slog.New(slog.NewTextHandler(&logs, nil))
	})
	id := env.createRepo(t)
	work := env.clone(t, id)
	first := env.commit(t, work, "a.txt", "1\n")
	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")
	rewritten := env.gitOK(t, work, "-c", "user.name=Test", "-c", "user.email=test@example.com",
		"commit-tree", "-m", "other", first+"^{tree}")

	env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "--force", "origin",
		rewritten+":refs/heads/main", rewritten+":refs/heads/new")

	pushes := env.pushes.all()
	if len(pushes) != 2 {
		t.Fatalf("%d pushes recorded, want 2: %+v", len(pushes), pushes)
	}
	got := slices.Clone(pushes[1].RefUpdates)
	slices.SortFunc(got, func(a, b repo.RefUpdate) int { return strings.Compare(a.Ref, b.Ref) })
	want := []repo.RefUpdate{
		{Ref: "refs/heads/main", OldSHA: first, NewSHA: rewritten, Forced: repo.ForcedUnknown},
		// Creating a ref needs no check, so the answer is known.
		{Ref: "refs/heads/new", OldSHA: zeroID, NewSHA: rewritten, Forced: repo.ForcedNo},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ref updates = %+v, want %+v", got, want)
	}
	if !strings.Contains(logs.String(), "ran out of time") {
		t.Errorf("logs do not report the unfinished check:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "RepositoryPushed is lost") {
		t.Errorf("the event was lost:\n%s", logs.String())
	}
}

// Trees and blobs have no ancestry, so "forced" has no answer for them.
func TestForcedIsUnknownForObjectsThatAreNotCommits(t *testing.T) {
	env := newTestEnv(t)
	id := env.createRepo(t)
	work := env.clone(t, id)
	push := func(args ...string) {
		t.Helper()
		env.gitOK(t, work, append([]string{"-c", env.userHeader(), "push", "--quiet", "origin"}, args...)...)
	}

	env.commit(t, work, "a.txt", "1\n")
	oldTree := env.gitOK(t, work, "rev-parse", "HEAD^{tree}")
	env.gitOK(t, work, "tag", "snapshot", oldTree)
	push("HEAD:refs/heads/main", "refs/tags/snapshot")

	env.commit(t, work, "a.txt", "2\n")
	newTree := env.gitOK(t, work, "rev-parse", "HEAD^{tree}")
	env.gitOK(t, work, "tag", "--force", "snapshot", newTree)
	push("--force", "refs/tags/snapshot")

	pushes := env.pushes.all()
	if len(pushes) != 2 {
		t.Fatalf("%d pushes recorded, want 2: %+v", len(pushes), pushes)
	}
	want := []repo.RefUpdate{{Ref: "refs/tags/snapshot", OldSHA: oldTree, NewSHA: newTree, Forced: repo.ForcedUnknown}}
	if !slices.Equal(pushes[1].RefUpdates, want) {
		t.Errorf("ref updates = %+v, want %+v", pushes[1].RefUpdates, want)
	}
}

// addManyRefs gives a repository enough refs that their advertisement exceeds
// what the socket buffers of a connection can hold. It writes packed-refs
// directly: a push of this size would take long.
func (e *testEnv) addManyRefs(t *testing.T, id uuid.UUID) {
	t.Helper()
	work := e.clone(t, id)
	sha := e.commit(t, work, "a.txt", "1\n")
	e.gitOK(t, work, "-c", e.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")

	var refs strings.Builder
	for i := range 200_000 {
		fmt.Fprintf(&refs, "%s refs/heads/many/%06d\n", sha, i)
	}
	path := filepath.Join(e.layout.RepoPath(id), "packed-refs")
	if err := os.WriteFile(path, []byte(refs.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rawConn opens a connection to the test server, closed when the test ends.
// Reads and writes fail after 30 seconds rather than hang the test.
func (e *testEnv) rawConn(t *testing.T) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", strings.TrimPrefix(e.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}
