package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/mcp"
)

type SearchInput struct {
	Query        string `json:"query" jsonschema_description:"Pattern to search for. Treated as an RE2 regular expression unless 'literal' is true. Examples: 'func\\s+Setup\\(', 'TODO\\(\\w+\\)', 'NewServer'."`
	Repos        string `json:"repos,omitempty" jsonschema_description:"Comma-separated list of repository names (as returned by list_repos) to restrict the search to. Use '*' or omit to search every indexed repository."`
	Files        string `json:"files,omitempty" jsonschema_description:"RE2 regex matched against file paths to include. Example: '\\.go$' restricts results to Go files; 'cmd/.*\\.go$' restricts to Go files under cmd/."`
	ExcludeFiles string `json:"excludeFiles,omitempty" jsonschema_description:"RE2 regex matched against file paths to exclude. Common patterns: '_test\\.go$' (Go tests), '\\.pb\\.go$' or '\\.pb\\.cc$' (generated protobuf), '_mock' or '_mocks?\\.' (generated mocks), 'vendor/' or 'node_modules/' (third-party), 'dist/' or 'build/' (build artifacts)."`
	IgnoreCase   bool   `json:"ignoreCase,omitempty" jsonschema_description:"If true the pattern matches case-insensitively. Defaults to false (case-sensitive)."`
	Literal      bool   `json:"literal,omitempty" jsonschema_description:"If true the query is treated as a literal string rather than a regex. Use this when searching for code containing regex metacharacters such as '.', '(' or '*'."`
	Context      *int   `json:"context,omitempty" jsonschema_description:"Number of context lines to include before and after each match. Default 0 (match line only); pass an explicit value (e.g., 2) when you actually need surrounding code. Clamped to a maximum of 20."`
	Limit        *int   `json:"limit,omitempty" jsonschema_description:"Maximum number of matches returned per repository. Lower this when a broad query would otherwise return very large results."`
	FilesOnly    bool   `json:"files_only,omitempty" jsonschema_description:"If true, return only the list of matching file paths with a per-file match count and no match bodies — analogous to 'rg -l'. Use for first-pass discovery, then read specific files with Read."`
}

type GetExcludesInput struct {
	Repo string `json:"repo" jsonschema_description:"Repository name to inspect. Must match one of the names returned by list_repos."`
}

const listReposDescription = "List every source-code repository indexed by this Hound server, keyed by repository name. Call this first to discover which repos exist and to pick names for the 'repos' parameter of the 'search' tool."

const searchToolDescription = `Search the source code of one or more indexed repositories for an RE2 regex (or a literal string when 'literal' is true). This is text matching, not semantic search.

For first-pass discovery prefer 'files_only': true — this returns just the matching file paths with per-file counts (analogous to 'rg -l') and is typically 5–10x cheaper than the default match-body output. Only switch to match-body mode when you actually need to read the hits inline and Read on the file is not a better choice.

Typical workflow:
  1. search({query: "NewServer", files_only: true})
  2. → pick the most relevant path from the result
  3. Read that file (and use search with a tighter regex if you need cross-file follow-ups)

Narrow noisy queries with 'repos', 'files', and 'excludeFiles' before lowering 'limit'. Common excludeFiles patterns: '_test\.go$', '\.pb\.go$', '_mock', 'vendor/', 'node_modules/', 'dist/'.`

const getExcludesDescription = "List the file patterns that Hound excluded from indexing for a given repository. Use this to explain why an expected file or directory is missing from 'search' results, or to confirm whether a path is searchable at all before running queries."

func doSearch(houndAddr string, input SearchInput) (json.RawMessage, error) {
	repos := input.Repos
	if repos == "" {
		repos = "*"
	}

	params := fmt.Sprintf("q=%s&repos=%s&stats=true",
		url.QueryEscape(input.Query),
		url.QueryEscape(repos))

	if input.Files != "" {
		params += "&files=" + url.QueryEscape(input.Files)
	}
	if input.ExcludeFiles != "" {
		params += "&excludeFiles=" + url.QueryEscape(input.ExcludeFiles)
	}
	if input.IgnoreCase {
		params += "&i=true"
	}
	if input.Literal {
		params += "&literal=true"
	}
	// Default context to 0 on the wrapper side so LLM callers get compact
	// match-only output without paying for surrounding lines they rarely use.
	// Callers can still opt back into surrounding context with an explicit value.
	ctxLines := 0
	if input.Context != nil {
		ctxLines = *input.Context
	}
	params += fmt.Sprintf("&ctx=%d", ctxLines)
	if input.Limit != nil {
		params += fmt.Sprintf("&limit=%d", *input.Limit)
	}

	resp, err := http.Get(fmt.Sprintf("%s/api/v1/search?%s", houndAddr, params))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Hound server at %s: %w", houndAddr, err)
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Check for API-level errors (Hound returns 200 with {"Error": "..."})
	var errCheck struct {
		Error string `json:"Error"`
	}
	if json.Unmarshal(raw, &errCheck) == nil && errCheck.Error != "" {
		return nil, fmt.Errorf("hound search error: %s", errCheck.Error)
	}

	if input.FilesOnly {
		return toFilesOnly(raw)
	}

	return raw, nil
}

// houndResponse is the subset of the Hound /api/v1/search response we need to
// transform into the files_only shape. Fields are exported so encoding/json can
// fill them via the matching upper-case keys Hound emits.
type houndResponse struct {
	Results map[string]struct {
		Matches []struct {
			Filename string `json:"Filename"`
			Matches  []struct {
				LineNumber int `json:"LineNumber"`
			} `json:"Matches"`
		} `json:"Matches"`
	} `json:"Results"`
}

type fileEntry struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Matches int    `json:"matches"`
}

type filesOnlyResponse struct {
	FilesOnly bool        `json:"files_only"`
	Files     []fileEntry `json:"files"`
	Truncated bool        `json:"truncated"`
}

func toFilesOnly(raw json.RawMessage) (json.RawMessage, error) {
	var parsed houndResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse hound response for files_only: %w", err)
	}

	out := filesOnlyResponse{FilesOnly: true, Files: []fileEntry{}}
	for repo, r := range parsed.Results {
		for _, fm := range r.Matches {
			out.Files = append(out.Files, fileEntry{
				Repo:    repo,
				Path:    fm.Filename,
				Matches: len(fm.Matches),
			})
		}
	}

	// Stable ordering: by repo then path. Go map iteration is non-deterministic
	// so without this the output flickers between calls.
	sort.Slice(out.Files, func(i, j int) bool {
		if out.Files[i].Repo != out.Files[j].Repo {
			return out.Files[i].Repo < out.Files[j].Repo
		}
		return out.Files[i].Path < out.Files[j].Path
	})

	return json.Marshal(out)
}

func runMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	addr := fs.String("hound-addr", "", "Address of the Hound server (default: http://localhost:6080)")
	fs.Parse(args)

	houndAddr := *addr
	if houndAddr == "" {
		houndAddr = os.Getenv("HOUND_ADDR")
	}
	if houndAddr == "" {
		houndAddr = "http://localhost:6080"
	}

	ctx := context.Background()
	g := genkit.Init(ctx)

	genkit.DefineTool(g, "list_repos",
		listReposDescription,
		func(ctx *ai.ToolContext, _ struct{}) (json.RawMessage, error) {
			resp, err := http.Get(fmt.Sprintf("%s/api/v1/repos", houndAddr))
			if err != nil {
				return nil, fmt.Errorf("failed to connect to Hound server at %s: %w", houndAddr, err)
			}
			defer resp.Body.Close()

			var result json.RawMessage
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return nil, fmt.Errorf("failed to decode response: %w", err)
			}
			return result, nil
		},
	)

	genkit.DefineTool(g, "search",
		searchToolDescription,
		func(ctx *ai.ToolContext, input SearchInput) (json.RawMessage, error) {
			return doSearch(houndAddr, input)
		},
	)

	genkit.DefineTool(g, "get_excludes",
		getExcludesDescription,
		func(ctx *ai.ToolContext, input GetExcludesInput) (json.RawMessage, error) {
			resp, err := http.Get(fmt.Sprintf("%s/api/v1/excludes?repo=%s",
				houndAddr, url.QueryEscape(input.Repo)))
			if err != nil {
				return nil, fmt.Errorf("failed to connect to Hound server at %s: %w", houndAddr, err)
			}
			defer resp.Body.Close()

			var result json.RawMessage
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return nil, fmt.Errorf("failed to decode response: %w", err)
			}
			return result, nil
		},
	)

	s := mcp.NewMCPServer(g, mcp.MCPServerOptions{
		Name:    "hound-mcp",
		Version: "1.0.0",
	})

	if err := s.ServeStdio(); err != nil {
		log.Fatal(err)
	}
}
