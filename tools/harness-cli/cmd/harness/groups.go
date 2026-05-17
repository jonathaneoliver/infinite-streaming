package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const groupsUsage = `harness groups <subcommand>

Subcommands:
  list                                  GET /api/v2/player-groups
  show <group_id>                       GET /api/v2/player-groups/<id> + ETag
  create --label LABEL --members T1,T2  create with named members
                                        (Tn = resolver targets)
  patch <group_id> --label L --labels k=v,...
                                        PATCH metadata
  add <group_id> <target>               add member (PATCH member_player_ids)
  remove <group_id> <target>            remove member
  rm <group_id> [--yes]                 DELETE group (members stay)
`

func cmdGroups(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(groupsUsage)
	}
	switch args[0] {
	case "list", "ls":
		return cmdGroupsList(client, args[1:], asJSON)
	case "show":
		return cmdGroupsShow(client, args[1:], asJSON)
	case "create", "new":
		return cmdGroupsCreate(client, args[1:], asJSON)
	case "patch":
		return cmdGroupsPatch(client, args[1:], asJSON)
	case "add":
		return cmdGroupsAdd(client, args[1:], asJSON)
	case "remove", "rm-member":
		return cmdGroupsRemove(client, args[1:], asJSON)
	case "rm", "delete":
		return cmdGroupsRm(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown groups subcommand: %s\n\n%s", args[0], groupsUsage)
	}
}

func cmdGroupsList(client *api.Client, args []string, asJSON bool) error {
	groups, err := client.Groups(context.Background())
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, groups)
	}
	if len(groups) == 0 {
		fmt.Println("no player groups")
		return nil
	}
	for _, g := range groups {
		label := "—"
		if g.Label != nil && *g.Label != "" {
			label = *g.Label
		}
		fmt.Printf("%s  label=%-30s members=%d\n", shortPlayer(g.Id.String()), label, len(g.MemberPlayerIds))
	}
	return nil
}

func cmdGroupsShow(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness groups show <group_id>")
	}
	grp, etag, err := client.Group(context.Background(), args[0])
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"group": grp, "etag": etag})
	}
	if err := format.JSON(os.Stdout, grp); err != nil {
		return err
	}
	fmt.Printf("\nETag: %q\n", etag)
	return nil
}

func cmdGroupsCreate(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("groups create", flag.ContinueOnError)
	label := fs.String("label", "", "group label (display name)")
	membersCSV := fs.String("members", "", "comma-separated resolver targets")
	labelsCSV := fs.String("labels", "", "k=v,k=v group-level labels")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	members := []uuid.UUID{}
	if *membersCSV != "" {
		for _, t := range strings.Split(*membersCSV, ",") {
			t = strings.TrimSpace(t)
			pid, err := client.Resolve(ctx, t)
			if err != nil {
				return fmt.Errorf("resolve member %q: %w", t, err)
			}
			uid, err := uuid.Parse(pid)
			if err != nil {
				return fmt.Errorf("uuid parse %q: %w", pid, err)
			}
			members = append(members, uid)
		}
	}
	body := proxy.PostApiV2PlayerGroupsJSONRequestBody{
		MemberPlayerIds: members,
	}
	if *label != "" {
		body.Label = label
	}
	if *labelsCSV != "" {
		l, err := parseLabels(*labelsCSV)
		if err != nil {
			return err
		}
		lp := proxy.Labels(l)
		body.Labels = &lp
	}
	grp, etag, err := client.CreateGroup(ctx, body)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"group": grp, "etag": etag})
	}
	fmt.Printf("created group %s (etag %s)\n", grp.Id, shortRev(etag))
	return nil
}

func cmdGroupsPatch(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness groups patch <group_id> [--label] [--labels k=v]")
	}
	fs := flag.NewFlagSet("groups patch", flag.ContinueOnError)
	label := fs.String("label", "", "")
	labelsCSV := fs.String("labels", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	patch := proxy.PlayerGroupPatch{}
	touched := false
	if *label != "" {
		patch.Label = label
		touched = true
	}
	if *labelsCSV != "" {
		l, err := parseLabels(*labelsCSV)
		if err != nil {
			return err
		}
		lp := proxy.Labels(l)
		patch.Labels = &lp
		touched = true
	}
	if !touched {
		return errors.New("nothing to patch — pass --label or --labels")
	}
	newETag, err := client.PatchGroup(context.Background(), args[0], "groups patch", patch)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"group_id": args[0], "etag": newETag})
	}
	fmt.Printf("patched group %s (etag %s)\n", args[0], shortRev(newETag))
	return nil
}

func cmdGroupsAdd(client *api.Client, args []string, asJSON bool) error {
	return modifyGroupMembership(client, args, asJSON, true)
}

func cmdGroupsRemove(client *api.Client, args []string, asJSON bool) error {
	return modifyGroupMembership(client, args, asJSON, false)
}

func modifyGroupMembership(client *api.Client, args []string, asJSON, add bool) error {
	if len(args) != 2 {
		op := "add"
		if !add {
			op = "remove"
		}
		return fmt.Errorf("usage: harness groups %s <group_id> <target>", op)
	}
	ctx := context.Background()
	grp, _, err := client.Group(ctx, args[0])
	if err != nil {
		return err
	}
	pid, err := client.Resolve(ctx, args[1])
	if err != nil {
		return err
	}
	uid, err := uuid.Parse(pid)
	if err != nil {
		return err
	}
	members := append([]uuid.UUID(nil), grp.MemberPlayerIds...)
	if add {
		for _, m := range members {
			if m == uid {
				return fmt.Errorf("%s is already a member", pid)
			}
		}
		members = append(members, uid)
	} else {
		out := members[:0]
		removed := false
		for _, m := range members {
			if m == uid {
				removed = true
				continue
			}
			out = append(out, m)
		}
		if !removed {
			return fmt.Errorf("%s is not a member", pid)
		}
		members = out
	}
	patch := proxy.PlayerGroupPatch{MemberPlayerIds: &members}
	op := "groups add"
	if !add {
		op = "groups remove"
	}
	newETag, err := client.PatchGroup(ctx, args[0], op+" "+pid, patch)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"group_id": args[0], "member_count": len(members), "etag": newETag})
	}
	fmt.Printf("group %s now has %d member(s) (etag %s)\n", args[0], len(members), shortRev(newETag))
	return nil
}

func cmdGroupsRm(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("groups rm", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("usage: harness groups rm <group_id> [--yes]")
	}
	if !*yes {
		fmt.Printf("delete group %s? [y/N] ", rest[0])
		var resp string
		fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" {
			return errors.New("aborted")
		}
	}
	if err := client.DeleteGroup(context.Background(), rest[0]); err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"group_id": rest[0], "deleted": true})
	}
	fmt.Printf("deleted group %s\n", rest[0])
	return nil
}
