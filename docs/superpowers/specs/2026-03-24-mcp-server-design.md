# Hound MCP Server Design

## Overview

A new MCP server binary (`cmds/hound-mcp/main.go`) that exposes Hound's search capabilities as MCP tools, allowing LLMs and editors to search code repositories via the MCP protocol.

## Architecture

- **Transport:** stdio (standard for MCP servers invoked by editors/LLMs)
- **Framework:** Genkit Go SDK with MCP plugin
- **Backend:** Connects to a running `houndd` instance over HTTP
- **Configuration:** `--hound-addr` flag with `HOUND_ADDR` env var fallback, defaults to `http://localhost:6080`

## Non-Goals

- No caching layer — the Hound server already handles index caching
- Does not start or manage `houndd` — expects it to be running
- No authentication — same trust model as the existing HTTP API
- Does not expose write endpoints (`/api/v1/update`, `/api/v1/github-webhook`)

## Tools

### `search`

Search across source code repositories using regex or literal patterns.

**Input:**

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `query` | string | yes | — | Search pattern |
| `repos` | string | no | `"*"` | Comma-separated repo names or `"*"` for all |
| `files` | string | no | — | File path regex filter |
| `excludeFiles` | string | no | — | File path regex to exclude |
| `ignoreCase` | bool | no | false | Case-insensitive search |
| `literal` | bool | no | false | Treat query as literal, not regex |
| `context` | int | no | 2 | Lines of context (0-20). 0 means no context lines. |
| `limit` | int | no | — | Max results across all repos. Omitted = server default. |

**Output:** JSON object with a `Results` map keyed by repo name. Each repo entry contains:

```json
{
  "Results": {
    "RepoName": {
      "Matches": [
        {
          "Filename": "path/to/file.go",
          "Matches": [
            {
              "Line": "    // TODO: fix this",
              "LineNumber": 42,
              "Before": ["func main() {"],
              "After": ["    x := 5"]
            }
          ]
        }
      ],
      "FilesWithMatch": 1,
      "Revision": "abc123"
    }
  }
}
```

When no results are found, `Results` is an empty map `{}`. If the Hound API returns an error (200 with `{"Error": "..."}` body), the tool returns that error string as a tool error.

### `list_repos`

List all available source code repositories.

**Input:** none

**Output:** JSON map of repo names to their metadata:

```json
{
  "RepoName": {
    "url": "https://github.com/org/repo",
    "vcs": "git"
  }
}
```

### `get_excludes`

Get excluded file patterns for a repository.

**Input:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `repo` | string | yes | Repository name |

**Output:** JSON object with excluded file patterns:

```json
{
  "ExcludedFiles": ["vendor/", "node_modules/"]
}
```

## Error Handling

- **`houndd` unreachable:** Tools return a clear error message: "Failed to connect to Hound server at <addr>. Ensure houndd is running."
- **API errors:** Hound returns some errors as 200 with `{"Error": "..."}`. These are surfaced as tool errors with the message from Hound.
- **No startup health check:** The server starts regardless of whether `houndd` is available. Errors surface per-tool-call, which gives the LLM actionable feedback.

## File Structure

### New files

- `cmds/hound-mcp/main.go` — single-file binary containing main, tool definitions, and HTTP helpers

### Dependencies (added to go.mod)

- `github.com/firebase/genkit/go/ai`
- `github.com/firebase/genkit/go/genkit`
- `github.com/firebase/genkit/go/plugins/mcp`

### HTTP Client

All three tools use direct HTTP calls to the Hound API. The existing `client` package is not reused — it expects a bare hostname (not a URL), and lacks support for `literal` and `excludeFiles` parameters. Direct HTTP calls are simpler and give full control over the query parameters.

### Build

- New Makefile target for `hound-mcp` alongside existing `houndd` and `hound` targets

## Configuration

The Hound server address is resolved in this order:

1. `--hound-addr` command-line flag (highest priority)
2. `HOUND_ADDR` environment variable
3. `http://localhost:6080` (default)
