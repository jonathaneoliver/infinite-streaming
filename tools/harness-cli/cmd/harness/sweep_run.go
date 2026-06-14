package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/forwarder"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

// This file is the device-bound half of the §7 run loop (issue #772): the CLI
// surface a /goal driver calls per iteration — apply a claimed experiment onto
// a live session, analyse the resulting play into a verdict + bucket move,
// promote a confirmed hit to a deduped Issue, and reap dead claims. The pure
// logic lives in internal/sweep (analyze.go / reap.go / promote.go); these
// wrappers do the I/O.

// --- sweep apply ----------------------------------------------------------

func cmdSweepApply(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: harness sweep apply <experiment-id> --target <player> [--from running]")
	}
	id := args[0]
	fs := flag.NewFlagSet("sweep apply", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	from := fs.String("from", "running", "bucket holding the experiment")
	target := fs.String("target", "", "player target (id/label/ip/UA substring) — required")
	noReset := fs.Bool("no-reset", false, "skip the pre-apply shape/fault reset (step 0)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *target == "" {
		return errors.New("--target is required")
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	e, err := s.Load(sweep.Status(*from), id)
	if err != nil {
		return fmt.Errorf("load %s/%s: %w", *from, id, err)
	}

	ctx := context.Background()
	pid, err := client.Resolve(ctx, *target)
	if err != nil {
		return err
	}

	applied := []string{}

	// Step 0 (§11): start from a clean target — residual shape/faults are
	// per-session kernel state that leaks across runs otherwise.
	if !*noReset {
		if _, err := client.ClearShape(ctx, pid, "sweep reset shape"); err != nil {
			return fmt.Errorf("reset shape: %w", err)
		}
		if _, err := client.ClearFaultRules(ctx, pid, "sweep reset faults"); err != nil {
			return fmt.Errorf("reset faults: %w", err)
		}
		applied = append(applied, "reset(shape+faults)")
	}

	// Labels (the what + why, §4.1) + content_manipulation in one PATCH.
	patch := proxy.PlayerPatch{}
	labelMap := sweep.RunLabels(e)
	pl := proxy.Labels(labelMap)
	patch.Labels = &pl
	if e.ContentManipulation != nil {
		allowed, err := resolveAllowedVariants(ctx, client, e.Content, e.ContentManipulation.AllowedVariants)
		if err != nil {
			return err
		}
		cm, err := toProxyContent(e.ContentManipulation, allowed)
		if err != nil {
			return fmt.Errorf("content_manipulation: %w", err)
		}
		patch.Content = cm
		applied = append(applied, "content")
	}
	if e.TransferTimeouts != nil {
		patch.TransferTimeouts = toProxyTransferTimeouts(e.TransferTimeouts)
		applied = append(applied, "transfer_timeouts")
	}
	if _, err := client.PatchPlayer(ctx, pid, "sweep apply labels+content", patch); err != nil {
		return fmt.Errorf("apply labels/content: %w", err)
	}
	applied = append(applied, fmt.Sprintf("labels(%d)", len(labelMap)))

	// Slider shaping (rate/delay/loss) — pattern shapes need a fetched
	// manifest, so they're applied post-launch via `harness shape --pattern`.
	if sh := e.Shape; sh != nil {
		if sh.Pattern != "" {
			applied = append(applied, fmt.Sprintf("pattern=%s(deferred: run `harness shape %s --pattern %s --step-seconds %d --margin %d` after the master fetch)",
				sh.Pattern, *target, sh.Pattern, nonZero(sh.StepSeconds, 12), sh.MarginPct))
		}
		if ps := toProxySliderShape(sh); ps != nil {
			if _, err := client.PatchShape(ctx, pid, "sweep apply shape", ps); err != nil {
				return fmt.Errorf("apply shape: %w", err)
			}
			applied = append(applied, "shape(sliders)")
		}
	}

	// HTTP fault rule.
	if e.Fault != nil {
		rule, err := toProxyFaultRule(e.Fault)
		if err != nil {
			return fmt.Errorf("fault: %w", err)
		}
		if _, err := client.AddFaultRule(ctx, pid, "sweep apply fault", rule); err != nil {
			return fmt.Errorf("add fault: %w", err)
		}
		applied = append(applied, "fault="+sweepFaultDesc(e.Fault))
	}

	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"experiment": e.ID, "player_id": pid, "applied": applied, "why": e.Why,
		})
	}
	fmt.Printf("applied %s → %s: %s\n", e.ID, pid, strings.Join(applied, ", "))
	if e.WhyText != "" {
		fmt.Printf("  why: %s\n", e.WhyText)
	}
	return nil
}

// --- sweep bootstrap (config-on-connect) ----------------------------------

// cmdSweepBootstrap is the config-on-connect probe hook (§7 step 2, the
// integration seam): it materialises an experiment's full recipe onto a proxy
// session BEFORE the app launches, by GETting the bootstrap master URL on the
// shaper port carrying `player_id` + a full-fidelity `proxy.cfg` patch. The cap
// /fault/content is then live from the player's first byte. The caller mints
// (or passes) the player_id and launches the platform app bound to that same id
// (`-is.player_id <id>`), so the mode plays the already-configured session.
func cmdSweepBootstrap(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: harness sweep bootstrap <experiment-id> [--player UUID] [--group G] [--content C] [--from running]")
	}
	id := args[0]
	fs := flag.NewFlagSet("sweep bootstrap", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	from := fs.String("from", "running", "bucket holding the experiment")
	player := fs.String("player", "", "player_id to configure (default: mint a fresh UUID)")
	group := fs.String("group", "", "group_id to born-group the session (A/B pairs)")
	content := fs.String("content", "", "clip to drive allocation (default: the experiment's content)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	e, err := s.Load(sweep.Status(*from), id)
	if err != nil {
		return fmt.Errorf("load %s/%s: %w", *from, id, err)
	}

	playerID := *player
	if playerID == "" {
		playerID = uuid.NewString()
	}
	clip := *content
	if clip == "" {
		clip = e.Content
	}
	if clip == "" {
		return errors.New("no content: pass --content or set the experiment's content")
	}

	patch, summary, err := experimentPlayerPatch(context.Background(), client, clip, e)
	if err != nil {
		return err
	}
	cfgJSON, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	cfgB64 := base64.RawURLEncoding.EncodeToString(cfgJSON)

	bootURL, err := shaperBootstrapURL(client.BaseURL, clip, playerID, *group, cfgB64)
	if err != nil {
		return err
	}

	// Reuse the CLI's configured transport (carries the --insecure TLS skip
	// test-dev's self-signed cert needs), but don't follow the 302 — receiving
	// it on the shaper port is the success signal, and the per-session port
	// need not be reachable from this host.
	hc := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     client.HTTP.Transport,
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, bootURL, nil)
	if err != nil {
		return err
	}
	if client.BasicAuth != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(client.BasicAuth)))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("bootstrap GET: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	// #712 materialises + applies the kernel rule BEFORE writing the 302, so a
	// 3xx (which we don't follow) already means "session configured". 4xx/5xx ⇒
	// the proxy rejected the args.
	if resp.StatusCode >= 400 {
		return fmt.Errorf("bootstrap: proxy returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Persist the player_id back onto the experiment so analyze (and the
	// post-mortem) can build the session-viewer link even though the play_id
	// isn't known until the probe runs.
	e.PlayerID = playerID
	if err := s.Save(sweep.Status(*from), e); err != nil {
		return err
	}

	viewer := viewerURL(client.BaseURL, playerID, "")
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"experiment": e.ID, "player_id": playerID, "group_id": *group,
			"clip": clip, "applied": summary, "status": resp.StatusCode,
			"session_viewer": viewer,
		})
	}
	fmt.Printf("configured session for %s → player_id=%s (%s)\n", e.ID, playerID, strings.Join(summary, ", "))
	fmt.Printf("  launch the app with -is.player_id %s to play the configured session\n", playerID)
	fmt.Printf("  session-viewer: %s\n", viewer)
	return nil
}

// viewerURL builds the dashboard session-viewer link (SessionViewerLink.vue
// contract: player_id required, play_id optional). baseURL is the API/UI
// origin. Empty playID ⇒ player-scoped link (works before the play exists).
func viewerURL(baseURL, playerID, playID string) string {
	base := strings.TrimRight(baseURL, "/")
	u := fmt.Sprintf("%s/dashboard/session-viewer.html?player_id=%s", base, url.QueryEscape(playerID))
	if playID != "" {
		u += "&play_id=" + url.QueryEscape(playID)
	}
	return u
}

// experimentPlayerPatch builds the combined PlayerPatch (labels + slider shape
// + content + fault) that both config-on-connect (proxy.cfg) and a live PATCH
// can carry. Pattern shapes are omitted — they need a fetched manifest's ladder
// and are applied post-launch via `harness shape --pattern`.
func experimentPlayerPatch(ctx context.Context, client *api.Client, content string, e *sweep.Experiment) (proxy.PlayerPatch, []string, error) {
	patch := proxy.PlayerPatch{}
	var summary []string

	labelMap := sweep.RunLabels(e)
	pl := proxy.Labels(labelMap)
	patch.Labels = &pl
	summary = append(summary, fmt.Sprintf("labels(%d)", len(labelMap)))

	if e.ContentManipulation != nil {
		allowed, err := resolveAllowedVariants(ctx, client, content, e.ContentManipulation.AllowedVariants)
		if err != nil {
			return patch, nil, err
		}
		cm, err := toProxyContent(e.ContentManipulation, allowed)
		if err != nil {
			return patch, nil, fmt.Errorf("content_manipulation: %w", err)
		}
		patch.Content = cm
		summary = append(summary, "content")
	}
	if e.Shape != nil {
		if ps := toProxySliderShape(e.Shape); ps != nil {
			patch.Shape = ps
			summary = append(summary, "shape(sliders)")
		}
		if e.Shape.Pattern != "" {
			summary = append(summary, "pattern(deferred→post-launch)")
		}
	}
	if e.TransferTimeouts != nil {
		patch.TransferTimeouts = toProxyTransferTimeouts(e.TransferTimeouts)
		summary = append(summary, "transfer_timeouts")
	}
	if e.Fault != nil {
		rule, err := toProxyFaultRule(e.Fault)
		if err != nil {
			return patch, nil, fmt.Errorf("fault: %w", err)
		}
		rules := []proxy.FaultRule{rule}
		patch.FaultRules = &rules
		summary = append(summary, "fault="+sweepFaultDesc(e.Fault))
	}
	return patch, summary, nil
}

// shaperBootstrapURL builds the config-on-connect GET URL: the master playlist
// on the shaper port (UI port with the last 3 digits → 081, mirroring the
// runner's shaperPortFromUIPort), carrying player_id + group_id + proxy.cfg.
func shaperBootstrapURL(baseURL, clip, playerID, groupID, cfgB64 string) (string, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("parse base URL %q: %w", baseURL, err)
	}
	host, port := splitHostPortSafe(u.Host)
	if len(port) >= 4 {
		port = port[:len(port)-3] + "081"
	}
	shaperHost := host
	if port != "" {
		shaperHost = net.JoinHostPort(host, port)
	}
	q := url.Values{}
	q.Set("player_id", playerID)
	if groupID != "" {
		q.Set("group_id", groupID)
	}
	q.Set("proxy.cfg", cfgB64)
	return fmt.Sprintf("%s://%s/go-live/%s/master_6s.m3u8?%s",
		u.Scheme, shaperHost, url.PathEscape(clip), q.Encode()), nil
}

func splitHostPortSafe(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return hostport, ""
}

// --- sweep analyze --------------------------------------------------------

func cmdSweepAnalyze(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: harness sweep analyze <experiment-id> --play <play_id> [--from running] [--confirm-reps N]")
	}
	id := args[0]
	fs := flag.NewFlagSet("sweep analyze", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	from := fs.String("from", "running", "bucket holding the experiment")
	play := fs.String("play", "", "play_id the probe produced — required")
	confirmReps := fs.Int("confirm-reps", 3, "reps to enqueue for a first-pass hit (n=1 guard); 0 disables")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *play == "" {
		return errors.New("--play <play_id> is required")
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	e, err := s.Load(sweep.Status(*from), id)
	if err != nil {
		return fmt.Errorf("load %s/%s: %w", *from, id, err)
	}

	// Oracle A: pull the play's severity-tagged labels (the label_histogram on
	// the plays row, unioned across all three source tables) and classify.
	pid, err := parsePlayID(*play)
	if err != nil {
		return err
	}
	limit := 1
	params := &forwarder.GetApiV2PlaysParams{PlayId: &pid, Limit: &limit}
	body, err := client.ArchivePlays(context.Background(), params)
	if err != nil {
		return fmt.Errorf("query play %s: %w", *play, err)
	}
	labels, err := sweep.LabelsFromPlayHistogram(body)
	if err != nil {
		return err
	}

	bucket := sweep.Analyze(e, *play, labels)

	// n=1 guard: a first single-rep hit enqueues confirmation reps instead of
	// being promoted straight away. The reps land in backlog; this experiment
	// still records its verdict and moves to its bucket for the post-mortem.
	var enqueued []string
	if *confirmReps > 0 && sweep.NeedsConfirmation(e) {
		for _, rep := range sweep.ConfirmationReps(e, *confirmReps, nowUTC()) {
			if err := s.Save(sweep.StatusBacklog, rep); err != nil {
				return err
			}
			enqueued = append(enqueued, rep.ID)
		}
	}

	if err := s.Move(sweep.Status(*from), bucket, e); err != nil {
		return err
	}

	viewer := viewerURL(client.BaseURL, e.PlayerID, *play)
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"experiment": e.ID, "player_id": e.PlayerID, "play_id": *play, "verdict": e.Result.Verdict,
			"labels": labels, "moved_to": bucket, "confirm_reps": enqueued,
			"session_viewer": viewer,
		})
	}
	fmt.Printf("%s: verdict=%s → %s/ (%d labels)\n", e.ID, e.Result.Verdict, bucket, len(labels))
	if k := sweep.PrimaryKind(labels); k != "" {
		fmt.Printf("  primary: %s\n", k)
	}
	fmt.Printf("  player_id=%s play_id=%s\n", e.PlayerID, *play)
	fmt.Printf("  session-viewer: %s\n", viewer)
	if len(enqueued) > 0 {
		fmt.Printf("  n=1 guard: enqueued %d confirmation reps → backlog (%s)\n", len(enqueued), strings.Join(enqueued, ", "))
	}
	return nil
}

// --- sweep promote --------------------------------------------------------

func cmdSweepPromote(_ *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: harness sweep promote <experiment-id> [--axis A] [--from found] [--dry-run]")
	}
	id := args[0]
	fs := flag.NewFlagSet("sweep promote", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	from := fs.String("from", "found", "bucket holding the experiment")
	axis := fs.String("axis", "", "isolation-attributed axis, appended to the signature")
	dryRun := fs.Bool("dry-run", false, "print the gh command + body, do not create/comment")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	e, err := s.Load(sweep.Status(*from), id)
	if err != nil {
		return fmt.Errorf("load %s/%s: %w", *from, id, err)
	}
	var labels []string
	if e.Result != nil {
		labels = e.Result.Labels
	}
	kind := sweep.PrimaryKind(labels)
	sig := sweep.Signature(e, kind, *axis)
	issueLabels := sweep.IssueLabels(sig, verdictOf(e))
	body := sweep.IssueBody(e, sig, *axis)
	title := sweep.IssueTitle(e, kind)

	if *dryRun {
		fmt.Printf("# would promote %s\nsignature: %s\nlabels: %s\ntitle: %s\n\n--- body ---\n%s\n",
			e.ID, sig, strings.Join(issueLabels, ","), title, body)
		return nil
	}

	// Dedup: an open Issue carrying this signature label → comment the new
	// repro instead of opening a duplicate.
	existing, err := ghFindIssue(sig)
	if err != nil {
		return err
	}
	bodyFile, err := os.CreateTemp("", "sweep-issue-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(bodyFile.Name())
	if _, err := bodyFile.WriteString(body); err != nil {
		return err
	}
	bodyFile.Close()

	var result string
	if existing != "" {
		if err := ghRun("issue", "comment", existing, "--body-file", bodyFile.Name()); err != nil {
			return err
		}
		result = "commented on #" + existing
	} else {
		out, err := ghOutput("issue", "create", "--title", title, "--body-file", bodyFile.Name(),
			"--label", strings.Join(issueLabels, ","))
		if err != nil {
			return err
		}
		result = "created " + strings.TrimSpace(out)
	}

	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"experiment": e.ID, "signature": sig, "result": result,
		})
	}
	fmt.Printf("%s → %s (sig %s)\n", e.ID, result, sig)
	return nil
}

// --- sweep publish (→ dashboard) ------------------------------------------

// cmdSweepPublish snapshots the whole .sweep/ queue and POSTs it to the
// forwarder so the dashboard's Sweep tab can show it (the .sweep/ files live on
// the runner, not the deploy). Idempotent: the forwarder upserts by exp_id, so
// the loop can call this after every iteration to keep the tab live.
func cmdSweepPublish(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep publish", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	// Recompute the scheduler score at publish time so the dashboard's
	// pending order matches what actually runs next. Stored scores can be
	// stale — isolation/bisect experiments are created with score 0 and only
	// get their true (kind-dominated) score at live selection — so trust the
	// scheduler, not the file.
	w := sweep.DefaultWeights()
	var rows []map[string]any
	for _, st := range sweep.AllStatuses {
		exps, err := s.List(st)
		if err != nil {
			return err
		}
		for _, e := range exps {
			verdict := ""
			if e.Result != nil {
				verdict = string(e.Result.Verdict)
			}
			rows = append(rows, map[string]any{
				"exp_id": e.ID, "class": string(e.ClassOrDefault()), "status": string(st),
				"kind": string(e.Kind), "platform": e.Platform, "protocol": e.Protocol,
				"mode": e.Mode, "recipe": sweep.RecipeSlug(e), "arm": string(e.Arm),
				"group_id": e.Group, "parent": e.Parent, "depth": e.Depth,
				"why": e.Why, "why_text": e.WhyText, "verdict": verdict,
				"player_id": e.PlayerID, "play_id": e.PlayID, "score": w.Score(e),
				"created_at": e.CreatedAt,
			})
		}
	}

	payload, err := json.Marshal(map[string]any{"experiments": rows})
	if err != nil {
		return err
	}
	url := strings.TrimRight(client.BaseURL, "/") + "/analytics/api/v2/sweep/experiments"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if client.BasicAuth != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(client.BasicAuth)))
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("publish POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("publish: %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if asJSON {
		_, _ = os.Stdout.Write(respBody)
		return nil
	}
	fmt.Printf("published %d experiments → %s\n", len(rows), url)
	return nil
}

// --- sweep reap -----------------------------------------------------------

func cmdSweepReap(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep reap", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	maxAgeMin := fs.Float64("max-age-min", 60, "minutes since claim before a running experiment is reaped")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	running, err := s.List(sweep.StatusRunning)
	if err != nil {
		return err
	}
	stale := sweep.ReapStale(running, nowUTC(), time.Duration(*maxAgeMin*float64(time.Minute)))
	reaped := make([]string, 0, len(stale))
	for _, e := range stale {
		sweep.Requeue(e)
		if err := s.Move(sweep.StatusRunning, sweep.StatusBacklog, e); err != nil {
			return err
		}
		reaped = append(reaped, e.ID)
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"reaped": reaped, "running": len(running)})
	}
	if len(reaped) == 0 {
		fmt.Printf("no stale claims (%d running)\n", len(running))
		return nil
	}
	fmt.Printf("reaped %d stale claim(s) → backlog: %s\n", len(reaped), strings.Join(reaped, ", "))
	return nil
}

// --- translators (sweep recipe → proxy types) -----------------------------

func toProxySliderShape(sh *sweep.Shape) *proxy.Shape {
	// Rate only — delay/loss are out of scope for the sweep (steady network
	// degradation isn't a realistic stream config nor an explicit error).
	// Pattern shapes are applied post-launch via `harness shape --pattern`.
	if sh.RateMbps == nil {
		return nil
	}
	v := float32(*sh.RateMbps)
	return &proxy.Shape{RateMbps: &v}
}

func toProxyTransferTimeouts(t *sweep.TransferTimeouts) *proxy.TransferTimeouts {
	out := &proxy.TransferTimeouts{}
	if t.ActiveSeconds > 0 {
		v := t.ActiveSeconds
		out.ActiveTimeoutSeconds = &v
	}
	if t.IdleSeconds > 0 {
		v := t.IdleSeconds
		out.IdleTimeoutSeconds = &v
	}
	if t.AppliesSegments {
		b := true
		out.AppliesSegments = &b
	}
	if t.AppliesManifests {
		b := true
		out.AppliesManifests = &b
	}
	if t.AppliesMaster {
		b := true
		out.AppliesMaster = &b
	}
	return out
}

// toProxyContent translates the sweep CM knobs. `allowedVariants` is the
// already-resolved keep-set (URIs or resolution heights) for the
// allowed_variants spec — resolved by the caller via resolveAllowedVariants,
// which needs network access to the master ladder, so it can't live in this
// pure translator.
func toProxyContent(cm *sweep.ContentManipulation, allowedVariants []string) (*proxy.ContentManipulation, error) {
	out := &proxy.ContentManipulation{}
	if cm.LiveOffset != nil {
		off := proxy.ContentManipulationLiveOffset(int(*cm.LiveOffset))
		if !off.Valid() {
			return nil, fmt.Errorf("live_offset %g is not a supported window (0|6|18|24)", *cm.LiveOffset)
		}
		out.LiveOffset = &off
	}
	if cm.VariantOrder != "" {
		vo := proxy.ContentManipulationVariantOrder(cm.VariantOrder)
		if !vo.Valid() {
			return nil, fmt.Errorf("variant_order %q invalid (ascending|descending|default|first_4mbps)", cm.VariantOrder)
		}
		out.VariantOrder = &vo
	}
	if len(allowedVariants) > 0 {
		vs := allowedVariants
		out.AllowedVariants = &vs
	}
	if cm.StripCodecs {
		b := true
		out.StripCodecs = &b
	}
	if cm.StripAvgBandwidth {
		b := true
		out.StripAverageBandwidth = &b
	}
	if cm.StripResolution {
		b := true
		out.StripResolution = &b
	}
	if cm.OverstateBandwidth != nil {
		b := true
		out.OverstateBandwidth = &b
	}
	return out, nil
}

func toProxyFaultRule(f *sweep.Fault) (proxy.FaultRule, error) {
	freq := nonZero(f.Frequency, 1)
	cons := nonZero(f.Consecutive, 1)
	rule := proxy.FaultRule{
		Type:        proxy.FaultRuleType(f.Type),
		Frequency:   &freq,
		Consecutive: &cons,
	}
	mode := f.Mode
	if mode == "" {
		mode = "requests"
	}
	m := proxy.FaultRuleMode(mode)
	rule.Mode = &m
	if filter, err := buildFilter(f.RequestKind, f.URLSubstr, ""); err != nil {
		return rule, err
	} else if filter != nil {
		rule.Filter = filter
	}
	return rule, nil
}

func sweepFaultDesc(f *sweep.Fault) string {
	if f.RequestKind != "" {
		return f.Type + "/" + f.RequestKind
	}
	return f.Type
}

func nonZero(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func verdictOf(e *sweep.Experiment) sweep.Verdict {
	if e.Result != nil {
		return e.Result.Verdict
	}
	return sweep.VerdictClean
}

// --- gh helpers -----------------------------------------------------------

// ghFindIssue returns the number of the first OPEN issue carrying the given
// signature label, or "" if none. Used for finding dedup (§4).
func ghFindIssue(sig string) (string, error) {
	out, err := ghOutput("issue", "list", "--label", sig, "--state", "open",
		"--json", "number", "--limit", "1")
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "[]" {
		return "", nil
	}
	var rows []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return "", fmt.Errorf("parse gh issue list: %w", err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	return fmt.Sprintf("%d", rows[0].Number), nil
}

func ghOutput(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func ghRun(args ...string) error {
	_, err := ghOutput(args...)
	return err
}
