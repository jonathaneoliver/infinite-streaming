package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

const sweepUsage = `harness sweep <subcommand>

The automated fault-injection sweep queue (issue #772, docs/sweep-design.md).
ClickHouse is the master store: every subcommand reads/writes the queue over the
forwarder API on the harness's --base deploy (no local files). Findings are
promoted to GitHub Issues elsewhere.

Subcommands:
  seed [--class C][--full][--contents a,b,c]
                           populate backlog/ with the starter set.
                           --class config (default: realistic stream/network)
                           or fault (explicit-error recovery). --full widens
                           past the narrow depth-first ipad-sim set. --contents
                           seeds the recipe set against each clip (default: the
                           .env CHAR_CONTENT single clip).
  add [flags]              enqueue ONE operator-authored experiment (kind=manual)
                           without running it — the runner picks it up and the
                           verdict lands in sweep_runs. Non-blocking authoring:
                           drop it and read 'sweep agenda' / 'query' later.
                           --class --platform --protocol --content --mode
                           --segment --duration-s --reps --why ; shape:
                           --rate-mbps --pattern --step-seconds --margin-pct ;
                           fault (class=fault): --fault-type --fault-kind
                           --fault-frequency --fault-mode .
  status                   counts per bucket (backlog/running/done/…)
  ls <status>              list experiments in a bucket (one line each)
  next [--claim --owner O] show the highest-score backlog experiment;
                           with --claim, atomically claim it for owner O
  bootstrap <id>           config-on-connect: materialise this recipe (shape/
                           content/labels/fault) onto a fresh proxy session
                           BEFORE the app launches, via proxy.cfg on the shaper
                           port. Prints the player_id to launch the app with
                           (-is.player_id). [--player UUID --group G --content C]
  apply <id> --target P    materialise a claimed experiment onto an ALREADY-live
                           player P: reset shape+faults, then apply this recipe's
                           shape/content/labels/fault (step 0+2 of the loop).
                           Pattern shapes need a fetched manifest → apply those
                           post-launch with 'harness shape --pattern'.
  analyze <id> --play UUID read the play's QoE labels, record the trichotomy
                           verdict, and move the file: clean→done, notable/
                           aberration→found. With --confirm-reps N, a first
                           single-rep hit instead enqueues N confirmation reps.
  promote <id>             open (or comment on) a deduped GitHub Issue for a
                           found experiment [--axis A --dry-run --from found].
  agenda [--max-age-min N] the next action for every actionable experiment,
                           derived purely from CH state (claim & run / analyze /
                           reap / isolate / promote / needs-human) — drive the
                           loop off this; resumes from the database alone.
  annotate <id> --note "…" record the interpretation (what happened / where /
                           how) onto Result.Note [--from found].
  reap [--max-age-min N]   return running experiments orphaned by a dead runner
                           to backlog (default 60; ~2× the longest expected run).
  isolate <id> --flip axis=value [--flip …]
                           materialise an OFAT isolation fan off a confirmed
                           hit (control + one variant per flip) into backlog/.
                           axes: platform protocol liveoffset ladder
                           variant_order strip_avg_bandwidth strip_codecs
                           strip_resolution overstate_bandwidth
  seed-from-triage [--days N] [--top N] [--min-skew X] [--dry-run]
                           read the observational triage signal (label
                           divergence) and seed config experiments for the
                           controllable dimension (test/platform/content) each
                           severe, dimension-driven label concentrates on;
                           skips observational/not-runnable dims with reasons.

Common flags (global, before the subcommand):
  --base URL               deploy whose ClickHouse holds the queue
  --depth-first[=false]    prefer non-seed work in 'next' peek (default true)
`

func cmdSweep(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(sweepUsage)
	}
	switch args[0] {
	case "seed":
		return cmdSweepSeed(client, args[1:], asJSON)
	case "add":
		return cmdSweepAdd(client, args[1:], asJSON)
	case "status":
		return cmdSweepStatus(client, args[1:], asJSON)
	case "ls":
		return cmdSweepLs(client, args[1:], asJSON)
	case "next":
		return cmdSweepNext(client, args[1:], asJSON)
	case "bootstrap":
		return cmdSweepBootstrap(client, args[1:], asJSON)
	case "apply":
		return cmdSweepApply(client, args[1:], asJSON)
	case "analyze", "analyse":
		return cmdSweepAnalyze(client, args[1:], asJSON)
	case "promote":
		return cmdSweepPromote(client, args[1:], asJSON)
	case "agenda":
		return cmdSweepAgenda(client, args[1:], asJSON)
	case "annotate":
		return cmdSweepAnnotate(client, args[1:], asJSON)
	case "reap":
		return cmdSweepReap(client, args[1:], asJSON)
	case "isolate":
		return cmdSweepIsolate(client, args[1:], asJSON)
	case "seed-from-triage":
		return cmdSweepSeedFromTriage(client, args[1:], asJSON)
	case "help", "--help", "-h":
		fmt.Fprint(os.Stdout, sweepUsage)
		return nil
	default:
		return fmt.Errorf("unknown sweep subcommand %q\n\n%s", args[0], sweepUsage)
	}
}

// openStore returns the ClickHouse-backed sweep queue (the master store, #772),
// reached over the forwarder API on the harness's configured deploy.
func openStore(client *api.Client) (*sweep.Store, error) {
	return sweep.OpenCH(client.BaseURL, client.HTTP, client.BasicAuth), nil
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func cmdSweepSeed(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep seed", flag.ContinueOnError)
	full := fs.Bool("full", false, "widen the seed across all platforms (default narrow depth-first)")
	class := fs.String("class", "config", "sweep class: config (realistic stream/network) | fault (error-recovery)")
	platform := fs.String("platform", "", "seed only this platform (e.g. androidtv); overrides --full/narrow")
	contents := fs.String("contents", "", "comma-separated clips to seed the recipe set against (default: the .env CHAR_CONTENT single clip)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := sweep.Class(*class)
	if c != sweep.ClassConfig && c != sweep.ClassFault {
		return fmt.Errorf("invalid --class %q: config|fault", *class)
	}
	var clips []string
	for _, p := range strings.Split(*contents, ",") {
		if p = strings.TrimSpace(p); p != "" {
			clips = append(clips, p)
		}
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}
	var platformsOverride []string
	if *platform != "" {
		platformsOverride = []string{*platform}
	}
	exps := sweep.SeedContents(c, *full, nowUTC(), clips, platformsOverride...)
	for _, e := range exps {
		if err := s.Save(sweep.StatusBacklog, e); err != nil {
			return err
		}
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"seeded": len(exps), "full": *full, "class": *class, "contents": clips})
	}
	mode := "narrow (depth-first, ipad-sim)"
	if *full {
		mode = "full (all platforms)"
	}
	if *platform != "" {
		mode = "platform=" + *platform
	}
	clipNote := "default clip"
	if len(clips) > 0 {
		clipNote = fmt.Sprintf("%d clip(s): %s", len(clips), strings.Join(clips, ", "))
	}
	fmt.Printf("seeded %d %s-class experiments into backlog (%s) — %s, %s\n", len(exps), c, s.Label(), mode, clipNote)
	return nil
}

// cmdSweepAdd enqueues ONE operator-authored experiment (kind=manual) onto the
// backlog without running it — the producer half of the producer/consumer loop.
// Authoring a test becomes a non-blocking drop: the runner (qe-offhours.sh)
// claims it, and the verdict lands in sweep_runs for later reading. Mirrors the
// Experiment construction in internal/sweep/seed.go so an ad-hoc item schedules
// consistently with seeded ones (same scoring, same launch mode).
func cmdSweepAdd(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep add", flag.ContinueOnError)
	class := fs.String("class", "config", "sweep class: config | fault")
	platform := fs.String("platform", "ipad-sim", "ipad-sim | iphone | appletv | androidtv | web")
	protocol := fs.String("protocol", "hls", "hls | dash")
	content := fs.String("content", "", "catalogue clip (default: .env CHAR_CONTENT); verify live via /api/content first")
	mode := fs.String("mode", "steps", "playback motion: steps | pyramid | rampup | rampdown | downshift_severity | transient_shock | startup | abort | …")
	segment := fs.String("segment", "", "master variant the probe requests: s2 | s6 | ll (empty = app default s6)")
	durationS := fs.Int("duration-s", 0, "per-run window in seconds (0 = runner/probe default)")
	reps := fs.Int("reps", 1, "confirmation reps requested")
	why := fs.String("why", "", "rationale recorded on the row (why this test)")
	id := fs.String("id", "", "explicit experiment id (default: auto manual-… with a unique stamp)")
	// shape (config-class network motion)
	rate := fs.Float64("rate-mbps", 0, "static bandwidth cap in Mbps (0 = no static cap)")
	pattern := fs.String("pattern", "", "ladder-derived pattern: pyramid | valley | ramp_up | ramp_down | square_wave | transient_shock")
	stepSeconds := fs.Int("step-seconds", 0, "pattern step length (6|12|18|24|60|120)")
	marginPct := fs.Int("margin-pct", 0, "pattern headroom margin %")
	// fault (fault-class only)
	faultType := fs.String("fault-type", "", "500 | timeout | corrupted | connection_refused | …")
	faultKind := fs.String("fault-kind", "", "request kind to fault: segment | manifest | master_manifest | init | audio_segment")
	faultFreq := fs.Int("fault-frequency", 0, "fault cadence count")
	faultMode := fs.String("fault-mode", "", "requests | seconds | failures_per_seconds | failures_per_packets")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c := sweep.Class(*class)
	if c != sweep.ClassConfig && c != sweep.ClassFault {
		return fmt.Errorf("invalid --class %q: config|fault", *class)
	}
	clip := sweep.ContentOrDefault(*content)

	e := &sweep.Experiment{
		CreatedAt:  nowUTC(),
		Class:      c,
		Platform:   *platform,
		LaunchMode: sweep.LaunchModeAppium, // the only mode TestSweepProbe supports
		Protocol:   *protocol,
		Content:    clip,
		Segment:    *segment,
		Mode:       *mode,
		DurationS:  *durationS,
		Kind:       sweep.KindManual,
		Reps:       *reps,
		Depth:      0,
		Why:        "manual_add",
		WhyText:    *why,
	}
	if strings.TrimSpace(*why) == "" {
		e.WhyText = fmt.Sprintf("operator-authored %s-class probe: %s on %s/%s", c, *mode, *platform, *protocol)
	}

	// Shape: a static cap and/or a ladder-derived pattern (config-class motion).
	if *rate > 0 || *pattern != "" {
		sh := &sweep.Shape{Pattern: *pattern, StepSeconds: *stepSeconds, MarginPct: *marginPct}
		if *rate > 0 {
			r := *rate
			sh.RateMbps = &r
		}
		e.Shape = sh
	}

	// Fault: fault-class only; building one on a config-class item is a mistake.
	if *faultType != "" {
		if c != sweep.ClassFault {
			return fmt.Errorf("--fault-type set but --class is %q; faults are fault-class only", c)
		}
		e.Fault = &sweep.Fault{
			Type:        *faultType,
			RequestKind: *faultKind,
			Frequency:   *faultFreq,
			Mode:        *faultMode,
		}
	}

	if *id != "" {
		e.ID = *id
	} else {
		// UnixNano keeps repeated adds in the same second distinct (the queue
		// collapses by exp_id, so a stable id would overwrite the prior add).
		e.ID = fmt.Sprintf("manual-%s-%s-%s-%s-%d", c, *platform, *protocol, *mode, time.Now().UnixNano())
	}
	e.Score = sweep.DefaultWeights().Score(e)

	s, err := openStore(client)
	if err != nil {
		return err
	}
	if err := s.Save(sweep.StatusBacklog, e); err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(e)
	}
	fmt.Printf("enqueued %s (kind=manual, score=%.1f) → backlog\n", e.ID, e.Score)
	fmt.Printf("  %s-class %s on %s/%s · content=%s%s\n", c, *mode, *platform, *protocol, clip,
		func() string {
			if *segment != "" {
				return " · segment=" + *segment
			}
			return ""
		}())
	fmt.Println("  the runner will claim it; read the verdict later via 'harness sweep agenda' / 'harness sweep ls done'")
	return nil
}

// cmdSweepAnnotate records the LLM's interpretation (what happened / where /
// how) onto an experiment's Result.Note — the structured "why it concluded
// that", so the row is self-explanatory from CH alone (vs the mechanical
// analyze note). Written during the loop's investigate step on a hit.
func cmdSweepAnnotate(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(`usage: harness sweep annotate <experiment-id> --note "what happened / where / how" [--from found]`)
	}
	id := args[0]
	fs := flag.NewFlagSet("sweep annotate", flag.ContinueOnError)
	from := fs.String("from", "found", "bucket holding the experiment")
	note := fs.String("note", "", "the interpretation (what happened / where / how) — required")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*note) == "" {
		return errors.New("--note is required")
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}
	e, err := s.Load(sweep.Status(*from), id)
	if err != nil {
		return fmt.Errorf("load %s/%s: %w", *from, id, err)
	}
	if e.Result == nil {
		e.Result = &sweep.Result{}
	}
	e.Result.Note = *note
	if err := s.Save(sweep.Status(*from), e); err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"experiment": id, "note": *note})
	}
	fmt.Printf("annotated %s: %s\n", id, *note)
	return nil
}

// cmdSweepAgenda prints the next action for every actionable experiment, derived
// purely from CH state — so a fresh runner knows what to do and can resume the
// loop from the database alone.
func cmdSweepAgenda(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep agenda", flag.ContinueOnError)
	maxAgeMin := fs.Float64("max-age-min", 60, "minutes since claim before a running experiment is 'stale'")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}
	all, err := s.List("") // every status
	if err != nil {
		return err
	}
	steps := sweep.Agenda(all, nowUTC(), time.Duration(*maxAgeMin*float64(time.Minute)))
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(steps)
	}
	if len(steps) == 0 {
		fmt.Println("agenda empty — nothing to do (backlog drained, no open hits)")
		return nil
	}
	for _, st := range steps {
		fmt.Printf("  %-9s %-44s %-18s %s\n", st.Status, st.ID, st.Action, st.Reason)
	}
	return nil
}

func cmdSweepStatus(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}
	counts, err := s.Counts()
	if err != nil {
		return err
	}
	if asJSON {
		m := make(map[string]int, len(counts))
		for k, v := range counts {
			m[string(k)] = v
		}
		return json.NewEncoder(os.Stdout).Encode(m)
	}
	for _, st := range sweep.AllStatuses {
		fmt.Printf("  %-9s %d\n", st, counts[st])
	}
	return nil
}

func cmdSweepLs(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: harness sweep ls <status>")
	}
	status := args[0]
	fs := flag.NewFlagSet("sweep ls", flag.ContinueOnError)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}
	exps, err := s.List(sweep.Status(status))
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(exps)
	}
	for _, e := range exps {
		verdict := ""
		if e.Result != nil {
			verdict = string(e.Result.Verdict)
		}
		fmt.Printf("  %-36s %-10s %-9s %-5s %-18s score=%6.1f %s\n",
			e.ID, e.Kind, e.Platform, e.Protocol, e.Mode, e.Score, verdict)
	}
	return nil
}

// flipList collects repeatable --flip axis=value pairs.
type flipList []sweep.Flip

func (f *flipList) String() string { return fmt.Sprintf("%d flips", len(*f)) }
func (f *flipList) Set(v string) error {
	i := strings.IndexByte(v, '=')
	if i < 0 {
		return fmt.Errorf("flip %q must be axis=value", v)
	}
	*f = append(*f, sweep.Flip{Axis: sweep.Axis(v[:i]), Value: v[i+1:]})
	return nil
}

func cmdSweepIsolate(client *api.Client, args []string, asJSON bool) error {
	// The experiment id is the first positional; Go's flag package stops at
	// the first non-flag token, so pull it out before parsing the flags.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: harness sweep isolate <experiment-id> --flip axis=value …")
	}
	id := args[0]
	fs := flag.NewFlagSet("sweep isolate", flag.ContinueOnError)
	from := fs.String("from", "found", "bucket holding the parent experiment")
	var flips flipList
	fs.Var(&flips, "flip", "axis=value (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}
	parent, err := s.Load(sweep.Status(*from), id)
	if err != nil {
		return fmt.Errorf("load parent %s/%s: %w", *from, id, err)
	}
	fan, err := sweep.IsolationFan(parent, flips, nowUTC())
	if err != nil {
		return err
	}
	for _, e := range fan {
		if err := s.Save(sweep.StatusBacklog, e); err != nil {
			return err
		}
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(fan)
	}
	fmt.Printf("inserted isolation fan for %s: %d experiments (control + %d variants) → backlog\n",
		parent.ID, len(fan), len(fan)-1)
	for _, e := range fan {
		fmt.Printf("  %-40s arm=%-7s group=%s\n", e.ID, e.Arm, e.Group)
	}
	return nil
}

func cmdSweepNext(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep next", flag.ContinueOnError)
	depthFirst := fs.Bool("depth-first", true, "prefer non-seed work (peek only; --claim uses server score order)")
	claim := fs.Bool("claim", false, "atomically claim the top eligible experiment (server-side)")
	owner := fs.String("owner", "", "owner id to stamp on claim")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}

	// --claim delegates the pick+claim to the server (concurrency-safe + scope-
	// gated). Without --claim we peek: list the backlog and run the local
	// scheduler so the operator sees what would run next.
	var pick *sweep.Experiment
	if *claim {
		if *owner == "" {
			return errors.New("--claim requires --owner")
		}
		if pick, err = s.ClaimNext(*owner); err != nil {
			return fmt.Errorf("claim: %w", err)
		}
	} else {
		backlog, err := s.List(sweep.StatusBacklog)
		if err != nil {
			return err
		}
		pick = sweep.DefaultWeights().SelectNext(backlog, *depthFirst)
	}
	if pick == nil {
		if asJSON {
			fmt.Println("null")
		} else if *claim {
			fmt.Println("nothing to claim (backlog empty or scope-gated)")
		} else {
			fmt.Println("backlog empty")
		}
		return nil
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(pick)
	}
	fmt.Printf("%s  kind=%s platform=%s protocol=%s mode=%s score=%.1f\n",
		pick.ID, pick.Kind, pick.Platform, pick.Protocol, pick.Mode, pick.Score)
	if *claim {
		fmt.Printf("claimed by %s → running\n", pick.Owner)
	}
	return nil
}
