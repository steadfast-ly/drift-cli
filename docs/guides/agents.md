# LLM agents

drift ships a **skill file** that teaches an LLM coding agent how to drive the
CLI: guardrails for destructive operations, the JSON output contract, exit
codes and their meaning, wait semantics, and common recipes.

## What the skill encodes

- **Guardrails** -- destructive and irreversible commands require human
  confirmation before the agent runs them. Production promotion is never
  handled by the agent autonomously.
- **Exit codes** -- how each exit code should be interpreted by the agent
  (e.g. exit 4 means ask the human to re-authenticate, exit 6 means the
  operation may still complete server-side).
- **JSON contract** -- `--json <fields>` is the recommended machine interface.
  The agent should never scrape table output.
- **Wait semantics** -- which commands block and which return, and how to use
  `drift env wait --for` afterwards.
- **Rate limits** -- max 20 mutations per minute; the agent should pace
  accordingly.

## Installing the skill

Review the skill file before installing. Prefer **project-level**
installation (`.claude/skills/` in the repo) over global when the team shares
a repository -- it keeps the skill versioned alongside the CLI.

### Claude Code

**Project-level** (recommended -- checked into the repo):

```bash
mkdir -p .claude/skills/drift-cli
cp skills/drift-cli/SKILL.md .claude/skills/drift-cli/SKILL.md
```

**User-level** (applies to all projects). Pin the download to a release tag
(check the [releases page](https://github.com/steadfast-ly/drift-cli/releases)
for the current tag):

```bash
mkdir -p ~/.claude/skills/drift-cli
# Replace <version> with the current release tag, e.g. v0.2.1
curl -fsSL \
  https://raw.githubusercontent.com/steadfast-ly/drift-cli/<version>/skills/drift-cli/SKILL.md \
  -o ~/.claude/skills/drift-cli/SKILL.md
```

Or clone the repo and symlink:

```bash
ln -s /path/to/drift-cli/skills/drift-cli ~/.claude/skills/drift-cli
```

### OpenCode

Place the skill file in `.opencode/skills/` (project) or the global
equivalent. Pin the download to a release tag as above:

```bash
mkdir -p .opencode/skills/drift-cli
cp skills/drift-cli/SKILL.md .opencode/skills/drift-cli/SKILL.md
```

### Other harnesses

Copy the body of `skills/drift-cli/SKILL.md` into your harness's context
file (e.g. `AGENTS.md`, system prompt, or equivalent).

## Design note

No MCP server exists or is planned. The CLI plus `--json` plus exit codes
are deliberately the machine interface. An agent that can run shell commands
has everything it needs.
