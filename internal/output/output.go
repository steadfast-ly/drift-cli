// Package output renders command results as a table, JSON, YAML or a wide
// table, from one description of the data.
//
// The central idea: a command describes WHAT it produced (`Doc`), never HOW to
// print it. Field names in a `Doc` are the CLI's stable machine contract —
// `--json <fields>` selects from exactly these names, and they are chosen by
// the CLI rather than inherited from the wire, so a server-side rename of a
// JSON property does not silently break somebody's script.
//
// Data goes to stdout. Diagnostics — warnings, progress, "using context x" —
// go to stderr, so `drift env list -o json > envs.json` produces a file that
// parses.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Format is an output format.
type Format string

const (
	FormatTable Format = "table"
	FormatWide  Format = "wide"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// ParseFormat validates a `-o` value.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatTable, FormatWide, FormatJSON, FormatYAML:
		return Format(s), nil
	default:
		return "", fmt.Errorf("unknown output format %q (want table, wide, json or yaml)", s)
	}
}

// Column is one field of a Doc.
type Column struct {
	// Name is the machine name: the JSON/YAML key and the token `--json`
	// accepts. Stable across releases.
	Name string
	// Header is the table heading. Conventionally the name upper-cased.
	Header string
	// Wide marks a column that appears only under `-o wide`. It still appears
	// in JSON and YAML — hiding data from a machine format to save terminal
	// width would be an odd trade.
	Wide bool
	// Color, when set, maps a cell value to an ANSI code.
	Color func(value any) string
}

// Row is one record, keyed by Column.Name.
type Row map[string]any

// Doc is a rendered result.
type Doc struct {
	Columns []Column
	Rows    []Row

	// Single marks a one-object result (`env get`) rather than a list. It
	// changes the JSON shape from an envelope to a bare object and the table
	// shape from columns to key/value lines, which is the difference between a
	// readable detail view and a 200-character-wide single row.
	Single bool

	// Extra carries structured data that has no column: `pagination` on a list,
	// `services` and `builds` on a detail. Emitted in JSON and YAML, and
	// rendered by the command itself for tables when it is worth showing.
	Extra map[string]any

	// EmptyMessage is printed to stderr when there are no rows, so that an
	// empty table is not mistaken for a broken command. Never to stdout: it is
	// a diagnostic, not data.
	EmptyMessage string
}

// Writer renders Docs.
type Writer struct {
	Out    io.Writer
	Err    io.Writer
	Format Format
	// JSONFields, when non-empty, projects output onto exactly these column
	// names — the stable field contract a script depends on. Implies JSON
	// unless the format is explicitly YAML. Key ORDER is not part of the
	// contract: JSON objects are unordered and both encoders sort keys.
	JSONFields []string
	// Color controls colour on the DATA stream. Callers set it from
	// ColorEnabled(Out).
	Color bool
	// ErrColor controls colour on the DIAGNOSTIC stream, decided separately
	// from ColorEnabled(Err).
	//
	// One flag for both was wrong in a common shape: `drift env list > /dev/null
	// 2> err.log` has a non-terminal stdout and a non-terminal stderr, but
	// `drift env list 2> err.log` on a terminal has a terminal stdout and a
	// FILE for stderr — and the single flag wrote ANSI escapes into the log.
	// The two streams are redirected independently, so they are decided
	// independently.
	ErrColor bool
}

// EffectiveFormat is the format that will actually be used, after `--json`
// field selection has been taken into account.
//
// Commands that render extra sections by hand — `env get`'s services and builds
// sub-tables — must branch on THIS, not on the raw flag. Branching on the flag
// makes `--json slug` print a JSON object followed by a table, which is neither
// valid JSON nor a readable table.
func (w *Writer) EffectiveFormat() Format {
	if len(w.JSONFields) > 0 && w.Format != FormatYAML {
		return FormatJSON
	}
	return w.Format
}

// Write renders the doc.
func (w *Writer) Write(d *Doc) error {
	switch w.EffectiveFormat() {
	case FormatJSON:
		return w.writeJSON(d)
	case FormatYAML:
		return w.writeYAML(d)
	case FormatWide:
		return w.writeTable(d, true)
	default:
		return w.writeTable(d, false)
	}
}

// Warnf writes a diagnostic to stderr. Never stdout: a warning in a JSON stream
// is a parse error at the other end.
func (w *Writer) Warnf(format string, args ...any) {
	if w.Err == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(w.Err, colorize(w.ErrColor, ansiYellow, msg))
}

// Infof writes an informational diagnostic to stderr.
func (w *Writer) Infof(format string, args ...any) {
	if w.Err == nil {
		return
	}
	fmt.Fprintf(w.Err, format+"\n", args...)
}

// --- structured formats -----------------------------------------------------

// payload converts a Doc into the value that JSON and YAML serialise.
//
// The list shape mirrors the API envelope (`items` + `pagination`) on purpose:
// a user who has read the API docs already knows what `jq '.items[]'` will do,
// and a CLI that invented a different envelope for the same data would be one
// more thing to learn.
func (w *Writer) payload(d *Doc) any {
	fields := w.JSONFields
	project := func(r Row) map[string]any {
		out := map[string]any{}
		if len(fields) == 0 {
			for _, c := range d.Columns {
				out[c.Name] = Normalize(r[c.Name])
			}
			return out
		}
		for _, f := range fields {
			out[f] = Normalize(r[f])
		}
		return out
	}

	if d.Single {
		var obj map[string]any
		if len(d.Rows) > 0 {
			obj = project(d.Rows[0])
		} else {
			obj = map[string]any{}
		}
		// Extra is merged into a single object rather than nested under a key:
		// `drift env get x -o json | jq .services` is the obvious thing to try.
		// Suppressed under an explicit field projection, which is a request for
		// exactly those fields and nothing else.
		if len(fields) == 0 {
			for k, v := range d.Extra {
				obj[k] = v
			}
		}
		return obj
	}

	items := make([]map[string]any, 0, len(d.Rows))
	for _, r := range d.Rows {
		items = append(items, project(r))
	}
	env := map[string]any{"items": items}
	if len(fields) == 0 {
		for k, v := range d.Extra {
			env[k] = v
		}
	}
	return env
}

func (w *Writer) writeJSON(d *Doc) error {
	enc := json.NewEncoder(w.Out)
	enc.SetIndent("", "  ")
	// HTML escaping off: these are branch names and image tags, not web page
	// fragments, and `&` rendered as & is unreadable in a terminal.
	enc.SetEscapeHTML(false)
	return enc.Encode(w.payload(d))
}

func (w *Writer) writeYAML(d *Doc) error {
	enc := yaml.NewEncoder(w.Out)
	enc.SetIndent(2)
	if err := enc.Encode(w.payload(d)); err != nil {
		return err
	}
	return enc.Close()
}

// Normalize converts values into shapes JSON and YAML both render sensibly.
// Exported because commands assemble nested payloads (`services`, `builds`) that
// bypass the column machinery and must serialise identically.
func Normalize(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case *time.Time:
		if t == nil {
			return nil
		}
		return t.UTC().Format(time.RFC3339)
	case *string:
		if t == nil {
			return nil
		}
		return *t
	case *int:
		if t == nil {
			return nil
		}
		return *t
	case *bool:
		if t == nil {
			return nil
		}
		return *t
	default:
		return v
	}
}

// --- table ------------------------------------------------------------------

func (w *Writer) writeTable(d *Doc, wide bool) error {
	if len(d.Rows) == 0 {
		if d.EmptyMessage != "" && w.Err != nil {
			fmt.Fprintln(w.Err, d.EmptyMessage)
		}
		return nil
	}
	if d.Single {
		return w.writeDetail(d, wide)
	}

	cols := visibleColumns(d.Columns, wide)
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = displayWidth(c.Header)
	}
	cells := make([][]string, 0, len(d.Rows))
	for _, r := range d.Rows {
		line := make([]string, len(cols))
		for i, c := range cols {
			s := formatCell(r[c.Name])
			line[i] = s
			if n := displayWidth(s); n > widths[i] {
				widths[i] = n
			}
		}
		cells = append(cells, line)
	}

	var b strings.Builder
	for i, c := range cols {
		writePadded(&b, strings.ToUpper(c.Header), widths[i], i == len(cols)-1)
	}
	b.WriteByte('\n')
	for ri, line := range cells {
		for i, c := range cols {
			code := ""
			if c.Color != nil {
				code = c.Color(d.Rows[ri][c.Name])
			}
			// Padding is computed from the UNCOLOURED width, then the colour is
			// applied to the text only. Colouring first would make every escape
			// sequence count towards the column width and the table would come
			// out ragged the moment colour is on.
			writeColoredPadded(&b, line[i], widths[i], i == len(cols)-1, w.Color, code)
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w.Out, b.String())
	return err
}

// writeDetail renders a single object as aligned key/value lines.
func (w *Writer) writeDetail(d *Doc, wide bool) error {
	cols := visibleColumns(d.Columns, wide)
	// Width is measured over the LABEL, colon included, so the value column
	// lines up. Measuring the bare header leaves every row off by one.
	width := 0
	for _, c := range cols {
		if n := displayWidth(c.Header) + 1; n > width {
			width = n
		}
	}
	row := d.Rows[0]
	var b strings.Builder
	for _, c := range cols {
		code := ""
		if c.Color != nil {
			code = c.Color(row[c.Name])
		}
		label := c.Header + ":"
		// Padded by hand rather than with %-*s, which pads by BYTES and so
		// mis-aligns the moment a header is not pure ASCII.
		b.WriteString(label)
		b.WriteString(strings.Repeat(" ", width-displayWidth(label)+2))
		b.WriteString(colorize(w.Color, code, formatCell(row[c.Name])))
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w.Out, b.String())
	return err
}

func visibleColumns(cols []Column, wide bool) []Column {
	if wide {
		return cols
	}
	out := make([]Column, 0, len(cols))
	for _, c := range cols {
		if !c.Wide {
			out = append(out, c)
		}
	}
	return out
}

// displayWidth is the column width a string occupies.
//
// Rune count, not byte count. Slugs derive from branch names, so a non-ASCII
// one is reachable, and `len()` counts a two-byte character as two columns —
// which shifted every following column on that row and only that row, producing
// a table that looks corrupted rather than merely misaligned.
//
// Known limitation, deliberately not solved here: this counts one column per
// rune, so East Asian wide characters and emoji still under-measure. Fixing
// that properly needs a width table (`go-runewidth`); the dependency is not
// justified until a real slug needs it, and the failure mode is now cosmetic
// rather than structural.
func displayWidth(s string) int {
	return utf8.RuneCountInString(s)
}

func writePadded(b *strings.Builder, s string, width int, last bool) {
	b.WriteString(s)
	if !last {
		b.WriteString(strings.Repeat(" ", width-displayWidth(s)+2))
	}
}

func writeColoredPadded(b *strings.Builder, s string, width int, last bool, color bool, code string) {
	b.WriteString(colorize(color, code, s))
	if !last {
		b.WriteString(strings.Repeat(" ", width-displayWidth(s)+2))
	}
}

// formatCell renders one value for a table.
//
// `-` for absent, rather than an empty cell: an empty cell in a padded table is
// indistinguishable from a column that failed to render.
func formatCell(v any) string {
	switch t := v.(type) {
	case nil:
		return "-"
	case string:
		if t == "" {
			return "-"
		}
		return t
	case *string:
		if t == nil || *t == "" {
			return "-"
		}
		return *t
	case time.Time:
		return t.Local().Format("2006-01-02 15:04")
	case *time.Time:
		if t == nil {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04")
	case *int:
		if t == nil {
			return "-"
		}
		return fmt.Sprintf("%d", *t)
	case *bool:
		if t == nil {
			return "-"
		}
		return fmt.Sprintf("%t", *t)
	case []string:
		if len(t) == 0 {
			return "-"
		}
		return strings.Join(t, ",")
	default:
		return fmt.Sprintf("%v", t)
	}
}

// StatusColumn builds the conventional status column, coloured by state.
func StatusColumn(name, header string) Column {
	return Column{
		Name:   name,
		Header: header,
		Color: func(v any) string {
			s, _ := v.(string)
			return statusColor(s)
		},
	}
}

// ValidateFields checks a `--json` selection against a doc's columns, so a typo
// is a usage error rather than a column of nulls.
func ValidateFields(fields []string, cols []Column) error {
	if len(fields) == 0 {
		return nil
	}
	known := map[string]bool{}
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		known[c.Name] = true
		names = append(names, c.Name)
	}
	sort.Strings(names)
	for _, f := range fields {
		if !known[f] {
			return fmt.Errorf("unknown field %q; available: %s", f, strings.Join(names, ", "))
		}
	}
	return nil
}

// SplitFields parses a comma-separated `--json` value.
func SplitFields(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
