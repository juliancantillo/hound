# Hound MCP Server

Hound exposes its code search as an [MCP](https://modelcontextprotocol.io/) server, allowing AI assistants like Claude to search your indexed repositories.

## Setup

Build the `hound` binary:

```bash
make
```

Ensure `houndd` is running with your repositories configured:

```bash
.build/bin/houndd --conf config.json
```

## Configure in Claude Desktop

Add the following to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS):

```json
{
  "mcpServers": {
    "hound": {
      "command": "/path/to/hound",
      "args": ["mcp"],
      "env": {
        "HOUND_ADDR": "http://localhost:6080"
      }
    }
  }
}
```

Replace `/path/to/hound` with the absolute path to your built binary (e.g., `/Users/you/src/hound/.build/bin/hound`).

## Configure in Claude Code

Add to your Claude Code settings (`~/.claude/settings.json`):

```json
{
  "mcpServers": {
    "hound": {
      "command": "/path/to/hound",
      "args": ["mcp"],
      "env": {
        "HOUND_ADDR": "http://localhost:6080"
      }
    }
  }
}
```

## Options

| Option | Description | Default |
|--------|-------------|---------|
| `--hound-addr` | Hound server URL | `http://localhost:6080` |
| `HOUND_ADDR` | Environment variable fallback for `--hound-addr` | `http://localhost:6080` |
| `--log-file` | Path to append one log line per tool call. Useful because Claude Code usually swallows the stderr of MCP servers it launches. | stderr only |
| `HOUND_MCP_LOG_FILE` | Environment variable fallback for `--log-file` | unset |

The `--hound-addr` flag takes priority over the `HOUND_ADDR` environment variable. Likewise `--log-file` takes priority over `HOUND_MCP_LOG_FILE`.

### What gets logged

One line per tool invocation, written to stderr (always) and to `--log-file` (if set):

```
[hound-mcp] 2026/05/19 16:30:58.868019 search query="NewServer" files_only=true lines_only=false kind="" → 612 bytes in 41.2ms truncated=false
[hound-mcp] 2026/05/19 16:30:59.102331 list_repos → 5 repos / 248 bytes in 7.8ms
[hound-mcp] 2026/05/19 16:31:00.554017 get_excludes repo="mn" → 322 bytes in 4.1ms
[hound-mcp] 2026/05/19 16:31:01.811220 search query="x" files_only=false lines_only=false kind="" ERROR in 12.4ms: failed to connect to Hound server at http://localhost:6080: dial tcp: connection refused
```

Plus one startup line: `starting hound-mcp pid=… hound_addr=… log_file=…`.

## Available Tools

| Tool | Description |
|------|-------------|
| `search` | Search across repositories using regex or literal patterns |
| `list_repos` | List all indexed repositories |
| `get_excludes` | Get excluded file patterns for a repository |
