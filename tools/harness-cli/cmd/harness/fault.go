package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const faultUsage = `harness fault <subcommand>

Subcommands:
  list <target>                       show current fault_rules
  add  <target> [flags]               POST a new fault rule
  edit <target> <rule_id> [flags]     PATCH one rule's fields
  rm   <target> <rule_id>             DELETE one rule by id (short id ok)
  clear <target>                      PATCH fault_rules → []

add flags:
  --type TYPE      fault type (required) — 403/404/500/503,
                   connection_refused, dns_failure, rate_limiting,
                   corrupted, request_body_hang|reset|delayed,
                   request_connect_delayed, none
  --kind KINDS     comma-separated request_kind filter
                   (segment, partial, manifest, master_manifest,
                   init, audio_segment, audio_manifest)
  --url-substr S   match URLs containing S; comma-separated for multi-pattern
                   (e.g. --url-substr 2160p,1440p,1080p)
  --url-regex R    match URLs matching R (regex mode)
  --frequency N    cadence numerator (default 1); 0 = one-shot burst
  --mode MODE      requests | seconds | failures_per_seconds
                   (default requests)
  --consecutive N  consecutive-failures count (default 1)
  --continuous     fault EVERY matching request until cleared
                   (shorthand for --frequency 0 --consecutive 0)
  --id ID          override server-generated rule_id (uuid)

variant-scope flags (match by manifest-declared properties; all AND
together, and never match audio/init/master requests):
  --resolution WxH   comma-separated resolutions, e.g. 3840x2160,2560x1440
  --rung-index N     comma-separated rung indexes (0 = lowest bitrate)
  --rung-position P  comma-separated symbolic positions, evaluated against
                     the current ladder: top, second_from_top,
                     bottom, second_from_bottom
  --bandwidth-above BPS  match variants whose BANDWIDTH > BPS
  --bandwidth-below BPS  match variants whose BANDWIDTH < BPS
  --codec-prefix S       match variants whose CODECS starts with S

<target> may be a full UUID, a >=6-char hex prefix, a label value
(device/name), a player IP, or a substring of the User-Agent.
`

func cmdFault(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(faultUsage)
	}
	switch args[0] {
	case "list":
		return cmdFaultList(client, args[1:], asJSON)
	case "add":
		return cmdFaultAdd(client, args[1:], asJSON)
	case "rm", "remove", "delete":
		return cmdFaultRm(client, args[1:], asJSON)
	case "edit":
		return cmdFaultEdit(client, args[1:], asJSON)
	case "clear":
		return cmdFaultClear(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown fault subcommand: %s\n\n%s", args[0], faultUsage)
	}
}

func cmdFaultList(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness fault list <target>")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	rec, _, err := client.Player(ctx, pid)
	if err != nil {
		return err
	}
	rules := []proxy.FaultRule{}
	if rec.FaultRules != nil {
		rules = *rec.FaultRules
	}
	if asJSON {
		return format.JSON(os.Stdout, rules)
	}
	if len(rules) == 0 {
		fmt.Println("no fault rules")
		return nil
	}
	for i, r := range rules {
		id := ""
		if r.Id != nil {
			id = *r.Id
		}
		kind := "*"
		if r.Filter != nil && r.Filter.RequestKind != nil {
			parts := make([]string, 0, len(*r.Filter.RequestKind))
			for _, k := range *r.Filter.RequestKind {
				parts = append(parts, string(k))
			}
			kind = strings.Join(parts, ",")
		}
		url := ""
		if r.Filter != nil && r.Filter.UrlMatch != nil {
			url = fmt.Sprintf(" url[%s]=%s", r.Filter.UrlMatch.Mode, strings.Join(r.Filter.UrlMatch.Patterns, "|"))
		}
		variant := ""
		if r.Filter != nil && r.Filter.Variant != nil {
			variant = " variant[" + variantSummary(r.Filter.Variant) + "]"
		}
		freq, mode, cons := 1, "requests", 1
		if r.Frequency != nil {
			freq = *r.Frequency
		}
		if r.Mode != nil {
			mode = string(*r.Mode)
		}
		if r.Consecutive != nil {
			cons = *r.Consecutive
		}
		cadence := fmt.Sprintf("freq=%d/%s consec=%d", freq, mode, cons)
		if freq == 0 && cons == 0 {
			cadence = "continuous"
		}
		fmt.Printf("%d. %-12s type=%-10s kind=%-10s %s%s%s\n",
			i+1, shortRuleID(id), string(r.Type), kind, cadence, variant, url)
	}
	return nil
}

func cmdFaultAdd(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness fault add <target> [flags]\n\n" + faultUsage)
	}
	fs := flag.NewFlagSet("fault add", flag.ContinueOnError)
	typ := fs.String("type", "", "fault type (required)")
	kindCSV := fs.String("kind", "", "comma-separated request_kind filter")
	urlSubstr := fs.String("url-substr", "", "URL substring match")
	urlRegex := fs.String("url-regex", "", "URL regex match")
	freq := fs.Int("frequency", 1, "cadence numerator")
	mode := fs.String("mode", "requests", "requests|seconds|failures_per_seconds")
	cons := fs.Int("consecutive", 1, "consecutive failures")
	continuous := fs.Bool("continuous", false, "fault every matching request until cleared (frequency=0 consecutive=0)")
	resolution := fs.String("resolution", "", "comma-separated variant resolutions (WxH)")
	rungIndex := fs.String("rung-index", "", "comma-separated rung indexes (0 = lowest bitrate)")
	rungPosition := fs.String("rung-position", "", "comma-separated symbolic rung positions")
	bwAbove := fs.Int("bandwidth-above", -1, "match variants with BANDWIDTH above this (bps)")
	bwBelow := fs.Int("bandwidth-below", -1, "match variants with BANDWIDTH below this (bps)")
	codecPrefix := fs.String("codec-prefix", "", "match variants whose CODECS starts with this")
	ruleID := fs.String("id", "", "explicit rule_id (uuid)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *typ == "" {
		return errors.New("--type is required")
	}
	// --continuous is shorthand for (frequency=0, consecutive=0). Reject
	// combining it with an explicit --frequency/--consecutive so the
	// operator can't quietly get a cadence they didn't mean.
	if *continuous {
		if flagSet(fs, "frequency") || flagSet(fs, "consecutive") {
			return errors.New("--continuous cannot be combined with --frequency/--consecutive")
		}
		*freq, *cons = 0, 0
	}

	rule := proxy.FaultRule{
		Type:        proxy.FaultRuleType(*typ),
		Frequency:   freq,
		Consecutive: cons,
	}
	m := proxy.FaultRuleMode(*mode)
	rule.Mode = &m
	if *ruleID != "" {
		rule.Id = ruleID
	}
	// Build (and validate) the filter before touching the network, so a
	// typo in --kind/--rung-position/etc. fails fast without a round-trip.
	filter, err := buildFilter(faultFilterInput{
		kindCSV:        *kindCSV,
		urlSubstr:      *urlSubstr,
		urlRegex:       *urlRegex,
		resolutions:    *resolution,
		rungIndexes:    *rungIndex,
		rungPositions:  *rungPosition,
		bandwidthAbove: *bwAbove,
		bandwidthBelow: *bwBelow,
		codecPrefix:    *codecPrefix,
	})
	if err != nil {
		return err
	}
	rule.Filter = filter

	target := args[0]
	ctx := context.Background()
	pid, err := client.Resolve(ctx, target)
	if err != nil {
		return err
	}

	action := fmt.Sprintf("fault add type=%s", *typ)
	if *kindCSV != "" {
		action += " kind=" + *kindCSV
	}
	if *continuous {
		action += " continuous"
	}
	if rule.Filter != nil && rule.Filter.Variant != nil {
		action += " variant[" + variantSummary(rule.Filter.Variant) + "]"
	}
	newETag, err := client.AddFaultRule(ctx, pid, action, rule)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid,
			"rule":      rule,
			"etag":      newETag,
		})
	}
	id := "(server-assigned)"
	if rule.Id != nil {
		id = *rule.Id
	}
	fmt.Printf("added rule %s on %s (etag %s)\n", id, pid, shortRev(newETag))
	return nil
}

func cmdFaultRm(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 2 {
		return errors.New("usage: harness fault rm <target> <rule_id>")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	ruleID := args[1]
	// If the user gave a short prefix, resolve to a full rule_id by
	// reading the current rule set. Saves them from copy-pasting full
	// UUIDs out of `fault list`.
	if len(ruleID) < 32 {
		rec, _, err := client.Player(ctx, pid)
		if err != nil {
			return err
		}
		full, err := matchRuleID(ruleID, rec.FaultRules)
		if err != nil {
			return err
		}
		ruleID = full
	}
	action := "fault rm " + shortRuleID(ruleID)
	newETag, err := client.DeleteFaultRule(ctx, pid, ruleID, action)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid,
			"rule_id":   ruleID,
			"etag":      newETag,
		})
	}
	fmt.Printf("removed rule %s from %s (etag %s)\n", shortRuleID(ruleID), pid, shortRev(newETag))
	return nil
}

func cmdFaultEdit(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 2 {
		return errors.New("usage: harness fault edit <target> <rule_id> [flags]")
	}
	fs := flag.NewFlagSet("fault edit", flag.ContinueOnError)
	typ := fs.String("type", "", "change fault type")
	freq := fs.Int("frequency", -1, "change cadence numerator")
	mode := fs.String("mode", "", "change mode (requests|seconds|failures_per_seconds)")
	cons := fs.Int("consecutive", -1, "change consecutive count")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	ruleID := args[1]
	if len(ruleID) < 32 {
		rec, _, err := client.Player(ctx, pid)
		if err != nil {
			return err
		}
		full, err := matchRuleID(ruleID, rec.FaultRules)
		if err != nil {
			return err
		}
		ruleID = full
	}
	// Merge-patch + the generated FaultRule type don't co-operate:
	// FaultRule.Type has no `omitempty`, so the typed marshal would
	// send `"type":""` for partial PATCHes that don't touch type, and
	// the server rejects empty type strings with a 501. Build the
	// patch body by hand and ship via PatchRaw — but call through the
	// EditFaultRule path so snapshot is captured.
	patchMap := map[string]any{}
	if *typ != "" {
		patchMap["type"] = *typ
	}
	if *freq >= 0 {
		patchMap["frequency"] = *freq
	}
	if *cons >= 0 {
		patchMap["consecutive"] = *cons
	}
	if *mode != "" {
		patchMap["mode"] = *mode
	}
	if len(patchMap) == 0 {
		return errors.New("nothing to patch — pass --type/--frequency/--mode/--consecutive")
	}
	action := fmt.Sprintf("fault edit %s", shortRuleID(ruleID))
	newETag, err := client.EditFaultRuleRaw(ctx, pid, ruleID, action, patchMap)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid, "rule_id": ruleID, "patch": patchMap, "etag": newETag,
		})
	}
	fmt.Printf("patched rule %s on %s (etag %s)\n", shortRuleID(ruleID), pid, shortRev(newETag))
	return nil
}

func cmdFaultClear(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness fault clear <target>")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	newETag, err := client.ClearFaultRules(ctx, pid, "fault clear")
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player_id": pid, "etag": newETag})
	}
	fmt.Printf("cleared fault_rules on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}

// faultFilterInput collects every CLI flag that contributes to a
// FaultFilter, so buildFilter has one testable signature rather than a
// growing positional argument list. Unset numeric variant bounds are
// signalled by -1.
type faultFilterInput struct {
	kindCSV        string
	urlSubstr      string
	urlRegex       string
	resolutions    string // csv "WxH,WxH"
	rungIndexes    string // csv ints
	rungPositions  string // csv symbolic positions
	bandwidthAbove int    // -1 = unset
	bandwidthBelow int    // -1 = unset
	codecPrefix    string
}

func (in faultFilterInput) empty() bool {
	return in.kindCSV == "" && in.urlSubstr == "" && in.urlRegex == "" &&
		in.resolutions == "" && in.rungIndexes == "" && in.rungPositions == "" &&
		in.bandwidthAbove < 0 && in.bandwidthBelow < 0 && in.codecPrefix == ""
}

func buildFilter(in faultFilterInput) (*proxy.FaultFilter, error) {
	if in.empty() {
		return nil, nil
	}
	f := &proxy.FaultFilter{}
	if in.kindCSV != "" {
		parts := strings.Split(in.kindCSV, ",")
		kinds := make([]proxy.FaultFilterRequestKind, 0, len(parts))
		for _, p := range parts {
			k := proxy.FaultFilterRequestKind(strings.TrimSpace(p))
			if !k.Valid() {
				return nil, fmt.Errorf("invalid --kind %q", p)
			}
			kinds = append(kinds, k)
		}
		f.RequestKind = &kinds
	}
	if in.urlSubstr != "" && in.urlRegex != "" {
		return nil, errors.New("--url-substr and --url-regex are mutually exclusive")
	}
	if in.urlSubstr != "" {
		// Comma-separated → multi-pattern. The matcher OR's the list,
		// so a single rule can scope to e.g. "2160p,1440p,1080p" and
		// fault every video variant without touching audio.
		patterns := splitTrimNonEmpty(in.urlSubstr)
		if len(patterns) > 0 {
			f.UrlMatch = &proxy.UrlMatch{Mode: proxy.Substring, Patterns: patterns}
		}
	}
	if in.urlRegex != "" {
		f.UrlMatch = &proxy.UrlMatch{Mode: proxy.Regex, Patterns: []string{in.urlRegex}}
	}
	variant, err := buildVariant(in)
	if err != nil {
		return nil, err
	}
	f.Variant = variant
	return f, nil
}

// buildVariant assembles a VariantPredicate from the variant-scope flags,
// or returns nil when none are set. The server rejects an empty
// `variant: {}`, so a nil (absent) predicate is the correct "no variant
// narrowing" signal — never an empty struct.
func buildVariant(in faultFilterInput) (*proxy.VariantPredicate, error) {
	v := &proxy.VariantPredicate{}
	set := false
	if in.resolutions != "" {
		res := splitTrimNonEmpty(in.resolutions)
		if len(res) > 0 {
			v.Resolutions = &res
			set = true
		}
	}
	if in.rungIndexes != "" {
		idxs, err := parseCSVInts(in.rungIndexes)
		if err != nil {
			return nil, fmt.Errorf("--rung-index: %w", err)
		}
		if len(idxs) > 0 {
			v.RungIndexes = &idxs
			set = true
		}
	}
	if in.rungPositions != "" {
		positions := make([]proxy.VariantPredicateRungPositions, 0)
		for _, p := range splitTrimNonEmpty(in.rungPositions) {
			pos := proxy.VariantPredicateRungPositions(p)
			if !pos.Valid() {
				return nil, fmt.Errorf("invalid --rung-position %q (want top|second_from_top|bottom|second_from_bottom)", p)
			}
			positions = append(positions, pos)
		}
		if len(positions) > 0 {
			v.RungPositions = &positions
			set = true
		}
	}
	if in.bandwidthAbove >= 0 {
		bw := in.bandwidthAbove
		v.BandwidthAbove = &bw
		set = true
	}
	if in.bandwidthBelow >= 0 {
		bw := in.bandwidthBelow
		v.BandwidthBelow = &bw
		set = true
	}
	if in.codecPrefix != "" {
		cp := in.codecPrefix
		v.CodecPrefix = &cp
		set = true
	}
	if !set {
		return nil, nil
	}
	return v, nil
}

// parseCSVInts parses a comma-separated list of integers, ignoring blank
// entries. Used by --rung-index.
func parseCSVInts(s string) ([]int, error) {
	out := []int{}
	for _, p := range splitTrimNonEmpty(s) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", p)
		}
		out = append(out, n)
	}
	return out, nil
}

// variantSummary renders a VariantPredicate compactly for `fault list`,
// showing only the sub-predicates that are set.
func variantSummary(v *proxy.VariantPredicate) string {
	parts := []string{}
	if v.Resolutions != nil && len(*v.Resolutions) > 0 {
		parts = append(parts, "res="+strings.Join(*v.Resolutions, "|"))
	}
	if v.RungIndexes != nil && len(*v.RungIndexes) > 0 {
		ss := make([]string, 0, len(*v.RungIndexes))
		for _, n := range *v.RungIndexes {
			ss = append(ss, strconv.Itoa(n))
		}
		parts = append(parts, "rung="+strings.Join(ss, "|"))
	}
	if v.RungPositions != nil && len(*v.RungPositions) > 0 {
		ss := make([]string, 0, len(*v.RungPositions))
		for _, p := range *v.RungPositions {
			ss = append(ss, string(p))
		}
		parts = append(parts, "pos="+strings.Join(ss, "|"))
	}
	if v.BandwidthAbove != nil {
		parts = append(parts, "bw>"+strconv.Itoa(*v.BandwidthAbove))
	}
	if v.BandwidthBelow != nil {
		parts = append(parts, "bw<"+strconv.Itoa(*v.BandwidthBelow))
	}
	if v.CodecPrefix != nil && *v.CodecPrefix != "" {
		parts = append(parts, "codec="+*v.CodecPrefix)
	}
	return strings.Join(parts, ",")
}

// flagSet reports whether the named flag was explicitly passed on the
// command line (as opposed to sitting at its default).
func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// splitTrimNonEmpty parses a comma-separated string into its trimmed
// non-empty entries. Used by --url-substr to accept multi-pattern
// scoping in a single rule.
func splitTrimNonEmpty(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func matchRuleID(prefix string, rules *[]proxy.FaultRule) (string, error) {
	if rules == nil || len(*rules) == 0 {
		return "", fmt.Errorf("no rules to match %q against", prefix)
	}
	lower := strings.ToLower(prefix)
	var hits []string
	for _, r := range *rules {
		if r.Id == nil {
			continue
		}
		id := *r.Id
		compact := strings.ReplaceAll(strings.ToLower(id), "-", "")
		if strings.HasPrefix(compact, lower) || strings.HasPrefix(strings.ToLower(id), lower) {
			hits = append(hits, id)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no rule matches %q", prefix)
	case 1:
		return hits[0], nil
	default:
		return "", fmt.Errorf("rule prefix %q is ambiguous (%d matches)", prefix, len(hits))
	}
}

func shortRuleID(id string) string {
	c := strings.ReplaceAll(id, "-", "")
	if len(c) > 8 {
		return c[:8]
	}
	if c == "" {
		return "—"
	}
	return c
}
