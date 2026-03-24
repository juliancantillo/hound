# Hound MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create a Genkit-based MCP server that exposes Hound's search, list_repos, and get_excludes API as MCP tools over stdio.

**Architecture:** A single Go binary at `cmds/hound-mcp/main.go` that uses Genkit's MCP plugin to serve three tools over stdio. Each tool makes direct HTTP calls to a running `houndd` instance. Configuration via `--hound-addr` flag with `HOUND_ADDR` env var fallback.

**Tech Stack:** Go (1.22+), Genkit Go SDK (`github.com/firebase/genkit/go`), MCP plugin (`github.com/firebase/genkit/go/plugins/mcp`)

**Spec:** `docs/superpowers/specs/2026-03-24-mcp-server-design.md`

---

## File Structure

- **Create:** `cmds/hound-mcp/main.go` — entry point, tool definitions, HTTP helpers
- **Modify:** `go.mod` — bump Go version to 1.22, add Genkit dependencies
- **Modify:** `Makefile:1` — add `hound-mcp` to CMDS and build target

---

### Task 1: Bump Go version and add Genkit dependencies

The Genkit Go SDK uses generics, which require Go 1.22+. The current `go.mod` specifies Go 1.16.

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Bump Go version in go.mod**

Edit `go.mod` line 3 to change `go 1.16` to `go 1.22`.

- [ ] **Step 2: Add Genkit dependencies**

Run:
```bash
cd /Users/julian/src/me/hound
go get github.com/firebase/genkit/go/ai github.com/firebase/genkit/go/genkit github.com/firebase/genkit/go/plugins/mcp
```

- [ ] **Step 3: Tidy modules**

Run:
```bash
go mod tidy
```

- [ ] **Step 4: Verify existing code still builds**

Run:
```bash
go build ./...
```
Expected: SUCCESS (no errors)

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: bump Go to 1.22 and add Genkit dependencies"
```

---

### Task 2: Create hound-mcp binary with list_repos tool

Start with the simplest tool to validate the Genkit MCP setup works end-to-end.

**Files:**
- Create: `cmds/hound-mcp/main.go`

- [ ] **Step 1: Create the main.go file**

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/mcp"
)

func main() {
	addr := flag.String("hound-addr", "", "Address of the Hound server (default: http://localhost:6080)")
	flag.Parse()

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
		"List all available source code repositories indexed by Hound",
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

	s := mcp.NewMCPServer(g, mcp.MCPServerOptions{
		Name:    "hound-mcp",
		Version: "1.0.0",
	})

	if err := s.ServeStdio(); err != nil {
		log.Fatal(err)
	}
}
```

- [ ] **Step 2: Verify it compiles**

Run:
```bash
go build -o /dev/null github.com/hound-search/hound/cmds/hound-mcp
```
Expected: SUCCESS

- [ ] **Step 3: Commit**

```bash
git add cmds/hound-mcp/main.go
git commit -m "feat: add hound-mcp binary with list_repos tool"
```

---

### Task 3: Add search tool

The most important tool — exposes the full search API.

**Files:**
- Modify: `cmds/hound-mcp/main.go`

- [ ] **Step 1: Add search input type and tool definition**

Add the following struct and tool definition after the `list_repos` tool:

```go
type SearchInput struct {
	Query        string `json:"query"`
	Repos        string `json:"repos,omitempty"`
	Files        string `json:"files,omitempty"`
	ExcludeFiles string `json:"excludeFiles,omitempty"`
	IgnoreCase   bool   `json:"ignoreCase,omitempty"`
	Literal      bool   `json:"literal,omitempty"`
	Context      *int   `json:"context,omitempty"`
	Limit        *int   `json:"limit,omitempty"`
}
```

```go
genkit.DefineTool(g, "search",
	"Search across source code repositories using regex or literal patterns. Returns matches with file paths, line numbers, and context lines.",
	func(ctx *ai.ToolContext, input SearchInput) (json.RawMessage, error) {
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
		if input.Context != nil {
			params += fmt.Sprintf("&ctx=%d", *input.Context)
		}
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

		return raw, nil
	},
)
```

Also add `"net/url"` to the imports.

- [ ] **Step 2: Verify it compiles**

Run:
```bash
go build -o /dev/null github.com/hound-search/hound/cmds/hound-mcp
```
Expected: SUCCESS

- [ ] **Step 3: Commit**

```bash
git add cmds/hound-mcp/main.go
git commit -m "feat: add search tool to hound-mcp"
```

---

### Task 4: Add get_excludes tool

**Files:**
- Modify: `cmds/hound-mcp/main.go`

- [ ] **Step 1: Add get_excludes input type and tool definition**

Add after the search tool:

```go
type GetExcludesInput struct {
	Repo string `json:"repo"`
}
```

```go
genkit.DefineTool(g, "get_excludes",
	"Get the list of excluded file patterns for a repository",
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
```

- [ ] **Step 2: Verify it compiles**

Run:
```bash
go build -o /dev/null github.com/hound-search/hound/cmds/hound-mcp
```
Expected: SUCCESS

- [ ] **Step 3: Commit**

```bash
git add cmds/hound-mcp/main.go
git commit -m "feat: add get_excludes tool to hound-mcp"
```

---

### Task 5: Add Makefile target

**Files:**
- Modify: `Makefile:1`

- [ ] **Step 1: Add hound-mcp to CMDS and add build rule**

Change line 1 of the Makefile from:
```makefile
CMDS := .build/bin/houndd .build/bin/hound
```
to:
```makefile
CMDS := .build/bin/houndd .build/bin/hound .build/bin/hound-mcp
```

Add a new build target after the `.build/bin/hound` target:
```makefile
.build/bin/hound-mcp: $(SRCS)
	go build -o $@ github.com/hound-search/hound/cmds/hound-mcp
```

- [ ] **Step 2: Verify make builds all targets**

Run:
```bash
make clean && make
```
Expected: SUCCESS — all three binaries built in `.build/bin/`

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "chore: add hound-mcp build target to Makefile"
```

---

### Task 6: Smoke test the MCP server

Verify the server works end-to-end against a running Hound instance.

- [ ] **Step 1: Test that the binary starts and responds to MCP initialize**

Run:
```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}' | .build/bin/hound-mcp
```
Expected: JSON response with server capabilities listing the three tools.

- [ ] **Step 2: Test with --hound-addr flag**

Run:
```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}' | .build/bin/hound-mcp --hound-addr http://localhost:6080
```
Expected: Same response — flag is accepted without error.
