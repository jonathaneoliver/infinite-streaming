package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const faultUsage = `harness fault <subcommand>

Subcommands:
  list <target>                       show current fault_rules
  add  <target> [flags]               POST a new fault rule
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
  --url-substr S   match URLs containing S (substring mode)
  --url-regex R    match URLs matching R (regex mode)
  --frequency N    cadence numerator (default 1)
  --mode MODE      requests | seconds | failures_per_seconds
                   (default requests)
  --consecutive N  consecutive-failures count (default 1)
  --id ID          override server-generated rule_id (uuid)

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
		fmt.Printf("%d. %-12s type=%-10s kind=%-10s freq=%d/%s consec=%d%s\n",
			i+1, shortRuleID(id), string(r.Type), kind, freq, mode, cons, url)
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
	ruleID := fs.String("id", "", "explicit rule_id (uuid)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *typ == "" {
		return errors.New("--type is required")
	}
	target := args[0]
	ctx := context.Background()
	pid, err := client.Resolve(ctx, target)
	if err != nil {
		return err
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
	if filter, err := buildFilter(*kindCSV, *urlSubstr, *urlRegex); err != nil {
		return err
	} else if filter != nil {
		rule.Filter = filter
	}

	newETag, err := client.AddFaultRule(ctx, pid, "", rule)
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
	newETag, err := client.DeleteFaultRule(ctx, pid, ruleID, "")
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

func cmdFaultClear(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness fault clear <target>")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	newETag, err := client.ClearFaultRules(ctx, pid, "")
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player_id": pid, "etag": newETag})
	}
	fmt.Printf("cleared fault_rules on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}

func buildFilter(kindCSV, urlSubstr, urlRegex string) (*proxy.FaultFilter, error) {
	if kindCSV == "" && urlSubstr == "" && urlRegex == "" {
		return nil, nil
	}
	f := &proxy.FaultFilter{}
	if kindCSV != "" {
		parts := strings.Split(kindCSV, ",")
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
	if urlSubstr != "" && urlRegex != "" {
		return nil, errors.New("--url-substr and --url-regex are mutually exclusive")
	}
	if urlSubstr != "" {
		f.UrlMatch = &proxy.UrlMatch{Mode: proxy.Substring, Patterns: []string{urlSubstr}}
	}
	if urlRegex != "" {
		f.UrlMatch = &proxy.UrlMatch{Mode: proxy.Regex, Patterns: []string{urlRegex}}
	}
	return f, nil
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
