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

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/mcp"
)

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

type GetExcludesInput struct {
	Repo string `json:"repo"`
}

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

	s := mcp.NewMCPServer(g, mcp.MCPServerOptions{
		Name:    "hound-mcp",
		Version: "1.0.0",
	})

	if err := s.ServeStdio(); err != nil {
		log.Fatal(err)
	}
}
