package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
)

const infoUsage = `harness info [--bundles]

Combined healthz + info from BOTH the proxy and (when enabled) the
forwarder. Useful for "is everything wired up" before running a soak.

Flags:
  --bundles   also include the forwarder's bundle catalogue
`

type infoOutput struct {
	BaseURL          string          `json:"base_url"`
	Proxy            json.RawMessage `json:"proxy_info,omitempty"`
	ProxyHealth      string          `json:"proxy_health"`
	Forwarder        json.RawMessage `json:"forwarder_info,omitempty"`
	ForwarderHealth  string          `json:"forwarder_health"`
	Bundles          json.RawMessage `json:"bundles,omitempty"`
	SnapshotStoreDir string          `json:"snapshot_store_dir,omitempty"`
}

func cmdInfo(client *api.Client, args []string, asJSON bool) error {
	includeBundles := false
	for _, a := range args {
		switch a {
		case "--bundles":
			includeBundles = true
		case "-h", "--help":
			return errors.New(infoUsage)
		}
	}
	ctx := context.Background()
	out := infoOutput{BaseURL: client.BaseURL}
	if client.Snap != nil {
		out.SnapshotStoreDir = client.Snap.Dir
	}

	out.Proxy = getJSON(ctx, client, client.BaseURL+"/api/v2/info")
	out.ProxyHealth = getStatus(ctx, client, client.BaseURL+"/api/v2/healthz")
	out.Forwarder = getJSON(ctx, client, client.BaseURL+"/analytics/api/v2/info")
	out.ForwarderHealth = getStatus(ctx, client, client.BaseURL+"/analytics/api/v2/healthz")

	if includeBundles {
		if b, err := client.ArchiveBundles(ctx); err == nil {
			out.Bundles = b
		}
	}

	if asJSON {
		return format.JSON(os.Stdout, out)
	}
	fmt.Printf("base_url:       %s\n", out.BaseURL)
	fmt.Printf("snapshot dir:   %s\n", out.SnapshotStoreDir)
	fmt.Printf("proxy health:   %s\n", out.ProxyHealth)
	fmt.Printf("forwarder hlth: %s\n", out.ForwarderHealth)
	if len(out.Proxy) > 0 {
		fmt.Println("proxy info:")
		fmt.Println(indent(string(out.Proxy), "  "))
	}
	if len(out.Forwarder) > 0 {
		fmt.Println("forwarder info:")
		fmt.Println(indent(string(out.Forwarder), "  "))
	}
	if includeBundles && len(out.Bundles) > 0 {
		fmt.Println("bundles:")
		fmt.Println(indent(string(out.Bundles), "  "))
	}
	return nil
}

// getJSON does a one-off GET and returns the body. On any error or
// non-2xx it returns nil so the rendered output shows the field as
// absent (better than rendering an internal error per-field).
func getJSON(ctx context.Context, client *api.Client, url string) json.RawMessage {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if client.BasicAuth != "" {
		req.SetBasicAuth(splitBasicAuth(client.BasicAuth))
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return body
}

func getStatus(ctx context.Context, client *api.Client, url string) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if client.BasicAuth != "" {
		req.SetBasicAuth(splitBasicAuth(client.BasicAuth))
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return "unreachable: " + err.Error()
	}
	resp.Body.Close()
	return fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
}

// splitBasicAuth duplicates internal/api/sse.go's splitBasic since
// that helper is package-private. Small enough not to bother hoisting.
func splitBasicAuth(s string) (string, string) {
	for i, ch := range s {
		if ch == ':' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}
