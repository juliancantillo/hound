package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestMain silences the package logger so tests don't spam stderr. Tests that
// specifically want to inspect log output call captureLogs to swap in a buffer.
func TestMain(m *testing.M) {
	logger = log.New(io.Discard, "", 0)
	os.Exit(m.Run())
}

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
	// follow up with lines_only when the file list is too long to read
	// whole, and use excludeFiles to filter out generated/vendor noise.
	want := []string{
		"files_only",        // recommended discovery mode
		"lines_only",        // middle-fidelity mode for targeted Reads
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

func TestDoSearch_FilesOnly_StripsCommonRootPrefix(t *testing.T) {
	hound := `{
		"Results": {
			"r": {
				"Matches": [
					{"Filename": "cmd/hound/mcp.go", "Matches": [{"Line":"a","LineNumber":1,"Before":[],"After":[]}]},
					{"Filename": "cmd/hound/mcp_test.go", "Matches": [{"Line":"b","LineNumber":2,"Before":[],"After":[]}]},
					{"Filename": "cmd/hound/main.go", "Matches": [{"Line":"c","LineNumber":3,"Before":[],"After":[]}]}
				],
				"FilesWithMatch": 3
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
		Root  string `json:"root"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if got.Root != "cmd/hound/" {
		t.Errorf("root = %q, want %q\nbody: %s", got.Root, "cmd/hound/", out)
	}
	wantPaths := map[string]bool{"mcp.go": true, "mcp_test.go": true, "main.go": true}
	for _, f := range got.Files {
		if !wantPaths[f.Path] {
			t.Errorf("unexpected path %q after root-stripping (want one of mcp.go/mcp_test.go/main.go)", f.Path)
		}
	}
}

func TestDoSearch_FilesOnly_NoCommonPrefix_OmitsRoot(t *testing.T) {
	hound := `{
		"Results": {
			"r": {
				"Matches": [
					{"Filename": "alpha/x.go", "Matches": [{"Line":"a","LineNumber":1,"Before":[],"After":[]}]},
					{"Filename": "beta/y.go", "Matches": [{"Line":"b","LineNumber":2,"Before":[],"After":[]}]}
				],
				"FilesWithMatch": 2
			}
		}
	}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := got["root"]; ok && string(v) != `""` {
		t.Errorf("expected no root key (or empty) when no common prefix, got root=%s", v)
	}
}

func TestDoSearch_FilesOnly_SingleRepoLiftsRepoToTopLevel(t *testing.T) {
	hound := `{
		"Results": {
			"only-repo": {
				"Matches": [
					{"Filename": "alpha/x.go", "Matches": [{"Line":"a","LineNumber":1,"Before":[],"After":[]}]},
					{"Filename": "beta/y.go", "Matches": [{"Line":"b","LineNumber":2,"Before":[],"After":[]}]}
				],
				"FilesWithMatch": 2
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
		Repo  string `json:"repo"`
		Files []struct {
			Repo string `json:"repo,omitempty"`
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Repo != "only-repo" {
		t.Errorf("top-level repo = %q, want only-repo\nbody: %s", got.Repo, out)
	}
	for _, f := range got.Files {
		if f.Repo != "" {
			t.Errorf("per-file repo should be empty when lifted, got %q", f.Repo)
		}
	}
}

func TestDoSearch_FilesOnly_MultiRepoKeepsRepoPerEntry(t *testing.T) {
	hound := `{
		"Results": {
			"repoA": {"Matches": [{"Filename": "a.go", "Matches": [{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}], "FilesWithMatch": 1},
			"repoB": {"Matches": [{"Filename": "b.go", "Matches": [{"Line":"y","LineNumber":2,"Before":[],"After":[]}]}], "FilesWithMatch": 1}
		}
	}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Repo  string `json:"repo"`
		Files []struct {
			Repo string `json:"repo"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Repo != "" {
		t.Errorf("multi-repo response should not lift repo, got %q", got.Repo)
	}
	for _, f := range got.Files {
		if f.Repo == "" {
			t.Errorf("multi-repo response should keep repo on each entry")
		}
	}
}

func TestDoSearch_FilesOnly_CommonPrefixOnlyStripsOnSeparatorBoundary(t *testing.T) {
	// Paths share the literal prefix "cmd/houn" but the boundary must respect
	// the path separator — otherwise we'd hand back broken relative paths.
	hound := `{
		"Results": {
			"r": {
				"Matches": [
					{"Filename": "cmd/hound/mcp.go", "Matches": [{"Line":"a","LineNumber":1,"Before":[],"After":[]}]},
					{"Filename": "cmd/houndd/main.go", "Matches": [{"Line":"b","LineNumber":2,"Before":[],"After":[]}]}
				],
				"FilesWithMatch": 2
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
		Root  string `json:"root"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Root != "cmd/" {
		t.Errorf("root should fall on a / boundary, got %q (want cmd/)\nbody: %s", got.Root, out)
	}
	// Each path must still begin AFTER the boundary, i.e., houn(d|dd)/...
	for _, f := range got.Files {
		if strings.HasPrefix(f.Path, "/") {
			t.Errorf("path should not start with /, got %q", f.Path)
		}
	}
}

func TestSymbolVariants(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"validateComputeMetric", []string{"validateComputeMetric", "ValidateComputeMetric", "validate_compute_metric"}},
		{"ValidateComputeMetric", []string{"validateComputeMetric", "ValidateComputeMetric", "validate_compute_metric"}},
		{"validate_compute_metric", []string{"validateComputeMetric", "ValidateComputeMetric", "validate_compute_metric"}},
		{"NewServer", []string{"newServer", "NewServer", "new_server"}},
		// Single-word identifiers: all three variants collapse to two
		// distinct strings (camel/snake equal, Pascal capitalised).
		{"server", []string{"server", "Server"}},
	}
	for _, c := range cases {
		got := symbolVariants(c.in)
		gotSet := map[string]bool{}
		for _, v := range got {
			gotSet[v] = true
		}
		for _, w := range c.want {
			if !gotSet[w] {
				t.Errorf("symbolVariants(%q): missing variant %q (got %v)", c.in, w, got)
			}
		}
		// No duplicates.
		seen := map[string]bool{}
		for _, v := range got {
			if seen[v] {
				t.Errorf("symbolVariants(%q): duplicate variant %q in %v", c.in, v, got)
			}
			seen[v] = true
		}
	}
}

func TestDoSearch_SymbolKind_SendsORRegexOfVariants(t *testing.T) {
	var captured string
	server := newCapturingHoundServer(t, `{"Results":{}}`, &captured)
	defer server.Close()

	_, err := doSearch(server.URL, SearchInput{Query: "newServer", Kind: "symbol"})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}

	// The upstream query string is URL-encoded. Decode and inspect.
	decoded, err := url.QueryUnescape(captured)
	if err != nil {
		t.Fatalf("decode captured query: %v\n%s", err, captured)
	}
	// All three variants should appear in the regex.
	for _, want := range []string{"newServer", "NewServer", "new_server"} {
		if !strings.Contains(decoded, want) {
			t.Errorf("symbol query missing variant %q in upstream query: %s", want, decoded)
		}
	}
	// The wrapper should not also pass literal=true (the regex would lose
	// alternation semantics) and should add word boundaries to keep noise low.
	if strings.Contains(decoded, "literal=true") {
		t.Errorf("symbol mode should not pass literal=true; query: %s", decoded)
	}
	if !strings.Contains(decoded, `\b`) {
		t.Errorf("symbol mode should anchor each variant with \\b; query: %s", decoded)
	}
}

// captureLogs swaps the package-level logger to a buffer for the duration of
// a test and returns the buffer plus a restore func. The MCP server normally
// writes to stderr; tests don't want that.
func captureLogs(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := logger
	logger = log.New(buf, "", 0)
	return buf, func() { logger = prev }
}

func TestDoSearch_LogsToolCallSummary(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	server := newFakeHoundServer(t, `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"hit","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`)
	defer server.Close()

	if _, err := doSearch(server.URL, SearchInput{Query: "needle", FilesOnly: true}); err != nil {
		t.Fatalf("doSearch: %v", err)
	}

	out := buf.String()
	want := []string{
		"search",   // tool name
		"needle",   // query echoed
		"files_only", // mode flag visible
		"bytes",    // response size mentioned
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("log missing %q\nfull log:\n%s", w, out)
		}
	}
}

func TestDoSearch_LogsErrorWhenHoundUnreachable(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	// Point at a closed server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	_, err := doSearch(srv.URL, SearchInput{Query: "x"})
	if err == nil {
		t.Fatalf("expected error talking to a closed server")
	}
	if !strings.Contains(buf.String(), "ERROR") && !strings.Contains(buf.String(), "error") {
		t.Errorf("expected the log to mention the error; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "search") {
		t.Errorf("expected the log to mention the tool name; got: %s", buf.String())
	}
	// The error returned to the model should also lead with the tool name so
	// the model knows which call failed without having to parse the message.
	if !strings.Contains(err.Error(), "hound search") {
		t.Errorf("error returned to caller should begin with 'hound search', got: %v", err)
	}
}

func TestDoSearch_LogIncludesTruncationFlag(t *testing.T) {
	buf, restore := captureLogs(t)
	defer restore()

	hound := makeFilesOnlyHoundResponse(200)
	server := newFakeHoundServer(t, hound)
	defer server.Close()
	budget := 200
	if _, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true, MaxResponseTokens: &budget}); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	if !strings.Contains(buf.String(), "truncated=true") {
		t.Errorf("expected log to flag truncation; got: %s", buf.String())
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

// linesOnlyDecoded is the shape we unmarshal lines_only responses into.
// Defined here once so multiple tests share the schema.
type linesOnlyDecoded struct {
	LinesOnly bool   `json:"lines_only"`
	Repo      string `json:"repo"`
	Root      string `json:"root"`
	Files     []struct {
		Repo  string `json:"repo"`
		Path  string `json:"path"`
		Lines []int  `json:"lines"`
	} `json:"files"`
	Truncated  bool `json:"truncated"`
	NextOffset int  `json:"next_offset"`
}

func TestDoSearch_LinesOnly_ReturnsLineNumbersOnly(t *testing.T) {
	hound := `{
		"Results": {
			"myrepo": {
				"Matches": [
					{"Filename": "pkg/a.go", "Matches": [
						{"Line": "foo", "LineNumber": 10, "Before": [], "After": []},
						{"Line": "bar", "LineNumber": 42, "Before": [], "After": []}
					]},
					{"Filename": "pkg/b.go", "Matches": [
						{"Line": "qux", "LineNumber": 7, "Before": [], "After": []}
					]}
				],
				"FilesWithMatch": 2,
				"Revision": "abc"
			}
		}
	}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "foo", LinesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}

	var got linesOnlyDecoded
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if !got.LinesOnly {
		t.Errorf("lines_only flag not set in response: %s", out)
	}
	if len(got.Files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(got.Files), got.Files)
	}

	// Files come back sorted by repo, then path; with the common-prefix lift
	// each path is just the basename.
	byPath := map[string][]int{}
	for _, f := range got.Files {
		byPath[f.Path] = f.Lines
	}
	if !equalIntSlice(byPath["a.go"], []int{10, 42}) {
		t.Errorf("a.go lines = %v, want [10 42]", byPath["a.go"])
	}
	if !equalIntSlice(byPath["b.go"], []int{7}) {
		t.Errorf("b.go lines = %v, want [7]", byPath["b.go"])
	}
}

func TestDoSearch_LinesOnly_StripsMatchBodies(t *testing.T) {
	hound := `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"SENSITIVE_LINE_CONTENT","LineNumber":1,"Before":["before-a","before-b"],"After":["after-a","after-b"]}]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", LinesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	for _, leak := range []string{"SENSITIVE_LINE_CONTENT", "before-a", "after-a"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("lines_only response leaked %q: %s", leak, out)
		}
	}
}

func TestDoSearch_LinesOnly_DedupesAndSortsLines(t *testing.T) {
	// Hound can return duplicate LineNumbers if the same line matches more
	// than once. lines_only should dedupe and emit ascending order.
	hound := `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[
		{"Line":"a","LineNumber":42,"Before":[],"After":[]},
		{"Line":"b","LineNumber":7,"Before":[],"After":[]},
		{"Line":"c","LineNumber":42,"Before":[],"After":[]},
		{"Line":"d","LineNumber":13,"Before":[],"After":[]}
	]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", LinesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got linesOnlyDecoded
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(got.Files))
	}
	if !equalIntSlice(got.Files[0].Lines, []int{7, 13, 42}) {
		t.Errorf("lines = %v, want [7 13 42] (sorted, deduped)", got.Files[0].Lines)
	}
}

func TestDoSearch_LinesOnly_StripsCommonRootPrefix(t *testing.T) {
	hound := `{"Results":{"r":{"Matches":[
		{"Filename":"go/src/fs/services/a/x.go","Matches":[{"Line":"a","LineNumber":1,"Before":[],"After":[]}]},
		{"Filename":"go/src/fs/services/b/y.go","Matches":[{"Line":"b","LineNumber":2,"Before":[],"After":[]}]},
		{"Filename":"go/src/fs/services/c/z.go","Matches":[{"Line":"c","LineNumber":3,"Before":[],"After":[]}]}
	],"FilesWithMatch":3}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", LinesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got linesOnlyDecoded
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Root != "go/src/fs/services/" {
		t.Errorf("root = %q, want %q", got.Root, "go/src/fs/services/")
	}
	gotPaths := map[string]bool{}
	for _, f := range got.Files {
		gotPaths[f.Path] = true
	}
	for _, want := range []string{"a/x.go", "b/y.go", "c/z.go"} {
		if !gotPaths[want] {
			t.Errorf("missing stripped path %q in %+v", want, got.Files)
		}
	}
}

func TestDoSearch_LinesOnly_SingleRepoLiftsRepoToTopLevel(t *testing.T) {
	hound := `{"Results":{"onlyrepo":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"a","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", LinesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got linesOnlyDecoded
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Repo != "onlyrepo" {
		t.Errorf("repo not lifted to top level; got %q in response: %s", got.Repo, out)
	}
	for _, f := range got.Files {
		if f.Repo != "" {
			t.Errorf("per-entry repo still populated after lift: %+v", f)
		}
	}
}

func TestDoSearch_LinesOnly_BudgetTruncates(t *testing.T) {
	// Build a response with many small files so the budget can drop some.
	var b strings.Builder
	b.WriteString(`{"Results":{"r":{"Matches":[`)
	const n = 50
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"Filename":"pkg/sub/file_%04d.go","Matches":[{"Line":"a","LineNumber":1,"Before":[],"After":[]}]}`, i)
	}
	b.WriteString(`],"FilesWithMatch":`)
	fmt.Fprintf(&b, `%d}}}`, n)
	server := newFakeHoundServer(t, b.String())
	defer server.Close()

	budget := 100 // ~400 bytes — too small to fit all 50 entries
	out, err := doSearch(server.URL, SearchInput{Query: "x", LinesOnly: true, MaxResponseTokens: &budget})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got linesOnlyDecoded
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Truncated {
		t.Errorf("expected truncated=true with tight budget, got: %s", out)
	}
	if got.NextOffset == 0 {
		t.Errorf("expected non-zero next_offset on truncated response, got: %s", out)
	}
	if len(got.Files) >= n {
		t.Errorf("budget did not drop any files: got %d/%d", len(got.Files), n)
	}
}

func TestDoSearch_LinesOnly_OffsetPaginates(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"Results":{"r":{"Matches":[`)
	for i := 0; i < 10; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"Filename":"file_%02d.go","Matches":[{"Line":"a","LineNumber":1,"Before":[],"After":[]}]}`, i)
	}
	b.WriteString(`],"FilesWithMatch":10}}}`)
	server := newFakeHoundServer(t, b.String())
	defer server.Close()

	offset := 5
	out, err := doSearch(server.URL, SearchInput{Query: "x", LinesOnly: true, Offset: &offset})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got linesOnlyDecoded
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Files) != 5 {
		t.Fatalf("offset=5 should leave 5 files, got %d", len(got.Files))
	}
	if got.Files[0].Path != "file_05.go" {
		t.Errorf("first file after offset = %q, want file_05.go", got.Files[0].Path)
	}
}

func TestDoSearch_FilesOnlyTakesPrecedenceOverLinesOnly(t *testing.T) {
	// When both flags are set, files_only wins (it's the cheaper output).
	hound := `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"a","LineNumber":42,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true, LinesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := top["files_only"]; !ok {
		t.Errorf("expected files_only response shape when both flags set: %s", out)
	}
	if _, ok := top["lines_only"]; ok {
		t.Errorf("lines_only field leaked into files_only response: %s", out)
	}
}

func TestDoSearch_LinesOnly_LogIncludesFlag(t *testing.T) {
	hound := `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"a","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	buf, restore := captureLogs(t)
	defer restore()

	if _, err := doSearch(server.URL, SearchInput{Query: "foo", LinesOnly: true}); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	if !strings.Contains(buf.String(), "lines_only=true") {
		t.Errorf("log line missing lines_only=true flag: %s", buf.String())
	}
}

func equalIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newDualRouteHoundServer returns a test server that responds to
// /api/v1/repos with reposBody and to anything else (e.g. /api/v1/search)
// with searchBody. Lets a single fixture drive both the wrapper's
// working_copy_root lookup and the actual search response.
func newDualRouteHoundServer(t *testing.T, reposBody, searchBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v1/repos") {
			_, _ = w.Write([]byte(reposBody))
			return
		}
		_, _ = w.Write([]byte(searchBody))
	}))
}

func TestDoSearch_FilesOnly_IncludesWorkingCopyRoot(t *testing.T) {
	resetRepoCache()
	reposBody := `{"only-repo":{"url":"https://example.com/x.git","working-copy-root":"/Users/julian/src/mn/projects/fullstory/"}}`
	searchBody := `{"Results":{"only-repo":{"Matches":[{"Filename":"go/src/fs/a.go","Matches":[{"Line":"hit","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Repo            string `json:"repo"`
		WorkingCopyRoot string `json:"working_copy_root"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if got.WorkingCopyRoot != "/Users/julian/src/mn/projects/fullstory/" {
		t.Errorf("working_copy_root = %q, want the configured path\nbody: %s", got.WorkingCopyRoot, out)
	}
	if got.Repo != "only-repo" {
		t.Errorf("expected repo lifted to top level, got %q", got.Repo)
	}
}

func TestDoSearch_FilesOnly_WorkingCopyRootOmittedWhenUnconfigured(t *testing.T) {
	resetRepoCache()
	reposBody := `{"only-repo":{"url":"https://example.com/x.git"}}` // no working-copy-root
	searchBody := `{"Results":{"only-repo":{"Matches":[{"Filename":"a.go","Matches":[{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := top["working_copy_root"]; ok {
		t.Errorf("working_copy_root key should be omitted when unconfigured: %s", out)
	}
}

func TestDoSearch_FilesOnly_MultiRepo_WorkingCopyRootPerEntry(t *testing.T) {
	resetRepoCache()
	reposBody := `{
		"repoA":{"url":"https://example.com/a.git","working-copy-root":"/disk/a/"},
		"repoB":{"url":"https://example.com/b.git","working-copy-root":"/disk/b/"}
	}`
	searchBody := `{
		"Results": {
			"repoA":{"Matches":[{"Filename":"a.go","Matches":[{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1},
			"repoB":{"Matches":[{"Filename":"b.go","Matches":[{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}
		}
	}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Repo            string `json:"repo"`
		WorkingCopyRoot string `json:"working_copy_root"`
		Files           []struct {
			Repo            string `json:"repo"`
			Path            string `json:"path"`
			WorkingCopyRoot string `json:"working_copy_root"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	// Top-level fields should be empty for multi-repo.
	if got.WorkingCopyRoot != "" {
		t.Errorf("top-level working_copy_root should be empty for multi-repo response, got %q", got.WorkingCopyRoot)
	}
	byRepo := map[string]string{}
	for _, f := range got.Files {
		byRepo[f.Repo] = f.WorkingCopyRoot
	}
	if byRepo["repoA"] != "/disk/a/" {
		t.Errorf("repoA file working_copy_root = %q, want /disk/a/", byRepo["repoA"])
	}
	if byRepo["repoB"] != "/disk/b/" {
		t.Errorf("repoB file working_copy_root = %q, want /disk/b/", byRepo["repoB"])
	}
}

func TestDoSearch_LinesOnly_IncludesWorkingCopyRoot(t *testing.T) {
	resetRepoCache()
	reposBody := `{"only-repo":{"working-copy-root":"/disk/proj/"}}`
	searchBody := `{"Results":{"only-repo":{"Matches":[{"Filename":"a.go","Matches":[{"Line":"x","LineNumber":7,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", LinesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		WorkingCopyRoot string `json:"working_copy_root"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.WorkingCopyRoot != "/disk/proj/" {
		t.Errorf("working_copy_root = %q, want /disk/proj/\nbody: %s", got.WorkingCopyRoot, out)
	}
}

func TestDoSearch_RawMode_PerRepoWorkingCopyRoot(t *testing.T) {
	resetRepoCache()
	// Two-repo response (forces wrapper into the structural rewrite path)
	// and one of them has a working-copy-root configured.
	reposBody := `{
		"repoA":{"working-copy-root":"/disk/a/"},
		"repoB":{}
	}`
	searchBody := `{
		"Results": {
			"repoA":{"Matches":[{"Filename":"a.go","Matches":[{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1},
			"repoB":{"Matches":[{"Filename":"b.go","Matches":[{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}
		}
	}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	// Force the wrapper through its rewrite path by setting a low token budget.
	budget := 50
	out, err := doSearch(server.URL, SearchInput{Query: "x", MaxResponseTokens: &budget})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Results map[string]struct {
			WorkingCopyRoot string `json:"working_copy_root"`
		} `json:"Results"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if rA, ok := got.Results["repoA"]; ok {
		if rA.WorkingCopyRoot != "/disk/a/" {
			t.Errorf("repoA.working_copy_root = %q, want /disk/a/", rA.WorkingCopyRoot)
		}
	}
	// repoB has no working_copy_root configured; if it survived the budget,
	// its key must be absent (not "").
	if _, ok := got.Results["repoB"]; ok {
		var raw map[string]map[string]json.RawMessage
		_ = json.Unmarshal(out, &raw)
		if _, present := raw["Results"]["repoB"]; present {
			if v, has := raw["Results"]["repoB"]; has {
				var asMap map[string]json.RawMessage
				_ = json.Unmarshal(v, &asMap)
				if _, present := asMap["working_copy_root"]; present {
					t.Errorf("repoB should not have working_copy_root key when unconfigured: %s", out)
				}
			}
		}
	}
}

func TestSearchToolDescription_MentionsWorkingCopyRoot(t *testing.T) {
	if !strings.Contains(searchToolDescription, "working_copy_root") {
		t.Errorf("searchToolDescription should explain working_copy_root usage:\n%s", searchToolDescription)
	}
}

func TestDoSearch_ZeroResults_NonSymbol_HintsSymbol(t *testing.T) {
	resetRepoCache()
	server := newFakeHoundServer(t, `{"Results":{}}`)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "noMatchAnywhere"})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if !strings.Contains(got.Hint, "symbol") {
		t.Errorf("zero-result hint should suggest kind=\"symbol\", got %q\nbody: %s", got.Hint, out)
	}
}

func TestDoSearch_ZeroResults_SymbolKind_HintsDroppingFilters(t *testing.T) {
	resetRepoCache()
	server := newFakeHoundServer(t, `{"Results":{}}`)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{
		Query: "noSuchIdentifier",
		Kind:  "symbol",
		Files: "\\.go$",
	})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	// On the second-tier hint we want a nudge toward removing files/excludeFiles.
	if !strings.Contains(strings.ToLower(got.Hint), "drop") &&
		!strings.Contains(strings.ToLower(got.Hint), "filter") {
		t.Errorf("zero-result+symbol hint should suggest dropping filters, got %q\nbody: %s", got.Hint, out)
	}
}

func TestDoSearch_NonZeroResults_NoHint(t *testing.T) {
	resetRepoCache()
	hound := `{"Results":{"r":{"Matches":[{"Filename":"x.go","Matches":[{"Line":"hit","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "hit"})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := top["hint"]; ok {
		t.Errorf("non-empty result should not include a hint key: %s", out)
	}
}

func TestDoSearch_FilesOnly_ZeroResults_HintsSymbol(t *testing.T) {
	resetRepoCache()
	server := newFakeHoundServer(t, `{"Results":{}}`)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "nothing", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if !strings.Contains(got.Hint, "symbol") {
		t.Errorf("files_only zero-result hint should suggest kind=\"symbol\", got %q\nbody: %s", got.Hint, out)
	}
}

func TestDoSearch_LinesOnly_ZeroResults_HintsSymbol(t *testing.T) {
	resetRepoCache()
	server := newFakeHoundServer(t, `{"Results":{}}`)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "nothing", LinesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if !strings.Contains(got.Hint, "symbol") {
		t.Errorf("lines_only zero-result hint should suggest kind=\"symbol\", got %q\nbody: %s", got.Hint, out)
	}
}

func TestDoSearch_StrictPaths_DropsMissingFiles(t *testing.T) {
	resetRepoCache()
	tmp := t.TempDir()
	// Create one file on disk; the other will be a hound-only ghost.
	realPath := "alpha/exists.go"
	if err := os.MkdirAll(tmp+"/alpha", 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(tmp+"/"+realPath, []byte("// ok\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	reposBody := fmt.Sprintf(`{"only-repo":{"working-copy-root":%q}}`, tmp+"/")
	searchBody := `{
		"Results": {
			"only-repo": {
				"Matches": [
					{"Filename": "alpha/exists.go", "Matches": [{"Line":"hit","LineNumber":1,"Before":[],"After":[]}]},
					{"Filename": "alpha/ghost.go",  "Matches": [{"Line":"hit","LineNumber":2,"Before":[],"After":[]}]}
				],
				"FilesWithMatch": 2
			}
		}
	}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "hit", FilesOnly: true, StrictPaths: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if len(got.Files) != 1 {
		t.Fatalf("strict_paths should leave only the existing file, got %d entries: %s", len(got.Files), out)
	}
	if !strings.HasSuffix(got.Files[0].Path, "exists.go") {
		t.Errorf("kept file = %q, want the one that exists on disk (exists.go)", got.Files[0].Path)
	}
}

func TestDoSearch_StrictPaths_LinesOnly_DropsMissingFiles(t *testing.T) {
	resetRepoCache()
	tmp := t.TempDir()
	if err := os.WriteFile(tmp+"/exists.go", []byte("ok"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	reposBody := fmt.Sprintf(`{"r":{"working-copy-root":%q}}`, tmp+"/")
	searchBody := `{
		"Results": {
			"r": {
				"Matches": [
					{"Filename": "exists.go", "Matches": [{"Line":"a","LineNumber":1,"Before":[],"After":[]}]},
					{"Filename": "ghost.go",  "Matches": [{"Line":"b","LineNumber":1,"Before":[],"After":[]}]}
				],
				"FilesWithMatch": 2
			}
		}
	}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", LinesOnly: true, StrictPaths: true})
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
	if len(got.Files) != 1 || !strings.Contains(got.Files[0].Path, "exists.go") {
		t.Errorf("expected only exists.go, got %+v\nbody: %s", got.Files, out)
	}
}

func TestDoSearch_StrictPaths_KeepsAllWhenNoWorkingCopyRoot(t *testing.T) {
	// strict_paths needs a working_copy_root to resolve files against. When
	// none is configured, we don't know where to look — keep entries rather
	// than silently dropping everything.
	resetRepoCache()
	reposBody := `{"r":{}}` // no working-copy-root
	searchBody := `{"Results":{"r":{"Matches":[
		{"Filename":"a.go","Matches":[{"Line":"x","LineNumber":1,"Before":[],"After":[]}]},
		{"Filename":"b.go","Matches":[{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}
	],"FilesWithMatch":2}}}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true, StrictPaths: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Files []json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Files) != 2 {
		t.Errorf("expected to keep both files when no working_copy_root, got %d: %s", len(got.Files), out)
	}
}

func TestDoSearch_StrictPaths_DefaultIsNoFiltering(t *testing.T) {
	// Without strict_paths, missing files survive.
	resetRepoCache()
	tmp := t.TempDir() // empty — nothing on disk
	reposBody := fmt.Sprintf(`{"r":{"working-copy-root":%q}}`, tmp+"/")
	searchBody := `{"Results":{"r":{"Matches":[{"Filename":"ghost.go","Matches":[{"Line":"x","LineNumber":1,"Before":[],"After":[]}]}],"FilesWithMatch":1}}}`
	server := newDualRouteHoundServer(t, reposBody, searchBody)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "x", FilesOnly: true})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Files []json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Files) != 1 {
		t.Errorf("strict_paths defaults to off; missing files should survive. got %d: %s", len(got.Files), out)
	}
}

func TestDoSearch_ErrorMessageIncludesToolName(t *testing.T) {
	resetRepoCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	_, err := doSearch(srv.URL, SearchInput{Query: "x"})
	if err == nil {
		t.Fatalf("expected error against closed server")
	}
	if !strings.Contains(err.Error(), "hound search") {
		t.Errorf("error should be tagged with tool name 'hound search', got: %v", err)
	}
}

func TestDoListRepos_ErrorMessageIncludesToolName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	_, err := doListRepos(srv.URL)
	if err == nil {
		t.Fatalf("expected error against closed server")
	}
	if !strings.Contains(err.Error(), "hound list_repos") {
		t.Errorf("error should be tagged with tool name 'hound list_repos', got: %v", err)
	}
}

func TestDoGetExcludes_ErrorMessageIncludesToolName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	_, err := doGetExcludes(srv.URL, GetExcludesInput{Repo: "x"})
	if err == nil {
		t.Fatalf("expected error against closed server")
	}
	if !strings.Contains(err.Error(), "hound get_excludes") {
		t.Errorf("error should be tagged with tool name 'hound get_excludes', got: %v", err)
	}
}

func TestDoSearch_AllRepoMatchesEmpty_HintsSymbol(t *testing.T) {
	// Hound might include the repo key with an empty Matches array; that's
	// still effectively a zero-result outcome.
	resetRepoCache()
	hound := `{"Results":{"r":{"Matches":[],"FilesWithMatch":0}}}`
	server := newFakeHoundServer(t, hound)
	defer server.Close()

	out, err := doSearch(server.URL, SearchInput{Query: "nothing"})
	if err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var got struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, out)
	}
	if !strings.Contains(got.Hint, "symbol") {
		t.Errorf("Results with only empty Matches should still hint symbol, got %q\nbody: %s", got.Hint, out)
	}
}
