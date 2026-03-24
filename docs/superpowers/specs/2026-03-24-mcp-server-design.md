# Hound MCP Server Design

## Overview

A new MCP server binary (`cmds/hound-mcp/main.go`) that exposes Hound's search capabilities as MCP tools, allowing LLMs and editors to search code repositories via the MCP protocol.

## Architecture

- **Transport:** stdio (standard for MCP servers invoked by editors/LLMs)
- **Framework:** Genkit Go SDK with MCP plugin
- **Backend:** Connects to a running `houndd` instance over HTTP
- **Configuration:** `--hound-addr` flag with `HOUND_ADDR` env var fallback, defaults to `http://localhost:6080`

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
| `context` | int | no | 2 | Lines of context (0-20) |
| `limit` | int | no | — | Max results across all repos |

**Output:** JSON search response — results keyed by repo name, each containing file matches with line numbers and context lines.

### `list_repos`

List all available source code repositories.

**Input:** none

**Output:** JSON map of repo names to their metadata (URL, VCS type, etc.)

### `get_excludes`

Get excluded file patterns for a repository.

**Input:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `repo` | string | yes | Repository name |

**Output:** JSON list of excluded file patterns.

## File Structure

### New files

- `cmds/hound-mcp/main.go` — single-file binary containing main, tool definitions, and HTTP helpers

### Dependencies (added to go.mod)

- `github.com/firebase/genkit/go/ai`
- `github.com/firebase/genkit/go/genkit`
- `github.com/firebase/genkit/go/plugins/mcp`

### Reuse

- Existing `client` package for the `search` tool
- Direct HTTP calls to `/api/v1/repos` and `/api/v1/excludes` for the other two tools (not covered by `client` package)

### Build

- New Makefile target for `hound-mcp` alongside existing `houndd` and `hound` targets

## Configuration

The Hound server address is resolved in this order:

1. `--hound-addr` command-line flag (highest priority)
2. `HOUND_ADDR` environment variable
3. `http://localhost:6080` (default)
