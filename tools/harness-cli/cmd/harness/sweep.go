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
Experiments live as JSON files under .sweep/<status>/ (override with --root or
$SWEEP_ROOT). Findings are promoted to GitHub Issues elsewhere.

Subcommands:
  seed [--class C][--full] populate backlog/ with the starter set.
                           --class config (default: realistic stream/network)
                           or fault (explicit-error recovery). --full widens
                           past the narrow depth-first ipad-sim set.
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
  publish                  snapshot the whole queue → the forwarder so the
                           dashboard Sweep tab can show it. Call after each
                           iteration to keep the tab live.
  reap [--max-age-min N]   return running/ files orphaned by a dead runner to
                           backlog (default 60; ~2× the longest expected run).
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

Common flags:
  --root DIR               sweep root (default $SWEEP_ROOT or .sweep)
  --depth-first[=false]    prefer non-seed work in 'next' (default true)
`

func cmdSweep(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(sweepUsage)
	}
	switch args[0] {
	case "seed":
		return cmdSweepSeed(args[1:], asJSON)
	case "status":
		return cmdSweepStatus(args[1:], asJSON)
	case "ls":
		return cmdSweepLs(args[1:], asJSON)
	case "next":
		return cmdSweepNext(args[1:], asJSON)
	case "bootstrap":
		return cmdSweepBootstrap(client, args[1:], asJSON)
	case "apply":
		return cmdSweepApply(client, args[1:], asJSON)
	case "analyze", "analyse":
		return cmdSweepAnalyze(client, args[1:], asJSON)
	case "promote":
		return cmdSweepPromote(client, args[1:], asJSON)
	case "publish":
		return cmdSweepPublish(client, args[1:], asJSON)
	case "reap":
		return cmdSweepReap(args[1:], asJSON)
	case "isolate":
		return cmdSweepIsolate(args[1:], asJSON)
	case "seed-from-triage":
		return cmdSweepSeedFromTriage(client, args[1:], asJSON)
	case "help", "--help", "-h":
		fmt.Fprint(os.Stdout, sweepUsage)
		return nil
	default:
		return fmt.Errorf("unknown sweep subcommand %q\n\n%s", args[0], sweepUsage)
	}
}

func openStore(root string) (*sweep.Store, error) {
	if root == "" {
		root = sweep.DefaultRoot()
	}
	return sweep.Open(root)
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func cmdSweepSeed(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep seed", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	full := fs.Bool("full", false, "widen the seed across all platforms (default narrow depth-first)")
	class := fs.String("class", "config", "sweep class: config (realistic stream/network) | fault (error-recovery)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := sweep.Class(*class)
	if c != sweep.ClassConfig && c != sweep.ClassFault {
		return fmt.Errorf("invalid --class %q: config|fault", *class)
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	exps := sweep.Seed(c, *full, nowUTC())
	for _, e := range exps {
		if err := s.Save(sweep.StatusBacklog, e); err != nil {
			return err
		}
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"seeded": len(exps), "full": *full, "class": *class})
	}
	mode := "narrow (depth-first, ipad-sim)"
	if *full {
		mode = "full (all platforms)"
	}
	fmt.Printf("seeded %d %s-class experiments into %s/backlog — %s\n", len(exps), c, s.Root, mode)
	return nil
}

func cmdSweepStatus(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep status", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openStore(*root)
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

func cmdSweepLs(args []string, asJSON bool) error {
	// status is the leading positional; pull it before parsing flags so
	// `ls found --root X` works (Go's flag package stops at the positional).
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: harness sweep ls <status> [--root DIR]")
	}
	status := args[0]
	fs := flag.NewFlagSet("sweep ls", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := openStore(*root)
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

func cmdSweepIsolate(args []string, asJSON bool) error {
	// The experiment id is the first positional; Go's flag package stops at
	// the first non-flag token, so pull it out before parsing the flags.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: harness sweep isolate <experiment-id> --flip axis=value …")
	}
	id := args[0]
	fs := flag.NewFlagSet("sweep isolate", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	from := fs.String("from", "found", "bucket holding the parent experiment")
	var flips flipList
	fs.Var(&flips, "flip", "axis=value (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := openStore(*root)
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

func cmdSweepNext(args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep next", flag.ContinueOnError)
	root := fs.String("root", "", "sweep root dir")
	depthFirst := fs.Bool("depth-first", true, "prefer non-seed work")
	claim := fs.Bool("claim", false, "atomically claim the selected experiment")
	owner := fs.String("owner", "", "owner id to stamp on claim")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openStore(*root)
	if err != nil {
		return err
	}
	backlog, err := s.List(sweep.StatusBacklog)
	if err != nil {
		return err
	}
	w := sweep.DefaultWeights()
	pick := w.SelectNext(backlog, *depthFirst)
	if pick == nil {
		if asJSON {
			fmt.Println("null")
		} else {
			fmt.Println("backlog empty")
		}
		return nil
	}
	if *claim {
		if *owner == "" {
			return errors.New("--claim requires --owner")
		}
		claimed, err := s.Claim(pick.ID, *owner, nowUTC())
		if err != nil {
			return fmt.Errorf("claim %s: %w", pick.ID, err)
		}
		pick = claimed
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(pick)
	}
	fmt.Printf("%s  kind=%s platform=%s protocol=%s mode=%s score=%.1f\n",
		pick.ID, pick.Kind, pick.Platform, pick.Protocol, pick.Mode, pick.Score)
	if *claim {
		fmt.Printf("claimed by %s → running/\n", pick.Owner)
	}
	return nil
}
