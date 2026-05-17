package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/snapshot"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const labelsUsage = `harness labels <subcommand>

Subcommands:
  show <target>                     print current labels
  set  <target> k=v [k=v...]        merge new labels (existing kept)
  rm   <target> k [k...]            delete listed keys
  clear <target>                    drop all labels

Labels are merge-patched: set is additive, rm sends explicit-null per
key, clear sends labels:{}.
`

func cmdLabels(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(labelsUsage)
	}
	switch args[0] {
	case "show":
		return cmdLabelsShow(client, args[1:], asJSON)
	case "set":
		return cmdLabelsSet(client, args[1:], asJSON)
	case "rm", "remove", "delete":
		return cmdLabelsRm(client, args[1:], asJSON)
	case "clear":
		return cmdLabelsClear(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown labels subcommand: %s\n\n%s", args[0], labelsUsage)
	}
}

func cmdLabelsShow(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness labels show <target>")
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
	labels := map[string]string{}
	if rec.Labels != nil {
		labels = map[string]string(*rec.Labels)
	}
	if asJSON {
		return format.JSON(os.Stdout, labels)
	}
	if len(labels) == 0 {
		fmt.Println("no labels")
		return nil
	}
	for k, v := range labels {
		fmt.Printf("%s=%s\n", k, v)
	}
	return nil
}

func cmdLabelsSet(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 2 {
		return errors.New("usage: harness labels set <target> k=v [k=v...]")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	pairs, err := parseLabels(strings.Join(args[1:], ","))
	if err != nil {
		return err
	}
	l := proxy.Labels(pairs)
	patch := proxy.PlayerPatch{Labels: &l}
	newETag, err := client.PatchPlayer(ctx, pid, "labels set "+strings.Join(args[1:], " "), patch)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player_id": pid, "labels": pairs, "etag": newETag})
	}
	fmt.Printf("labels updated on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}

// cmdLabelsRm uses PatchRaw because merge-patch deletion of map
// entries requires `{"labels": {"k": null}}` and the typed PlayerPatch
// can only set/replace the whole map.
func cmdLabelsRm(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 2 {
		return errors.New("usage: harness labels rm <target> k [k...]")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	// Pre-fetch for snapshot since we're bypassing the typed facade.
	rec, etag, err := client.Player(ctx, pid)
	if err != nil {
		return err
	}
	nulls := make(map[string]any, len(args[1:]))
	for _, k := range args[1:] {
		nulls[k] = nil
	}
	body, err := json.Marshal(map[string]any{"labels": nulls})
	if err != nil {
		return err
	}
	if client.Snap != nil {
		beforeJSON, _ := json.Marshal(rec)
		_, _ = client.Snap.Save(snapshot.Snapshot{
			PlayerID:   pid,
			Action:     "labels rm " + strings.Join(args[1:], ","),
			EtagBefore: etag,
			Before:     beforeJSON,
			Patch:      body,
		})
	}
	newETag, err := client.PatchRaw(ctx, pid, etag, body)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player_id": pid, "removed": args[1:], "etag": newETag})
	}
	fmt.Printf("removed %d label(s) from %s (etag %s)\n", len(args[1:]), pid, shortRev(newETag))
	return nil
}

func cmdLabelsClear(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness labels clear <target>")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	empty := proxy.Labels{}
	patch := proxy.PlayerPatch{Labels: &empty}
	newETag, err := client.PatchPlayer(ctx, pid, "labels clear", patch)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player_id": pid, "cleared": true, "etag": newETag})
	}
	fmt.Printf("cleared labels on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}
