package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/mcp"
)

// logger is the package-level logger used by every tool handler. It writes to
// stderr by default, which is the only stream MCP-over-stdio leaves free for
// non-protocol output. Tests swap it out via captureLogs. Pass --log-file at
// startup to redirect it to a file instead — useful because Claude Code
// usually swallows the stderr of MCP servers it launches.
var logger = log.New(os.Stderr, "[hound-mcp] ", log.LstdFlags|log.Lmicroseconds)

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
	MaxResponseTokens *int `json:"max_response_tokens,omitempty" jsonschema_description:"Approximate cap on the response token count (1 token ≈ 4 bytes). Default 2000. When exceeded, files are dropped from the end of the result; the response gains 'truncated': true and 'next_offset' so the caller can paginate via the 'offset' field. Set to 0 to disable the cap."`
	Offset            *int `json:"offset,omitempty" jsonschema_description:"Number of files to skip from the start of the result set, for paginating responses that were previously truncated. Pair with the 'next_offset' value returned in a prior truncated response."`
	Kind              string `json:"kind,omitempty" jsonschema_description:"Optional query helper. 'symbol' treats 'query' as an identifier and searches for its case/style variants (camelCase, PascalCase, snake_case) in a single call, with word-boundary anchoring. Default (empty/unset) treats 'query' as an RE2 regex (or literal when 'literal' is true)."`
}

type GetExcludesInput struct {
	Repo string `json:"repo" jsonschema_description:"Repository name to inspect. Must match one of the names returned by list_repos."`
}

const listReposDescription = "List every source-code repository indexed by this Hound server, keyed by repository name. Call this first to discover which repos exist and to pick names for the 'repos' parameter of the 'search' tool."

const searchToolDescription = `Search the source code of one or more indexed repositories for an RE2 regex (or a literal string when 'literal' is true). This is text matching, not semantic search.

For first-pass discovery prefer 'files_only': true — this returns just the matching file paths with per-file counts (analogous to 'rg -l') and is typically 5–10x cheaper than the default match-body output. Only switch to match-body mode when you actually need to read the hits inline and Read on the file is not a better choice.

If you don't know the casing convention used in the codebase (camelCase vs PascalCase vs snake_case), pass kind: "symbol" with an identifier; the wrapper runs all three variants in one search instead of you probing each.

Typical workflow:
  1. search({query: "NewServer", files_only: true})
  2. → pick the most relevant path from the result
  3. Read that file (and use search with a tighter regex if you need cross-file follow-ups)

Narrow noisy queries with 'repos', 'files', and 'excludeFiles' before lowering 'limit'. Common excludeFiles patterns: '_test\.go$', '\.pb\.go$', '_mock', 'vendor/', 'node_modules/', 'dist/'.`

const getExcludesDescription = "List the file patterns that Hound excluded from indexing for a given repository. Use this to explain why an expected file or directory is missing from 'search' results, or to confirm whether a path is searchable at all before running queries."

func doSearch(houndAddr string, input SearchInput) (json.RawMessage, error) {
	start := time.Now()
	out, err := doSearchInner(houndAddr, input)
	dur := time.Since(start)
	if err != nil {
		logger.Printf("search query=%q files_only=%v kind=%q ERROR in %s: %v",
			input.Query, input.FilesOnly, input.Kind, dur, err)
		return nil, err
	}
	truncated := bytes.Contains(out, []byte(`"truncated":true`))
	logger.Printf("search query=%q files_only=%v kind=%q → %d bytes in %s truncated=%v",
		input.Query, input.FilesOnly, input.Kind, len(out), dur, truncated)
	return out, nil
}

func doSearchInner(houndAddr string, input SearchInput) (json.RawMessage, error) {
	repos := input.Repos
	if repos == "" {
		repos = "*"
	}

	query := input.Query
	literal := input.Literal
	if input.Kind == "symbol" {
		// Build an OR-regex of identifier variants, word-anchored, so a single
		// search covers the cases a model would otherwise probe one-by-one.
		// Force literal off — literal mode would treat the parentheses and pipes
		// as literal characters and the alternation would never match.
		query = symbolQueryRegex(input.Query)
		literal = false
	}

	params := fmt.Sprintf("q=%s&repos=%s&stats=true",
		url.QueryEscape(query),
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
	if literal {
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

	budget := defaultMaxResponseTokens
	if input.MaxResponseTokens != nil {
		budget = *input.MaxResponseTokens
	}
	offset := 0
	if input.Offset != nil {
		offset = *input.Offset
	}

	if input.FilesOnly {
		return toFilesOnly(raw, budget, offset)
	}

	return applyTokenBudget(raw, budget, offset)
}

// defaultMaxResponseTokens is the wrapper-side cap on the approximate token
// count of any search response. 2000 tokens keeps the per-call cost bounded
// even when the underlying query is broad. Set to 0 to disable.
const defaultMaxResponseTokens = 2000

// bytesPerToken is the rough Anthropic byte→token ratio. JSON tends to use
// short ASCII, so this slightly underestimates token count, which is fine for
// "cap the response" — we'd rather truncate a little early than blow the cap.
const bytesPerToken = 4

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
	Repo    string `json:"repo,omitempty"`
	Path    string `json:"path"`
	Matches int    `json:"matches"`
}

type filesOnlyResponse struct {
	FilesOnly  bool        `json:"files_only"`
	Repo       string      `json:"repo,omitempty"`
	Root       string      `json:"root,omitempty"`
	Files      []fileEntry `json:"files"`
	Truncated  bool        `json:"truncated"`
	NextOffset int         `json:"next_offset,omitempty"`
}

// splitIdentifier breaks an identifier into its word-parts regardless of the
// casing convention used by the input. Recognises:
//   - snake_case: split on '_'
//   - camelCase / PascalCase: split before each uppercase letter that follows
//     a lowercase letter or digit.
// Parts are returned lowercased so callers can re-emit in any casing.
func splitIdentifier(s string) []string {
	if s == "" {
		return nil
	}
	if strings.Contains(s, "_") {
		parts := []string{}
		for _, p := range strings.Split(s, "_") {
			if p != "" {
				parts = append(parts, strings.ToLower(p))
			}
		}
		return parts
	}
	parts := []string{}
	start := 0
	for i := 1; i < len(s); i++ {
		c := s[i]
		prev := s[i-1]
		if isUpper(c) && !isUpper(prev) {
			parts = append(parts, strings.ToLower(s[start:i]))
			start = i
		}
	}
	parts = append(parts, strings.ToLower(s[start:]))
	return parts
}

func isUpper(c byte) bool {
	return c >= 'A' && c <= 'Z'
}

// symbolVariants returns the de-duplicated set of casing variants for an
// identifier — camelCase, PascalCase, snake_case. For a single-word input
// (e.g. "server"), camel and snake collapse to one entry.
func symbolVariants(s string) []string {
	parts := splitIdentifier(s)
	if len(parts) == 0 {
		return nil
	}
	camel := parts[0]
	pascal := strings.ToUpper(parts[0][:1]) + parts[0][1:]
	for _, p := range parts[1:] {
		camel += strings.ToUpper(p[:1]) + p[1:]
		pascal += strings.ToUpper(p[:1]) + p[1:]
	}
	snake := strings.Join(parts, "_")

	out := []string{}
	seen := map[string]bool{}
	for _, v := range []string{camel, pascal, snake} {
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out
}

// symbolQueryRegex builds the RE2 alternation hound should run for kind=symbol.
// Each variant is anchored with \b so we don't match inside other identifiers.
func symbolQueryRegex(s string) string {
	variants := symbolVariants(s)
	if len(variants) == 0 {
		return s
	}
	if len(variants) == 1 {
		return `\b` + variants[0] + `\b`
	}
	return `\b(` + strings.Join(variants, "|") + `)\b`
}

// commonPathPrefix returns the longest path-segment-aligned prefix shared by
// all input paths. "Path-segment-aligned" means the returned prefix ends at a
// '/' boundary, so stripping it never produces broken-looking sub-paths.
// Returns "" if there is no common prefix or only one path.
func commonPathPrefix(paths []string) string {
	if len(paths) < 2 {
		return ""
	}
	pfx := paths[0]
	for _, p := range paths[1:] {
		n := 0
		for n < len(pfx) && n < len(p) && pfx[n] == p[n] {
			n++
		}
		pfx = pfx[:n]
		if pfx == "" {
			return ""
		}
	}
	// Truncate to the last '/' so we don't split a path segment.
	i := strings.LastIndex(pfx, "/")
	if i < 0 {
		return ""
	}
	return pfx[:i+1]
}

func toFilesOnly(raw json.RawMessage, budget, offset int) (json.RawMessage, error) {
	var parsed houndResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse hound response for files_only: %w", err)
	}

	all := []fileEntry{}
	for repo, r := range parsed.Results {
		for _, fm := range r.Matches {
			all = append(all, fileEntry{
				Repo:    repo,
				Path:    fm.Filename,
				Matches: len(fm.Matches),
			})
		}
	}

	// Stable ordering: by repo then path. Go map iteration is non-deterministic
	// so without this the output flickers between calls and pagination breaks.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Repo != all[j].Repo {
			return all[i].Repo < all[j].Repo
		}
		return all[i].Path < all[j].Path
	})

	if offset > len(all) {
		offset = len(all)
	}
	windowed := all[offset:]

	out := filesOnlyResponse{FilesOnly: true, Files: []fileEntry{}}
	compactFilesOnly(&out, windowed)
	if budget <= 0 {
		// budget=0 disables truncation entirely.
		return json.Marshal(out)
	}

	// At this point compactFilesOnly has already populated out.Files with the
	// compacted (root-stripped, repo-lifted) entries.
	compactEntries := out.Files
	out.Files = []fileEntry{}
	budgetBytes := budget * bytesPerToken
	for i, f := range compactEntries {
		out.Files = append(out.Files, f)
		size, err := approxJSONSize(out)
		if err != nil {
			return nil, err
		}
		if size > budgetBytes {
			// Roll back the last entry — it pushed us over.
			out.Files = out.Files[:len(out.Files)-1]
			out.Truncated = true
			out.NextOffset = offset + i
			break
		}
	}

	return json.Marshal(out)
}

// compactFilesOnly populates out.Files from the given entries while applying
// two token-saving rewrites:
//   - if every entry comes from the same repo, lift the repo name to the
//     top-level out.Repo and clear it on each entry;
//   - if every entry's path shares a common '/'-aligned prefix, strip it and
//     report it as out.Root.
//
// Entries are expected to be already sorted (by repo then path) so callers
// can rely on stable output.
func compactFilesOnly(out *filesOnlyResponse, entries []fileEntry) {
	if len(entries) == 0 {
		out.Files = []fileEntry{}
		return
	}

	// Single-repo lift.
	singleRepo := entries[0].Repo
	for _, e := range entries[1:] {
		if e.Repo != singleRepo {
			singleRepo = ""
			break
		}
	}
	if singleRepo != "" {
		out.Repo = singleRepo
	}

	// Common-path-prefix strip.
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	root := commonPathPrefix(paths)
	if root != "" {
		out.Root = root
	}

	out.Files = make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		c := e
		if singleRepo != "" {
			c.Repo = ""
		}
		if root != "" {
			c.Path = strings.TrimPrefix(c.Path, root)
		}
		out.Files = append(out.Files, c)
	}
}

// approxJSONSize returns the JSON-encoded length of v. Used to gauge whether
// we're under the token budget; cheaper than counting tokens directly.
func approxJSONSize(v interface{}) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// applyTokenBudget caps the size of a raw hound search response by dropping
// whole files from the per-repo Matches arrays (in deterministic order)
// until the encoded response fits within budget*bytesPerToken bytes. The
// returned message has a top-level 'truncated' and 'next_offset' added when
// truncation occurred; otherwise the original raw response passes through.
func applyTokenBudget(raw json.RawMessage, budget, offset int) (json.RawMessage, error) {
	if budget <= 0 && offset == 0 {
		return raw, nil
	}
	budgetBytes := budget * bytesPerToken
	if budget > 0 && len(raw) <= budgetBytes && offset == 0 {
		// Cheap pass-through: small enough already and nothing to skip.
		return raw, nil
	}

	// Parse the response into a shape we can edit.
	var parsed struct {
		Results map[string]json.RawMessage `json:"Results"`
		Stats   json.RawMessage            `json:"Stats,omitempty"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Unparseable — pass through rather than failing the caller.
		return raw, nil
	}

	// Walk (repo, file) pairs in deterministic order so offset/next_offset
	// remain stable across calls.
	repos := make([]string, 0, len(parsed.Results))
	for r := range parsed.Results {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	type repoMatches struct {
		Matches        []json.RawMessage `json:"Matches"`
		FilesWithMatch int               `json:"FilesWithMatch"`
		Revision       string            `json:"Revision,omitempty"`
	}
	repoData := map[string]*repoMatches{}
	for _, repo := range repos {
		var rm repoMatches
		if err := json.Unmarshal(parsed.Results[repo], &rm); err != nil {
			continue
		}
		repoData[repo] = &rm
	}

	// Build a flat ordered list of (repo, fileMatchJSON) pairs.
	type pair struct {
		repo string
		file json.RawMessage
	}
	flat := []pair{}
	for _, repo := range repos {
		rm := repoData[repo]
		if rm == nil {
			continue
		}
		for _, fm := range rm.Matches {
			flat = append(flat, pair{repo: repo, file: fm})
		}
	}

	if offset > len(flat) {
		offset = len(flat)
	}
	windowed := flat[offset:]

	// Greedily include whole files until budget exhausted.
	included := map[string][]json.RawMessage{}
	truncated := false
	nextOffset := 0

	// We track an estimate of the response size as we go to avoid re-marshalling
	// after every append in pathological cases. Start from the overhead of the
	// outer wrapper, then add each file's encoded length plus a comma.
	const overhead = len(`{"Results":{},"Stats":,"truncated":true,"next_offset":000000}`)
	estimate := overhead
	if parsed.Stats != nil {
		estimate += len(parsed.Stats)
	}
	// Repo wrapper overhead per repo: `"reponame":{"Matches":[],"FilesWithMatch":0,"Revision":""}`
	repoOverhead := func(repo string, rm *repoMatches) int {
		// 2 for quotes around name, 4 for `":{`, scaffolding for the rest.
		return len(repo) + 2 + 50 + len(rm.Revision)
	}

	usedRepos := map[string]bool{}

	for i, p := range windowed {
		entrySize := len(p.file) + 1 // +1 for comma
		if !usedRepos[p.repo] {
			entrySize += repoOverhead(p.repo, repoData[p.repo])
		}
		if budget > 0 && estimate+entrySize > budgetBytes && len(included) > 0 {
			truncated = true
			nextOffset = offset + i
			break
		}
		included[p.repo] = append(included[p.repo], p.file)
		usedRepos[p.repo] = true
		estimate += entrySize
		// If we let a single oversized file through (because we had nothing),
		// stop after it so the response doesn't keep ballooning.
		if budget > 0 && estimate > budgetBytes {
			truncated = true
			nextOffset = offset + i + 1
			if nextOffset >= len(flat) {
				truncated = false
				nextOffset = 0
			}
			break
		}
	}

	// If nothing was dropped and offset was 0, return the raw response.
	if !truncated && offset == 0 {
		return raw, nil
	}

	// Rebuild Results dict from included matches.
	resultsOut := map[string]json.RawMessage{}
	for _, repo := range repos {
		matches, ok := included[repo]
		if !ok {
			continue
		}
		rm := repoData[repo]
		// Encode a fresh per-repo object with the trimmed Matches list.
		out := struct {
			Matches        []json.RawMessage `json:"Matches"`
			FilesWithMatch int               `json:"FilesWithMatch"`
			Revision       string            `json:"Revision,omitempty"`
		}{
			Matches:        matches,
			FilesWithMatch: rm.FilesWithMatch,
			Revision:       rm.Revision,
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		resultsOut[repo] = encoded
	}

	final := struct {
		Results    map[string]json.RawMessage `json:"Results"`
		Stats      json.RawMessage            `json:"Stats,omitempty"`
		Truncated  bool                       `json:"truncated,omitempty"`
		NextOffset int                        `json:"next_offset,omitempty"`
	}{
		Results:    resultsOut,
		Stats:      parsed.Stats,
		Truncated:  truncated,
		NextOffset: nextOffset,
	}
	if !truncated {
		final.NextOffset = 0
	}
	return json.Marshal(final)
}

func runMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	addr := fs.String("hound-addr", "", "Address of the Hound server (default: http://localhost:6080)")
	logFile := fs.String("log-file", "", "Path to write tool-call logs. Defaults to stderr (often invisible when launched as an MCP server). HOUND_MCP_LOG_FILE env var is used if this flag is empty.")
	fs.Parse(args)

	houndAddr := *addr
	if houndAddr == "" {
		houndAddr = os.Getenv("HOUND_ADDR")
	}
	if houndAddr == "" {
		houndAddr = "http://localhost:6080"
	}

	logPath := *logFile
	if logPath == "" {
		logPath = os.Getenv("HOUND_MCP_LOG_FILE")
	}
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			// We can't log this failure to the file we just failed to open;
			// fall back to stderr and keep going so the server still runs.
			fmt.Fprintf(os.Stderr, "[hound-mcp] failed to open log file %s: %v; logging to stderr\n", logPath, err)
		} else {
			logger.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}

	logger.Printf("starting hound-mcp pid=%d hound_addr=%s log_file=%q", os.Getpid(), houndAddr, logPath)

	ctx := context.Background()
	g := genkit.Init(ctx)

	genkit.DefineTool(g, "list_repos",
		listReposDescription,
		func(ctx *ai.ToolContext, _ struct{}) (json.RawMessage, error) {
			return doListRepos(houndAddr)
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
			return doGetExcludes(houndAddr, input)
		},
	)

	// Log the registered tool names so we can see exactly what gets exposed
	// over MCP. genkit's own "MCP Server setup complete tools=N" line counts
	// raw registry actions, which is misleading because genkit's DefineTool
	// registers each tool under TWO action keys (tool/<name> and
	// tool-v2/<name>) for backward compatibility. We de-duplicate here so
	// the number you read matches the tools exposed over MCP.
	seen := map[string]bool{}
	toolNames := []string{}
	for _, t := range genkit.ListTools(g) {
		n := t.Definition().Name
		if seen[n] {
			continue
		}
		seen[n] = true
		toolNames = append(toolNames, n)
	}
	sort.Strings(toolNames)
	logger.Printf("registered %d tools: %s", len(toolNames), strings.Join(toolNames, ", "))

	s := mcp.NewMCPServer(g, mcp.MCPServerOptions{
		Name:    "hound-mcp",
		Version: "1.0.0",
	})

	if err := s.ServeStdio(); err != nil {
		log.Fatal(err)
	}
}

func doListRepos(houndAddr string) (json.RawMessage, error) {
	start := time.Now()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/repos", houndAddr))
	if err != nil {
		logger.Printf("list_repos ERROR in %s: %v", time.Since(start), err)
		return nil, fmt.Errorf("failed to connect to Hound server at %s: %w", houndAddr, err)
	}
	defer resp.Body.Close()

	var result json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logger.Printf("list_repos ERROR in %s: %v", time.Since(start), err)
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	// Count keys at the top level — cheaper than fully parsing.
	var asMap map[string]json.RawMessage
	_ = json.Unmarshal(result, &asMap)
	logger.Printf("list_repos → %d repos / %d bytes in %s", len(asMap), len(result), time.Since(start))
	return result, nil
}

func doGetExcludes(houndAddr string, input GetExcludesInput) (json.RawMessage, error) {
	start := time.Now()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/excludes?repo=%s",
		houndAddr, url.QueryEscape(input.Repo)))
	if err != nil {
		logger.Printf("get_excludes repo=%q ERROR in %s: %v", input.Repo, time.Since(start), err)
		return nil, fmt.Errorf("failed to connect to Hound server at %s: %w", houndAddr, err)
	}
	defer resp.Body.Close()

	var result json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logger.Printf("get_excludes repo=%q ERROR in %s: %v", input.Repo, time.Since(start), err)
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	logger.Printf("get_excludes repo=%q → %d bytes in %s", input.Repo, len(result), time.Since(start))
	return result, nil
}
