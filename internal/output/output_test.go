package output

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -update rewrites the golden files. Golden output is a contract with users'
// terminals and their scripts; regenerating it must be an explicit act with a
// reviewable diff, not a side effect of running the suite.
var update = flag.Bool("update", false, "rewrite golden files")

func goldenPath(name string) string { return filepath.Join("testdata", name) }

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := goldenPath(name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file %s (run `go test ./internal/output -update`): %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("output does not match %s\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// A fixed instant in UTC. Table rendering prints local time, so the tests pin
// TZ=UTC via TestMain rather than letting the developer's zone leak into the
// golden files.
var fixed = time.Date(2026, 7, 25, 14, 30, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	_ = os.Setenv("TZ", "UTC")
	time.Local = time.UTC
	os.Exit(m.Run())
}

func sampleDoc() *Doc {
	ticket := "AUS-10001"
	slept := fixed.Add(-3 * time.Hour)
	return &Doc{
		Columns: []Column{
			{Name: "slug", Header: "Slug"},
			StatusColumn("status", "Status"),
			{Name: "ticket", Header: "Ticket"},
			{Name: "expires", Header: "Expires"},
			{Name: "id", Header: "Id", Wide: true},
			{Name: "slept_at", Header: "Slept", Wide: true},
			{Name: "public", Header: "Public", Wide: true},
		},
		Rows: []Row{
			{
				"slug": "proof-alpha", "status": "running", "ticket": &ticket,
				"expires": fixed, "id": "b92b68a9-877a-4f14-a92e-db1a62b803d9",
				"slept_at": (*time.Time)(nil), "public": true,
			},
			{
				"slug": "a-much-longer-environment-slug", "status": "deploy_failed",
				"ticket": (*string)(nil), "expires": fixed.Add(48 * time.Hour),
				"id": "093c8639-3405-441e-8bc2-b9c75f32a3c0", "slept_at": &slept, "public": false,
			},
		},
		Extra: map[string]any{"pagination": map[string]any{
			"limit": 20, "offset": 0, "hasMore": false,
		}},
	}
}

func render(t *testing.T, w *Writer, d *Doc) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	w.Out, w.Err = &out, &errOut
	if err := w.Write(d); err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String()
}

func TestGoldenTable(t *testing.T) {
	got, _ := render(t, &Writer{Format: FormatTable}, sampleDoc())
	assertGolden(t, "env_list_table.golden", got)
}

func TestGoldenWide(t *testing.T) {
	got, _ := render(t, &Writer{Format: FormatWide}, sampleDoc())
	assertGolden(t, "env_list_wide.golden", got)
}

func TestGoldenJSON(t *testing.T) {
	got, _ := render(t, &Writer{Format: FormatJSON}, sampleDoc())
	assertGolden(t, "env_list_json.golden", got)
}

func TestGoldenYAML(t *testing.T) {
	got, _ := render(t, &Writer{Format: FormatYAML}, sampleDoc())
	assertGolden(t, "env_list_yaml.golden", got)
}

func TestGoldenJSONFieldProjection(t *testing.T) {
	w := &Writer{Format: FormatTable, JSONFields: []string{"slug", "status"}}
	got, _ := render(t, w, sampleDoc())
	assertGolden(t, "env_list_json_fields.golden", got)
}

func TestGoldenDetail(t *testing.T) {
	d := sampleDoc()
	d.Single = true
	d.Rows = d.Rows[:1]
	d.Extra = map[string]any{"services": []map[string]any{{"branch": "main", "pr": nil}}}
	got, _ := render(t, &Writer{Format: FormatTable}, d)
	assertGolden(t, "env_get_table.golden", got)
}

func TestGoldenDetailJSON(t *testing.T) {
	d := sampleDoc()
	d.Single = true
	d.Rows = d.Rows[:1]
	d.Extra = map[string]any{"services": []map[string]any{{"branch": "main", "pr": nil}}}
	got, _ := render(t, &Writer{Format: FormatJSON}, d)
	assertGolden(t, "env_get_json.golden", got)
}

// --- properties the golden files alone would not pin ------------------------

// Colour must change ONLY the escape sequences: same rows, same columns, same
// widths, so `drift env list | grep` sees what the operator saw.
func TestColourChangesNothingButEscapes(t *testing.T) {
	plain, _ := render(t, &Writer{Format: FormatTable}, sampleDoc())
	colored, _ := render(t, &Writer{Format: FormatTable, Color: true}, sampleDoc())

	if plain == colored {
		t.Fatal("colour produced identical bytes; the test proves nothing")
	}
	stripped := stripANSI(colored)
	if stripped != plain {
		t.Fatalf("colour changed the layout\n--- plain ---\n%s\n--- stripped ---\n%s", plain, stripped)
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// An empty result writes its explanation to STDERR. On stdout it would be a
// parse error at the other end of a pipe.
func TestEmptyResultKeepsStdoutClean(t *testing.T) {
	d := &Doc{Columns: sampleDoc().Columns, EmptyMessage: "No environments matched."}
	out, errOut := render(t, &Writer{Format: FormatTable}, d)
	if out != "" {
		t.Fatalf("stdout was not empty: %q", out)
	}
	if !strings.Contains(errOut, "No environments matched.") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// Diagnostics never go to stdout, in any format.
func TestWarningsGoToStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	w := &Writer{Out: &out, Err: &errOut, Format: FormatJSON}
	w.Warnf("client is older than the server floor")
	w.Infof("using context %q", "au")
	if out.Len() != 0 {
		t.Fatalf("a diagnostic reached stdout: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "older than") || !strings.Contains(errOut.String(), "using context") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

// `--json` implies JSON even when the format flag says table, because asking
// for specific fields is asking for machine output.
func TestJSONFieldsImplyJSON(t *testing.T) {
	got, _ := render(t, &Writer{Format: FormatTable, JSONFields: []string{"slug"}}, sampleDoc())
	if !strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Fatalf("expected JSON, got:\n%s", got)
	}
	// ...but an explicit YAML request is honoured, since that is also machine
	// output and the user was specific.
	got, _ = render(t, &Writer{Format: FormatYAML, JSONFields: []string{"slug"}}, sampleDoc())
	if strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Fatalf("expected YAML, got:\n%s", got)
	}
}

// A field projection suppresses Extra: the user asked for exactly these fields.
func TestFieldProjectionSuppressesExtra(t *testing.T) {
	got, _ := render(t, &Writer{Format: FormatJSON, JSONFields: []string{"slug"}}, sampleDoc())
	if strings.Contains(got, "pagination") {
		t.Fatalf("a projection leaked Extra:\n%s", got)
	}
}

func TestValidateFields(t *testing.T) {
	cols := sampleDoc().Columns
	if err := ValidateFields([]string{"slug", "id"}, cols); err != nil {
		t.Fatal(err)
	}
	err := ValidateFields([]string{"slugg"}, cols)
	if err == nil {
		t.Fatal("a typo must be an error, not a column of nulls")
	}
	// The message must list what IS available, or the user has to read source.
	if !strings.Contains(err.Error(), "slug") || !strings.Contains(err.Error(), "status") {
		t.Fatalf("error does not name the alternatives: %v", err)
	}
	if err := ValidateFields(nil, cols); err != nil {
		t.Fatal("no selection must always validate")
	}
}

// Wide columns are hidden from the TABLE only. Hiding data from a machine
// format to save terminal width would be an odd trade.
func TestWideColumnsAlwaysAppearInJSON(t *testing.T) {
	got, _ := render(t, &Writer{Format: FormatJSON}, sampleDoc())
	for _, name := range []string{"id", "slept_at", "public"} {
		if !strings.Contains(got, `"`+name+`"`) {
			t.Fatalf("wide column %q missing from JSON:\n%s", name, got)
		}
	}
	table, _ := render(t, &Writer{Format: FormatTable}, sampleDoc())
	if strings.Contains(table, "b92b68a9") {
		t.Fatalf("a wide column leaked into the narrow table:\n%s", table)
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"table", "wide", "json", "yaml"} {
		if _, err := ParseFormat(s); err != nil {
			t.Fatalf("ParseFormat(%q): %v", s, err)
		}
	}
	if _, err := ParseFormat("toml"); err == nil {
		t.Fatal("ParseFormat accepted an unknown format")
	}
}

func TestSplitFields(t *testing.T) {
	got := SplitFields(" slug , status ,, ")
	if len(got) != 2 || got[0] != "slug" || got[1] != "status" {
		t.Fatalf("SplitFields = %#v", got)
	}
	if SplitFields("   ") != nil {
		t.Fatal("a blank selection must be nil, not a one-element slice")
	}
}

func TestFormatCellAbsentValues(t *testing.T) {
	// `-` rather than an empty cell: an empty cell in a padded table is
	// indistinguishable from a column that failed to render.
	for _, v := range []any{nil, "", (*string)(nil), (*time.Time)(nil), (*int)(nil), []string{}} {
		if got := formatCell(v); got != "-" {
			t.Fatalf("formatCell(%#v) = %q, want \"-\"", v, got)
		}
	}
}

func TestNormalizeProducesRFC3339UTC(t *testing.T) {
	got := Normalize(fixed.In(time.FixedZone("x", 3600)))
	if got != "2026-07-25T14:30:00Z" {
		t.Fatalf("Normalize(time) = %v", got)
	}
	if Normalize((*time.Time)(nil)) != nil {
		t.Fatal("a nil time must normalise to null")
	}
}

// REGRESSION. Column widths were measured with len(), i.e. in BYTES, so a
// non-ASCII slug — and slugs derive from branch names — shifted every following
// column on that row and only that row, producing a table that reads as
// corrupted rather than merely misaligned.
func TestNonASCIIValuesDoNotShiftColumns(t *testing.T) {
	d := &Doc{
		Columns: []Column{
			{Name: "slug", Header: "Slug"},
			{Name: "status", Header: "Status"},
			{Name: "ticket", Header: "Ticket"},
		},
		Rows: []Row{
			{"slug": "feature-café-ünïcode", "status": "running", "ticket": "AUS-1"},
			{"slug": "plain-ascii", "status": "sleeping", "ticket": "AUS-2"},
		},
	}
	got, _ := render(t, &Writer{Format: FormatTable}, d)

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected a header and two rows:\n%s", got)
	}
	// The status column must start at the same rune offset on every line.
	want := runeIndexOfColumn(lines[0], 1)
	for i, line := range lines[1:] {
		if got := runeIndexOfColumn(line, 1); got != want {
			t.Fatalf("row %d starts column 2 at rune %d, header starts it at %d:\n%s",
				i+1, got, want, got2str(lines))
		}
	}
}

// runeIndexOfColumn returns the rune offset at which the n-th whitespace-
// separated column begins.
func runeIndexOfColumn(line string, n int) int {
	runes := []rune(line)
	col, i := 0, 0
	for i < len(runes) {
		for i < len(runes) && runes[i] == ' ' {
			i++
		}
		if col == n {
			return i
		}
		for i < len(runes) && runes[i] != ' ' {
			i++
		}
		col++
	}
	return -1
}

func got2str(lines []string) string { return strings.Join(lines, "\n") }

// The detail view pads a label column and had the same byte-vs-rune bug.
func TestNonASCIIHeadersAlignInTheDetailView(t *testing.T) {
	d := &Doc{
		Single:  true,
		Columns: []Column{{Name: "a", Header: "Ünïcode"}, {Name: "b", Header: "Plain"}},
		Rows:    []Row{{"a": "1", "b": "2"}},
	}
	got, _ := render(t, &Writer{Format: FormatTable}, d)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	first, second := runeIndexOfColumn(lines[0], 1), runeIndexOfColumn(lines[1], 1)
	if first != second {
		t.Fatalf("values start at runes %d and %d:\n%s", first, second, got)
	}
}

// REGRESSION. Colour for the DIAGNOSTIC stream was decided by looking at
// stdout, so `drift env list 2> err.log` on a terminal wrote ANSI escapes into
// the log file. The two streams are redirected independently.
func TestDiagnosticColourIsDecidedSeparately(t *testing.T) {
	var out, errOut bytes.Buffer
	w := &Writer{Out: &out, Err: &errOut, Format: FormatTable, Color: true, ErrColor: false}
	w.Warnf("something")
	if strings.Contains(errOut.String(), "\x1b[") {
		t.Fatalf("stderr got ANSI escapes despite ErrColor=false: %q", errOut.String())
	}

	errOut.Reset()
	w.ErrColor = true
	w.Warnf("something")
	if !strings.Contains(errOut.String(), "\x1b[") {
		t.Fatalf("ErrColor=true produced no colour: %q", errOut.String())
	}
}

// REGRESSION. `NO_COLOR=""` disabled colour, but no-color.org specifies the
// variable acts when present AND non-empty — an empty value is the documented
// way to un-set it for one command.
func TestEmptyNoColorDoesNotDisableColour(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")
	if !ColorEnabled(nil) {
		t.Fatal("NO_COLOR= (empty) disabled colour")
	}
	// CLICOLOR_FORCE deliberately outranks NO_COLOR: an explicit request this
	// run beats a standing preference, which is how a user gets colour through
	// `less -R`. Pinned because it is a decision, not an accident.
	t.Setenv("NO_COLOR", "1")
	if !ColorEnabled(nil) {
		t.Fatal("CLICOLOR_FORCE should outrank NO_COLOR")
	}

	t.Setenv("CLICOLOR_FORCE", "")
	if ColorEnabled(nil) {
		t.Fatal("NO_COLOR=1 did not disable colour")
	}
}

// REGRESSION. IsTerminal used ModeCharDevice, and /dev/null is a character
// device — so redirecting to /dev/null looked interactive. That decides colour
// today and, from step 4, whether a destructive command may prompt instead of
// refusing.
func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(f) {
		t.Fatalf("%s reported as a terminal", os.DevNull)
	}
}
