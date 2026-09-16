package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		"main:rules/demo.yml":         localFile,
		"noisefloor/x:rules/demo.yml": localFile + "      - alert: B\n        expr: up == 1\n",
	}}
	g := stub.provider(t)

	err := g.CommitFiles(context.Background(), "acme", "rules", "main", "noisefloor/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "noisefloor: retire A"},
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
		"main:rules/demo.yml":         localFile + "      - alert: NewFromSomeoneElse\n        expr: up == 2\n",
		"noisefloor/x:rules/demo.yml": localFile,
	}}
	g := stub.provider(t)

	err := g.CommitFiles(context.Background(), "acme", "rules", "main", "noisefloor/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "noisefloor: retire A"},
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
			"noisefloor/x:rules/demo.yml": localFile,
		},
		errOn: map[string]int{
			"main:rules/demo.yml": http.StatusInternalServerError,
		},
	}
	g := stub.provider(t)

	err := g.CommitFiles(context.Background(), "acme", "rules", "main", "noisefloor/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "noisefloor: retire A"},
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
		"noisefloor/x:rules/demo.yml": localFile,
		// deliberately no "main:rules/demo.yml" entry.
	}}
	g := stub.provider(t)

	if err := g.CommitFiles(context.Background(), "acme", "rules", "main", "noisefloor/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "noisefloor: add A"},
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
		"main:rules/demo.yml":         localFile,
		"noisefloor/x:rules/demo.yml": localFile,
	}}
	g := stub.provider(t)

	if err := g.CommitFiles(context.Background(), "acme", "rules", "main", "noisefloor/x", []FileChange{
		{Path: "rules/demo.yml", Content: "edited", BaseContent: localFile, Message: "noisefloor: retire A"},
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

	err := g.CommitFiles(context.Background(), "acme", "rules", "main", "noisefloor/x", []FileChange{
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
			"head": map[string]any{"ref": "noisefloor/x"}},
	}}
	g := stub.provider(t)

	pr, err := g.FindPR(context.Background(), "acme", "rules", "main", "noisefloor/x")
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
			"head": map[string]any{"ref": "noisefloor/x"}},
		{"number": 11, "html_url": "https://example/11", "state": "open",
			"head": map[string]any{"ref": "noisefloor/x"}},
	}}
	g := stub.provider(t)

	pr, err := g.FindPR(context.Background(), "acme", "rules", "main", "noisefloor/x")
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
			"head": map[string]any{"ref": "noisefloor/x"}},
	}}
	g := stub.provider(t)

	pr, err := g.FindPR(context.Background(), "acme", "rules", "main", "noisefloor/x")
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
