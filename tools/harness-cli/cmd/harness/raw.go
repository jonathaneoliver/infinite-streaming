package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
)

const rawUsage = `harness raw <METHOD> <PATH> [--body @file|--body STRING] [--header K=V ...]

Escape hatch. Sends an HTTP request with the harness's auth + TLS
config but no body inference, no snapshotting, no resolver. Intended
for one-off probing of endpoints not yet wrapped by typed commands,
or for spec-debugging.

PATH may start with /api/v2/... (proxy) or /analytics/api/v2/...
(forwarder). Other prefixes pass through verbatim.

Examples:
  harness raw GET /api/v2/info
  harness raw POST /api/v2/players --body '{"manifest_url":"https://..."}'
  harness raw PATCH /api/v2/players/UUID --body @patch.json \\
       --header 'If-Match="2026-05-...."' \\
       --header 'Content-Type=application/merge-patch+json'
`

func cmdRaw(client *api.Client, args []string, _ bool) error {
	if len(args) < 2 {
		return errors.New(rawUsage)
	}
	method := strings.ToUpper(args[0])
	path := args[1]
	fs := flag.NewFlagSet("raw", flag.ContinueOnError)
	body := fs.String("body", "", "body literal, or @path for file contents")
	headers := stringSliceFlag{}
	fs.Var(&headers, "header", "K=V (repeatable)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	var bodyReader io.Reader
	if *body != "" {
		if strings.HasPrefix(*body, "@") {
			f, err := os.Open((*body)[1:])
			if err != nil {
				return err
			}
			defer f.Close()
			bodyReader = f
		} else {
			bodyReader = strings.NewReader(*body)
		}
	}

	url := client.BaseURL + path
	req, err := http.NewRequestWithContext(context.Background(), method, url, bodyReader)
	if err != nil {
		return err
	}
	if client.BasicAuth != "" {
		req.SetBasicAuth(splitBasicAuth(client.BasicAuth))
	}
	// Default Content-Type for merge-patch (most common harness need).
	if bodyReader != nil && req.Header.Get("Content-Type") == "" && method == "PATCH" {
		req.Header.Set("Content-Type", "application/merge-patch+json")
	}
	for _, h := range headers {
		k, v, ok := strings.Cut(h, "=")
		if !ok {
			return fmt.Errorf("bad --header %q (want K=V)", h)
		}
		req.Header.Set(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`))
	}

	resp, err := client.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	fmt.Fprintf(os.Stderr, "→ %s %s\n← %d %s  etag=%s\n", method, url, resp.StatusCode, resp.Status, resp.Header.Get("ETag"))
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		// Return an error so main's fail() handles exit — preserves
		// the deferred body.Close() + any --body @file close that
		// os.Exit would otherwise skip.
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, resp.Status)
	}
	return nil
}

// stringSliceFlag collects repeated --header K=V into a slice.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }
