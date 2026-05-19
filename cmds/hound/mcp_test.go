package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newCapturingHoundServer returns a test server that records the most recent
// upstream query string in *captured and replies with the given JSON body.
func newCapturingHoundServer(t *testing.T, body string, captured *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*captured = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// newFakeHoundServer returns an httptest server that responds to any request
// with the given JSON body. Tests that need per-route behaviour can pass a
// custom handler.
func newFakeHoundServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestDoSearch_FilesOnly_ReturnsCompactShape(t *testing.T) {
	hound := `{
		"Results": {
			"myrepo": {
				"Matches": [
					{"Filename": "a/b.go", "Matches": [
						{"Line": "foo", "LineNumber": 10, "Before": [], "After": []},
						{"Line": "bar", "LineNumber": 20, "Before": [], "After": []},
						{"Line": "baz", "LineNumber": 30, "Before": [], "After": []}
					]},
					{"Filename": "c/d.go", "Matches": [
						{"Line": "qux", "LineNumber": 5, "Before": [], "After": []}
					]}
				],
				"FilesWithMatch": 2,
				"Revision": "abc"
			}
		}
	}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "foo", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}

	var got struct {
		FilesOnly bool `json:"files_only"`
		Files     []struct {
			Repo    string `json:"repo"`
			Path    string `json:"path"`
			Matches int    `json:"matches"`
		} `json:"files"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}

	if !got.FilesOnly {
		t.Errorf("files_only not set in response: %s", out)
	}
	if len(got.Files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(got.Files), got.Files)
	}
	byPath := map[string]int{}
	for _, f := range got.Files {
		byPath[f.Path] = f.Matches
	}
	if byPath["a/b.go"] != 3 {
		t.Errorf("a/b.go matches = %d, want 3", byPath["a/b.go"])
	}
	if byPath["c/d.go"] != 1 {
		t.Errorf("c/d.go matches = %d, want 1", byPath["c/d.go"])
	}
	if got.Truncated {
		t.Errorf("expected truncated=false")
	}
}

func TestDoSearch_FilesOnly_StripsMatchBodies(t *testing.T) {
	hound := `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"SENSITIVE_LINE_CONTENT","LineNumber":1,"Before":["a","b"],"After":["c","d"]}]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	// The match body / surrounding context lines must not appear in files_only output.
	if containsAny(string(out), "SENSITIVE_LINE_CONTENT", "Before", "After", "LineNumber") {
		t.Errorf("files_only response leaked match body content: %s", out)
	}
}

func TestDoSearch_FilesOnlyOmitted_ReturnsRawHoundResponse(t *testing.T) {
	hound := `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"foo","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "foo"})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}

	var direct map[string]json.RawMessage
	if err := json.Unmarshal(out, &direct); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, ok := direct["Results"]; !ok {
		t.Errorf("expected raw response with Results key, got: %s", out)
	}
	if _, ok := direct["files_only"]; ok {
		t.Errorf("unexpected files_only field in non-files_only response: %s", out)
	}
}

func TestDoSearch_FilesOnly_MultiRepo(t *testing.T) {
	hound := `{
		"Results": {
			"repoA": {
				"Matches": [{"Filename": "a.go", "Matches": [{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}],
				"FilesWithMatch": 1
			},
			"repoB": {
				"Matches": [{"Filename": "b.go", "Matches": [{"Line":"y","LineNumber":2,"Before":[],"After":[]}, {"Line":"z","LineNumber":3,"Before":[],"After":[]}]}],
				"FilesWithMatch": 1
			}
		}
	}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}

	var got struct {
		Files []struct {
			Repo    string `json:"repo"`
			Path    string `json:"path"`
			Matches int    `json:"matches"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(got.Files), got.Files)
	}
	repos := map[string]int{}
	for _, f := range got.Files {
		repos[f.Repo] += f.Matches
	}
	if repos["repoA"] != 1 || repos["repoB"] != 2 {
		t.Errorf("repos = %+v; want {repoA:1, repoB:2}", repos)
	}
}

func TestDoSearch_DefaultContext_IsZero(t *testing.T) {
	var captured string
	server := newCapturingHoundServer(t, `{"Results":{}}`, &captured)
	defer server.Close()

	if _, err := doSearch(server.URL, SearchInput{Query: "foo"}); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	if !strings.Contains(captured, "ctx=0") {
		t.Errorf("expected ctx=0 in upstream query when Context omitted, got: %q", captured)
	}
}

func TestSearchToolDescription_MentionsFilesOnlyAndExcludePatterns(t *testing.T) {
	// The tool description is what a model sees first when picking a tool.
	// It must teach the cheap idioms: prefer files_only for discovery,
	// and use excludeFiles to filter out generated/vendor noise.
	want := []string{
		"files_only",        // recommended discovery mode
		"Read",              // pairs with Read for drill-down
		"excludeFiles",      // common-pattern hint
		"_test",             // a representative exclusion pattern
		"vendor",            // another representative exclusion pattern
		"RE2",               // tells the model about regex flavour
	}
	for _, sub := range want {
		if !strings.Contains(searchToolDescription, sub) {
			t.Errorf("searchToolDescription missing required keyword %q", sub)
		}
	}
}

func TestListReposDescription_MentionsDiscoveryFlow(t *testing.T) {
	if !strings.Contains(listReposDescription, "search") {
		t.Errorf("listReposDescription should point at the search tool, got: %q", listReposDescription)
	}
}

func TestGetExcludesDescription_ExplainsPurpose(t *testing.T) {
	if !strings.Contains(getExcludesDescription, "excluded") &&
		!strings.Contains(getExcludesDescription, "indexing") {
		t.Errorf("getExcludesDescription should explain its purpose, got: %q", getExcludesDescription)
	}
}

func TestDoSearch_ExplicitContext_PassedThrough(t *testing.T) {
	var captured string
	server := newCapturingHoundServer(t, `{"Results":{}}`, &captured)
	defer server.Close()

	ctx := 4
	if _, err := doSearch(server.URL, SearchInput{Query: "foo", Context: &ctx}); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	if !strings.Contains(captured, "ctx=4") {
		t.Errorf("expected ctx=4 in upstream query when Context explicit, got: %q", captured)
	}
}

// makeFilesOnlyHoundResponse returns a hound response with n files in a single
// repo, each with one match. Useful for sizing the response in budget tests.
func makeFilesOnlyHoundResponse(n int) string {
	var b strings.Builder
	b.WriteString(`{"Results":{"r":{"Matches":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		// Each entry includes a moderately long path so size is meaningful.
		fmt.Fprintf(&b, `{"Filename":"deeply/nested/directory/structure/file_with_long_name_%04d.go","Matches":[{"Line":"hit","LineNumber":1,"Before":[],"After":[]}]}`, i)
	}
	b.WriteString(`],"FilesWithMatch":`)
	fmt.Fprintf(&b, `%d}}}`, n)
	return b.String()
}

func TestDoSearch_TokenBudget_PassesThroughSmallResponse(t *testing.T) {
	hound := `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"foo","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	budget := 2000
	out, err := doSearch(server.URL, SearchInput{Query: "foo", MaxResponseTokens: &budget})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	// Tiny response should pass through verbatim — no truncated/next_offset keys.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := top["truncated"]; ok {
		t.Errorf("did not expect truncated key for small response: %s", out)
	}
}

func TestDoSearch_TokenBudget_TruncatesLargeFilesOnlyResponse(t *testing.T) {
	hound := makeFilesOnlyHoundResponse(200)
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	// 200 files × ~80 bytes per files-only entry ~ 16k bytes ~ 4000 tokens.
	// Set budget to 200 tokens (~800 bytes) so truncation must kick in.
	budget := 200
	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true, MaxResponseTokens: &budget})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}

	var got struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
		Truncated  bool `json:"truncated"`
		NextOffset int  `json:"next_offset"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if !got.Truncated {
		t.Errorf("expected truncated=true with budget=%d, got %d files in %d bytes", budget, len(got.Files), len(out))
	}
	if len(got.Files) >= 200 {
		t.Errorf("expected fewer than 200 files after truncation, got %d", len(got.Files))
	}
	if got.NextOffset != len(got.Files) {
		t.Errorf("next_offset = %d, want %d (number of files included)", got.NextOffset, len(got.Files))
	}
	if len(out) > budget*8 {
		// Allow generous slack since the budget is approximate (bytes ≈ 4 tokens).
		t.Errorf("output %d bytes exceeded budget*8 (%d) — truncation too lax", len(out), budget*8)
	}
}

func TestDoSearch_TokenBudget_FilesOnlyOffsetSkipsFiles(t *testing.T) {
	hound := makeFilesOnlyHoundResponse(50)
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	offset := 10
	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true, Offset: &offset})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Files) != 40 {
		t.Errorf("got %d files after offset=10, want 40", len(got.Files))
	}
	// First file in the sorted list with offset=10 should be ..._0010.go
	if len(got.Files) > 0 && !strings.Contains(got.Files[0].Path, "_0010.go") {
		t.Errorf("after offset=10, expected first file to be _0010.go, got %q", got.Files[0].Path)
	}
}

func TestDoSearch_TokenBudget_ZeroMeansUnlimited(t *testing.T) {
	hound := makeFilesOnlyHoundResponse(100)
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	budget := 0 // explicit opt-out of budgeting
	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true, MaxResponseTokens: &budget})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Files     []json.RawMessage `json:"files"`
		Truncated bool              `json:"truncated"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Truncated {
		t.Errorf("budget=0 should mean unlimited; expected truncated=false")
	}
	if len(got.Files) != 100 {
		t.Errorf("expected all 100 files with budget=0, got %d", len(got.Files))
	}
}

func TestDoSearch_TokenBudget_TruncatesLargeRawResponse(t *testing.T) {
	// Build a hound response with 20 files each containing 30 matches with
	// substantial body content, so the raw response is well over 2000 tokens.
	var b strings.Builder
	b.WriteString(`{"Results":{"r":{"Matches":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"Filename":"file_%02d.go","Matches":[`, i)
		for j := 0; j < 30; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"Line":"some moderately long matching line of code that costs tokens %d","LineNumber":%d,"Before":[],"After":[]}`, j, j)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`],"FilesWithMatch":20}}}`)
	hound := b.String()

	server := newFakeHoundServer(t, hound)
	defer server.Close()

	budget := 500
	out, err := doSearch(server.URL, SearchInput{Query: "x", MaxResponseTokens: &budget})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}

	var got struct {
		Results    map[string]json.RawMessage `json:"Results"`
		Truncated  bool                       `json:"truncated"`
		NextOffset int                        `json:"next_offset"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if !got.Truncated {
		t.Errorf("expected truncated=true on oversized raw response (%d bytes)", len(out))
	}
	if got.NextOffset <= 0 {
		t.Errorf("expected next_offset > 0, got %d", got.NextOffset)
	}
	if len(out) > budget*8 {
		t.Errorf("output %d bytes greatly exceeded budget*8 = %d", len(out), budget*8)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
