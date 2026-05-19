package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
