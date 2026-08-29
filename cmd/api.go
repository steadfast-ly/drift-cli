package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steadfast/drift-cli/internal/client"
	"github.com/steadfast/drift-cli/internal/cliexit"
)

func newAPICommand(app *App) *cobra.Command {
	var (
		body    string
		headers []string
		include bool
	)

	cmd := &cobra.Command{
		Use:   "api <method> <path>",
		Short: "Raw API passthrough",
		Long: "Send a raw request to the drift API.\n\n" +
			"This is the escape hatch: when a server endpoint exists but the CLI\n" +
			"has no typed command for it yet, `drift api` lets you reach it with\n" +
			"the same credential, endpoint resolution and error mapping the typed\n" +
			"commands use.\n\n" +
			"<path> is relative to the discovered API base (e.g. `/repositories`\n" +
			"or `/environments?status=running`). A query string is carried through.\n" +
			"An absolute URL or a path that escapes the base is refused.\n\n" +
			"`-o json` has no effect: the response body is written to stdout\n" +
			"byte-exact. Non-2xx responses are decoded through the problem\n" +
			"envelope so exit codes and hints match the typed commands; the raw\n" +
			"body is written to stderr.\n\n" +
			"`--include` prints the status line and headers to stderr before the\n" +
			"body.\n\n" +
			"No feature gate: this is the escape hatch.\n\n" +
			cliexit.Help,
		Args: exactArgs(2, "<method> <path>"),
		RunE: func(c *cobra.Command, args []string) error {
			return runAPI(c.Context(), app, args[0], args[1], body, headers, include)
		},
	}
	cmd.Flags().StringVar(&body, "body", "", "request body: @file reads a file, - reads stdin")
	cmd.Flags().StringArrayVarP(&headers, "header", "H", nil, "header in key=value form (repeatable)")
	cmd.Flags().BoolVar(&include, "include", false, "print status line and headers to stderr")
	return cmd
}

// validMethods is the set of HTTP methods the escape hatch accepts.
var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

func runAPI(ctx context.Context, app *App, rawMethod, path, body string, headers []string, include bool) error {
	method := strings.ToUpper(rawMethod)
	if !validMethods[method] {
		return usageErrorf("unsupported method %q; valid: GET, POST, PUT, PATCH, DELETE", rawMethod)
	}

	// Parse the argument to extract path, query string and detect absolute URLs.
	parsed, err := url.Parse(path)
	if err != nil {
		return usageErrorf("invalid path: %s", err.Error())
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		return usageErrorf("absolute URLs are not allowed; use a relative path (e.g. /repositories)")
	}

	// A backslash anywhere in the path — literal, percent-encoded, or in the
	// raw argument — is refused. A downstream normaliser could treat it as a
	// separator and defeat the base-containment check.
	if containsBackslash(path, parsed) {
		return usageErrorf("backslashes are not allowed in the path")
	}

	// Connect with no feature gate — the escape hatch has none.
	sess, err := app.Connect(ctx, "")
	if err != nil {
		return err
	}

	// Build the full URL by joining the base with only the path component,
	// then re-attach the query string. Fragments are dropped — they have no
	// meaning in an API call.
	reqURL, err := safeJoin(sess.BaseURL, parsed.Path, parsed.RawQuery)
	if err != nil {
		return usageErrorf("%s", err.Error())
	}

	// Read the body.
	var bodyReader io.Reader
	if body != "" {
		r, err := readBody(body, app.Stdin)
		if err != nil {
			return err
		}
		bodyReader = r
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return &cliexit.ExitError{Code: cliexit.Error, Message: err.Error()}
	}

	// Default headers.
	req.Header.Set("Accept", "application/json")
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Apply the bearer credential through the same editor the typed client uses.
	if sess.Credential.Token != "" {
		req.Header.Set("Authorization", "Bearer "+sess.Credential.Token)
	}
	if app.Version != "" {
		req.Header.Set(client.ClientVersionHeader, app.Version)
	}

	// User-supplied headers override defaults. Authorization is refused: the
	// bearer comes from the context's credential or DRIFT_TOKEN, not from a
	// flag that would silently replace it.
	for _, h := range headers {
		k, v, ok := strings.Cut(h, "=")
		if !ok {
			return usageErrorf("header %q must be key=value", h)
		}
		key := strings.TrimSpace(k)
		if strings.EqualFold(key, "Authorization") {
			return usageErrorf(
				"the Authorization header cannot be set with -H; the bearer comes from the context's credential or DRIFT_TOKEN")
		}
		req.Header.Set(key, strings.TrimSpace(v))
	}

	hc := app.httpClient()
	resp, err := hc.Do(req)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	defer resp.Body.Close()

	// --include: status line + headers to stderr.
	if include {
		fmt.Fprintf(app.Stderr, "HTTP/%d.%d %s\n", resp.ProtoMajor, resp.ProtoMinor, resp.Status)
		for k, vs := range resp.Header {
			for _, v := range vs {
				fmt.Fprintf(app.Stderr, "%s: %s\n", k, v)
			}
		}
		fmt.Fprintln(app.Stderr)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return &cliexit.ExitError{Code: cliexit.Error, Message: fmt.Sprintf("reading response: %s", err.Error())}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if _, err := app.Stdout.Write(respBody); err != nil {
			return &cliexit.ExitError{
				Code:    cliexit.Error,
				Message: fmt.Sprintf("writing response: %s", err.Error()),
			}
		}
		return nil
	}

	// Non-2xx: write the raw body to stderr, decode through Problem for
	// exit codes and hints.
	_, _ = app.Stderr.Write(respBody)
	fmt.Fprintln(app.Stderr)
	return client.Problem(nil, respBody, resp.StatusCode)
}

// safeJoin joins a base URL and a relative path, refusing anything that would
// move the request to a different origin or escape the API base through `..`.
func safeJoin(base, relPath, rawQuery string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("base URL %q is not valid: %w", base, err)
	}

	// Ensure the path starts with / for a clean join.
	if !strings.HasPrefix(relPath, "/") {
		relPath = "/" + relPath
	}

	joined := b.JoinPath(relPath)
	if joined.Scheme != b.Scheme || joined.Host != b.Host {
		return "", fmt.Errorf("the path would move the request from %s://%s to %s://%s",
			b.Scheme, b.Host, joined.Scheme, joined.Host)
	}

	// After JoinPath resolves `..`, the resulting path must still start with
	// the base path. Without this check, `/../x` resolves to `/api/x` which
	// is outside `/api/v1`.
	basePath := strings.TrimRight(b.Path, "/") + "/"
	joinedPath := joined.Path
	if !strings.HasPrefix(joinedPath+"/", basePath) && joinedPath != strings.TrimRight(b.Path, "/") {
		return "", fmt.Errorf("the path escapes the API base %q", b.Path)
	}

	// Re-attach the query string (JoinPath percent-encodes `?` in path
	// segments, so it must be carried separately).
	joined.RawQuery = rawQuery

	return joined.String(), nil
}

// containsBackslash checks the path argument, the parsed Path and RawPath for
// backslashes (literal or percent-encoded). A downstream normaliser could treat
// `\` as a separator and defeat the base-containment check.
func containsBackslash(raw string, u *url.URL) bool {
	if strings.ContainsRune(u.Path, '\\') {
		return true
	}
	if strings.ContainsRune(u.RawPath, '\\') {
		return true
	}
	low := strings.ToLower(raw)
	return strings.Contains(low, "%5c")
}

// readBody resolves --body: @file reads a file, - reads stdin.
func readBody(spec string, stdin io.Reader) (io.Reader, error) {
	if spec == "-" {
		return stdin, nil
	}
	if strings.HasPrefix(spec, "@") {
		f, err := os.Open(spec[1:])
		if err != nil {
			return nil, &cliexit.ExitError{Code: cliexit.Error, Message: fmt.Sprintf("reading body: %s", err.Error())}
		}
		return f, nil
	}
	return strings.NewReader(spec), nil
}
