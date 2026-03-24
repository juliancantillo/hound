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
| `HOUND_ADDR` | Environment variable fallback | `http://localhost:6080` |

The `--hound-addr` flag takes priority over the `HOUND_ADDR` environment variable.

## Available Tools

| Tool | Description |
|------|-------------|
| `search` | Search across repositories using regex or literal patterns |
| `list_repos` | List all indexed repositories |
| `get_excludes` | Get excluded file patterns for a repository |
