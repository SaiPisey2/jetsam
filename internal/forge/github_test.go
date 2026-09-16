package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ghStub is a minimal stand-in for the GitHub REST API: enough of the
// Contents and Pulls endpoints to drive GitHubProvider against a real
// net/http round trip, so the code under test is the actual request path
// and not a re-implementation of it.
type ghStub struct {
	// blobs maps "ref:path" to the file content that ref holds.
	blobs map[string]string
	// pulls is what the pulls list endpoint returns.
	pulls []map[string]any
	// errOn maps "ref:path" to an HTTP status this ref/path GET should fail
	// with, simulating a transient API error (rate limit, 5xx) rather than
	// a clean 404. Distinct from blobs missing an entry, which is a
	// legitimate "file not found there".
	errOn map[string]int

	puts []map[string]any
}

func (s *ghStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/acme/rules/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/rules/contents/")
		switch r.Method {
		case http.MethodGet:
			key := r.URL.Query().Get("ref") + ":" + path
			if status, ok := s.errOn[key]; ok {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"simulated transient error"}`)
				return
			}
			content, ok := s.blobs[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			// GitHub wraps base64 at 60 columns; the decoder has to cope.
			b64 := base64.StdEncoding.EncodeToString([]byte(content))
			var wrapped strings.Builder
			for i := 0; i < len(b64); i += 60 {
				end := i + 60
				if end > len(b64) {
					end = len(b64)
				}
				wrapped.WriteString(b64[i:end] + "\n")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"sha": "blob-" + path, "encoding": "base64", "content": wrapped.String(),
			})
		case http.MethodPut:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			s.puts = append(s.puts, body)
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"sha": "new"}})
		}
	})

	// Honours ?state= exactly as GitHub does, so a caller that asks only for
	// open PRs genuinely cannot see a closed one. Without this the stub
	// would hand back closed PRs to a state=open request and the test would
	// pass against the very filter it exists to catch.
	mux.HandleFunc("/repos/acme/rules/pulls", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open" // GitHub's default
		}
		out := []map[string]any{}
		for _, p := range s.pulls {
			if state == "all" || p["state"] == state {
				out = append(out, p)
			}
		}
		json.NewEncoder(w).Encode(out)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (s *ghStub) provider(t *testing.T) *GitHubProvider {
	return &GitHubProvider{BaseURL: s.server(t).URL, Token: "test", Client: http.DefaultClient}
}

const localFile = "groups:\n  - name: demo\n    rules:\n      - alert: A\n        expr: up == 0\n"

// TestCommitFilesRefusesWhenTheBranchBlobDiffers is the core of the
// "a PR can revert unrelated work" defect, against the real request path.
// The bot's branch holds a file a human pushed to; PUTting the locally
// computed whole file would drop their change without a word.
func TestCommitFilesRefusesWhenTheBranchBlobDiffers(t *testing.T) {
	stub := &ghStub{blobs: map[string]string{
		"main:rules/demo.yml":     localFile,
		"jetsam/x:rules/demo.yml": localFile + "      - alert: B\n        expr: up == 1\n",
	}}
	g := stub.provider(t)

	err := g.CommitFiles(context.Background(), "acme", "rules", "main", "jetsam/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "jetsam: retire A"},
	})
	if err == nil {
		t.Fatal("CommitFiles overwrote a branch that had moved on")
	}
	if !strings.Contains(err.Error(), "refusing to commit") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
	if len(stub.puts) != 0 {
		t.Errorf("a refused commit still issued %d PUT(s)", len(stub.puts))
	}
}

// TestCommitFilesRefusesWhenTheBaseBlobDiffers covers the stale-checkout
// half: the branch still agrees with what was edited, but base has moved on
// since, so merging this PR would revert base's change.
func TestCommitFilesRefusesWhenTheBaseBlobDiffers(t *testing.T) {
	stub := &ghStub{blobs: map[string]string{
		"main:rules/demo.yml":     localFile + "      - alert: NewFromSomeoneElse\n        expr: up == 2\n",
		"jetsam/x:rules/demo.yml": localFile,
	}}
	g := stub.provider(t)

	err := g.CommitFiles(context.Background(), "acme", "rules", "main", "jetsam/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "jetsam: retire A"},
	})
	if err == nil {
		t.Fatal("CommitFiles proceeded against a base that had moved on")
	}
	if !strings.Contains(err.Error(), "main has moved on") {
		t.Errorf("error does not name the stale base: %v", err)
	}
	if len(stub.puts) != 0 {
		t.Errorf("a refused commit still issued %d PUT(s)", len(stub.puts))
	}
}

// TestCommitFilesRefusesWhenTheBaseBlobFetchErrors is the fail-closed half
// of the base-blob guard: a transient error fetching base (rate limit, 5xx)
// must not be read as "base agrees", because that is exactly the condition
// under which a stale checkout would otherwise commit a silent revert.
func TestCommitFilesRefusesWhenTheBaseBlobFetchErrors(t *testing.T) {
	stub := &ghStub{
		blobs: map[string]string{
			"jetsam/x:rules/demo.yml": localFile,
		},
		errOn: map[string]int{
			"main:rules/demo.yml": http.StatusInternalServerError,
		},
	}
	g := stub.provider(t)

	err := g.CommitFiles(context.Background(), "acme", "rules", "main", "jetsam/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "jetsam: retire A"},
	})
	if err == nil {
		t.Fatal("CommitFiles proceeded after the base-blob fetch errored")
	}
	if len(stub.puts) != 0 {
		t.Errorf("a base-blob fetch error still issued %d PUT(s)", len(stub.puts))
	}
}

// TestCommitFilesWritesWhenTheFileIsNewOnBase covers the legitimate
// !baseFound case: this proposal is adding a rule file that does not exist
// on base at all. That is not evidence base has moved on, so it must not be
// refused.
func TestCommitFilesWritesWhenTheFileIsNewOnBase(t *testing.T) {
	stub := &ghStub{blobs: map[string]string{
		"jetsam/x:rules/demo.yml": localFile,
		// deliberately no "main:rules/demo.yml" entry.
	}}
	g := stub.provider(t)

	if err := g.CommitFiles(context.Background(), "acme", "rules", "main", "jetsam/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "jetsam: add A"},
	}); err != nil {
		t.Fatalf("CommitFiles refused a file that is legitimately new on base: %v", err)
	}
	if len(stub.puts) != 1 {
		t.Errorf("issued %d PUTs, want 1", len(stub.puts))
	}
}

// TestCommitFilesWritesWhenEverythingAgrees is the ordinary path: the
// safety check must not stand in the way of the commit it exists to guard.
func TestCommitFilesWritesWhenEverythingAgrees(t *testing.T) {
	stub := &ghStub{blobs: map[string]string{
		"main:rules/demo.yml":     localFile,
		"jetsam/x:rules/demo.yml": localFile,
	}}
	g := stub.provider(t)

	if err := g.CommitFiles(context.Background(), "acme", "rules", "main", "jetsam/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "jetsam: retire A"},
	}); err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	if len(stub.puts) != 1 {
		t.Fatalf("issued %d PUTs, want 1", len(stub.puts))
	}
	if got := stub.puts[0]["sha"]; got != "blob-rules/demo.yml" {
		t.Errorf("PUT sha = %v, want the branch blob's sha", got)
	}
	want := base64.StdEncoding.EncodeToString([]byte("edited"))
	if got := stub.puts[0]["content"]; got != want {
		t.Errorf("PUT content = %v, want %v", got, want)
	}
}

// TestCommitFilesRefusesAMissingFile: the edit was computed against real
// bytes, so a file that is not there means this is not the repository (or
// not the path) the proposal is about.
func TestCommitFilesRefusesAMissingFile(t *testing.T) {
	stub := &ghStub{blobs: map[string]string{}}
	g := stub.provider(t)

	err := g.CommitFiles(context.Background(), "acme", "rules", "main", "jetsam/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "m"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want a refusal naming the missing file", err)
	}
}

// TestFindPRReportsAClosedPR: a PR a maintainer closed must come back, not
// be filtered away as though the branch had never had one.
func TestFindPRReportsAClosedPR(t *testing.T) {
	stub := &ghStub{pulls: []map[string]any{
		{"number": 7, "html_url": "https://example/7", "state": "closed",
			"head": map[string]any{"ref": "jetsam/x"}},
	}}
	g := stub.provider(t)

	pr, err := g.FindPR(context.Background(), "acme", "rules", "main", "jetsam/x")
	if err != nil {
		t.Fatalf("FindPR: %v", err)
	}
	if pr == nil {
		t.Fatal("a closed PR was reported as no PR at all; the bot would reopen it")
	}
	if pr.Number != 7 || pr.State != "closed" {
		t.Errorf("pr = %+v, want #7 closed", pr)
	}
}

// TestFindPRPrefersTheOpenPR: with both an old closed PR and a live one on
// the branch, the open one is the answer -- that is the idempotence case.
func TestFindPRPrefersTheOpenPR(t *testing.T) {
	stub := &ghStub{pulls: []map[string]any{
		{"number": 9, "html_url": "https://example/9", "state": "closed",
			"head": map[string]any{"ref": "jetsam/x"}},
		{"number": 11, "html_url": "https://example/11", "state": "open",
			"head": map[string]any{"ref": "jetsam/x"}},
	}}
	g := stub.provider(t)

	pr, err := g.FindPR(context.Background(), "acme", "rules", "main", "jetsam/x")
	if err != nil {
		t.Fatalf("FindPR: %v", err)
	}
	if pr.Number != 11 || pr.State != "open" {
		t.Errorf("pr = %+v, want #11 open", pr)
	}
}

// TestFindPRReportsAMergedPRAsMerged keeps "merged" distinguishable from
// "closed" in the output a human reads, though neither is reopened.
func TestFindPRReportsAMergedPRAsMerged(t *testing.T) {
	merged := "2026-09-01T00:00:00Z"
	stub := &ghStub{pulls: []map[string]any{
		{"number": 3, "html_url": "https://example/3", "state": "closed", "merged_at": merged,
			"head": map[string]any{"ref": "jetsam/x"}},
	}}
	g := stub.provider(t)

	pr, err := g.FindPR(context.Background(), "acme", "rules", "main", "jetsam/x")
	if err != nil {
		t.Fatalf("FindPR: %v", err)
	}
	if pr.State != "merged" {
		t.Errorf("state = %q, want merged", pr.State)
	}
}

// TestDoErrorNeverEchoesUserinfoOrQueryString: 127.0.0.1:1 is a reserved
// port nothing listens on, so this dials and fails, exercising the
// *url.Error path from http.Client.Do. The token itself never reaches the
// URL (NewGitHubProvider sends it only via the Authorization header), but
// the query string this provider builds for FindPR (state=all&base=...&
// head=...) is exactly where a token would sit if that ever changed, and
// userinfo in a misconfigured BaseURL is an equally live way for a
// credential to end up in the URL that http.Client.Do embeds in its own
// error text.
func TestDoErrorNeverEchoesUserinfoOrQueryString(t *testing.T) {
	g := &GitHubProvider{
		BaseURL: "http://user:sekrit@127.0.0.1:1",
		Client:  &http.Client{Timeout: 2 * time.Second},
	}
	_, err := g.FindPR(context.Background(), "acme", "rules", "main", "somebranch")
	if err == nil {
		t.Fatal("FindPR succeeded against an unreachable address, want an error")
	}
	if strings.Contains(err.Error(), "sekrit") {
		t.Errorf("error leaks userinfo: %v", err)
	}
	if strings.Contains(err.Error(), "state=all") {
		t.Errorf("error leaks the query string: %v", err)
	}
}

// TestDoRejectsAnOversizedBodyRatherThanTruncating: a body over
// maxResponseBytes must produce a clear error, never a silently truncated
// decode. For CommitFiles specifically, a truncated getContent response
// fed to json.Unmarshal would either fail to parse (the ordinary case,
// since truncating mid-object almost always breaks JSON syntax) or, in
// the worst case nothing here should rely on avoiding by luck, decode into
// content that does not match what is actually on the remote -- which
// could make the BaseContent safety check compare against the wrong
// bytes. Catching the oversized body before any parsing is attempted
// closes that off entirely, deterministically, regardless of where the
// cut lands.
func TestDoRejectsAnOversizedBodyRatherThanTruncating(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A well-formed (if empty) pulls array, padded well past
		// maxResponseBytes with whitespace insignificant to JSON -- so if
		// the size limit were not enforced, this would decode just fine.
		w.Write([]byte("["))
		w.Write([]byte(strings.Repeat(" ", maxResponseBytes+1024)))
		w.Write([]byte("]"))
	}))
	defer srv.Close()

	g := &GitHubProvider{BaseURL: srv.URL, Client: &http.Client{Timeout: 30 * time.Second}}
	_, err := g.FindPR(context.Background(), "acme", "rules", "main", "somebranch")
	if err == nil {
		t.Fatal("FindPR succeeded against an oversized body, want an error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error does not report the size limit was exceeded: %v", err)
	}
}

// TestDoRedactsTokenFromErrorBody: a reverse proxy, WAF or misconfigured
// GitHub Enterprise gateway can reflect the incoming Authorization header
// into its error body -- a realistic failure mode, not a hypothetical one.
// This stub server does exactly that on every non-2xx response, the same
// way a real one might. Every one of do()'s call sites (FindPR here,
// EnsureBranch and CommitFiles by the same code path) must never let that
// token reach the error string this provider returns, since it ends up on
// stderr and, in CI, gets archived, screenshotted and pasted into tickets.
func TestDoRedactsTokenFromErrorBody(t *testing.T) {
	const token = "ghp_supersecrettoken12345"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"message":"upstream rejected request","received_auth":%q}`, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	g := &GitHubProvider{BaseURL: srv.URL, Token: token, Client: &http.Client{Timeout: 5 * time.Second}}
	_, err := g.FindPR(context.Background(), "acme", "rules", "main", "somebranch")
	if err == nil {
		t.Fatal("FindPR succeeded against a 500 response, want an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error echoes the bearer token back out:\n%v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("error does not mark the token as redacted:\n%v", err)
	}
}

// --- status codes, not message text ---------------------------------------
//
// Every "was this a 404?" decision used to be a substring search over the
// formatted error, which embeds the request path and the response body.
// See apiError.

func alwaysStatus(t *testing.T, status int, body string) *GitHubProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return &GitHubProvider{BaseURL: srv.URL, Token: "t", Client: srv.Client()}
}

func TestGetContentTreatsOnlyARealNotFoundAsMissing(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		owner     string
		repo      string
		wantErr   bool
		wantFound bool
	}{
		{
			name:   "a 502 quoting an upstream 404 is an error, not a missing file",
			status: http.StatusBadGateway,
			body:   `{"message":"upstream returned 404 to the cache"}`,
			owner:  "acme", repo: "infra",
			wantErr: true,
		},
		{
			// github.com/acme/status-404-page is an ordinary repository
			// name. The request path is echoed into the error message, so
			// this used to make EVERY failure on that repo read as a 404.
			name:   "a 500 on a repo whose name contains 404 is still an error",
			status: http.StatusInternalServerError,
			body:   `{"message":"internal error"}`,
			owner:  "acme", repo: "status-404-page",
			wantErr: true,
		},
		{
			name:   "a 403 rate limit is an error, not a missing file",
			status: http.StatusForbidden,
			body:   `{"message":"API rate limit exceeded"}`,
			owner:  "acme", repo: "infra",
			wantErr: true,
		},
		{
			name:   "a real 404 is a missing file",
			status: http.StatusNotFound,
			body:   `{"message":"Not Found"}`,
			owner:  "acme", repo: "infra",
			wantErr: false, wantFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := alwaysStatus(t, tc.status, tc.body)
			_, found, err := g.getContent(context.Background(), tc.owner, tc.repo, "prometheus.yml", "main")
			if tc.wantErr && err == nil {
				t.Fatalf("http %d: got found=%v err=nil, want an error -- a non-404 was read as a missing file, "+
					"which is the one answer that makes CommitFiles skip its base-moved-on check", tc.status, found)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("http %d: unexpected error: %v", tc.status, err)
			}
			if !tc.wantErr && found != tc.wantFound {
				t.Fatalf("http %d: found = %v, want %v", tc.status, found, tc.wantFound)
			}
		})
	}
}

func TestEnsureBranchOnlyCreatesOnARealNotFound(t *testing.T) {
	// A 502 mentioning 404 must not be taken as "the branch does not
	// exist", because the next step is to create it.
	g := alwaysStatus(t, http.StatusBadGateway, `{"message":"proxy saw 404 downstream"}`)
	err := g.EnsureBranch(context.Background(), "acme", "infra", "jetsam/drop-abc", "main")
	if err == nil {
		t.Fatal("EnsureBranch returned nil on a 502; a transient failure was read as 'branch does not exist'")
	}
	if !strings.Contains(err.Error(), "check branch") {
		t.Fatalf("error = %v, want it to report the branch check failing", err)
	}
}

// --- URL construction ------------------------------------------------------
//
// owner, repo, base and the config's prometheus_file were interpolated raw
// into the request path. See pathSegment.

func TestGetContentSendsTheRefItWasAskedFor(t *testing.T) {
	var gotRawPath, gotRef string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RequestURI is the path exactly as it went over the wire, before
		// net/http decodes percent-escapes back into r.URL.Path.
		gotRawPath, _, _ = strings.Cut(r.RequestURI, "?")
		gotQuery = r.URL.Query()
		gotRef = gotQuery.Get("ref")
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	g := &GitHubProvider{BaseURL: srv.URL, Token: "t", Client: srv.Client()}

	// A "?" in the file path used to start the query string early, so the
	// ref the caller asked for was replaced by one the path carried -- and
	// that blob is exactly what CommitFiles compares against BaseContent
	// before deciding the write is safe.
	if _, _, err := g.getContent(context.Background(), "acme", "infra", "conf/a?ref=attacker&x.yml", "main"); err != nil {
		t.Fatalf("getContent: %v", err)
	}
	if gotRef != "main" {
		t.Fatalf("server saw ref=%q, want %q -- the file path hijacked the ref", gotRef, "main")
	}
	if len(gotQuery) != 1 {
		t.Fatalf("server saw query %v, want only ref -- the file path injected parameters", gotQuery)
	}
	// Only "?" has to be escaped: query parsing starts at the first
	// unescaped one, so once it is %3F everything after it stays in the
	// path, where "&" is an ordinary character.
	if strings.Contains(gotRawPath, "?") {
		t.Fatalf("server saw raw path %q, want the '?' percent-escaped inside the path segment", gotRawPath)
	}
}

func TestRepoAndPathTraversalIsRefused(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.RequestURI
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	g := &GitHubProvider{BaseURL: srv.URL, Token: "t", Client: srv.Client()}

	cases := []struct{ name, owner, repo, path string }{
		{"repo traverses out of /repos", "acme", "..", "p.yml"},
		{"file path traverses out of the repo", "acme", "infra", "../../../etc/passwd"},
		{"absolute file path", "acme", "infra", "/etc/passwd"},
		{"empty owner", "", "infra", "p.yml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPath = ""
			if _, _, err := g.getContent(context.Background(), tc.owner, tc.repo, tc.path, "main"); err == nil {
				t.Fatalf("getContent accepted it and requested %q; want a refusal", gotPath)
			}
			if gotPath != "" {
				t.Fatalf("a request was sent (%q) before the refusal", gotPath)
			}
		})
	}
}
