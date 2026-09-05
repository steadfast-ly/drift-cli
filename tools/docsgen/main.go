// docsgen generates the command-reference pages under docs/reference/commands/.
//
// It uses cobra/doc to produce one Markdown page per command, then writes an
// index page that lists them all grouped by parent.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
	"github.com/steadfast-ly/drift-cli/cmd"
)

func main() {
	out := flag.String("out", "docs/reference/commands", "output directory for generated pages")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("mkdir: %s", err)
	}

	app := cmd.NewApp()
	root := cmd.NewRootCommand(app)

	// Suppress the auto-generated timestamp line at the bottom of each page.
	root.DisableAutoGenTag = true
	disableAutoGenTag(root)

	// linkHandler rewrites inter-page links so they work under MkDocs.
	// cobra/doc generates filenames like "drift_env_create.md" and links like
	// "drift_env_create.md" — MkDocs resolves them as-is, so no rewriting
	// is needed, but we normalise to use the same directory.
	linkHandler := func(name string) string {
		return name
	}

	filePrepender := func(_ string) string {
		return ""
	}

	if err := doc.GenMarkdownTreeCustom(root, *out, filePrepender, linkHandler); err != nil {
		log.Fatalf("GenMarkdownTreeCustom: %s", err)
	}

	if err := writeIndex(root, *out); err != nil {
		log.Fatalf("writeIndex: %s", err)
	}

	fmt.Fprintf(os.Stderr, "docsgen: wrote command reference to %s\n", *out)
}

// disableAutoGenTag walks the tree and sets DisableAutoGenTag on every command.
func disableAutoGenTag(c *cobra.Command) {
	c.DisableAutoGenTag = true
	for _, sub := range c.Commands() {
		disableAutoGenTag(sub)
	}
}

// writeIndex creates an index.md that lists all generated pages grouped by
// top-level command.
func writeIndex(root *cobra.Command, dir string) error {
	var b strings.Builder
	b.WriteString("# Command reference\n\n")
	b.WriteString("Auto-generated from the CLI's built-in help.\n\n")

	// Root page.
	b.WriteString("## drift\n\n")
	b.WriteString(fmt.Sprintf("- [drift](%s) -- %s\n\n", fileName(root), root.Short))

	// Group by top-level subcommand.
	groups := groupCommands(root)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, group := range keys {
		cmds := groups[group]
		b.WriteString(fmt.Sprintf("## %s\n\n", group))
		for _, c := range cmds {
			b.WriteString(fmt.Sprintf("- [%s](%s) -- %s\n", c.CommandPath(), fileName(c), c.Short))
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(dir, "index.md"), []byte(b.String()), 0o644)
}

// groupCommands returns subcommands grouped by their top-level parent name.
func groupCommands(root *cobra.Command) map[string][]*cobra.Command {
	groups := map[string][]*cobra.Command{}
	for _, top := range root.Commands() {
		if !top.IsAvailableCommand() || top.Name() == "help" {
			continue
		}
		name := "drift " + top.Name()
		groups[name] = append(groups[name], top)
		addSubcommands(groups, name, top)
	}
	return groups
}

func addSubcommands(groups map[string][]*cobra.Command, groupName string, parent *cobra.Command) {
	for _, sub := range parent.Commands() {
		if !sub.IsAvailableCommand() || sub.Name() == "help" {
			continue
		}
		groups[groupName] = append(groups[groupName], sub)
		addSubcommands(groups, groupName, sub)
	}
}

// fileName matches the naming convention cobra/doc uses: spaces replaced with
// underscores, plus ".md".
func fileName(c *cobra.Command) string {
	return strings.ReplaceAll(c.CommandPath(), " ", "_") + ".md"
}
