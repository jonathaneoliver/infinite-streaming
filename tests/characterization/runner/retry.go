package runner

import (
	"sort"
	"strings"
	"time"
)

// ObserveRetryCycle scans network rows + samples for a persistent-
// fault observation window and builds a RetryCycleResult. Reuses the
// abort-test detection primitives but groups attempts BY URL to
// compute per-URL retry intervals (the backoff curve).
//
// Caller passes:
//   - pre: snapshot taken just before the fault was armed (provides
//     pre_variant, pre_variant_dir, pre_buffer_s)
//   - armedAt: the moment ArmFault returned (t=0 for everything below)
//   - observeWindow: how long the cycle observed (e.g. 60s)
//   - rows: network_requests rows in [armedAt - small_window, end]
//   - samples: in-process samples already collected by the sampler
//   - giveUpThreshold: gap (e.g. 30s) with no follow-up on a URL that
//     marks it as "abandoned"
//
// Detection logic:
//   - Group rows by URL post-armedAt.
//   - For each URL with >=1 attempt that carries fault_type or
//     fault_action!="", compute intervals = pairwise time diffs of
//     consecutive attempts (sorted by ts).
//   - Aggregate mean/median across all URLs' intervals.
//   - Downshift "decided" = first post-armedAt sample with
//     VideoResolution != preVariant.
//   - Downshift "committed" = first post-armedAt manifest row whose
//     URL pathParent != preVariantDir.
//   - Gave-up = URL with attempt_count > 0 AND
//     (windowEnd - lastAttempt) > giveUpThreshold.
//   - Stalled = any sample with position frozen for >5s post-armedAt.
//
// See .claude/standards/retry-backoff-characterization-test.md for
// data-model documentation.
func ObserveRetryCycle(
	pre RetryCycleResult, armedAt time.Time,
	observeWindow time.Duration, giveUpThreshold time.Duration,
	rows []NetworkRow, samples []Sample,
) RetryCycleResult {
	out := pre
	out.ArmedAt = armedAt
	out.ObserveWindowS = observeWindow.Seconds()
	windowEnd := armedAt.Add(observeWindow)

	// Group rows by URL, only those after armedAt.
	attemptsByURL := map[string][]NetworkRow{}
	for _, r := range rows {
		if r.Ts.Before(armedAt) {
			continue
		}
		attemptsByURL[r.URL] = append(attemptsByURL[r.URL], r)
	}

	// Process each URL's attempts.
	var allIntervalsMs []int64
	type firstAttempt struct {
		url string
		ts  time.Time
	}
	firsts := []firstAttempt{}
	for url, attempts := range attemptsByURL {
		sort.Slice(attempts, func(i, j int) bool {
			return attempts[i].Ts.Before(attempts[j].Ts)
		})
		// Only URLs with at least one faulted attempt are of interest
		// for retry characterization. URLs without any faulted row
		// are normal background fetches; skip.
		anyFaulted := false
		faultKinds := map[string]struct{}{}
		for _, a := range attempts {
			if a.FaultType != "" || a.FaultAction == "transfer_abandoned" {
				anyFaulted = true
				if a.FaultType != "" {
					faultKinds[a.FaultType] = struct{}{}
				} else if a.FaultAction != "" {
					faultKinds[a.FaultAction] = struct{}{}
				}
			}
		}
		if !anyFaulted {
			continue
		}

		intervals := make([]int64, 0, len(attempts)-1)
		for i := 1; i < len(attempts); i++ {
			d := attempts[i].Ts.Sub(attempts[i-1].Ts)
			intervals = append(intervals, d.Milliseconds())
		}
		allIntervalsMs = append(allIntervalsMs, intervals...)

		kinds := []string{}
		for k := range faultKinds {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		allFaulted := true
		for _, a := range attempts {
			if a.FaultType == "" && a.FaultAction != "transfer_abandoned" {
				allFaulted = false
				break
			}
		}

		info := URLRetryInfo{
			URL:             url,
			AttemptCount:    len(attempts),
			IntervalsMs:     intervals,
			FirstAttemptAtS: attempts[0].Ts.Sub(armedAt).Seconds(),
			LastAttemptAtS:  attempts[len(attempts)-1].Ts.Sub(armedAt).Seconds(),
			AllFaulted:      allFaulted,
			FaultKinds:      kinds,
		}
		out.PerURLRetries = append(out.PerURLRetries, info)
		firsts = append(firsts, firstAttempt{url: url, ts: attempts[0].Ts})

		// Give-up detection — first URL whose last attempt is older
		// than (windowEnd - giveUpThreshold) AND whose attempts were
		// all faulted.
		if out.GaveUpURL == "" && allFaulted {
			last := attempts[len(attempts)-1].Ts
			if windowEnd.Sub(last) > giveUpThreshold {
				out.GaveUpURL = url
				out.GaveUpAtS = last.Sub(armedAt).Seconds()
			}
		}
	}

	// Sort PerURLRetries by the URL's first attempt time so the dashboard
	// renders them in temporal order.
	sort.Slice(out.PerURLRetries, func(i, j int) bool {
		return out.PerURLRetries[i].FirstAttemptAtS < out.PerURLRetries[j].FirstAttemptAtS
	})
	out.FaultedURLs = len(out.PerURLRetries)
	for _, info := range out.PerURLRetries {
		out.TotalFailedFetches += info.AttemptCount
	}

	if len(allIntervalsMs) > 0 {
		sort.Slice(allIntervalsMs, func(i, j int) bool { return allIntervalsMs[i] < allIntervalsMs[j] })
		var sum int64
		for _, v := range allIntervalsMs {
			sum += v
		}
		out.MeanRetryIntervalMs = float64(sum) / float64(len(allIntervalsMs))
		out.MedianRetryIntervalMs = float64(allIntervalsMs[len(allIntervalsMs)/2])
	}

	// Downshift detection from samples (decided).
	for _, s := range samples {
		if s.Ts.Before(armedAt) {
			continue
		}
		if s.VideoResolution != "" && s.VideoResolution != pre.PreVariant {
			out.DownshiftDecidedTo = s.VideoResolution
			out.DownshiftDecidedAtS = s.Ts.Sub(armedAt).Seconds()
			break
		}
	}

	// Downshift detection from network (committed).
	// Sort rows chronologically.
	chronoRows := make([]NetworkRow, len(rows))
	copy(chronoRows, rows)
	sort.Slice(chronoRows, func(i, j int) bool { return chronoRows[i].Ts.Before(chronoRows[j].Ts) })
	for _, r := range chronoRows {
		if r.Ts.Before(armedAt) {
			continue
		}
		if r.RequestKind != "manifest" {
			continue
		}
		dir := VariantDirFromPath(r.URL)
		if dir != "" && dir != pre.PreVariantDir {
			out.DownshiftCommittedTo = dir
			out.DownshiftCommittedAtS = r.Ts.Sub(armedAt).Seconds()
			break
		}
	}

	// Stall detection — position frozen for >5s post-arm.
	var stallStart time.Time
	var lastPos float64
	var lastTs time.Time
	for _, s := range samples {
		if s.Ts.Before(armedAt) {
			continue
		}
		if !lastTs.IsZero() && s.PositionS == lastPos {
			if stallStart.IsZero() {
				stallStart = lastTs
			} else if s.Ts.Sub(stallStart) > 5*time.Second {
				out.PlayerStalled = true
				break
			}
		} else {
			stallStart = time.Time{}
		}
		lastPos = s.PositionS
		lastTs = s.Ts
	}

	return out
}

// VariantDirFromPath returns the segment-directory name from a URL
// path of the form ".../2160p/segment_NN.m4s" — i.e. the pathParent.
// For variant playlists ".../playlist_6s_2160p.m3u8" it falls back to
// the regex-based extraction shared with VideoVariantDirs.
//
// Used by characterization tests to map every URL the player fetches
// back to a known variant (or "audio" for audio media).
func VariantDirFromPath(url string) string {
	// Strip query string + leading slash.
	clean := url
	if i := strings.Index(clean, "?"); i >= 0 {
		clean = clean[:i]
	}
	clean = strings.TrimPrefix(clean, "/")
	parts := strings.Split(clean, "/")
	if len(parts) >= 2 {
		// For ".../<dir>/<file>", the dir is the directory name.
		candidate := parts[len(parts)-2]
		// Filter out the content root name (which would also be a
		// "parent" of segment paths). Heuristic: dir names that look
		// like content roots are long and contain underscores +
		// timestamps. The variant dirs we want (e.g. "2160p") are
		// short and primarily alphanumeric without underscores.
		if !strings.ContainsAny(candidate, "_") && len(candidate) <= 12 {
			return candidate
		}
	}
	// Fall back to the playlist-URL pattern.
	return variantDirFromPlaylistURL(url)
}
