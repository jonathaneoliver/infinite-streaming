package runner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Config-on-connect (#714) bootstrap helper.
//
// Instead of the old cold-start choreography (LaunchToHome → discover
// player_id → PATCH shape → settle → ResumePlayback), a config-on-connect run
// mints the player_id itself, GETs the bootstrap master URL on the shaper port
// carrying `player_id` + `proxy.*` args, and lets #712 materialize the session
// config + install the kernel rule BEFORE the 302 is written. The app then
// launches with the same player_id (`-is.player_id <id>`) and inherits the
// already-configured session via the existing-session reattach — so the cap is
// live from the player's first byte, with no race and no PATCH round-trip.

// bootstrapDefaultBaseURL mirrors the harness CLI default
// (tools/harness-cli/internal/api/client.go DefaultBaseURL).
const bootstrapDefaultBaseURL = "https://jonathanoliver-ubuntu.local:21000"

// NewPlayerID mints a fresh player_id for a config-on-connect run. The harness
// passes it to BOTH ConfigureOnConnect and the app launch (`-is.player_id`).
// All Appium-driven platforms honor the launch-arg id: iOS/tvOS via
// NSArgumentDomain, Android via the `is.player_id` intent extra.
func NewPlayerID() string { return uuid.NewString() }

// BootstrapConfig is the proxy.* config to materialize at session allocation.
// Keys are proxy.* arg paths WITHOUT the "proxy." prefix (e.g.
// "shape.rate_mbps", "fault_rules[0].type"); values are the string form.
type BootstrapConfig map[string]string

// ShapeRateConfig caps the session rate. 0 Mbps means "no cap" — the proxy
// resolves the deployment baseline.
func ShapeRateConfig(rateMbps float64) BootstrapConfig {
	return BootstrapConfig{"shape.rate_mbps": strconv.FormatFloat(rateMbps, 'g', -1, 64)}
}

// ShapeConfig is ShapeRateConfig plus, when xferTimeout>0, the server-side
// active transfer timeout on segment fetches — folded into config-on-connect so
// it's armed before the first segment (not via a post-bind PATCH). 0 timeout ⇒
// rate only. The transfer_timeouts.* keys ride the same parseProxyArgs nesting
// the PATCH API uses, so the proxy materializes them identically.
func ShapeConfig(rateMbps float64, xferTimeout time.Duration) BootstrapConfig {
	cfg := ShapeRateConfig(rateMbps)
	if xferTimeout > 0 {
		cfg["transfer_timeouts.active_timeout_seconds"] = strconv.Itoa(int(xferTimeout.Seconds()))
		cfg["transfer_timeouts.applies_segments"] = "true"
	}
	return cfg
}

// ConfigureOnConnect allocates and configures the proxy session for playerID
// BEFORE the app launches. Clip-agnostic: shape/fault config is session-scoped,
// so any discoverable clip triggers the allocation; the app's real clip
// reattaches by player_id.
func ConfigureOnConnect(ctx context.Context, playerID string, cfg BootstrapConfig) error {
	if playerID == "" {
		return fmt.Errorf("ConfigureOnConnect: empty playerID")
	}
	u, err := url.Parse(strings.TrimRight(bootstrapBaseURL(), "/"))
	if err != nil {
		return fmt.Errorf("ConfigureOnConnect: parse base URL: %w", err)
	}
	host, apiPort := splitHostPort(u.Host)
	shaperBase := net.JoinHostPort(host, shaperPortFromUIPort(apiPort))

	client := bootstrapHTTPClient()
	clip, err := discoverClip(ctx, client, u.Scheme, u.Host)
	if err != nil {
		return fmt.Errorf("ConfigureOnConnect: %w", err)
	}

	q := url.Values{}
	q.Set("player_id", playerID)
	for k, v := range cfg {
		q.Set("proxy."+k, v)
	}
	bootURL := fmt.Sprintf("%s://%s/go-live/%s/master_6s.m3u8?%s",
		u.Scheme, shaperBase, url.PathEscape(clip), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bootURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ConfigureOnConnect GET %s: %w", bootURL, err)
	}
	defer resp.Body.Close()
	// #712 does all materialization + kernel apply BEFORE writing the 302, so
	// the 302 to the session port (we don't follow it) already means
	// "configured". A 4xx/5xx means the proxy rejected the args or is down.
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ConfigureOnConnect: proxy returned %d for %s: %s",
			resp.StatusCode, bootURL, strings.TrimSpace(string(body)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

func bootstrapBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("HARNESS_BASE_URL")); v != "" {
		return v
	}
	return bootstrapDefaultBaseURL
}

// bootstrapHTTPClient does not follow redirects — receiving the proxy's 302 to
// the session port is the success signal, and not following it keeps the helper
// independent of the per-session port being reachable from the runner.
func bootstrapHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: HarnessInsecure},
		},
	}
}

// splitHostPort splits "host:port"; if no port is present the whole string is
// the host and the port is empty.
func splitHostPort(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return hostport, ""
}

// shaperPortFromUIPort mirrors the dashboard's normalizeStreamUrl and the
// server-behavior tests: replace the last 3 digits of the UI/API port with
// "081" (21000 → 21081). That's the proxy's bootstrap port for new sessions.
func shaperPortFromUIPort(uiPort string) string {
	if len(uiPort) < 4 {
		return uiPort
	}
	return uiPort[:len(uiPort)-3] + "081"
}

// MasterLadder fetches the content's master playlist directly (no proxy
// session, no player) and builds the cap ladder from its declared variants.
// This lets config-on-connect compute the EXACT cold-start floor without
// having to play the content once first to learn its ladder (#714). Pass
// content="" to use a discovered clip.
func MasterLadder(ctx context.Context, content string) ([]VariantRate, error) {
	vs, err := fetchMasterVariants(ctx, content)
	if err != nil {
		return nil, err
	}
	return LadderRatesFromVariants(vs)
}

// fetchMasterVariants GETs the master_6s.m3u8 on the API origin (served by
// go-live directly — no per-session proxy, no 302) and parses its variant
// bandwidths.
func fetchMasterVariants(ctx context.Context, content string) ([]ManifestVariant, error) {
	u, err := url.Parse(strings.TrimRight(bootstrapBaseURL(), "/"))
	if err != nil {
		return nil, fmt.Errorf("MasterLadder: parse base URL: %w", err)
	}
	client := bootstrapHTTPClient()
	if content == "" {
		if content, err = discoverClip(ctx, client, u.Scheme, u.Host); err != nil {
			return nil, fmt.Errorf("MasterLadder: %w", err)
		}
	}
	masterURL := fmt.Sprintf("%s://%s/go-live/%s/master_6s.m3u8", u.Scheme, u.Host, url.PathEscape(content))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, masterURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MasterLadder GET %s: %w", masterURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("MasterLadder: %s returned %d", masterURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	vs := parseMasterVariants(string(body))
	if len(vs) == 0 {
		return nil, fmt.Errorf("MasterLadder: no variants parsed from %s", masterURL)
	}
	return vs, nil
}

// HLS master-playlist attribute matchers. The `(?:^|,)` anchor disambiguates
// BANDWIDTH from AVERAGE-BANDWIDTH (the latter is preceded by `-`, not `^|,`).
var (
	reStreamBandwidth    = regexp.MustCompile(`(?:^|,)BANDWIDTH=(\d+)`)
	reStreamAvgBandwidth = regexp.MustCompile(`(?:^|,)AVERAGE-BANDWIDTH=(\d+)`)
	reStreamResolution   = regexp.MustCompile(`(?:^|,)RESOLUTION=([0-9]+x[0-9]+)`)
)

// parseMasterVariants pulls {BANDWIDTH, AVERAGE-BANDWIDTH, RESOLUTION} + the URI
// off each #EXT-X-STREAM-INF entry. Audio-only / bandwidth-less entries are
// dropped.
func parseMasterVariants(body string) []ManifestVariant {
	var out []ManifestVariant
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(strings.ToUpper(line), "#EXT-X-STREAM-INF:") {
			continue
		}
		attrs := line[strings.IndexByte(line, ':')+1:]
		v := ManifestVariant{
			Bandwidth:        firstInt(reStreamBandwidth, attrs),
			AverageBandwidth: firstInt(reStreamAvgBandwidth, attrs),
			Resolution:       firstStr(reStreamResolution, attrs),
		}
		for j := i + 1; j < len(lines); j++ {
			uri := strings.TrimSpace(lines[j])
			if uri == "" || strings.HasPrefix(uri, "#") {
				continue
			}
			v.URL = uri
			i = j
			break
		}
		if v.Bandwidth > 0 {
			out = append(out, v)
		}
	}
	return out
}

func firstInt(re *regexp.Regexp, s string) int {
	if m := re.FindStringSubmatch(s); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

func firstStr(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// discoverClip returns a content clip name to drive session allocation. The
// clip is irrelevant to the (session-scoped) config, so any one works; override
// with CHAR_CONTENT. Tolerates the several /api/content response shapes.
func discoverClip(ctx context.Context, c *http.Client, scheme, apiHost string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("CHAR_CONTENT")); v != "" {
		return v, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s://%s/api/content", scheme, apiHost), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("/api/content: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("/api/content read: %w", err)
	}
	var asObjs []map[string]any
	if json.Unmarshal(body, &asObjs) == nil {
		for _, e := range asObjs {
			if name, _ := e["name"].(string); name != "" {
				return name, nil
			}
			if id, _ := e["id"].(string); id != "" {
				return id, nil
			}
		}
	}
	var asStrs []string
	if json.Unmarshal(body, &asStrs) == nil {
		for _, s := range asStrs {
			if s != "" {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("/api/content: no clip found in response")
}
