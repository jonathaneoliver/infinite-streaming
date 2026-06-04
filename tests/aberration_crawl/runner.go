package aberration_crawl

import (
	"context"
	"fmt"
	"strings"
)

// ArchiveFloor is the earliest data in the retained archive; rules with
// unparseable/TBD applicable_since dates census from here.
const ArchiveFloor = "2026-05-15"

// versionCols is the #550 Phase 4 taxonomy. session_events carries them
// natively; network_requests / control_events get them via a per-play
// argMax JOIN (pv CTE).
var versionCols = []string{"app_version", "os_version_major", "device_class", "player_tech"}

func grpExprNative() string {
	return "concat(app_version, '|', toString(os_version_major), '|', device_class, '|', player_tech)"
}

func grpExprPlayAgg() string {
	return "concat(argMax(app_version, ts), '|', toString(argMax(os_version_major, ts)), '|', argMax(device_class, ts), '|', argMax(player_tech, ts))"
}

func pvCTE(db, since string) string {
	return fmt.Sprintf(`WITH pv AS (
  SELECT play_id,
    argMax(app_version, ts) AS app_version,
    argMax(os_version_major, ts) AS os_version_major,
    argMax(device_class, ts) AS device_class,
    argMax(player_tech, ts) AS player_tech
  FROM %s.session_events WHERE ts >= '%s' GROUP BY play_id
)`, db, since)
}

// GroupResult is one (rule × table × version-group) census line.
type GroupResult struct {
	Group      string `json:"group"` // "app|osMajor|class|tech" or "unversioned"
	Checked    int64  `json:"checked"`
	Violations int64  `json:"violations"`
	Excluded   int64  `json:"excluded,omitempty"`
}

type Exemplar struct {
	PlayerID string `json:"player_id"`
	PlayID   string `json:"play_id"`
	TS       string `json:"ts,omitempty"`
}

type ValueCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

type RuleResult struct {
	RuleID        string        `json:"rule_id"`
	Table         string        `json:"table"`
	Kind          string        `json:"kind"`
	Severity      string        `json:"severity"`
	Mode          string        `json:"mode"`
	Pending       bool          `json:"pending,omitempty"`
	Since         string        `json:"since"`
	Groups        []GroupResult `json:"groups,omitempty"`
	UnknownValues []ValueCount  `json:"unknown_values,omitempty"`
	Exemplars     []Exemplar    `json:"exemplars,omitempty"`
	Skipped       string        `json:"skipped,omitempty"` // reason (kind: sql sketches)
	Err           string        `json:"error,omitempty"`
}

func (rr RuleResult) TotalViolations() int64 {
	var n int64
	for _, g := range rr.Groups {
		n += g.Violations
	}
	for _, v := range rr.UnknownValues {
		n += v.Count
	}
	return n
}

// RunAll executes every rule×table in the catalogue. Errors are
// captured per-result, never aborting the crawl.
func RunAll(ctx context.Context, ch *CHClient, cat *Catalogue) []RuleResult {
	var out []RuleResult
	for _, rule := range cat.Rules {
		for _, table := range rule.Tables {
			out = append(out, runOne(ctx, ch, cat, rule, table))
		}
	}
	return out
}

func runOne(ctx context.Context, ch *CHClient, cat *Catalogue, r Rule, table string) RuleResult {
	res := RuleResult{
		RuleID:   r.ID,
		Table:    table,
		Kind:     r.Kind,
		Severity: r.Severity,
		Mode:     r.EffectiveMode(cat.Defaults.Mode),
		Pending:  r.Pending,
		Since:    r.SinceDate(ArchiveFloor),
	}
	var err error
	switch r.Kind {
	case "row":
		err = runRow(ctx, ch, r, table, &res)
	case "vocab":
		err = runVocab(ctx, ch, r, table, &res)
	case "monotonic":
		err = runMonotonic(ctx, ch, r, table, &res)
	case "sequence":
		err = runSequence(ctx, ch, r, table, &res)
	case "play":
		err = runPlay(ctx, ch, r, table, &res)
	case "sql":
		res.Skipped = "kind=sql is sketch-only in Phase 2; implemented in Phase 3"
	}
	if err != nil {
		res.Err = err.Error()
	}
	return res
}

// exclusionExpr ORs together all exclusions that carry a where clause.
func exclusionExpr(r Rule) string {
	var parts []string
	for _, e := range r.Exclusions {
		if strings.TrimSpace(e.Where) != "" {
			parts = append(parts, "("+e.Where+")")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}

func whereClause(r Rule, since string) string {
	w := fmt.Sprintf("ts >= '%s'", since)
	if strings.TrimSpace(r.Where) != "" {
		w += " AND (" + r.Where + ")"
	}
	return w
}

// fromWithVersions returns the FROM fragment + group expression for a
// table: session_events natively, others via pv JOIN. The CTE prefix is
// returned separately (empty for native).
func fromWithVersions(db, table, since string) (cte, from, grp string) {
	if table == "session_events" {
		return "", fmt.Sprintf("%s.%s", db, table), grpExprNative()
	}
	return pvCTE(db, since) + "\n",
		fmt.Sprintf("%s.%s AS t LEFT JOIN pv USING (play_id)", db, table),
		grpExprNative()
}

func scanGroups(rows []map[string]any, res *RuleResult) {
	for _, row := range rows {
		g := asString(row["grp"])
		if g == "|0||" || g == "" {
			g = "unversioned"
		}
		res.Groups = append(res.Groups, GroupResult{
			Group:      g,
			Checked:    asInt64(row["checked"]),
			Violations: asInt64(row["violations"]),
			Excluded:   asInt64(row["excluded"]),
		})
	}
}

func scanExemplars(rows []map[string]any, res *RuleResult) {
	for _, row := range rows {
		res.Exemplars = append(res.Exemplars, Exemplar{
			PlayerID: asString(row["player_id"]),
			PlayID:   asString(row["play_id"]),
			TS:       asString(row["ts"]),
		})
	}
}

func runRow(ctx context.Context, ch *CHClient, r Rule, table string, res *RuleResult) error {
	since := r.SinceDate(ArchiveFloor)
	cte, from, grp := fromWithVersions(ch.Database, table, since)
	excl := exclusionExpr(r)
	violations := fmt.Sprintf("countIf(%s)", r.Violation)
	excluded := "0"
	if excl != "" {
		violations = fmt.Sprintf("countIf((%s) AND NOT (%s))", r.Violation, excl)
		excluded = fmt.Sprintf("countIf(%s)", excl)
	}
	sql := fmt.Sprintf(`%sSELECT %s AS grp, count() AS checked, %s AS violations, %s AS excluded
FROM %s WHERE %s GROUP BY grp`, cte, grp, violations, excluded, from, whereClause(r, since))
	rows, err := ch.Query(ctx, sql)
	if err != nil {
		return err
	}
	scanGroups(rows, res)

	if res.TotalViolations() > 0 {
		cond := r.Violation
		if excl != "" {
			cond = fmt.Sprintf("(%s) AND NOT (%s)", r.Violation, excl)
		}
		ex := fmt.Sprintf(`%sSELECT player_id, play_id, toString(ts) AS ts FROM %s
WHERE %s AND (%s) LIMIT 3`, cte, from, whereClause(r, since), cond)
		exRows, err := ch.Query(ctx, ex)
		if err == nil {
			scanExemplars(exRows, res)
		}
	}
	return nil
}

func runVocab(ctx context.Context, ch *CHClient, r Rule, table string, res *RuleResult) error {
	since := r.SinceDate(ArchiveFloor)
	quoted := make([]string, len(r.Allowed))
	for i, v := range r.Allowed {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "\\'") + "'"
	}
	sql := fmt.Sprintf(`SELECT toString(%s) AS value, count() AS c FROM %s.%s
WHERE %s AND %s NOT IN (%s)
GROUP BY value ORDER BY c DESC LIMIT 20`,
		r.Field, ch.Database, table, whereClause(r, since), r.Field, strings.Join(quoted, ", "))
	rows, err := ch.Query(ctx, sql)
	if err != nil {
		return err
	}
	for _, row := range rows {
		res.UnknownValues = append(res.UnknownValues, ValueCount{
			Value: asString(row["value"]),
			Count: asInt64(row["c"]),
		})
	}
	total, err := ch.Query(ctx, fmt.Sprintf(
		"SELECT count() AS checked FROM %s.%s WHERE %s", ch.Database, table, whereClause(r, since)))
	if err == nil && len(total) == 1 {
		res.Groups = append(res.Groups, GroupResult{Group: "all", Checked: asInt64(total[0]["checked"])})
	}
	return nil
}

func runMonotonic(ctx context.Context, ch *CHClient, r Rule, table string, res *RuleResult) error {
	since := r.SinceDate(ArchiveFloor)
	for _, f := range r.Fields {
		inner := fmt.Sprintf(`SELECT player_id, play_id, ts, %s AS grp, %s AS _cur,
  lagInFrame(%s) OVER w AS _prev, row_number() OVER w AS _rn
FROM %s.%s WHERE %s
WINDOW w AS (PARTITION BY player_id, play_id ORDER BY ts)`,
			grpExprNative(), f, f, ch.Database, table, whereClause(r, since))
		sql := fmt.Sprintf(`SELECT grp, count() AS checked,
  countIf(_rn > 1 AND _cur < _prev) AS violations
FROM (%s) GROUP BY grp`, inner)
		rows, err := ch.Query(ctx, sql)
		if err != nil {
			return fmt.Errorf("field %s: %w", f, err)
		}
		for _, row := range rows {
			g := asString(row["grp"])
			if g == "|0||" || g == "" {
				g = "unversioned"
			}
			res.Groups = append(res.Groups, GroupResult{
				Group:      g + " [" + f + "]",
				Checked:    asInt64(row["checked"]),
				Violations: asInt64(row["violations"]),
			})
		}
		// Exemplars per field only when violations exist.
		var fieldViol int64
		for _, row := range rows {
			fieldViol += asInt64(row["violations"])
		}
		if fieldViol > 0 && len(res.Exemplars) < 3 {
			ex := fmt.Sprintf(`SELECT player_id, play_id, toString(ts) AS ts
FROM (%s) WHERE _rn > 1 AND _cur < _prev LIMIT 3`, inner)
			exRows, err := ch.Query(ctx, ex)
			if err == nil {
				scanExemplars(exRows, res)
			}
		}
	}
	return nil
}

func runSequence(ctx context.Context, ch *CHClient, r Rule, table string, res *RuleResult) error {
	since := r.SinceDate(ArchiveFloor)
	var sel []string
	sel = append(sel, "player_id", "play_id", "ts", grpExprNative()+" AS grp")
	for _, c := range r.LagCols {
		sel = append(sel, c, fmt.Sprintf("lagInFrame(%s) OVER w AS prev_%s", c, c))
	}
	sel = append(sel, r.Columns...)
	sel = append(sel, "row_number() OVER w AS _rn")
	inner := fmt.Sprintf(`SELECT %s
FROM %s.%s WHERE %s
WINDOW w AS (PARTITION BY player_id, play_id ORDER BY ts)`,
		strings.Join(sel, ",\n  "), ch.Database, table, whereClause(r, since))
	sql := fmt.Sprintf(`SELECT grp, count() AS checked,
  countIf(_rn > 1 AND (%s)) AS violations
FROM (%s) GROUP BY grp`, r.Violation, inner)
	rows, err := ch.Query(ctx, sql)
	if err != nil {
		return err
	}
	scanGroups(rows, res)
	if res.TotalViolations() > 0 {
		ex := fmt.Sprintf(`SELECT player_id, play_id, toString(ts) AS ts
FROM (%s) WHERE _rn > 1 AND (%s) LIMIT 3`, inner, r.Violation)
		exRows, err := ch.Query(ctx, ex)
		if err == nil {
			scanExemplars(exRows, res)
		}
	}
	return nil
}

func runPlay(ctx context.Context, ch *CHClient, r Rule, table string, res *RuleResult) error {
	since := r.SinceDate(ArchiveFloor)
	var aggs []string
	for name, expr := range r.Aggregates {
		aggs = append(aggs, fmt.Sprintf("%s AS %s", expr, name))
	}
	inner := fmt.Sprintf(`SELECT player_id, play_id, %s AS grp,
  %s
FROM %s.%s WHERE %s GROUP BY player_id, play_id`,
		grpExprPlayAgg(), strings.Join(aggs, ",\n  "), ch.Database, table, whereClause(r, since))
	sql := fmt.Sprintf(`SELECT grp, count() AS checked, countIf(%s) AS violations
FROM (%s) GROUP BY grp`, r.Violation, inner)
	rows, err := ch.Query(ctx, sql)
	if err != nil {
		return err
	}
	scanGroups(rows, res)
	if res.TotalViolations() > 0 {
		ex := fmt.Sprintf(`SELECT player_id, play_id, '' AS ts FROM (%s) WHERE %s LIMIT 3`, inner, r.Violation)
		exRows, err := ch.Query(ctx, ex)
		if err == nil {
			scanExemplars(exRows, res)
		}
	}
	return nil
}
