(function () {
    const ABRCHAR_LOG_TAG = '[ABRCHAR]';

    function toNumber(value, fallback = null) {
        const numeric = Number(value);
        return Number.isFinite(numeric) ? numeric : fallback;
    }

    function sleep(ms) {
        return new Promise((resolve) => setTimeout(resolve, ms));
    }

    function median(values) {
        if (!values.length) return null;
        const sorted = values.slice().sort((a, b) => a - b);
        const mid = Math.floor(sorted.length / 2);
        if (sorted.length % 2 === 0) {
            return (sorted[mid - 1] + sorted[mid]) / 2;
        }
        return sorted[mid];
    }

    function average(values) {
        if (!Array.isArray(values) || !values.length) return null;
        const numeric = values.map((value) => toNumber(value, null)).filter((value) => value !== null);
        if (!numeric.length) return null;
        const total = numeric.reduce((sum, value) => sum + value, 0);
        return total / numeric.length;
    }

    function standardDeviation(values) {
        if (!Array.isArray(values) || values.length < 2) return null;
        const numeric = values.map((value) => toNumber(value, null)).filter((value) => value !== null);
        if (numeric.length < 2) return null;
        const avg = average(numeric);
        if (avg === null) return null;
        const variance = numeric.reduce((sum, value) => {
            const delta = value - avg;
            return sum + (delta * delta);
        }, 0) / numeric.length;
        return Math.sqrt(variance);
    }

    function modeRounded(values, decimals = 1) {
        if (!Array.isArray(values) || !values.length) return null;
        const multiplier = Math.pow(10, decimals);
        const counts = new Map();
        values.forEach((value) => {
            const numeric = toNumber(value, null);
            if (numeric === null) return;
            const rounded = Math.round(numeric * multiplier) / multiplier;
            counts.set(rounded, (counts.get(rounded) || 0) + 1);
        });
        if (!counts.size) return null;
        let bestValue = null;
        let bestCount = -1;
        Array.from(counts.entries()).forEach(([value, count]) => {
            if (count > bestCount || (count === bestCount && (bestValue === null || value < bestValue))) {
                bestValue = value;
                bestCount = count;
            }
        });
        return bestValue;
    }

    function formatMbps(value) {
        const numeric = toNumber(value, null);
        if (!Number.isFinite(numeric)) return '—';
        return `${Math.round(numeric * 100) / 100} Mbps`;
    }

    function nearestVariantIndex(ladderMbps, valueMbps) {
        const value = toNumber(valueMbps, null);
        if (value === null || !Array.isArray(ladderMbps) || !ladderMbps.length) return -1;
        let bestIndex = -1;
        let bestDistance = Number.POSITIVE_INFINITY;
        ladderMbps.forEach((ladderValue, index) => {
            const numeric = toNumber(ladderValue, null);
            if (numeric === null) return;
            const distance = Math.abs(numeric - value);
            if (distance < bestDistance) {
                bestDistance = distance;
                bestIndex = index;
            }
        });
        return bestIndex;
    }

    function formatVariantWithLabel(valueMbps, ladderMbps) {
        const value = toNumber(valueMbps, null);
        if (value === null) {
            return { label: '—', text: '—', index: -1, mbps: null };
        }
        const idx = nearestVariantIndex(ladderMbps, value);
        if (idx < 0) {
            const rounded = Math.round(value * 100) / 100;
            return { label: '—', text: `${rounded.toFixed(2)} Mbps`, index: -1, mbps: rounded };
        }
        const rounded = Math.round(value * 100) / 100;
        const variantLabel = `V${idx + 1}`;
        return {
            label: variantLabel,
            text: `${variantLabel} ${rounded.toFixed(2)} Mbps`,
            index: idx,
            mbps: rounded
        };
    }

    function phaseLabel(direction) {
        return direction === 'down' ? 'ramp-down' : 'ramp-up';
    }

    function buildPeriodThroughputStats(runState) {
        const summarize = (values) => {
            const numeric = values
                .map((value) => toNumber(value, null))
                .filter((value) => value !== null);
            return {
                n: numeric.length,
                avg: average(numeric),
                median: median(numeric),
                mode: modeRounded(numeric, 1)
            };
        };

        const steps = Array.isArray(runState?.steps) ? runState.steps : [];
        const samples = Array.isArray(runState?.samples) ? runState.samples : [];
        const playbackStoppedEvents = Array.isArray(runState?.playbackStoppedEvents) ? runState.playbackStoppedEvents : [];
        return steps.map((step, index) => {
            const stepSamples = samples
                .filter((sample) => Number(sample.stepIndex) === index);
            const active6s = stepSamples
                .map((sample) => toNumber(sample.active6sThroughputMbps, null));
            const wireThroughput = stepSamples
                .map((sample) => toNumber(sample.wireThroughputMbps, null));
            const playerEstimate = stepSamples
                .map((sample) => toNumber(sample.playerEstimateMbps, null));

            const activeSummary = summarize(active6s);
            const wireSummary = summarize(wireThroughput);
            const estimateSummary = summarize(playerEstimate);
            const stallCounts = stepSamples
                .map((sample) => toNumber(sample.stallCount, null))
                .filter((value) => value !== null);
            const stallTimes = stepSamples
                .map((sample) => toNumber(sample.stallTimeSeconds, null))
                .filter((value) => value !== null);
            const framesDisplayed = stepSamples
                .map((sample) => toNumber(sample.framesDisplayed, null))
                .filter((value) => value !== null);

            const stallCountDelta = stallCounts.length
                ? (Math.max(...stallCounts) - Math.min(...stallCounts))
                : null;
            const stallTimeDeltaSeconds = stallTimes.length
                ? (Math.max(...stallTimes) - Math.min(...stallTimes))
                : null;
            const framesDisplayedDelta = framesDisplayed.length
                ? (Math.max(...framesDisplayed) - Math.min(...framesDisplayed))
                : null;
            const restartCount = playbackStoppedEvents.filter((event) => Number(event.stepIndex) === index).length;
            const target = toNumber(step.targetMbps, null);
            const activeAvgDelta = (activeSummary.avg !== null && target !== null) ? (activeSummary.avg - target) : null;
            const activeAvgPctOfTarget = (activeSummary.avg !== null && target !== null && target > 0)
                ? ((activeSummary.avg / target) * 100)
                : null;

            return {
                period: index + 1,
                phase: phaseLabel(step.direction),
                targetMbps: target,
                active_6s: {
                    ...activeSummary,
                    avg_delta_mbps: activeAvgDelta,
                    avg_pct_of_target: activeAvgPctOfTarget
                },
                wire_throughput: wireSummary,
                player_est_network: estimateSummary,
                diagnostics: {
                    stall_count_delta: stallCountDelta,
                    stall_time_delta_s: stallTimeDeltaSeconds,
                    frames_displayed_delta: framesDisplayedDelta,
                    restart_count: restartCount
                }
            };
        });
    }

    function buildPeriodBufferDepthStats(runState) {
        const steps = Array.isArray(runState?.steps) ? runState.steps : [];
        const samples = Array.isArray(runState?.samples) ? runState.samples : [];
        return steps.map((step, index) => {
            const bufferDepths = samples
                .filter((sample) => Number(sample.stepIndex) === index)
                .map((sample) => toNumber(sample.bufferSeconds, null))
                .filter((value) => value !== null);
            const avg = average(bufferDepths);
            const med = median(bufferDepths);
            const mode = modeRounded(bufferDepths, 1);
            const stdev = standardDeviation(bufferDepths);
            const histogram = buildBufferDepthHistogram(bufferDepths);
            return {
                period: index + 1,
                phase: phaseLabel(step.direction),
                sampleCount: bufferDepths.length,
                avgSeconds: avg,
                meanSeconds: avg,
                medianSeconds: med,
                modeSeconds: mode,
                stdevSeconds: stdev,
                histogram,
                histogramInline: formatHistogramInline(histogram)
            };
        });
    }

    function buildBufferDepthHistogram(values) {
        const bins = [
            { key: '0-1s', min: 0, max: 1, count: 0 },
            { key: '1-2s', min: 1, max: 2, count: 0 },
            { key: '2-3s', min: 2, max: 3, count: 0 },
            { key: '3-5s', min: 3, max: 5, count: 0 },
            { key: '5-8s', min: 5, max: 8, count: 0 },
            { key: '8-12s', min: 8, max: 12, count: 0 },
            { key: '12-20s', min: 12, max: 20, count: 0 },
            { key: '20s+', min: 20, max: Number.POSITIVE_INFINITY, count: 0 }
        ];
        values.forEach((value) => {
            const numeric = toNumber(value, null);
            if (numeric === null || numeric < 0) return;
            const targetBin = bins.find((bin) => numeric >= bin.min && numeric < bin.max);
            if (targetBin) {
                targetBin.count += 1;
            }
        });
        return bins;
    }

    function formatHistogramInline(histogram) {
        if (!Array.isArray(histogram) || !histogram.length) return '-';
        const populated = histogram.filter((bin) => Number(bin.count) > 0);
        if (!populated.length) return '-';
        return populated.map((bin) => `${bin.key}:${bin.count}`).join(', ');
    }

    function buildLimitResponseLatencies(runState) {
        const events = Array.isArray(runState?.switchEvents) ? runState.switchEvents : [];
        const firstSwitchByStep = new Map();
        events.forEach((event) => {
            const stepIndex = Number(event.stepIndex);
            if (!Number.isFinite(stepIndex)) return;
            if (!firstSwitchByStep.has(stepIndex)) {
                firstSwitchByStep.set(stepIndex, event);
            }
        });

        return Array.from(firstSwitchByStep.entries())
            .sort((a, b) => a[0] - b[0])
            .map(([stepIndex, event]) => ({
                period: stepIndex + 1,
                step_index: stepIndex,
                phase: phaseLabel(event.stepDirection),
                target_mbps: toNumber(event.stepTargetMbps, null),
                switch_latency_s: toNumber(event.secondsAfterLimitChange, null),
                from_variant_label: event.fromVariantLabel || null,
                from_variant_mbps: toNumber(event.fromVariantMbps, null),
                to_variant_label: event.toVariantLabel || null,
                to_variant_mbps: toNumber(event.toVariantMbps, null)
            }));
    }

    function percentile(values, p) {
        if (!Array.isArray(values) || !values.length) return null;
        const numeric = values
            .map((value) => toNumber(value, null))
            .filter((value) => value !== null)
            .sort((a, b) => a - b);
        if (!numeric.length) return null;
        const clamped = Math.max(0, Math.min(100, Number(p) || 0));
        const idx = Math.ceil((clamped / 100) * numeric.length) - 1;
        const boundedIdx = Math.max(0, Math.min(numeric.length - 1, idx));
        return numeric[boundedIdx];
    }

    function buildShiftLatencySummary(runState) {
        const events = Array.isArray(runState?.switchEvents) ? runState.switchEvents : [];
        const steps = Array.isArray(runState?.steps) ? runState.steps : [];
        const summarize = (subset) => {
            const latencies = subset
                .map((event) => toNumber(event.secondsAfterLimitChange, null))
                .filter((value) => value !== null);
            if (!latencies.length) {
                return {
                    count: 0,
                    min_s: null,
                    median_s: null,
                    p95_s: null,
                    max_s: null
                };
            }
            return {
                count: latencies.length,
                min_s: Math.min(...latencies),
                median_s: median(latencies),
                p95_s: percentile(latencies, 95),
                max_s: Math.max(...latencies)
            };
        };

        const down = events.filter((event) => toNumber(event.toVariantMbps, null) < toNumber(event.fromVariantMbps, null));
        const up = events.filter((event) => toNumber(event.toVariantMbps, null) > toNumber(event.fromVariantMbps, null));

        const severityBuckets = [
            { key: 'small', min: 20, max: 40 },
            { key: 'medium', min: 40, max: 70 },
            { key: 'severe', min: 70, max: 101 }
        ];
        const severity = {};
        severityBuckets.forEach((bucket) => {
            const downSubset = down.filter((event) => {
                const step = steps[Number(event.stepIndex)] || null;
                if (!step || step.mode !== 'downshift-severity') return false;
                const dropPct = toNumber(step.severityDropPct, null);
                if (dropPct === null) return false;
                return dropPct >= bucket.min && dropPct < bucket.max;
            });
            severity[bucket.key] = summarize(downSubset);
        });

        return {
            downshift: summarize(down),
            upshift: summarize(up),
            downshift_by_severity: severity
        };
    }

    function parseAttributes(line) {
        const attrs = {};
        const body = line.replace(/^#EXT-X-STREAM-INF:/, '');
        const parts = body.match(/(?:[^,"]+|"[^"]*")+/g) || [];
        parts.forEach((part) => {
            const idx = part.indexOf('=');
            if (idx <= 0) return;
            const key = part.slice(0, idx).trim();
            let value = part.slice(idx + 1).trim();
            value = value.replace(/^"|"$/g, '');
            attrs[key] = value;
        });
        return attrs;
    }

    function normalizeUrlCandidate(raw, baseUrl) {
        const value = String(raw || '').trim();
        if (!value) return '';
        try {
            const parsed = new URL(value);
            return parsed.toString();
        } catch (_) {
            // continue
        }

        if (value.startsWith('go-live/')) {
            const rooted = `/${value}`;
            try {
                return new URL(rooted, baseUrl || window.location.origin).toString();
            } catch (_) {
                return rooted;
            }
        }

        if (value.startsWith('/')) {
            try {
                return new URL(value, baseUrl || window.location.origin).toString();
            } catch (_) {
                return value;
            }
        }

        try {
            return new URL(value, baseUrl || window.location.origin).toString();
        } catch (_) {
            return value;
        }
    }

    function buildMasterUrlCandidates(session, currentUrl) {
        const rawCurrent = String(currentUrl || '').trim();
        let currentBase = '';
        let currentPath = '';
        if (rawCurrent) {
            try {
                const parsed = new URL(rawCurrent, window.location.origin);
                parsed.searchParams.delete('player_id');
                currentBase = parsed.origin;
                currentPath = parsed.toString();
            } catch (_) {
                // continue
            }
        }

        const candidatesRaw = [
            currentPath,
            session?.master_manifest_url,
            session?.manifest_url,
            session?.last_request_url,
            rawCurrent
        ];

        const seen = new Set();
        const output = [];
        candidatesRaw.forEach((candidate) => {
            if (!candidate) return;
            const normalizedWithCurrentBase = normalizeUrlCandidate(candidate, currentBase || undefined);
            const normalizedWithWindowBase = normalizeUrlCandidate(candidate, window.location.origin);
            [normalizedWithCurrentBase, normalizedWithWindowBase].forEach((normalized) => {
                if (!normalized) return;
                if (!normalized.includes('.m3u8')) return;
                if (seen.has(normalized)) return;
                seen.add(normalized);
                output.push(normalized);
            });
        });
        return output;
    }

    async function parseHlsLadder(masterUrl) {
        if (!masterUrl) {
            return [];
        }
        const response = await fetch(masterUrl, { cache: 'no-store' });
        if (!response.ok) {
            throw new Error(`Failed to load master playlist (${response.status})`);
        }
        const text = await response.text();
        const lines = text.split(/\r?\n/);
        const variants = [];
        for (let index = 0; index < lines.length; index += 1) {
            const line = (lines[index] || '').trim();
            if (!line.startsWith('#EXT-X-STREAM-INF:')) continue;
            const attrs = parseAttributes(line);
            let uri = '';
            for (let next = index + 1; next < lines.length; next += 1) {
                const candidate = (lines[next] || '').trim();
                if (!candidate || candidate.startsWith('#')) continue;
                uri = candidate;
                break;
            }
            const bandwidth = Number(attrs.BANDWIDTH || 0);
            const averageBandwidth = Number(attrs['AVERAGE-BANDWIDTH'] || 0);
            if (!Number.isFinite(bandwidth) || bandwidth <= 0) continue;
            variants.push({
                uri,
                bandwidth,
                averageBandwidth: Number.isFinite(averageBandwidth) && averageBandwidth > 0 ? averageBandwidth : null
            });
        }
        variants.sort((a, b) => a.bandwidth - b.bandwidth);
        return variants;
    }

    function makeSchedule(options) {
        const {
            direction,
            maxMbps,
            minMbps,
            stepPct,
            holdSeconds
        } = options;
        const steps = [];
        const pct = Math.max(1, Math.min(90, stepPct));
        const multiplier = 1 - (pct / 100);
        const minRate = Math.max(0.1, minMbps);
        const maxRate = Math.max(minRate, maxMbps);
        const down = [];
        let cursor = maxRate;
        down.push(maxRate);
        while (cursor > minRate) {
            cursor = Math.max(minRate, Math.round((cursor * multiplier) * 1000) / 1000);
            if (down[down.length - 1] === cursor) break;
            down.push(cursor);
            if (cursor === minRate) break;
        }

        const up = down.slice(0, -1).reverse();

        if (direction === 'down') {
            down.forEach((target) => steps.push({ targetMbps: target, holdSeconds, direction: 'down' }));
        } else if (direction === 'up') {
            const base = [minRate].concat(up.length ? up : [maxRate]);
            base.forEach((target) => steps.push({ targetMbps: target, holdSeconds, direction: 'up' }));
        } else {
            down.forEach((target) => steps.push({ targetMbps: target, holdSeconds, direction: 'down' }));
            up.forEach((target) => steps.push({ targetMbps: target, holdSeconds, direction: 'up' }));
        }
        return steps;
    }

    function toMbpsFromVariant(variant) {
        if (!variant) return null;
        const preferred = Number(variant.averageBandwidth || 0);
        const fallback = Number(variant.bandwidth || 0);
        const bps = (Number.isFinite(preferred) && preferred > 0) ? preferred : fallback;
        if (!Number.isFinite(bps) || bps <= 0) return null;
        return bps / 1000000;
    }

    function uniqueSortedPositive(values) {
        const seen = new Set();
        return values
            .map((value) => Number(value))
            .filter((value) => Number.isFinite(value) && value > 0)
            .map((value) => Math.round(value * 1000) / 1000)
            .filter((value) => {
                if (seen.has(value)) return false;
                seen.add(value);
                return true;
            })
            .sort((a, b) => a - b);
    }

    function makeVariantAwareSchedule(options) {
        const {
            variants,
            direction,
            holdSeconds,
            minMbps,
            maxMbps,
            networkOverheadPct
        } = options;
        const steps = [];
        const ladderMbps = uniqueSortedPositive((variants || []).map((variant) => toMbpsFromVariant(variant)));
        if (!ladderMbps.length) {
            return steps;
        }

        const overheadPct = Number.isFinite(Number(networkOverheadPct))
            ? Math.max(0, Math.min(25, Number(networkOverheadPct)))
            : 10;
        const wireMultiplier = 1 / (1 - (overheadPct / 100));

        const low = Math.max(0.1, Number.isFinite(minMbps) ? minMbps : ladderMbps[0] * wireMultiplier);
        const high = Math.max(low, Number.isFinite(maxMbps) ? maxMbps : ladderMbps[ladderMbps.length - 1] * wireMultiplier * 1.1);

        const interpolationPoints = [0, 5, 10, 25, 50, 75, 90, 95];
        const rampProbeTargets = [];
        for (let index = 0; index < ladderMbps.length - 1; index += 1) {
            const fromWire = ladderMbps[index] * wireMultiplier;
            const toWire = ladderMbps[index + 1] * wireMultiplier;
            const gap = toWire - fromWire;
            interpolationPoints.forEach((pointPct) => {
                const ratio = pointPct / 100;
                const target = fromWire + (gap * ratio);
                const bounded = Math.max(low, Math.min(high, target));
                rampProbeTargets.push(bounded);
            });
        }

        if (ladderMbps.length === 1) {
            const single = Math.max(low, Math.min(high, ladderMbps[0] * wireMultiplier));
            rampProbeTargets.push(single);
        }

        const ordered = uniqueSortedPositive(rampProbeTargets).filter((value) => value >= low && value <= high);
        if (!ordered.length) {
            return steps;
        }

        const apply = (list, dirLabel) => {
            list.forEach((target) => {
                steps.push({
                    targetMbps: target,
                    holdSeconds,
                    direction: dirLabel
                });
            });
        };

        if (direction === 'up') {
            apply(ordered, 'up');
        } else if (direction === 'down') {
            apply(ordered.slice().reverse(), 'down');
        } else {
            apply(ordered.slice().reverse(), 'down');
            apply(ordered, 'up');
        }

        return steps;
    }

    function buildEmergencyDownshiftSchedule(options) {
        const {
            variants,
            minMbps,
            maxMbps,
            networkOverheadPct,
            segmentSeconds,
            bottomMarginPct,
            downshiftLatencyThresholdSeconds,
            bottomReachThresholdSeconds
        } = options;
        const ladderMbps = uniqueSortedPositive((variants || []).map((variant) => toMbpsFromVariant(variant)));
        if (!ladderMbps.length) {
            return { steps: [], ladderMbps, emergencyConfig: null };
        }

        const overheadPct = Number.isFinite(Number(networkOverheadPct))
            ? Math.max(0, Math.min(25, Number(networkOverheadPct)))
            : 10;
        const wireMultiplier = 1 / (1 - (overheadPct / 100));
        const floor = Math.max(0.1, Number.isFinite(minMbps) ? minMbps : ladderMbps[0] * wireMultiplier);
        const ceiling = Math.max(floor, Number.isFinite(maxMbps) ? maxMbps : ladderMbps[ladderMbps.length - 1] * wireMultiplier * 6);
        const segSeconds = Math.max(1, Math.min(12, Number(segmentSeconds) || 2));
        const marginRatio = Math.max(0, Math.min(0.3, Number(bottomMarginPct) / 100 || 0));
        const downshiftLatencyThresholdS = Math.max(2, Math.min(30, Number(downshiftLatencyThresholdSeconds) || (segSeconds * 2)));
        const bottomReachThresholdS = Math.max(downshiftLatencyThresholdS, Math.min(45, Number(bottomReachThresholdSeconds) || (segSeconds * 4)));

        const topWireMbps = ladderMbps[ladderMbps.length - 1] * wireMultiplier;
        const highTargetMbps = Math.max(floor, Math.min(ceiling, topWireMbps * 3));
        const bottomMediaMbps = ladderMbps[0];
        const secondBottomMediaMbps = ladderMbps.length > 1 ? ladderMbps[1] : ladderMbps[0];
        const lowMidpointMediaMbps = (bottomMediaMbps + secondBottomMediaMbps) / 2;
        const lowTargetMbps = Math.max(floor, Math.min(ceiling, (lowMidpointMediaMbps * wireMultiplier) * (1 + marginRatio)));

        const totalDurationSeconds = 20 * 60;
        const nominalCycleSeconds = 60;
        const cycleCount = Math.max(1, Math.ceil(totalDurationSeconds / nominalCycleSeconds));

        const steps = [];
        for (let cycleIndex = 1; cycleIndex <= cycleCount; cycleIndex += 1) {
            steps.push({
                targetMbps: highTargetMbps,
                holdSeconds: 30,
                postSettleHoldSeconds: 30,
                maxStepSeconds: 240,
                direction: 'up',
                mode: 'emergency-downshift',
                stepKind: 'emergency-high',
                settleOn: 'rendition-match',
                settleTargetRenditionMbps: ladderMbps[ladderMbps.length - 1],
                settleTolerancePct: 10,
                cycleIndex,
                phase: 'emergency-high'
            });
            steps.push({
                targetMbps: lowTargetMbps,
                holdSeconds: 30,
                postSettleHoldSeconds: 30,
                maxStepSeconds: 240,
                direction: 'down',
                mode: 'emergency-downshift',
                stepKind: 'emergency-low',
                settleOn: 'rendition-match',
                settleTargetRenditionMbps: ladderMbps[0],
                settleTolerancePct: 10,
                cycleIndex,
                phase: 'emergency-low'
            });
        }

        return {
            steps,
            ladderMbps,
            emergencyConfig: {
                cycleCount,
                totalDurationSeconds,
                segmentSeconds: segSeconds,
                overheadPct,
                highTargetMbps,
                lowTargetMbps,
                topVariantMediaMbps: ladderMbps[ladderMbps.length - 1],
                bottomVariantMediaMbps: ladderMbps[0],
                lowMidpointMediaMbps,
                bottomMarginPct: Math.round(marginRatio * 1000) / 10,
                topShiftMode: 'top_variant_x3',
                downshiftLatencyThresholdSeconds: downshiftLatencyThresholdS,
                bottomReachThresholdSeconds: bottomReachThresholdS
            }
        };
    }

    function buildEmergencyDownshiftResults(runState) {
        const medianOf = (values) => median(values.map((value) => toNumber(value, null)).filter((value) => value !== null));
        const config = runState && runState.emergencyConfig ? runState.emergencyConfig : null;
        if (!config) return null;
        const ladder = Array.isArray(runState.ladderMbps) ? runState.ladderMbps : [];
        if (!ladder.length) return null;

        const samples = Array.isArray(runState.samples) ? runState.samples : [];
        const switchEvents = Array.isArray(runState.switchEvents) ? runState.switchEvents : [];
        const cycles = [];

        for (let cycleIndex = 1; cycleIndex <= config.cycleCount; cycleIndex += 1) {
            const highStepIndex = runState.steps.findIndex((step) => Number(step.cycleIndex) === cycleIndex && step.stepKind === 'emergency-high');
            const lowStepIndex = runState.steps.findIndex((step) => Number(step.cycleIndex) === cycleIndex && step.stepKind === 'emergency-low');
            const preconditionStepIndex = highStepIndex;
            const cliffStepIndex = lowStepIndex;
            const preconditionTiming = preconditionStepIndex >= 0 ? runState.stepTiming[preconditionStepIndex] : null;
            const stepTiming = cliffStepIndex >= 0 ? runState.stepTiming[cliffStepIndex] : null;
            const preconditionSamples = samples.filter((sample) => Number(sample.stepIndex) === preconditionStepIndex);
            const stepSamples = samples.filter((sample) => Number(sample.stepIndex) === cliffStepIndex);
            const stepSwitches = switchEvents
                .filter((event) => Number(event.stepIndex) === cliffStepIndex && toNumber(event.toVariantMbps, null) < toNumber(event.fromVariantMbps, null))
                .sort((a, b) => toNumber(a.tSinceStart, 0) - toNumber(b.tSinceStart, 0));
            const upSwitches = switchEvents
                .filter((event) => Number(event.stepIndex) === preconditionStepIndex && toNumber(event.toVariantMbps, null) > toNumber(event.fromVariantMbps, null))
                .sort((a, b) => toNumber(a.tSinceStart, 0) - toNumber(b.tSinceStart, 0));

            const stallCounts = stepSamples
                .map((sample) => toNumber(sample.stallCount, null))
                .filter((value) => value !== null);
            const stallTimes = stepSamples
                .map((sample) => toNumber(sample.stallTimeSeconds, null))
                .filter((value) => value !== null);
            const buffers = stepSamples
                .map((sample) => toNumber(sample.bufferSeconds, null))
                .filter((value) => value !== null);

            const stallCountDelta = stallCounts.length ? (Math.max(...stallCounts) - Math.min(...stallCounts)) : null;
            const stallTimeDeltaSeconds = stallTimes.length ? (Math.max(...stallTimes) - Math.min(...stallTimes)) : null;
            const minimumBufferSeconds = buffers.length ? Math.min(...buffers) : null;
            const firstDownswitchLatencySeconds = stepSwitches.length
                ? toNumber(stepSwitches[0].secondsAfterLimitChange, null)
                : null;
            const firstUpswitchLatencySeconds = upSwitches.length
                ? toNumber(upSwitches[0].secondsAfterLimitChange, null)
                : null;

            let reachedBottomSeconds = null;
            if (stepSamples.length && stepTiming && Number.isFinite(stepTiming.limitAppliedAt)) {
                const bottomSample = stepSamples.find((sample) => nearestVariantIndex(ladder, sample.variantMbps) === 0);
                if (bottomSample) {
                    reachedBottomSeconds = (bottomSample.ts - stepTiming.limitAppliedAt) / 1000;
                }
            }

            let reachedTopSeconds = null;
            if (preconditionSamples.length && preconditionTiming && Number.isFinite(preconditionTiming.limitAppliedAt)) {
                const topSample = preconditionSamples.find((sample) => nearestVariantIndex(ladder, sample.variantMbps) === (ladder.length - 1));
                if (topSample) {
                    reachedTopSeconds = (topSample.ts - preconditionTiming.limitAppliedAt) / 1000;
                }
            }

            const noStall = (stallCountDelta === null || stallCountDelta <= 0) && (stallTimeDeltaSeconds === null || stallTimeDeltaSeconds <= 0);
            const downshiftInTime = firstDownswitchLatencySeconds !== null && firstDownswitchLatencySeconds <= config.downshiftLatencyThresholdSeconds;
            const reachedBottomInTime = reachedBottomSeconds !== null && reachedBottomSeconds <= config.bottomReachThresholdSeconds;
            const pass = noStall && downshiftInTime && reachedBottomInTime;

            const failureReasons = [];
            if (!noStall) failureReasons.push('stall_observed');
            if (!downshiftInTime) failureReasons.push('downshift_latency_exceeded_or_missing');
            if (!reachedBottomInTime) failureReasons.push('bottom_rung_not_reached_in_threshold');
            if (cliffStepIndex < 0) failureReasons.push('missing_cliff_step');
            if (preconditionStepIndex < 0) failureReasons.push('missing_precondition_step');

            cycles.push({
                cycle: cycleIndex,
                precondition_step_index: preconditionStepIndex,
                cliff_step_index: cliffStepIndex,
                sample_count: stepSamples.length,
                first_downswitch_latency_s: firstDownswitchLatencySeconds,
                reached_bottom_s: reachedBottomSeconds,
                first_upswitch_latency_s: firstUpswitchLatencySeconds,
                reached_top_s: reachedTopSeconds,
                stall_count_delta: stallCountDelta,
                stall_time_delta_s: stallTimeDeltaSeconds,
                minimum_buffer_s: minimumBufferSeconds,
                pass,
                failure_reasons: failureReasons
            });
        }

        const passCount = cycles.filter((cycle) => cycle.pass).length;
        const passRatePct = cycles.length ? (passCount / cycles.length) * 100 : 0;
        const aggregate = {
            upshift: {
                first_switch_latency_median_s: medianOf(cycles.map((cycle) => cycle.first_upswitch_latency_s)),
                reach_target_median_s: medianOf(cycles.map((cycle) => cycle.reached_top_s))
            },
            downshift: {
                first_switch_latency_median_s: medianOf(cycles.map((cycle) => cycle.first_downswitch_latency_s)),
                reach_target_median_s: medianOf(cycles.map((cycle) => cycle.reached_bottom_s))
            }
        };

        return {
            thresholds: {
                downshift_latency_s: config.downshiftLatencyThresholdSeconds,
                reach_bottom_s: config.bottomReachThresholdSeconds,
                no_stall_required: true
            },
            cycles,
            aggregate,
            pass_count: passCount,
            trial_count: cycles.length,
            pass_rate_pct: Math.round(passRatePct * 10) / 10
        };
    }

    function buildTransientShockSchedule(options) {
        const { variants, minMbps, maxMbps, networkOverheadPct } = options;
        const ladderMbps = uniqueSortedPositive((variants || []).map((variant) => toMbpsFromVariant(variant)));
        if (!ladderMbps.length) {
            return { steps: [], config: null };
        }

        const overheadPct = Number.isFinite(Number(networkOverheadPct))
            ? Math.max(0, Math.min(25, Number(networkOverheadPct)))
            : 10;
        const wireMultiplier = 1 / (1 - (overheadPct / 100));
        const floor = Math.max(0.1, Number.isFinite(minMbps) ? minMbps : ladderMbps[0] * wireMultiplier);
        const ceiling = Math.max(floor, Number.isFinite(maxMbps) ? maxMbps : ladderMbps[ladderMbps.length - 1] * wireMultiplier * 2);
        const topWire = ladderMbps[ladderMbps.length - 1] * wireMultiplier;
        const baselineTarget = Math.max(floor, Math.min(ceiling, topWire * 1.15));
        const severities = [
            { key: 'small', dropPct: 30 },
            { key: 'medium', dropPct: 55 },
            { key: 'severe', dropPct: 80 }
        ];

        const steps = [];
        severities.forEach((severity, orderIndex) => {
            const shockTarget = Math.max(floor, Math.min(ceiling, baselineTarget * (1 - (severity.dropPct / 100))));
            steps.push({
                targetMbps: baselineTarget,
                holdSeconds: 20,
                direction: 'up',
                mode: 'transient-shock',
                stepKind: 'transient-precondition',
                shockSeverity: severity.key,
                shockDropPct: severity.dropPct,
                shockOrder: orderIndex + 1
            });
            steps.push({
                targetMbps: shockTarget,
                holdSeconds: 8,
                maxStepSeconds: 40,
                skipSettle: true,
                forceHoldWithoutSettle: true,
                direction: 'down',
                mode: 'transient-shock',
                stepKind: 'transient-shock',
                shockSeverity: severity.key,
                shockDropPct: severity.dropPct,
                shockOrder: orderIndex + 1
            });
            steps.push({
                targetMbps: baselineTarget,
                holdSeconds: 20,
                maxStepSeconds: 80,
                direction: 'up',
                mode: 'transient-shock',
                stepKind: 'transient-recovery',
                shockSeverity: severity.key,
                shockDropPct: severity.dropPct,
                shockOrder: orderIndex + 1
            });
        });

        return {
            steps,
            config: {
                baselineTargetMbps: baselineTarget,
                severities
            }
        };
    }

    function buildStartupCapsSchedule(options) {
        const { variants, minMbps, maxMbps, networkOverheadPct } = options;
        const ladderMbps = uniqueSortedPositive((variants || []).map((variant) => toMbpsFromVariant(variant)));
        if (!ladderMbps.length) {
            return { steps: [], config: null };
        }

        const overheadPct = Number.isFinite(Number(networkOverheadPct))
            ? Math.max(0, Math.min(25, Number(networkOverheadPct)))
            : 10;
        const wireMultiplier = 1 / (1 - (overheadPct / 100));
        const floor = Math.max(0.1, Number.isFinite(minMbps) ? minMbps : ladderMbps[0] * wireMultiplier);
        const ceiling = Math.max(floor, Number.isFinite(maxMbps) ? maxMbps : ladderMbps[ladderMbps.length - 1] * wireMultiplier * 2);
        const midIndex = Math.floor((ladderMbps.length - 1) / 2);
        const topIndex = Math.max(0, ladderMbps.length - 2);
        const caps = [
            {
                capLabel: 'low',
                capTargetMbps: Math.max(floor, Math.min(ceiling, ladderMbps[0] * wireMultiplier * 1.08))
            },
            {
                capLabel: 'mid',
                capTargetMbps: Math.max(floor, Math.min(ceiling, ladderMbps[midIndex] * wireMultiplier * 1.08))
            },
            {
                capLabel: 'high',
                capTargetMbps: Math.max(floor, Math.min(ceiling, ladderMbps[topIndex] * wireMultiplier * 1.08))
            }
        ];

        const steps = caps.map((cap, index) => ({
            targetMbps: cap.capTargetMbps,
            holdSeconds: 45,
            maxStepSeconds: 120,
            skipSettle: true,
            forceHoldWithoutSettle: true,
            restartPlaybackBeforeStep: true,
            direction: 'up',
            mode: 'startup-caps',
            stepKind: 'startup-cap',
            startupCapLabel: cap.capLabel,
            startupScenarioIndex: index + 1
        }));

        return {
            steps,
            config: {
                caps
            }
        };
    }

    function buildDownshiftSeveritySchedule(options) {
        const { variants, minMbps, maxMbps, networkOverheadPct } = options;
        const ladderMbps = uniqueSortedPositive((variants || []).map((variant) => toMbpsFromVariant(variant)));
        if (!ladderMbps.length) {
            return { steps: [], config: null };
        }
        const overheadPct = Number.isFinite(Number(networkOverheadPct))
            ? Math.max(0, Math.min(25, Number(networkOverheadPct)))
            : 10;
        const wireMultiplier = 1 / (1 - (overheadPct / 100));
        const floor = Math.max(0.1, Number.isFinite(minMbps) ? minMbps : ladderMbps[0] * wireMultiplier);
        const ceiling = Math.max(floor, Number.isFinite(maxMbps) ? maxMbps : ladderMbps[ladderMbps.length - 1] * wireMultiplier * 2);
        const topWire = ladderMbps[ladderMbps.length - 1] * wireMultiplier;
        const highTarget = Math.max(floor, Math.min(ceiling, topWire * 1.12));
        const severityBuckets = [
            { key: 'small', minDropPct: 20, maxDropPct: 40, targetDropPct: 30 },
            { key: 'medium', minDropPct: 40, maxDropPct: 70, targetDropPct: 55 },
            { key: 'severe', minDropPct: 70, maxDropPct: 100, targetDropPct: 80 }
        ];

        const steps = [];
        severityBuckets.forEach((bucket, index) => {
            const target = Math.max(floor, Math.min(ceiling, highTarget * (1 - (bucket.targetDropPct / 100))));
            steps.push({
                targetMbps: highTarget,
                holdSeconds: 15,
                direction: 'up',
                mode: 'downshift-severity',
                stepKind: 'severity-precondition',
                severityBucket: bucket.key,
                severityDropPct: bucket.targetDropPct,
                severityOrder: index + 1
            });
            steps.push({
                targetMbps: target,
                holdSeconds: 25,
                maxStepSeconds: 90,
                direction: 'down',
                mode: 'downshift-severity',
                stepKind: 'severity-drop',
                severityBucket: bucket.key,
                severityDropPct: bucket.targetDropPct,
                severityOrder: index + 1
            });
        });

        return {
            steps,
            config: {
                highTargetMbps: highTarget,
                severityBuckets
            }
        };
    }

    function buildHysteresisGapSchedule(options) {
        const { variants, minMbps, maxMbps, networkOverheadPct } = options;
        const ladderMbps = uniqueSortedPositive((variants || []).map((variant) => toMbpsFromVariant(variant)));
        if (ladderMbps.length < 2) {
            return { steps: [], config: null };
        }
        const overheadPct = Number.isFinite(Number(networkOverheadPct))
            ? Math.max(0, Math.min(25, Number(networkOverheadPct)))
            : 10;
        const wireMultiplier = 1 / (1 - (overheadPct / 100));
        const floor = Math.max(0.1, Number.isFinite(minMbps) ? minMbps : ladderMbps[0] * wireMultiplier);
        const ceiling = Math.max(floor, Number.isFinite(maxMbps) ? maxMbps : ladderMbps[ladderMbps.length - 1] * wireMultiplier * 2);
        const steps = [];

        for (let lowIndex = 0; lowIndex < ladderMbps.length - 1; lowIndex += 1) {
            const highIndex = lowIndex + 1;
            const downProbeTarget = Math.max(floor, Math.min(ceiling, ladderMbps[lowIndex] * wireMultiplier * 1.02));
            const upProbeTarget = Math.max(floor, Math.min(ceiling, ladderMbps[highIndex] * wireMultiplier * 0.98));
            steps.push({
                targetMbps: downProbeTarget,
                holdSeconds: 18,
                maxStepSeconds: 90,
                direction: 'down',
                mode: 'hysteresis-gap',
                stepKind: 'hysteresis-down-probe',
                rungLowIndex: lowIndex,
                rungHighIndex: highIndex,
                rungLabel: `V${lowIndex + 1}↔V${highIndex + 1}`
            });
            steps.push({
                targetMbps: upProbeTarget,
                holdSeconds: 18,
                maxStepSeconds: 90,
                direction: 'up',
                mode: 'hysteresis-gap',
                stepKind: 'hysteresis-up-probe',
                rungLowIndex: lowIndex,
                rungHighIndex: highIndex,
                rungLabel: `V${lowIndex + 1}↔V${highIndex + 1}`
            });
        }

        return {
            steps,
            config: {
                pairCount: ladderMbps.length - 1
            }
        };
    }

    function computeStallDeltas(samples) {
        const stallCounts = samples
            .map((sample) => toNumber(sample.stallCount, null))
            .filter((value) => value !== null);
        const stallTimes = samples
            .map((sample) => toNumber(sample.stallTimeSeconds, null))
            .filter((value) => value !== null);
        return {
            stallCountDelta: stallCounts.length ? (Math.max(...stallCounts) - Math.min(...stallCounts)) : null,
            stallTimeDeltaSeconds: stallTimes.length ? (Math.max(...stallTimes) - Math.min(...stallTimes)) : null
        };
    }

    function buildStartupCapsResults(runState) {
        const startupSteps = (runState.steps || [])
            .map((step, index) => ({ step, index }))
            .filter(({ step }) => step.mode === 'startup-caps' && step.stepKind === 'startup-cap');
        if (!startupSteps.length) return null;

        const scenarios = startupSteps.map(({ step, index }) => {
            const stepSamples = (runState.samples || []).filter((sample) => Number(sample.stepIndex) === index);
            const stepTiming = runState.stepTiming[index] || null;
            const firstVariantSample = stepSamples.find((sample) => toNumber(sample.variantMbps, null) !== null);
            const firstStartupSeconds = (firstVariantSample && stepTiming && Number.isFinite(stepTiming.limitAppliedAt))
                ? ((firstVariantSample.ts - stepTiming.limitAppliedAt) / 1000)
                : null;
            const firstRendition = firstVariantSample ? toNumber(firstVariantSample.variantMbps, null) : null;
            const buffers = stepSamples
                .map((sample) => toNumber(sample.bufferSeconds, null))
                .filter((value) => value !== null);
            const minBuffer = buffers.length ? Math.min(...buffers) : null;
            const stalls = computeStallDeltas(stepSamples);
            const restartCount = (runState.playbackStoppedEvents || []).filter((event) => Number(event.stepIndex) === index).length;
            return {
                scenario: step.startupScenarioIndex || (index + 1),
                cap_label: step.startupCapLabel || 'unknown',
                cap_target_mbps: toNumber(step.targetMbps, null),
                startup_latency_s: firstStartupSeconds,
                first_rendition_mbps: firstRendition,
                minimum_buffer_s: minBuffer,
                stall_count_delta: stalls.stallCountDelta,
                stall_time_delta_s: stalls.stallTimeDeltaSeconds,
                restart_count: restartCount
            };
        });

        return {
            scenarios,
            aggregate: {
                startup_latency_median_s: median(scenarios.map((scenario) => toNumber(scenario.startup_latency_s, null)).filter((value) => value !== null)),
                stall_count_delta_total: scenarios.reduce((sum, scenario) => sum + (toNumber(scenario.stall_count_delta, 0) || 0), 0),
                restart_count_total: scenarios.reduce((sum, scenario) => sum + (toNumber(scenario.restart_count, 0) || 0), 0)
            }
        };
    }

    function buildTransientShockResults(runState) {
        const severities = ['small', 'medium', 'severe'];
        const summaries = severities.map((severity) => {
            const shockSteps = (runState.steps || [])
                .map((step, index) => ({ step, index }))
                .filter(({ step }) => step.mode === 'transient-shock' && step.shockSeverity === severity && step.stepKind === 'transient-shock');
            if (!shockSteps.length) {
                return {
                    severity,
                    drop_pct: null,
                    downswitch_count: 0,
                    downswitch_latency_median_s: null,
                    recovery_upshift_latency_median_s: null,
                    stall_count_delta_total: 0,
                    stall_time_delta_s_total: 0,
                    unexpected_downswitch_during_recovery: 0
                };
            }

            const downLatencies = [];
            const recoveryLatencies = [];
            let downswitchCount = 0;
            let unexpectedRecoveryDownswitchCount = 0;
            let stallCountTotal = 0;
            let stallTimeTotal = 0;
            let representativeDrop = null;

            shockSteps.forEach(({ step, index }) => {
                const shockSwitches = (runState.switchEvents || [])
                    .filter((event) => Number(event.stepIndex) === index);
                const downSwitches = shockSwitches.filter((event) => toNumber(event.toVariantMbps, null) < toNumber(event.fromVariantMbps, null));
                const firstDownLatency = downSwitches.length ? toNumber(downSwitches[0].secondsAfterLimitChange, null) : null;
                if (firstDownLatency !== null) downLatencies.push(firstDownLatency);
                downswitchCount += downSwitches.length;
                representativeDrop = representativeDrop === null ? toNumber(step.shockDropPct, null) : representativeDrop;

                const shockSamples = (runState.samples || []).filter((sample) => Number(sample.stepIndex) === index);
                const shockStalls = computeStallDeltas(shockSamples);
                stallCountTotal += toNumber(shockStalls.stallCountDelta, 0) || 0;
                stallTimeTotal += toNumber(shockStalls.stallTimeDeltaSeconds, 0) || 0;

                const recoveryStepIndex = (runState.steps || []).findIndex((candidate, candidateIndex) => {
                    if (candidateIndex <= index) return false;
                    return candidate.mode === 'transient-shock'
                        && candidate.stepKind === 'transient-recovery'
                        && candidate.shockSeverity === severity
                        && Number(candidate.shockOrder) === Number(step.shockOrder);
                });
                if (recoveryStepIndex >= 0) {
                    const recoverySwitches = (runState.switchEvents || []).filter((event) => Number(event.stepIndex) === recoveryStepIndex);
                    const recoveryUp = recoverySwitches.filter((event) => toNumber(event.toVariantMbps, null) > toNumber(event.fromVariantMbps, null));
                    const recoveryDown = recoverySwitches.filter((event) => toNumber(event.toVariantMbps, null) < toNumber(event.fromVariantMbps, null));
                    if (recoveryUp.length) {
                        const firstRecoveryUp = toNumber(recoveryUp[0].secondsAfterLimitChange, null);
                        if (firstRecoveryUp !== null) recoveryLatencies.push(firstRecoveryUp);
                    }
                    unexpectedRecoveryDownswitchCount += recoveryDown.length;
                    const recoverySamples = (runState.samples || []).filter((sample) => Number(sample.stepIndex) === recoveryStepIndex);
                    const recoveryStalls = computeStallDeltas(recoverySamples);
                    stallCountTotal += toNumber(recoveryStalls.stallCountDelta, 0) || 0;
                    stallTimeTotal += toNumber(recoveryStalls.stallTimeDeltaSeconds, 0) || 0;
                }
            });

            return {
                severity,
                drop_pct: representativeDrop,
                downswitch_count: downswitchCount,
                downswitch_latency_median_s: median(downLatencies),
                recovery_upshift_latency_median_s: median(recoveryLatencies),
                stall_count_delta_total: stallCountTotal,
                stall_time_delta_s_total: Math.round(stallTimeTotal * 100) / 100,
                unexpected_downswitch_during_recovery: unexpectedRecoveryDownswitchCount
            };
        });

        if (!summaries.some((summary) => summary.downswitch_count > 0 || summary.recovery_upshift_latency_median_s !== null)) {
            return null;
        }
        return {
            severities: summaries
        };
    }

    function buildDownshiftSeverityResults(runState) {
        const steps = (runState.steps || [])
            .map((step, index) => ({ step, index }))
            .filter(({ step }) => step.mode === 'downshift-severity' && step.stepKind === 'severity-drop');
        if (!steps.length) return null;

        const buckets = [
            { key: 'small', minDropPct: 20, maxDropPct: 40 },
            { key: 'medium', minDropPct: 40, maxDropPct: 70 },
            { key: 'severe', minDropPct: 70, maxDropPct: 100 }
        ];

        const summaryByBucket = buckets.map((bucket) => {
            const latencies = [];
            let matchedSteps = 0;
            steps.forEach(({ step, index }) => {
                const dropPct = toNumber(step.severityDropPct, null);
                if (dropPct === null || dropPct < bucket.minDropPct || dropPct >= bucket.maxDropPct) return;
                matchedSteps += 1;
                const downSwitches = (runState.switchEvents || [])
                    .filter((event) => Number(event.stepIndex) === index)
                    .filter((event) => toNumber(event.toVariantMbps, null) < toNumber(event.fromVariantMbps, null));
                const firstLatency = downSwitches.length ? toNumber(downSwitches[0].secondsAfterLimitChange, null) : null;
                if (firstLatency !== null) {
                    latencies.push(firstLatency);
                }
            });
            return {
                severity: bucket.key,
                drop_pct_range: `${bucket.minDropPct}-${bucket.maxDropPct}`,
                sample_count: matchedSteps,
                min_latency_s: latencies.length ? Math.min(...latencies) : null,
                median_latency_s: latencies.length ? median(latencies) : null,
                p95_latency_s: latencies.length ? percentile(latencies, 95) : null,
                max_latency_s: latencies.length ? Math.max(...latencies) : null
            };
        });

        return {
            buckets: summaryByBucket
        };
    }

    function buildHysteresisGapResults(runState) {
        const stepMap = (runState.steps || []).map((step, index) => ({ step, index }));
        const pairRows = [];
        const pairKeys = new Set(
            stepMap
                .filter(({ step }) => step.mode === 'hysteresis-gap')
                .map(({ step }) => `${step.rungLowIndex}:${step.rungHighIndex}`)
        );
        if (!pairKeys.size) return null;

        pairKeys.forEach((pairKey) => {
            const [lowIndexRaw, highIndexRaw] = pairKey.split(':');
            const lowIndex = Number(lowIndexRaw);
            const highIndex = Number(highIndexRaw);
            const downStep = stepMap.find(({ step }) => step.mode === 'hysteresis-gap' && step.stepKind === 'hysteresis-down-probe' && Number(step.rungLowIndex) === lowIndex && Number(step.rungHighIndex) === highIndex);
            const upStep = stepMap.find(({ step }) => step.mode === 'hysteresis-gap' && step.stepKind === 'hysteresis-up-probe' && Number(step.rungLowIndex) === lowIndex && Number(step.rungHighIndex) === highIndex);

            const downSwitches = downStep
                ? (runState.switchEvents || []).filter((event) => Number(event.stepIndex) === downStep.index && toNumber(event.toVariantMbps, null) < toNumber(event.fromVariantMbps, null))
                : [];
            const upSwitches = upStep
                ? (runState.switchEvents || []).filter((event) => Number(event.stepIndex) === upStep.index && toNumber(event.toVariantMbps, null) > toNumber(event.fromVariantMbps, null))
                : [];

            const alphaDownValues = downSwitches
                .filter((event) => toNumber(event.throughputMbps, null) && toNumber(event.toVariantMbps, null))
                .map((event) => event.toVariantMbps / event.throughputMbps);
            const alphaUpValues = upSwitches
                .filter((event) => toNumber(event.throughputMbps, null) && toNumber(event.toVariantMbps, null))
                .map((event) => event.toVariantMbps / event.throughputMbps);
            const alphaDownMedian = median(alphaDownValues);
            const alphaUpMedian = median(alphaUpValues);
            const gap = (alphaDownMedian !== null && alphaUpMedian !== null)
                ? (alphaUpMedian - alphaDownMedian)
                : null;

            pairRows.push({
                rung_pair: `V${lowIndex + 1}↔V${highIndex + 1}`,
                alpha_down_median: alphaDownMedian,
                alpha_up_median: alphaUpMedian,
                hysteresis_gap: gap,
                downshift_events: downSwitches.length,
                upshift_events: upSwitches.length
            });
        });

        return {
            pairs: pairRows,
            aggregate: {
                gap_median: median(pairRows.map((row) => toNumber(row.hysteresis_gap, null)).filter((value) => value !== null))
            }
        };
    }

    function renderSummary(summary) {
        const lines = [];
        lines.push(`Run complete in ${summary.durationSeconds}s across ${summary.stepCount} steps.`);
        lines.push(`Detected ${summary.switchCount} variant switch events (${summary.downswitches} down / ${summary.upswitches} up).`);
        lines.push(`Playback-stopped events: ${summary.playbackStoppedCount}.`);
        if (toNumber(summary.shapingWarningCount, 0) > 0) {
            lines.push(`Shaping dataplane warnings: ${summary.shapingWarningCount}.`);
        }
        lines.push(`Median throughput at downswitch: ${formatMbps(summary.downswitchMedianMbps)}.`);
        lines.push(`Median throughput at upswitch: ${formatMbps(summary.upswitchMedianMbps)}.`);
        if (summary.alphaDownMedian !== null) {
            lines.push(`Median α_down: ${Math.round(summary.alphaDownMedian * 1000) / 1000}.`);
        }
        if (summary.alphaUpMedian !== null) {
            lines.push(`Median α_up: ${Math.round(summary.alphaUpMedian * 1000) / 1000}.`);
        }
        if (summary.emergencyDownshift) {
            const emergency = summary.emergencyDownshift;
            lines.push(`Emergency pass rate: ${emergency.pass_count}/${emergency.trial_count} (${emergency.pass_rate_pct.toFixed(1)}%).`);
        }
        if (summary.transientShock && Array.isArray(summary.transientShock.severities)) {
            const severe = summary.transientShock.severities.find((entry) => entry.severity === 'severe');
            if (severe && toNumber(severe.downswitch_latency_median_s, null) !== null) {
                lines.push(`Transient severe-shock median downshift latency: ${toNumber(severe.downswitch_latency_median_s, 0).toFixed(2)}s.`);
            }
        }
        if (summary.startupCaps && summary.startupCaps.aggregate && toNumber(summary.startupCaps.aggregate.startup_latency_median_s, null) !== null) {
            lines.push(`Startup-caps median startup latency: ${toNumber(summary.startupCaps.aggregate.startup_latency_median_s, 0).toFixed(2)}s.`);
        }
        if (summary.hysteresisGap && summary.hysteresisGap.aggregate && toNumber(summary.hysteresisGap.aggregate.gap_median, null) !== null) {
            lines.push(`Hysteresis median gap (per-rung): ${(Math.round(summary.hysteresisGap.aggregate.gap_median * 1000) / 1000)}.`);
        }
        return lines.join(' ');
    }

    function buildReportMarkdown(runState, summary) {
        const pad = (value, width) => String(value).padEnd(width, ' ');
        const fmt = (value, digits = 2) => {
            const numeric = toNumber(value, null);
            return numeric === null ? '—' : numeric.toFixed(digits);
        };
        const fmtPct = (value) => {
            const numeric = toNumber(value, null);
            return numeric === null ? '—' : `${numeric.toFixed(1)}%`;
        };
        const fmtInt = (value) => {
            const numeric = toNumber(value, null);
            return numeric === null ? '-' : String(Math.round(numeric));
        };
        const fmtVariant = (label, mbps) => {
            if (!label || label === '—') return fmt(mbps);
            return `${label} ${fmt(mbps)}`;
        };
        const fmtStallCount = (value) => {
            const numeric = toNumber(value, null);
            return numeric === null ? '-' : String(Math.round(numeric));
        };
        const fmtStallTime = (value) => {
            const numeric = toNumber(value, null);
            return numeric === null ? '-' : numeric.toFixed(2);
        };

        const lines = [];
        lines.push('# Player Characterization Report');
        lines.push('');
        lines.push(`- Session: ${runState.sessionId}`);
        lines.push(`- Master URL: ${runState.masterUrl || 'unknown'}`);
        lines.push(`- Started: ${new Date(runState.startedAt).toISOString()}`);
        lines.push(`- Duration: ${summary.durationSeconds}s`);
        lines.push(`- Steps: ${summary.stepCount}`);
        lines.push('');
        lines.push('## Summary');
        lines.push('');
        lines.push(`- Switches: ${summary.switchCount} (${summary.downswitches} down / ${summary.upswitches} up)`);
        lines.push(`- Playback-stopped events: ${summary.playbackStoppedCount}`);
        if (toNumber(summary.shapingWarningCount, 0) > 0) {
            lines.push(`- Shaping dataplane warnings: ${summary.shapingWarningCount}`);
        }
        lines.push(`- Median downswitch throughput: ${formatMbps(summary.downswitchMedianMbps)}`);
        lines.push(`- Median upswitch throughput: ${formatMbps(summary.upswitchMedianMbps)}`);
        lines.push(`- Median α_down: ${summary.alphaDownMedian !== null ? (Math.round(summary.alphaDownMedian * 1000) / 1000) : '—'}`);
        lines.push(`- Median α_up: ${summary.alphaUpMedian !== null ? (Math.round(summary.alphaUpMedian * 1000) / 1000) : '—'}`);
        if (summary.emergencyDownshift) {
            lines.push(`- Emergency pass rate: ${summary.emergencyDownshift.pass_count}/${summary.emergencyDownshift.trial_count} (${summary.emergencyDownshift.pass_rate_pct.toFixed(1)}%)`);
            lines.push(`- Emergency threshold (downshift): ≤ ${summary.emergencyDownshift.thresholds.downshift_latency_s}s`);
            lines.push(`- Emergency threshold (reach bottom): ≤ ${summary.emergencyDownshift.thresholds.reach_bottom_s}s`);
            const aggregate = summary.emergencyDownshift.aggregate || {};
            if (aggregate.upshift) {
                lines.push(`- Emergency upshift median latency: ${fmt(aggregate.upshift.first_switch_latency_median_s)}s (reach-top ${fmt(aggregate.upshift.reach_target_median_s)}s)`);
            }
            if (aggregate.downshift) {
                lines.push(`- Emergency downshift median latency: ${fmt(aggregate.downshift.first_switch_latency_median_s)}s (reach-bottom ${fmt(aggregate.downshift.reach_target_median_s)}s)`);
            }
        }
        if (summary.transientShock && Array.isArray(summary.transientShock.severities)) {
            const severeShock = summary.transientShock.severities.find((entry) => entry.severity === 'severe');
            if (severeShock) {
                lines.push(`- Transient shock (severe) median downshift latency: ${fmt(severeShock.downswitch_latency_median_s)}s`);
                lines.push(`- Transient shock (severe) median recovery-upshift latency: ${fmt(severeShock.recovery_upshift_latency_median_s)}s`);
            }
        }
        if (summary.startupCaps && summary.startupCaps.aggregate) {
            lines.push(`- Startup-caps median startup latency: ${fmt(summary.startupCaps.aggregate.startup_latency_median_s)}s`);
            lines.push(`- Startup-caps total restart count: ${fmtInt(summary.startupCaps.aggregate.restart_count_total)}`);
        }
        if (summary.hysteresisGap && summary.hysteresisGap.aggregate) {
            lines.push(`- Hysteresis per-rung median gap: ${fmt(summary.hysteresisGap.aggregate.gap_median, 3)}`);
        }
        lines.push('');
        if (summary.emergencyDownshift) {
            lines.push('## Emergency Downshift Trials');
            lines.push('');
            const emergencyHeader = [
                pad('cycle', 7),
                pad('pass', 6),
                pad('downshift_s', 12),
                pad('bottom_s', 10),
                pad('upswitch_s', 10),
                pad('top_s', 8),
                pad('stall_cntΔ', 10),
                pad('stall_sΔ', 9),
                pad('min_buf_s', 10),
                pad('reasons', 30)
            ].join(' | ');
            lines.push(emergencyHeader);
            lines.push('-'.repeat(emergencyHeader.length));
            (summary.emergencyDownshift.cycles || []).forEach((cycle) => {
                lines.push([
                    pad(String(cycle.cycle), 7),
                    pad(cycle.pass ? 'yes' : 'no', 6),
                    pad(fmt(cycle.first_downswitch_latency_s), 12),
                    pad(fmt(cycle.reached_bottom_s), 10),
                    pad(fmt(cycle.first_upswitch_latency_s), 10),
                    pad(fmt(cycle.reached_top_s), 8),
                    pad(fmtInt(cycle.stall_count_delta), 10),
                    pad(fmt(cycle.stall_time_delta_s), 9),
                    pad(fmt(cycle.minimum_buffer_s), 10),
                    pad((cycle.failure_reasons || []).join(', ') || '-', 30)
                ].join(' | '));
            });
            lines.push('');
        }
        if (summary.transientShock && Array.isArray(summary.transientShock.severities)) {
            lines.push('## Transient Shock Tolerance');
            lines.push('');
            const transientHeader = [
                pad('severity', 10),
                pad('drop_%', 8),
                pad('down_sw', 8),
                pad('down_lat_s', 11),
                pad('recover_up_s', 12),
                pad('stall_cntΔ', 10),
                pad('stall_sΔ', 9),
                pad('recovery_down_sw', 17)
            ].join(' | ');
            lines.push(transientHeader);
            lines.push('-'.repeat(transientHeader.length));
            summary.transientShock.severities.forEach((row) => {
                lines.push([
                    pad(row.severity || '-', 10),
                    pad(fmt(row.drop_pct), 8),
                    pad(fmtInt(row.downswitch_count), 8),
                    pad(fmt(row.downswitch_latency_median_s), 11),
                    pad(fmt(row.recovery_upshift_latency_median_s), 12),
                    pad(fmtInt(row.stall_count_delta_total), 10),
                    pad(fmt(row.stall_time_delta_s_total), 9),
                    pad(fmtInt(row.unexpected_downswitch_during_recovery), 17)
                ].join(' | '));
            });
            lines.push('');
        }
        if (summary.startupCaps && Array.isArray(summary.startupCaps.scenarios)) {
            lines.push('## Startup Robustness Under Caps');
            lines.push('');
            const startupHeader = [
                pad('scenario', 9),
                pad('cap', 8),
                pad('target', 8),
                pad('startup_s', 10),
                pad('first_rend', 11),
                pad('min_buf_s', 10),
                pad('stall_cntΔ', 10),
                pad('stall_sΔ', 9),
                pad('restarts', 8)
            ].join(' | ');
            lines.push(startupHeader);
            lines.push('-'.repeat(startupHeader.length));
            summary.startupCaps.scenarios.forEach((row) => {
                lines.push([
                    pad(String(row.scenario), 9),
                    pad(row.cap_label || '-', 8),
                    pad(fmt(row.cap_target_mbps), 8),
                    pad(fmt(row.startup_latency_s), 10),
                    pad(fmt(row.first_rendition_mbps), 11),
                    pad(fmt(row.minimum_buffer_s), 10),
                    pad(fmtInt(row.stall_count_delta), 10),
                    pad(fmt(row.stall_time_delta_s), 9),
                    pad(fmtInt(row.restart_count), 8)
                ].join(' | '));
            });
            lines.push('');
        }
        if (summary.downshiftSeverity && Array.isArray(summary.downshiftSeverity.buckets)) {
            lines.push('## Downshift Latency by Severity');
            lines.push('');
            const severityHeader = [
                pad('severity', 10),
                pad('drop_%', 10),
                pad('n', 4),
                pad('min_s', 8),
                pad('median_s', 10),
                pad('p95_s', 8),
                pad('max_s', 8)
            ].join(' | ');
            lines.push(severityHeader);
            lines.push('-'.repeat(severityHeader.length));
            summary.downshiftSeverity.buckets.forEach((row) => {
                lines.push([
                    pad(row.severity || '-', 10),
                    pad(row.drop_pct_range || '-', 10),
                    pad(fmtInt(row.sample_count), 4),
                    pad(fmt(row.min_latency_s), 8),
                    pad(fmt(row.median_latency_s), 10),
                    pad(fmt(row.p95_latency_s), 8),
                    pad(fmt(row.max_latency_s), 8)
                ].join(' | '));
            });
            lines.push('');
        }
        if (summary.hysteresisGap && Array.isArray(summary.hysteresisGap.pairs)) {
            lines.push('## Hysteresis Gap (Per Rung)');
            lines.push('');
            const hysteresisHeader = [
                pad('rung_pair', 12),
                pad('α_down', 8),
                pad('α_up', 8),
                pad('gap', 8),
                pad('down_ev', 8),
                pad('up_ev', 8)
            ].join(' | ');
            lines.push(hysteresisHeader);
            lines.push('-'.repeat(hysteresisHeader.length));
            summary.hysteresisGap.pairs.forEach((row) => {
                lines.push([
                    pad(row.rung_pair || '-', 12),
                    pad(fmt(row.alpha_down_median, 3), 8),
                    pad(fmt(row.alpha_up_median, 3), 8),
                    pad(fmt(row.hysteresis_gap, 3), 8),
                    pad(fmtInt(row.downshift_events), 8),
                    pad(fmtInt(row.upshift_events), 8)
                ].join(' | '));
            });
            lines.push('');
        }
        lines.push('## Shift Latency Summary');
        lines.push('');
        const shiftSummary = buildShiftLatencySummary(runState);
        const shiftHeader = [
            pad('direction', 10),
            pad('count', 6),
            pad('min_s', 8),
            pad('median_s', 10),
            pad('p95_s', 8),
            pad('max_s', 8)
        ].join(' | ');
        lines.push(shiftHeader);
        lines.push('-'.repeat(shiftHeader.length));
        [
            { key: 'downshift', label: 'downshift' },
            { key: 'upshift', label: 'upshift' }
        ].forEach((entry) => {
            const row = shiftSummary[entry.key] || {};
            lines.push([
                pad(entry.label, 10),
                pad(fmtInt(row.count), 6),
                pad(fmt(row.min_s), 8),
                pad(fmt(row.median_s), 10),
                pad(fmt(row.p95_s), 8),
                pad(fmt(row.max_s), 8)
            ].join(' | '));
        });
        [
            { key: 'small', label: 'down-small' },
            { key: 'medium', label: 'down-medium' },
            { key: 'severe', label: 'down-severe' }
        ].forEach((entry) => {
            const row = (shiftSummary.downshift_by_severity || {})[entry.key] || {};
            lines.push([
                pad(entry.label, 10),
                pad(fmtInt(row.count), 6),
                pad(fmt(row.min_s), 8),
                pad(fmt(row.median_s), 10),
                pad(fmt(row.p95_s), 8),
                pad(fmt(row.max_s), 8)
            ].join(' | '));
        });
        lines.push('');
        lines.push('## Period Throughput Stats');
        lines.push('');
        const periodHeader = [
            pad('period', 7),
            pad('phase', 10),
            pad('metric', 19),
            pad('target', 8),
            pad('n', 4),
            pad('avg', 8),
            pad('median', 8),
            pad('mode', 8),
            pad('avg_delta', 10),
            pad('avg_%target', 11),
            pad('stall_cntΔ', 10),
            pad('stall_sΔ', 9),
            pad('framesΔ', 8),
            pad('restarts', 8)
        ].join(' | ');
        lines.push(periodHeader);
        lines.push('-'.repeat(periodHeader.length));
        const periodStats = buildPeriodThroughputStats(runState);
        periodStats.forEach((item) => {
            const metrics = [
                {
                    name: 'active_6s',
                    stats: item.active_6s,
                    includeTarget: true
                },
                {
                    name: 'wire_throughput',
                    stats: item.wire_throughput,
                    includeTarget: false
                },
                {
                    name: 'player_est_network',
                    stats: item.player_est_network,
                    includeTarget: false
                }
            ];
            metrics.forEach((metric) => {
                lines.push([
                    pad(String(item.period), 7),
                    pad(item.phase, 10),
                    pad(metric.name, 19),
                    pad(metric.includeTarget ? fmt(item.targetMbps) : '-', 8),
                    pad(String(metric.stats.n), 4),
                    pad(fmt(metric.stats.avg), 8),
                    pad(fmt(metric.stats.median), 8),
                    pad(fmt(metric.stats.mode), 8),
                    pad(metric.includeTarget ? fmt(metric.stats.avg_delta_mbps) : '-', 10),
                    pad(metric.includeTarget ? fmtPct(metric.stats.avg_pct_of_target) : '-', 11),
                    pad(fmtInt(item.diagnostics.stall_count_delta), 10),
                    pad(fmt(item.diagnostics.stall_time_delta_s), 9),
                    pad(fmtInt(item.diagnostics.frames_displayed_delta), 8),
                    pad(fmtInt(item.diagnostics.restart_count), 8)
                ].join(' | '));
            });
        });
        lines.push('');
        lines.push('## Period Buffer Depth Stats');
        lines.push('');
        const periodBufferHeader = [
            pad('period', 7),
            pad('phase', 10),
            pad('n', 4),
            pad('avg_s', 8),
            pad('mean_s', 8),
            pad('mode_s', 8),
            pad('stdev_s', 8),
            pad('histogram', 40)
        ].join(' | ');
        lines.push(periodBufferHeader);
        lines.push('-'.repeat(periodBufferHeader.length));
        const periodBufferStats = buildPeriodBufferDepthStats(runState);
        periodBufferStats.forEach((item) => {
            lines.push([
                pad(String(item.period), 7),
                pad(item.phase, 10),
                pad(String(item.sampleCount), 4),
                pad(fmt(item.avgSeconds), 8),
                pad(fmt(item.meanSeconds), 8),
                pad(fmt(item.modeSeconds), 8),
                pad(fmt(item.stdevSeconds), 8),
                pad(item.histogramInline, 40)
            ].join(' | '));
        });
        lines.push('');
        lines.push('## Limit Response Latency');
        lines.push('');
        const limitLatencyHeader = [
            pad('period', 7),
            pad('phase', 10),
            pad('target', 8),
            pad('latency_s', 10),
            pad('transition', 26)
        ].join(' | ');
        lines.push(limitLatencyHeader);
        lines.push('-'.repeat(limitLatencyHeader.length));
        const limitLatencies = buildLimitResponseLatencies(runState);
        limitLatencies.forEach((item) => {
            const transition = `${item.from_variant_label || '—'} -> ${item.to_variant_label || '—'}`;
            lines.push([
                pad(String(item.period), 7),
                pad(item.phase, 10),
                pad(fmt(item.target_mbps), 8),
                pad(fmt(item.switch_latency_s, 2), 10),
                pad(transition, 26)
            ].join(' | '));
        });
        lines.push('');
        lines.push('## Switch Events');
        lines.push('');
        const header = [
            pad('time_s', 8),
            pad('phase', 10),
            pad('from_variant', 18),
            pad('to_variant', 18),
            pad('throughput', 11),
            pad('target', 8),
            pad('latency_s', 10),
            pad('stall_count', 11),
            pad('stall_time_s', 12)
        ].join(' | ');
        lines.push(header);
        lines.push('-'.repeat(header.length));
        (runState.switchEvents || []).forEach((event) => {
            lines.push([
                pad(fmt(event.tSinceStart, 1), 8),
                pad(phaseLabel(event.stepDirection), 10),
                pad(fmtVariant(event.fromVariantLabel, event.fromVariantMbps), 18),
                pad(fmtVariant(event.toVariantLabel, event.toVariantMbps), 18),
                pad(fmt(event.throughputMbps), 11),
                pad(fmt(event.stepTargetMbps), 8),
                pad(fmt(event.secondsAfterLimitChange, 2), 10),
                pad(fmtStallCount(event.stallCount), 11),
                pad(fmtStallTime(event.stallTimeSeconds), 12)
            ].join(' | '));
        });
        lines.push('');
        lines.push('## Backlog (Next Player Characteristics)');
        lines.push('');
        lines.push('- Live-edge resilience under sustained caps and post-restore catch-up behavior');
        lines.push('- Estimator accuracy drift (player_est_network vs wire throughput bias and lag)');
        lines.push('- Buffer depletion/refill slope modeling under noisy and bursty limits');
        lines.push('- Floor stickiness and conservative-upshift persistence after recovery');
        lines.push('');
        lines.push('## LLM Input (JSON)');
        lines.push('');
        const llmPayload = {
            session_id: runState.sessionId,
            master_url: runState.masterUrl || null,
            started_at: new Date(runState.startedAt).toISOString(),
            summary,
            period_throughput_stats: periodStats.map((item) => ({
                period: item.period,
                phase: item.phase,
                target_mbps: toNumber(item.targetMbps, null),
                active_6s: {
                    sample_count: item.active_6s.n,
                    avg_mbps: toNumber(item.active_6s.avg, null),
                    median_mbps: toNumber(item.active_6s.median, null),
                    mode_mbps: toNumber(item.active_6s.mode, null),
                    avg_delta_mbps: toNumber(item.active_6s.avg_delta_mbps, null),
                    avg_pct_of_target: toNumber(item.active_6s.avg_pct_of_target, null)
                },
                wire_throughput: {
                    sample_count: item.wire_throughput.n,
                    avg_mbps: toNumber(item.wire_throughput.avg, null),
                    median_mbps: toNumber(item.wire_throughput.median, null),
                    mode_mbps: toNumber(item.wire_throughput.mode, null)
                },
                player_est_network: {
                    sample_count: item.player_est_network.n,
                    avg_mbps: toNumber(item.player_est_network.avg, null),
                    median_mbps: toNumber(item.player_est_network.median, null),
                    mode_mbps: toNumber(item.player_est_network.mode, null)
                },
                diagnostics: {
                    stall_count_delta: toNumber(item.diagnostics.stall_count_delta, null),
                    stall_time_delta_s: toNumber(item.diagnostics.stall_time_delta_s, null),
                    frames_displayed_delta: toNumber(item.diagnostics.frames_displayed_delta, null),
                    restart_count: toNumber(item.diagnostics.restart_count, null)
                }
            })),
            period_buffer_depth_stats: periodBufferStats.map((item) => ({
                period: item.period,
                phase: item.phase,
                sample_count: item.sampleCount,
                avg_s: toNumber(item.avgSeconds, null),
                mean_s: toNumber(item.meanSeconds, null),
                median_s: toNumber(item.medianSeconds, null),
                mode_s: toNumber(item.modeSeconds, null),
                stdev_s: toNumber(item.stdevSeconds, null),
                histogram: (item.histogram || []).map((bin) => ({
                    bucket: bin.key,
                    count: Number(bin.count) || 0
                }))
            })),
            limit_response_latency: limitLatencies,
            playback_stopped_events: (runState.playbackStoppedEvents || []).map((event) => ({
                time_s: toNumber(event.tSinceStart, null),
                reason: event.reason || null,
                observed_stall_seconds: toNumber(event.observedStallSeconds, null),
                metric_source: event.metricSource || null
            })),
            switches: (runState.switchEvents || []).map((event) => ({
                time_s: toNumber(event.tSinceStart, null),
                phase: phaseLabel(event.stepDirection),
                from_variant_label: event.fromVariantLabel || null,
                from_variant_mbps: toNumber(event.fromVariantMbps, null),
                to_variant_label: event.toVariantLabel || null,
                to_variant_mbps: toNumber(event.toVariantMbps, null),
                transition: `${event.fromVariantLabel || '—'} -> ${event.toVariantLabel || '—'}`,
                throughput_mbps: toNumber(event.throughputMbps, null),
                target_mbps: toNumber(event.stepTargetMbps, null),
                switch_latency_s: toNumber(event.secondsAfterLimitChange, null),
                buffer_seconds: toNumber(event.bufferSeconds, null),
                stall_count: toNumber(event.stallCount, null),
                stall_time_s: toNumber(event.stallTimeSeconds, null)
            })),
            transient_shock: summary.transientShock || null,
            startup_caps: summary.startupCaps || null,
            downshift_severity: summary.downshiftSeverity || null,
            hysteresis_gap: summary.hysteresisGap || null,
            shift_latency_summary: shiftSummary,
            notes: [
                'Null values indicate missing telemetry in that sample.',
                'Use phase + transition fields to identify expected vs missing behavior.'
            ]
        };
        lines.push(JSON.stringify(llmPayload, null, 2));
        lines.push('');
        return lines.join('\n');
    }

    function downloadFile(name, content, mimeType) {
        const blob = new Blob([content], { type: mimeType });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = name;
        document.body.appendChild(a);
        a.click();
        a.remove();
        URL.revokeObjectURL(url);
    }

    function createPanel(host, idPrefix) {
        host.innerHTML = `
            <div class="abrchar-panel">
                <div class="abrchar-body">
                    <div class="abrchar-controls">
                        <label>Tests
                            <select id="${idPrefix}-tests" multiple size="7">
                                <option value="ramp-down" selected>Ramp down</option>
                                <option value="ramp-up" selected>Ramp up</option>
                                <option value="emergency-downshift">Emergency top→bottom downshift</option>
                                <option value="transient-shock">Transient shock tolerance</option>
                                <option value="startup-caps">Startup robustness under caps</option>
                                <option value="downshift-severity">Downshift latency by severity</option>
                                <option value="hysteresis-gap">Hysteresis gap</option>
                            </select>
                        </label>
                        <label>Step %
                            <input id="${idPrefix}-step" type="number" min="1" max="90" step="1" value="10" />
                        </label>
                        <label>Hold (s)
                            <input id="${idPrefix}-hold" type="number" min="5" max="120" step="1" value="20" />
                        </label>
                        <label>Net Overhead
                            <select id="${idPrefix}-overhead">
                                <option value="5">5%</option>
                                <option value="10" selected>10%</option>
                            </select>
                        </label>
                        <label>Min Mbps
                            <input id="${idPrefix}-min" type="number" min="0.1" step="0.1" value="0.6" />
                        </label>
                        <label>Max Mbps
                            <input id="${idPrefix}-max" type="number" min="0.1" step="0.1" value="0" />
                        </label>
                        <label>Seg (s)
                            <input id="${idPrefix}-segment" type="number" min="1" max="12" step="1" value="2" />
                        </label>
                        <label>Trials
                            <input id="${idPrefix}-trials" type="number" min="1" max="12" step="1" value="5" />
                        </label>
                        <label>Bottom margin %
                            <input id="${idPrefix}-bottom-margin" type="number" min="0" max="30" step="1" value="5" />
                        </label>
                        <label>Top headroom %
                            <input id="${idPrefix}-top-headroom" type="number" min="5" max="75" step="5" value="25" />
                        </label>
                        <label>Downshift SLA (s)
                            <input id="${idPrefix}-downshift-sla" type="number" min="2" max="30" step="1" value="4" />
                        </label>
                        <label>Reach-bottom SLA (s)
                            <input id="${idPrefix}-bottom-sla" type="number" min="4" max="45" step="1" value="8" />
                        </label>
                    </div>
                    <div class="abrchar-actions">
                        <button class="btn btn-primary" id="${idPrefix}-start">Start ABR Characterization</button>
                        <button class="btn btn-secondary" id="${idPrefix}-stop" disabled>Stop</button>
                        <button class="btn btn-secondary" id="${idPrefix}-force-clear">Force Clear</button>
                        <span class="status-message" id="${idPrefix}-status">Idle</span>
                    </div>
                    <div class="abrchar-ruler-wrap">
                        <div class="abrchar-ruler-head">Bitrate Ruler (advertised variants vs live limit/throughput)</div>
                        <div class="abrchar-ruler" id="${idPrefix}-ruler"></div>
                    </div>
                    <div class="abrchar-progress" id="${idPrefix}-progress"></div>
                    <div class="abrchar-summary-box" id="${idPrefix}-summary">No run yet.</div>
                    <div class="abrchar-report-box" id="${idPrefix}-report" style="display:none;"></div>
                    <div class="abrchar-downloads" id="${idPrefix}-downloads" style="display:none;">
                        <button class="btn btn-secondary" id="${idPrefix}-download-json">Download summary.json</button>
                        <button class="btn btn-secondary" id="${idPrefix}-download-md">Download report.md</button>
                    </div>
                </div>
            </div>
        `;
    }

    function mount(options) {
        const host = options.hostElement || document.getElementById(options.hostId);
        if (!host) return;

        const idBase = options.instanceKey || options.hostId || host.dataset.field || 'host';
        const idPrefix = `abrchar-${idBase}`;
        createPanel(host, idPrefix);

        const testsInput = document.getElementById(`${idPrefix}-tests`);
        const stepInput = document.getElementById(`${idPrefix}-step`);
        const holdInput = document.getElementById(`${idPrefix}-hold`);
        const overheadInput = document.getElementById(`${idPrefix}-overhead`);
        const minInput = document.getElementById(`${idPrefix}-min`);
        const maxInput = document.getElementById(`${idPrefix}-max`);
        const segmentInput = document.getElementById(`${idPrefix}-segment`);
        const trialsInput = document.getElementById(`${idPrefix}-trials`);
        const bottomMarginInput = document.getElementById(`${idPrefix}-bottom-margin`);
        const topHeadroomInput = document.getElementById(`${idPrefix}-top-headroom`);
        const downshiftSlaInput = document.getElementById(`${idPrefix}-downshift-sla`);
        const bottomSlaInput = document.getElementById(`${idPrefix}-bottom-sla`);
        const startButton = document.getElementById(`${idPrefix}-start`);
        const stopButton = document.getElementById(`${idPrefix}-stop`);
        const forceClearButton = document.getElementById(`${idPrefix}-force-clear`);
        const statusEl = document.getElementById(`${idPrefix}-status`);
        const progressEl = document.getElementById(`${idPrefix}-progress`);
        const summaryEl = document.getElementById(`${idPrefix}-summary`);
        const reportEl = document.getElementById(`${idPrefix}-report`);
        const downloadsEl = document.getElementById(`${idPrefix}-downloads`);
        const rulerEl = document.getElementById(`${idPrefix}-ruler`);
        const downloadJsonButton = document.getElementById(`${idPrefix}-download-json`);
        const downloadMdButton = document.getElementById(`${idPrefix}-download-md`);

        let runState = null;
        let cancelRequested = false;
        let latestSummary = null;
        let latestReport = '';
        let latestBufferDepthSeconds = null;
        let zeroBufferSampleCount = 0;
        let lastPlaybackRestartAt = 0;
        const rulerState = {
            ladder: [],
            limitMbps: null,
            throughputMbps: null,
            wireThroughputMbps: null,
            playerEstimateMbps: null,
            currentVariantMbps: null
        };

        function renderRuler() {
            if (!rulerEl) return;
            const ladder = Array.isArray(rulerState.ladder) ? rulerState.ladder : [];
            const values = [];
            ladder.forEach((value) => {
                const numeric = toNumber(value, null);
                if (numeric !== null) values.push(numeric);
            });
            const limit = toNumber(rulerState.limitMbps, null);
            const throughput = toNumber(rulerState.throughputMbps, null);
            const wireThroughput = toNumber(rulerState.wireThroughputMbps, null);
            const playerEstimate = toNumber(rulerState.playerEstimateMbps, null);
            const currentVariant = toNumber(rulerState.currentVariantMbps, null);
            const topVariant = ladder.length ? ladder[ladder.length - 1] : null;
            const topRangeLimit = topVariant !== null ? (topVariant * 2) : null;
            if (limit !== null) values.push(limit);
            if (throughput !== null) values.push(throughput);
            if (wireThroughput !== null) values.push(wireThroughput);
            if (playerEstimate !== null) values.push(playerEstimate);
            if (currentVariant !== null) values.push(currentVariant);
            if (topRangeLimit !== null) values.push(topRangeLimit);

            const maxValue = Math.max(1, ...values, 1);
            const minValue = 0;
            const range = Math.max(0.001, maxValue - minValue);
            const toPct = (value) => {
                const numeric = toNumber(value, null);
                if (numeric === null) return null;
                const pct = ((numeric - minValue) / range) * 100;
                return Math.max(0, Math.min(100, pct));
            };

            const markers = [];
            let currentVariantMarker = null;
            let currentVariantIndex = -1;
            if (currentVariant !== null && ladder.length) {
                let bestDistance = Number.POSITIVE_INFINITY;
                ladder.forEach((value, idx) => {
                    const distance = Math.abs(value - currentVariant);
                    if (distance < bestDistance) {
                        bestDistance = distance;
                        currentVariantIndex = idx;
                    }
                });
            }
            ladder.forEach((value, idx) => {
                const pct = toPct(value);
                if (pct === null) return;
                const marker = {
                    cls: 'variant',
                    left: pct,
                    label: `V${idx + 1}`,
                    markerType: 'variant'
                };
                if (idx === currentVariantIndex) {
                    currentVariantMarker = {
                        cls: 'current-variant',
                        left: pct,
                        label: `Playing variant V${idx + 1} ${value.toFixed(2)} Mbps`,
                        markerType: 'current-variant'
                    };
                }
                markers.push(marker);
            });
            if (limit !== null) {
                markers.push({
                    cls: 'limit',
                    left: toPct(limit),
                    label: `Limit ${limit.toFixed(2)} Mbps`,
                    markerType: 'limit'
                });
            }
            if (throughput !== null) {
                markers.push({
                    cls: 'throughput',
                    left: toPct(throughput),
                    label: `Active 6s ${throughput.toFixed(2)} Mbps`,
                    markerType: 'throughput'
                });
            }
            if (wireThroughput !== null) {
                markers.push({
                    cls: 'wire-throughput',
                    left: toPct(wireThroughput),
                    label: `Wire throughput ${wireThroughput.toFixed(2)} Mbps`,
                    markerType: 'wire-throughput'
                });
            }
            if (playerEstimate !== null) {
                markers.push({
                    cls: 'player-estimate',
                    left: toPct(playerEstimate),
                    label: `Player est. network ${playerEstimate.toFixed(2)} Mbps`,
                    markerType: 'player-estimate'
                });
            }
            if (topRangeLimit !== null) {
                markers.push({
                    cls: 'top-range',
                    left: toPct(topRangeLimit),
                    label: `200% top variant ${topRangeLimit.toFixed(2)} Mbps`,
                    markerType: 'top-range'
                });
            }
            if (currentVariantMarker) {
                markers.push(currentVariantMarker);
            }

            const laneByType = {
                'variant': 0,
                'limit': 1,
                'current-variant': 2,
                'throughput': 3,
                'wire-throughput': 4,
                'player-estimate': 5,
                'top-range': 6
            };
            const markersWithLanes = markers.map((marker) => ({
                ...marker,
                lane: laneByType[marker.markerType] ?? 0
            }));

            const maxLane = markersWithLanes.reduce((max, marker) => Math.max(max, marker.lane || 0), 0);
            const laneSpacing = 16;
            const baseHeight = 136;
            const markerTop = 18;
            const lineTopOffset = 14;
            const lineBaseHeight = 54;
            const labelBaseOffset = 2;
            const labelHeight = 12;
            const labelExtra = 6;
            const laneOffset = maxLane * laneSpacing;
            const requiredHeight = markerTop
                + lineTopOffset
                + (lineBaseHeight + laneOffset)
                + (labelBaseOffset + laneOffset)
                + labelHeight
                + labelExtra;
            rulerEl.style.height = `${Math.max(baseHeight, requiredHeight)}px`;

            rulerEl.innerHTML = `
                <div class="abrchar-ruler-track"></div>
                <div class="abrchar-ruler-max">${maxValue.toFixed(2)} Mbps</div>
                <div class="abrchar-ruler-min">0</div>
                ${markersWithLanes.map((marker) => `
                    <div class="abrchar-ruler-marker ${marker.cls}" style="left:${marker.left}%; --label-lane:${marker.lane || 0}">
                        <div class="abrchar-ruler-line"></div>
                        <div class="abrchar-ruler-label">${marker.label}</div>
                    </div>
                `).join('')}
            `;
        }

        function updateRuler(partial) {
            if (!partial || typeof partial !== 'object') return;
            if (Array.isArray(partial.ladder)) {
                rulerState.ladder = partial.ladder;
            }
            if (Object.prototype.hasOwnProperty.call(partial, 'limitMbps')) {
                rulerState.limitMbps = toNumber(partial.limitMbps, null);
            }
            if (Object.prototype.hasOwnProperty.call(partial, 'throughputMbps')) {
                rulerState.throughputMbps = toNumber(partial.throughputMbps, null);
            }
            if (Object.prototype.hasOwnProperty.call(partial, 'wireThroughputMbps')) {
                rulerState.wireThroughputMbps = toNumber(partial.wireThroughputMbps, null);
            }
            if (Object.prototype.hasOwnProperty.call(partial, 'playerEstimateMbps')) {
                rulerState.playerEstimateMbps = toNumber(partial.playerEstimateMbps, null);
            }
            if (Object.prototype.hasOwnProperty.call(partial, 'currentVariantMbps')) {
                rulerState.currentVariantMbps = toNumber(partial.currentVariantMbps, null);
            }
            renderRuler();
        }

        renderRuler();

        function emitConsole(level, message) {
            const text = `${new Date().toISOString()} ${ABRCHAR_LOG_TAG} ${message}`;
            if (level === 'error') {
                console.error(text);
                return;
            }
            if (level === 'warn') {
                console.warn(text);
                return;
            }
            console.info(text);
        }

        function withBufferDepth(message) {
            const bufferText = Number.isFinite(Number(latestBufferDepthSeconds))
                ? `${Number(latestBufferDepthSeconds).toFixed(2)}s`
                : '—';
            return `${message} | buffer=${bufferText}`;
        }

        function appendProgress(message, level = 'info') {
            const messageWithBuffer = withBufferDepth(message);
            const line = document.createElement('div');
            line.className = `abrchar-line abrchar-${level}`;
            line.textContent = `${new Date().toLocaleTimeString()} ${ABRCHAR_LOG_TAG} ${messageWithBuffer}`;
            progressEl.appendChild(line);
            progressEl.scrollTop = progressEl.scrollHeight;
            emitConsole(level, messageWithBuffer);
        }

        async function attemptPlaybackRestart() {
            const restartButton = document.getElementById('restartPlayback');
            if (restartButton) {
                restartButton.click();
                return true;
            }
            const video = document.querySelector('video#player') || document.querySelector('video');
            if (!video) return false;
            try {
                if (video.seekable && video.seekable.length > 0) {
                    const seekableEnd = video.seekable.end(video.seekable.length - 1);
                    const target = Math.max(0, seekableEnd - 0.5);
                    if (Number.isFinite(target)) {
                        video.currentTime = target;
                    }
                }
            } catch (_) {
                // ignore seek errors
            }
            try {
                await video.play();
                return true;
            } catch (_) {
                // ignore play errors
            }
            try {
                video.load();
                await video.play();
                return true;
            } catch (_) {
                return false;
            }
        }

        function formatLimitWithVariant(limitMbps) {
            const limit = toNumber(limitMbps, null);
            if (limit === null) return '—';
            const ladder = Array.isArray(rulerState.ladder) ? rulerState.ladder : [];
            if (!ladder.length) return `${limit.toFixed(2)} Mbps`;

            let nearestIndex = 0;
            let nearestValue = ladder[0];
            let nearestDistance = Math.abs(limit - nearestValue);
            for (let idx = 1; idx < ladder.length; idx += 1) {
                const candidate = ladder[idx];
                const distance = Math.abs(limit - candidate);
                if (distance < nearestDistance) {
                    nearestIndex = idx;
                    nearestValue = candidate;
                    nearestDistance = distance;
                }
            }

            const deltaPct = nearestValue > 0 ? ((limit - nearestValue) / nearestValue) * 100 : 0;
            const signedPct = `${deltaPct >= 0 ? '+' : ''}${deltaPct.toFixed(1)}%`;
            return `${limit.toFixed(2)} Mbps (V${nearestIndex + 1} ${nearestValue.toFixed(2)} Mbps ${signedPct})`;
        }

        async function fetchSessionById(sessionId) {
            if (!sessionId) return null;
            const safeJson = async (response, fallback) => {
                if (!response || !response.ok) return fallback;
                try {
                    return await response.json();
                } catch (_) {
                    return fallback;
                }
            };

            const [listResult, singleResult] = await Promise.allSettled([
                fetch('/api/sessions', { cache: 'no-store' }),
                fetch(`/api/session/${encodeURIComponent(sessionId)}`, { cache: 'no-store' })
            ]);

            const listResponse = listResult.status === 'fulfilled' ? listResult.value : null;
            const singleResponse = singleResult.status === 'fulfilled' ? singleResult.value : null;

            const sessions = await safeJson(listResponse, []);
            const fromList = Array.isArray(sessions)
                ? (sessions.find((session) => String(session?.session_id || '') === String(sessionId)) || null)
                : null;
            const fromSingle = await safeJson(singleResponse, null);

            if (fromList && !fromSingle) return fromList;
            if (fromSingle && !fromList) return fromSingle;
            if (!fromList && !fromSingle) return null;

            const throughputScore = (session) => {
                if (!session || typeof session !== 'object') return -1;
                const wireTotalBytes = toNumber(session.wire_total_bytes, -1);
                const wireActiveBytes = toNumber(session.wire_active_bytes, -1);
                const lastRequestTs = Date.parse(String(session.last_request || ''));
                if (wireTotalBytes > 0) return wireTotalBytes;
                if (wireActiveBytes > 0) return wireActiveBytes;
                return Number.isFinite(lastRequestTs) ? lastRequestTs : -1;
            };

            const listScore = throughputScore(fromList);
            const singleScore = throughputScore(fromSingle);
            if (singleScore > listScore) {
                return fromSingle;
            }
            return fromList;
        }

        async function patchSessionControlFields(sessionId, set) {
            if (!sessionId || !set || typeof set !== 'object') return false;
            const fields = Object.keys(set);
            if (!fields.length) return false;
            const response = await fetch(`/api/session/${encodeURIComponent(sessionId)}`, {
                method: 'PATCH',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ set, fields })
            });
            return response.ok;
        }

        async function setCharacterizationRunLock(sessionId, locked, owner) {
            const lockPayload = locked
                ? {
                    abrchar_run_lock: true,
                    abrchar_run_owner: owner,
                    abrchar_run_started_at: new Date().toISOString()
                }
                : {
                    abrchar_run_lock: false,
                    abrchar_run_owner: '',
                    abrchar_run_started_at: ''
                };
            try {
                return await patchSessionControlFields(sessionId, lockPayload);
            } catch (_) {
                return false;
            }
        }

        async function applyRate(sessionId, targetMbps) {
            const ok = await patchSessionControlFields(sessionId, {
                nftables_bandwidth_mbps: targetMbps,
                nftables_delay_ms: 0,
                nftables_packet_loss: 0,
                nftables_pattern_enabled: false,
                nftables_pattern_steps: []
            });
            if (!ok) {
                throw new Error(`Failed to apply shaping rate ${targetMbps} Mbps through session patch`);
            }
        }

        function rateMatchesTarget(observedRateMbps, targetRateMbps) {
            const observed = toNumber(observedRateMbps, null);
            const target = toNumber(targetRateMbps, null);
            if (observed === null || target === null) return false;
            if (target <= 0.01) {
                return Math.abs(observed) <= 0.05;
            }
            const tolerance = Math.max(0.05, target * 0.02);
            return Math.abs(observed - target) <= tolerance;
        }

        async function confirmRateApplied(sessionId, targetRateMbps, timeoutMs = 12000) {
            const startedAt = Date.now();
            let lastObservedRate = null;
            while (Date.now() - startedAt < timeoutMs) {
                const latest = await fetchSessionById(sessionId);
                const observed = toNumber(latest && latest.nftables_bandwidth_mbps, null);
                if (observed !== null) {
                    lastObservedRate = observed;
                }
                if (rateMatchesTarget(observed, targetRateMbps)) {
                    return {
                        ok: true,
                        observedRateMbps: observed
                    };
                }
                await sleep(500);
            }
            return {
                ok: false,
                observedRateMbps: lastObservedRate
            };
        }

        function extractWireThroughputForValidation(latest) {
            if (!latest || typeof latest !== 'object') return null;
            return toNumber(
                latest.mbps_wire_active_6s,
                toNumber(
                    latest.mbps_wire_throughput,
                    toNumber(latest.mbps_wire_sustained_6s, toNumber(latest.mbps_wire_sustained_1s, toNumber(latest.measured_mbps, null)))
                )
            );
        }

        async function validateDataPlaneRateEffect(sessionId, targetRateMbps, options = {}) {
            const target = toNumber(targetRateMbps, null);
            if (target === null || target <= 0.05) {
                return { checked: false, reason: 'unbounded_target' };
            }
            if (target > 120) {
                return { checked: false, reason: 'target_too_high_for_signal_check' };
            }

            const sampleSeconds = Math.max(10, Math.min(30, Number(options.sampleSeconds) || 20));
            const samples = [];
            for (let index = 0; index < sampleSeconds; index += 1) {
                const latest = await fetchSessionById(sessionId);
                const wire = extractWireThroughputForValidation(latest);
                if (wire !== null) {
                    samples.push(wire);
                }
                await sleep(1000);
            }
            if (samples.length < 3) {
                return { checked: false, reason: 'insufficient_wire_samples', sampleCount: samples.length };
            }

            const medianWireMbps = median(samples);
            const ratio = target > 0 ? (medianWireMbps / target) : null;
            const minWireMbps = Math.min(...samples);
            const maxWireMbps = Math.max(...samples);
            const deltaWireMbps = maxWireMbps - minWireMbps;
            const stagnantThreshold = Math.max(0.5, target * 0.05);
            const stagnant = deltaWireMbps < stagnantThreshold;
            const suspicious = ratio !== null && ratio > 1.35;
            return {
                checked: true,
                suspicious,
                stagnant,
                medianWireMbps,
                ratio,
                minWireMbps,
                maxWireMbps,
                deltaWireMbps,
                sampleCount: samples.length
            };
        }

        function buildSummary(run) {
            const down = run.switchEvents.filter((event) => event.toVariantMbps < event.fromVariantMbps);
            const up = run.switchEvents.filter((event) => event.toVariantMbps > event.fromVariantMbps);
            const downThroughputs = down.map((event) => event.throughputMbps).filter((value) => value !== null);
            const upThroughputs = up.map((event) => event.throughputMbps).filter((value) => value !== null);
            const alphaDown = down
                .filter((event) => event.throughputMbps && event.toVariantMbps)
                .map((event) => event.toVariantMbps / event.throughputMbps);
            const alphaUp = up
                .filter((event) => event.throughputMbps && event.toVariantMbps)
                .map((event) => event.toVariantMbps / event.throughputMbps);
            return {
                stepCount: run.steps.length,
                switchCount: run.switchEvents.length,
                playbackStoppedCount: Array.isArray(run.playbackStoppedEvents) ? run.playbackStoppedEvents.length : 0,
                shapingWarningCount: Array.isArray(run.shapingWarnings) ? run.shapingWarnings.length : 0,
                downswitches: down.length,
                upswitches: up.length,
                downswitchMedianMbps: median(downThroughputs),
                upswitchMedianMbps: median(upThroughputs),
                alphaDownMedian: median(alphaDown),
                alphaUpMedian: median(alphaUp),
                transientShock: run.transientShockResults || null,
                startupCaps: run.startupCapsResults || null,
                downshiftSeverity: run.downshiftSeverityResults || null,
                hysteresisGap: run.hysteresisGapResults || null,
                durationSeconds: Math.round((Date.now() - run.startedAt) / 1000)
            };
        }

        async function runCharacterization() {
            if (runState) return;
            cancelRequested = false;
            let runLockAcquired = false;
            const runOwnerToken = `abrchar-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
            progressEl.innerHTML = '';
            summaryEl.textContent = 'Running…';
            if (reportEl) {
                reportEl.style.display = '';
                reportEl.textContent = '# Player Characterization Report\n\nPreparing run...';
            }
            downloadsEl.style.display = 'none';
            latestSummary = null;
            latestReport = '';

            const session = options.getSession ? options.getSession() : null;
            if (!session || !session.session_id) {
                statusEl.textContent = 'No active session selected.';
                appendProgress('No active session selected.', 'error');
                return;
            }

            const port = session.x_forwarded_port_external || session.x_forwarded_port;
            if (!port) {
                statusEl.textContent = 'Session port is missing.';
                appendProgress('Session port is missing; cannot shape throughput.', 'error');
                return;
            }

            const currentPlaybackUrl = options.getCurrentUrl ? options.getCurrentUrl() : '';
            const masterCandidates = buildMasterUrlCandidates(session, currentPlaybackUrl);
            const masterUrl = masterCandidates[0] || '';
            if (!masterUrl || !masterUrl.includes('.m3u8')) {
                statusEl.textContent = 'HLS master URL unavailable.';
                appendProgress('Characterization requires an HLS URL with a bitrate ladder.', 'error');
                return;
            }

            const lockSessionId = String(session.session_id);
            const lockSnapshot = await fetchSessionById(lockSessionId);
            const lockedByOther = lockSnapshot
                && (lockSnapshot.abrchar_run_lock === true || lockSnapshot.abrchar_run_lock === 1 || lockSnapshot.abrchar_run_lock === 'true')
                && String(lockSnapshot.abrchar_run_owner || '')
                && String(lockSnapshot.abrchar_run_owner || '') !== runOwnerToken;
            if (lockedByOther) {
                statusEl.textContent = 'Characterization run lock active.';
                appendProgress(
                    `Another characterization run lock is active (${String(lockSnapshot.abrchar_run_owner || 'unknown')}). Stop that run or clear lock before starting.`,
                    'error'
                );
                return;
            }
            runLockAcquired = await setCharacterizationRunLock(lockSessionId, true, runOwnerToken);
            if (!runLockAcquired) {
                appendProgress('WARNING: Unable to persist characterization run lock; manual shaping changes may interfere.', 'warn');
            }

            startButton.disabled = true;
            stopButton.disabled = false;
            statusEl.textContent = 'Preparing ladder and schedule…';

            try {
                let variants = [];
                let loadedMasterUrl = '';
                let lastLoadError = null;
                for (const candidate of masterCandidates) {
                    try {
                        appendProgress(`Trying ladder URL: ${candidate}`, 'info');
                        variants = await parseHlsLadder(candidate);
                        loadedMasterUrl = candidate;
                        break;
                    } catch (error) {
                        lastLoadError = error;
                        appendProgress(`Ladder URL failed (${candidate}): ${error.message || error}`, 'warn');
                    }
                }
                if (!loadedMasterUrl) {
                    throw (lastLoadError || new Error('Unable to load any master playlist candidate'));
                }
                const topVariant = variants.length ? variants[variants.length - 1] : null;
                const minVariant = variants.length ? variants[0] : null;
                const topMbps = topVariant ? ((topVariant.averageBandwidth || topVariant.bandwidth) / 1000000) : 8;
                const minDefaultMbps = minVariant ? Math.max(0.3, (minVariant.bandwidth / 1000000) * 0.7) : 0.6;
                updateRuler({
                    ladder: uniqueSortedPositive(variants.map((variant) => toMbpsFromVariant(variant)))
                });

                if (!toNumber(maxInput.value, null) || toNumber(maxInput.value, 0) <= 0) {
                    maxInput.value = String(Math.round(Math.max(topMbps * 2, topMbps + 2) * 100) / 100);
                }
                if (!toNumber(minInput.value, null) || toNumber(minInput.value, 0) <= 0) {
                    minInput.value = String(Math.round(minDefaultMbps * 100) / 100);
                }

                const holdSeconds = Math.max(5, toNumber(holdInput.value, 20) || 20);
                const maxMbps = toNumber(maxInput.value, topMbps * 2) || (topMbps * 2);
                const minMbps = toNumber(minInput.value, minDefaultMbps) || minDefaultMbps;
                const stepPct = toNumber(stepInput.value, 10) || 10;
                const networkOverheadPct = Math.max(0, Math.min(25, toNumber(overheadInput && overheadInput.value, 10) || 10));
                const selectedTests = Array.from((testsInput && testsInput.selectedOptions) || [])
                    .map((option) => String(option.value || '').trim())
                    .filter(Boolean);
                const emergencySegmentSeconds = Math.max(1, Math.min(12, toNumber(segmentInput && segmentInput.value, 2) || 2));
                const emergencyTrials = Math.max(1, Math.min(12, toNumber(trialsInput && trialsInput.value, 5) || 5));
                const emergencyBottomMarginPct = Math.max(0, Math.min(30, toNumber(bottomMarginInput && bottomMarginInput.value, 5) || 5));
                const emergencyTopHeadroomPct = Math.max(5, Math.min(75, toNumber(topHeadroomInput && topHeadroomInput.value, 25) || 25));
                const emergencyDownshiftSlaSeconds = Math.max(2, Math.min(30, toNumber(downshiftSlaInput && downshiftSlaInput.value, 4) || 4));
                const emergencyBottomSlaSeconds = Math.max(4, Math.min(45, toNumber(bottomSlaInput && bottomSlaInput.value, 8) || 8));

                const buildPhaseSchedule = (phaseDirection) => {
                    let phase = makeVariantAwareSchedule({
                        variants,
                        direction: phaseDirection,
                        holdSeconds,
                        maxMbps,
                        minMbps,
                        networkOverheadPct
                    });
                    if (!phase.length) {
                        phase = makeSchedule({
                            direction: phaseDirection,
                            maxMbps,
                            minMbps,
                            stepPct,
                            holdSeconds
                        });
                    }
                    return phase;
                };

                let schedule = [];
                let emergencyConfig = null;
                let transientShockConfig = null;
                let startupCapsConfig = null;
                let downshiftSeverityConfig = null;
                let hysteresisGapConfig = null;
                const downPhase = buildPhaseSchedule('down');
                const upPhase = buildPhaseSchedule('up');
                if (selectedTests.includes('ramp-down')) {
                    schedule = schedule.concat(downPhase);
                }
                if (selectedTests.includes('ramp-up')) {
                    schedule = schedule.concat(upPhase);
                }
                if (selectedTests.includes('emergency-downshift')) {
                    const emergency = buildEmergencyDownshiftSchedule({
                        variants,
                        minMbps,
                        maxMbps,
                        networkOverheadPct,
                        segmentSeconds: emergencySegmentSeconds,
                        trials: emergencyTrials,
                        bottomMarginPct: emergencyBottomMarginPct,
                        topHeadroomPct: emergencyTopHeadroomPct,
                        downshiftLatencyThresholdSeconds: emergencyDownshiftSlaSeconds,
                        bottomReachThresholdSeconds: emergencyBottomSlaSeconds
                    });
                    emergencyConfig = emergency.emergencyConfig;
                    if (!emergency.steps.length || !emergencyConfig) {
                        throw new Error('Unable to build emergency downshift schedule from current ladder.');
                    }
                    schedule = schedule.concat(emergency.steps);
                }
                if (selectedTests.includes('transient-shock')) {
                    const transientShock = buildTransientShockSchedule({
                        variants,
                        minMbps,
                        maxMbps,
                        networkOverheadPct
                    });
                    transientShockConfig = transientShock.config;
                    schedule = schedule.concat(transientShock.steps);
                }
                if (selectedTests.includes('startup-caps')) {
                    const startupCaps = buildStartupCapsSchedule({
                        variants,
                        minMbps,
                        maxMbps,
                        networkOverheadPct
                    });
                    startupCapsConfig = startupCaps.config;
                    schedule = schedule.concat(startupCaps.steps);
                }
                if (selectedTests.includes('downshift-severity')) {
                    const downshiftSeverity = buildDownshiftSeveritySchedule({
                        variants,
                        minMbps,
                        maxMbps,
                        networkOverheadPct
                    });
                    downshiftSeverityConfig = downshiftSeverity.config;
                    schedule = schedule.concat(downshiftSeverity.steps);
                }
                if (selectedTests.includes('hysteresis-gap')) {
                    const hysteresisGap = buildHysteresisGapSchedule({
                        variants,
                        minMbps,
                        maxMbps,
                        networkOverheadPct
                    });
                    hysteresisGapConfig = hysteresisGap.config;
                    schedule = schedule.concat(hysteresisGap.steps);
                }
                if (!schedule.length) {
                    throw new Error('No tests selected; choose at least one test in the Tests multi-select.');
                }

                const selectedLabels = {
                    'ramp-down': 'ramp-down',
                    'ramp-up': 'ramp-up',
                    'emergency-downshift': 'emergency-downshift',
                    'transient-shock': 'transient-shock',
                    'startup-caps': 'startup-caps',
                    'downshift-severity': 'downshift-severity',
                    'hysteresis-gap': 'hysteresis-gap'
                };
                appendProgress(`Selected tests: ${selectedTests.map((key) => selectedLabels[key] || key).join(', ')}.`, 'info');
                if (selectedTests.includes('emergency-downshift')) {
                    appendProgress(`Emergency mode enabled: ${emergencyConfig ? emergencyConfig.cycleCount : emergencyTrials} high/low cycles (~20 min total).`, 'warn');
                }
                if (selectedTests.includes('transient-shock') && transientShockConfig) {
                    appendProgress('Transient shock mode enabled: short small/medium/severe shocks with recovery windows.', 'info');
                }
                if (selectedTests.includes('startup-caps') && startupCapsConfig) {
                    appendProgress(`Startup cap mode enabled: ${startupCapsConfig.caps.length} cap scenarios (low/mid/high).`, 'info');
                }
                if (selectedTests.includes('downshift-severity') && downshiftSeverityConfig) {
                    appendProgress('Downshift severity mode enabled: latency buckets for small/medium/severe drops.', 'info');
                }
                if (selectedTests.includes('hysteresis-gap') && hysteresisGapConfig) {
                    appendProgress(`Hysteresis mode enabled: ${hysteresisGapConfig.pairCount} adjacent variant-pair probes.`, 'info');
                }
                appendProgress(`Planned targets: ${schedule.map((item) => item.targetMbps.toFixed(2)).join(', ')} Mbps`, 'info');
                appendProgress(`Network overhead assumption: ${networkOverheadPct.toFixed(0)}% (limits are overhead-adjusted wire targets).`, 'info');
                if (selectedTests.includes('emergency-downshift') && emergencyConfig) {
                    appendProgress(
                        `Emergency pass thresholds: first downshift ≤ ${emergencyConfig.downshiftLatencyThresholdSeconds}s, bottom rung ≤ ${emergencyConfig.bottomReachThresholdSeconds}s, and no stall growth. High target is 3x top variant; low target is midpoint(bottom two)+overhead. Each step waits for rendition match (±10%), then holds 30s.`,
                        'info'
                    );
                }

                runState = {
                    sessionId: String(session.session_id),
                    port,
                    masterUrl: loadedMasterUrl,
                    ladderMbps: uniqueSortedPositive(variants.map((variant) => toMbpsFromVariant(variant))),
                    mode: selectedTests.length > 1 ? 'mixed' : (selectedTests[0] || 'normal'),
                    selectedTests,
                    emergencyConfig,
                    transientShockConfig,
                    startupCapsConfig,
                    downshiftSeverityConfig,
                    hysteresisGapConfig,
                    steps: schedule,
                    stepTiming: [],
                    samples: [],
                    switchEvents: [],
                    playbackStoppedEvents: [],
                    shapingWarnings: [],
                    startedAt: Date.now()
                };

                appendProgress(`Loaded ${variants.length} ladder variants.`, 'info');
                appendProgress(`Schedule has ${schedule.length} steps.`, 'info');
                if (selectedTests.includes('emergency-downshift')) {
                    appendProgress(`Emergency cycles included: ${emergencyConfig ? emergencyConfig.cycleCount : emergencyTrials}.`, 'info');
                }
                appendProgress('Step timing policy: all steps wait for ±25% settle; emergency steps then hold 30s at each target.', 'info');
                appendProgress('Tracking policy: metrics/events begin after the first settled step.', 'info');

                let previousVariant = null;
                let previousStallCount = null;
                let previousStallTimeSeconds = null;
                let previousRestartCheckStallTimeSeconds = toNumber(session.player_metrics_stall_time_s, null);
                let continuousStallAccumulatedSeconds = 0;
                let stalledSignalAccumulatedSeconds = 0;
                const settleToleranceRatio = 0.25;
                const stallRestartThresholdSeconds = 20;
                const zeroBufferRestartThresholdSamples = Math.max(1, Math.ceil(stallRestartThresholdSeconds));
                let trackingStarted = false;

                const refreshLiveReport = () => {
                    if (!reportEl || !runState) return;
                    const summary = buildSummary(runState);
                    reportEl.textContent = buildReportMarkdown(runState, summary);
                    reportEl.style.display = '';
                };

                const extractThroughput = (latest) => toNumber(
                    latest.mbps_wire_active_6s,
                    toNumber(
                        latest.mbps_wire_throughput,
                        toNumber(latest.mbps_wire_sustained_6s, toNumber(latest.mbps_wire_sustained_1s, toNumber(latest.measured_mbps, null)))
                    )
                );
                const extractVariant = (latest) => toNumber(latest.player_metrics_video_bitrate_mbps, null);
                const extractBufferDepth = (latest) => toNumber(latest.player_metrics_buffer_depth_s, null);

                const maybeRestartForPlaybackFailure = (latest, now, stepIndex, step) => {
                    const totalStallTime = toNumber(latest.player_metrics_stall_time_s, null);
                    const bufferSeconds = extractBufferDepth(latest);
                    const playerStateRaw = String(latest.player_metrics_state || '').toLowerCase();
                    const stalledLikeState = playerStateRaw.includes('stall') || playerStateRaw.includes('buffer') || playerStateRaw.includes('waiting');
                    const stalledLikeSample = stalledLikeState || (bufferSeconds !== null && bufferSeconds <= 0.25);
                    let sampleStallDeltaSeconds = null;
                    if (totalStallTime !== null && previousRestartCheckStallTimeSeconds !== null) {
                        sampleStallDeltaSeconds = Math.max(0, totalStallTime - previousRestartCheckStallTimeSeconds);
                    }
                    if (totalStallTime !== null) {
                        previousRestartCheckStallTimeSeconds = totalStallTime;
                    }
                    if (sampleStallDeltaSeconds !== null) {
                        if (sampleStallDeltaSeconds > 0) {
                            continuousStallAccumulatedSeconds += sampleStallDeltaSeconds;
                        } else {
                            continuousStallAccumulatedSeconds = 0;
                        }
                    }
                    if (stalledLikeSample) {
                        stalledSignalAccumulatedSeconds += 1;
                    } else {
                        stalledSignalAccumulatedSeconds = 0;
                    }

                    if (now - lastPlaybackRestartAt <= 15000) {
                        return false;
                    }

                    const triggerPlaybackRestart = (reason, detailMessage, eventDetails) => {
                        appendProgress(`${detailMessage} Restarting playback without changing shaping limits.`, 'warn');
                        runState.playbackStoppedEvents.push({
                            tSinceStart: (Date.now() - runState.startedAt) / 1000,
                            stepIndex,
                            phase: phaseLabel(step?.direction),
                            targetMbps: toNumber(step?.targetMbps, null),
                            reason,
                            ...eventDetails
                        });
                        lastPlaybackRestartAt = now;

                        const causeSummaryParts = [];
                        if (eventDetails.metricSource) {
                            causeSummaryParts.push(`metric=${eventDetails.metricSource}`);
                        }
                        if (eventDetails.observedStallSeconds !== null && eventDetails.observedStallSeconds !== undefined) {
                            causeSummaryParts.push(`stall_s=${Number(eventDetails.observedStallSeconds).toFixed(2)}`);
                        }
                        if (eventDetails.zeroBufferSamples !== null && eventDetails.zeroBufferSamples !== undefined) {
                            causeSummaryParts.push(`zero_buffer_samples=${eventDetails.zeroBufferSamples}`);
                        }
                        if (eventDetails.zeroBufferApproxSeconds !== null && eventDetails.zeroBufferApproxSeconds !== undefined) {
                            causeSummaryParts.push(`zero_buffer_s~${Number(eventDetails.zeroBufferApproxSeconds).toFixed(2)}`);
                        }
                        const causeSummary = causeSummaryParts.length ? causeSummaryParts.join(', ') : 'no details';

                        attemptPlaybackRestart().then((ok) => {
                            appendProgress(
                                ok
                                    ? `Playback restart triggered successfully (cause=${reason}; ${causeSummary}).`
                                    : `Playback restart attempted but no player restart path was available (cause=${reason}; ${causeSummary}).`,
                                ok ? 'info' : 'warn'
                            );
                        });
                        return true;
                    };

                    if (zeroBufferSampleCount >= zeroBufferRestartThresholdSamples) {
                        continuousStallAccumulatedSeconds = 0;
                        stalledSignalAccumulatedSeconds = 0;
                        return triggerPlaybackRestart(
                            'repeated_zero_buffer',
                            `Buffer depth remained at 0s for ~${stallRestartThresholdSeconds}s`,
                            {
                                observedStallSeconds: totalStallTime,
                                metricSource: 'player_metrics_stall_time_s',
                                zeroBufferSamples: zeroBufferSampleCount,
                                zeroBufferApproxSeconds: zeroBufferSampleCount
                            }
                        );
                    }

                    if (stalledSignalAccumulatedSeconds >= stallRestartThresholdSeconds) {
                        const observedStallDuration = stalledSignalAccumulatedSeconds;
                        continuousStallAccumulatedSeconds = 0;
                        stalledSignalAccumulatedSeconds = 0;
                        return triggerPlaybackRestart(
                            'persistent_stalled_signal',
                            `Persistent stalled-like samples detected for ${observedStallDuration.toFixed(0)}s`,
                            {
                                observedStallSeconds: observedStallDuration,
                                metricSource: 'player_metrics_state/buffer_depth consecutive stalled samples',
                                sampleDeltaSeconds: sampleStallDeltaSeconds,
                                playerState: latest.player_metrics_state || null,
                                zeroBufferSamples: zeroBufferSampleCount,
                                zeroBufferApproxSeconds: zeroBufferSampleCount
                            }
                        );
                    }

                    if (continuousStallAccumulatedSeconds >= stallRestartThresholdSeconds) {
                        const observedStallDuration = continuousStallAccumulatedSeconds;
                        continuousStallAccumulatedSeconds = 0;
                        stalledSignalAccumulatedSeconds = 0;
                        return triggerPlaybackRestart(
                            'long_stall',
                            `Long stall detected (${observedStallDuration.toFixed(2)}s via cumulative player_metrics_stall_time_s sample deltas)`,
                            {
                                observedStallSeconds: observedStallDuration,
                                metricSource: 'player_metrics_stall_time_s cumulative Δ(samples)',
                                sampleDeltaSeconds: sampleStallDeltaSeconds,
                                zeroBufferSamples: zeroBufferSampleCount,
                                zeroBufferApproxSeconds: zeroBufferSampleCount
                            }
                        );
                    }

                    return false;
                };

                const recordSample = (latest, index, step) => {
                    const throughput = extractThroughput(latest);
                    const active6sThroughput = toNumber(latest.mbps_wire_active_6s, null);
                    const wireThroughput = toNumber(latest.mbps_wire_throughput, null);
                    const playerEstimate = toNumber(latest.player_metrics_network_bitrate_mbps, null);
                    const framesDisplayed = toNumber(latest.player_metrics_frames_displayed, null);
                    const variant = extractVariant(latest);
                    const sample = {
                        ts: Date.now(),
                        stepIndex: index,
                        stepTargetMbps: step.targetMbps,
                        stepDirection: step.direction,
                        throughputMbps: throughput,
                        active6sThroughputMbps: active6sThroughput,
                        wireThroughputMbps: wireThroughput,
                        playerEstimateMbps: playerEstimate,
                        framesDisplayed,
                        variantMbps: variant,
                        bufferSeconds: extractBufferDepth(latest),
                        stallCount: toNumber(latest.player_metrics_stall_count, null),
                        stallTimeSeconds: toNumber(latest.player_metrics_stall_time_s, null)
                    };
                    latestBufferDepthSeconds = sample.bufferSeconds;
                    if (sample.bufferSeconds !== null && sample.bufferSeconds <= 0.01) {
                        zeroBufferSampleCount += 1;
                    } else {
                        zeroBufferSampleCount = 0;
                    }
                    runState.samples.push(sample);
                    if (sample.stallCount !== null) {
                        if (previousStallCount !== null && sample.stallCount > previousStallCount) {
                            const newStalls = sample.stallCount - previousStallCount;
                            const stallTimeDelta = (previousStallTimeSeconds !== null && sample.stallTimeSeconds !== null)
                                ? Math.max(0, sample.stallTimeSeconds - previousStallTimeSeconds)
                                : null;
                            const stallTimeText = sample.stallTimeSeconds === null
                                ? 'stall_time_s -'
                                : `stall_time_s ${sample.stallTimeSeconds.toFixed(2)}s${stallTimeDelta === null ? '' : ` (Δ${stallTimeDelta.toFixed(2)}s)`}`;
                            if (step.direction === 'down') {
                                appendProgress(
                                    `ERROR: Unexpected stall increase during ramp-down: +${newStalls} (total ${sample.stallCount}, ${stallTimeText}).`,
                                    'error'
                                );
                            } else {
                                appendProgress(
                                    `Stall count increased: +${newStalls} (total ${sample.stallCount}, ${stallTimeText}).`,
                                    'warn'
                                );
                            }
                        }
                        previousStallCount = sample.stallCount;
                    }
                    if (sample.stallTimeSeconds !== null) {
                        previousStallTimeSeconds = sample.stallTimeSeconds;
                    }
                    updateRuler({
                        limitMbps: step.targetMbps,
                        throughputMbps: throughput,
                        wireThroughputMbps: wireThroughput,
                        playerEstimateMbps: playerEstimate,
                        currentVariantMbps: variant
                    });

                    if (previousVariant !== null && variant !== null && Math.abs(variant - previousVariant) >= 0.05) {
                        const fromVariant = formatVariantWithLabel(previousVariant, runState.ladderMbps || []);
                        const toVariant = formatVariantWithLabel(variant, runState.ladderMbps || []);
                        const phase = phaseLabel(step.direction);
                        const event = {
                            tSinceStart: (Date.now() - runState.startedAt) / 1000,
                            stepIndex: index,
                            fromVariantMbps: previousVariant,
                            fromVariantLabel: fromVariant.label,
                            toVariantMbps: variant,
                            toVariantLabel: toVariant.label,
                            throughputMbps: throughput,
                            stepTargetMbps: step.targetMbps,
                            stepDirection: step.direction,
                            phase,
                            secondsAfterLimitChange: (runState.stepTiming[index] && Number.isFinite(runState.stepTiming[index].limitAppliedAt))
                                ? ((Date.now() - runState.stepTiming[index].limitAppliedAt) / 1000)
                                : null,
                            bufferSeconds: sample.bufferSeconds,
                            stallCount: sample.stallCount,
                            stallTimeSeconds: sample.stallTimeSeconds
                        };
                        runState.switchEvents.push(event);
                        const direction = variant > previousVariant ? 'upswitch' : 'downswitch';
                        const expectedSwitch = step.direction === 'down' ? 'downswitch' : 'upswitch';
                        if (direction === expectedSwitch) {
                            appendProgress(
                                `Expected ${phase} switch (${direction}): variant ${fromVariant.text} -> ${toVariant.text} (throughput ${throughput !== null ? throughput.toFixed(2) : '—'} Mbps, wire_throughput ${wireThroughput !== null ? wireThroughput.toFixed(2) : '—'} Mbps, player_est_network ${playerEstimate !== null ? playerEstimate.toFixed(2) : '—'} Mbps, stall_count ${sample.stallCount !== null ? sample.stallCount : '-'}, stall_time_s ${sample.stallTimeSeconds !== null ? sample.stallTimeSeconds.toFixed(2) : '-'}).`,
                                'success'
                            );
                        } else {
                            appendProgress(
                                `ERROR: Unexpected ${phase} switch (${direction}): variant ${fromVariant.text} -> ${toVariant.text} (throughput ${throughput !== null ? throughput.toFixed(2) : '—'} Mbps, wire_throughput ${wireThroughput !== null ? wireThroughput.toFixed(2) : '—'} Mbps, player_est_network ${playerEstimate !== null ? playerEstimate.toFixed(2) : '—'} Mbps, stall_count ${sample.stallCount !== null ? sample.stallCount : '-'}, stall_time_s ${sample.stallTimeSeconds !== null ? sample.stallTimeSeconds.toFixed(2) : '-'}).`,
                                'error'
                            );
                        }
                    }
                    previousVariant = variant;
                    refreshLiveReport();
                    return { throughput, variant, wireThroughput, playerEstimate };
                };

                refreshLiveReport();

                const stepRestartAttempts = new Map();

                for (let index = 0; index < schedule.length; index += 1) {
                    if (cancelRequested) {
                        appendProgress('Run cancelled by user.', 'warn');
                        break;
                    }
                    const step = schedule[index];
                    if (step.restartPlaybackBeforeStep) {
                        appendProgress(
                            `Restarting playback before startup-cap scenario (${step.startupCapLabel || 'cap'}) while preserving current shaping policy.`,
                            'info'
                        );
                        try {
                            if (typeof options.onStop === 'function') {
                                options.onStop();
                                await sleep(300);
                            }
                            if (typeof options.onPlay === 'function') {
                                options.onPlay();
                                await sleep(1200);
                            }
                            runState.playbackStoppedEvents.push({
                                tSinceStart: (Date.now() - runState.startedAt) / 1000,
                                stepIndex: index,
                                phase: phaseLabel(step.direction),
                                targetMbps: toNumber(step.targetMbps, null),
                                reason: 'startup_cap_replay',
                                metricSource: 'startup-caps scenario precondition'
                            });
                        } catch (restartError) {
                            appendProgress(
                                `WARNING: Playback restart before startup-cap step failed: ${restartError && restartError.message ? restartError.message : 'unknown error'}`,
                                'warn'
                            );
                        }
                    }
                    const stepStartedAt = Date.now();
                    const requiredHoldAfterSettleMs = Math.max(5, Number(step.postSettleHoldSeconds || 30)) * 1000;
                    const maxStepDurationMs = Math.max(requiredHoldAfterSettleMs, Math.max(60_000, Number(step.maxStepSeconds || 60) * 1000));
                    await applyRate(runState.sessionId, step.targetMbps);
                    const shapeConfirmation = await confirmRateApplied(runState.sessionId, step.targetMbps, 12000);
                    if (!shapeConfirmation.ok) {
                        appendProgress(
                            `ERROR: Shaping limit confirmation failed for target ${formatLimitWithVariant(step.targetMbps)}; observed control rate ${shapeConfirmation.observedRateMbps !== null ? `${shapeConfirmation.observedRateMbps.toFixed(2)} Mbps` : '—'}.`,
                            'error'
                        );
                        throw new Error(`Unable to confirm shaping target ${step.targetMbps.toFixed(2)} Mbps was applied`);
                    }
                    appendProgress(
                        `Confirmed shaping control rate ${shapeConfirmation.observedRateMbps !== null ? shapeConfirmation.observedRateMbps.toFixed(2) : step.targetMbps.toFixed(2)} Mbps for target ${formatLimitWithVariant(step.targetMbps)}.`,
                        'success'
                    );
                    const dataPlaneValidation = await validateDataPlaneRateEffect(runState.sessionId, step.targetMbps, { sampleSeconds: 15 });
                    if (dataPlaneValidation.checked && dataPlaneValidation.suspicious) {
                        const warning = {
                            stepIndex: index,
                            targetMbps: step.targetMbps,
                            medianWireMbps: dataPlaneValidation.medianWireMbps,
                            ratio: dataPlaneValidation.ratio,
                            sampleCount: dataPlaneValidation.sampleCount
                        };
                        runState.shapingWarnings.push(warning);
                        appendProgress(
                            `WARNING: Dataplane check suggests shaping may not be fully effective at ${formatLimitWithVariant(step.targetMbps)} (median wire ${dataPlaneValidation.medianWireMbps.toFixed(2)} Mbps, ${(dataPlaneValidation.ratio * 100).toFixed(0)}% of target).`,
                            'warn'
                        );
                    } else if (dataPlaneValidation.checked) {
                        appendProgress(
                            `Dataplane check passed for ${formatLimitWithVariant(step.targetMbps)} (median wire ${dataPlaneValidation.medianWireMbps.toFixed(2)} Mbps over ${dataPlaneValidation.sampleCount}s).`,
                            'info'
                        );
                    }
                    const restartAttempt = stepRestartAttempts.get(index) || 0;
                    if (dataPlaneValidation.checked && dataPlaneValidation.stagnant) {
                        const warning = {
                            stepIndex: index,
                            targetMbps: step.targetMbps,
                            reason: 'stagnant_throughput_after_limit_change',
                            medianWireMbps: dataPlaneValidation.medianWireMbps,
                            minWireMbps: dataPlaneValidation.minWireMbps,
                            maxWireMbps: dataPlaneValidation.maxWireMbps,
                            deltaWireMbps: dataPlaneValidation.deltaWireMbps,
                            sampleCount: dataPlaneValidation.sampleCount,
                            restartAttempt: restartAttempt + 1
                        };
                        runState.shapingWarnings.push(warning);
                        if (restartAttempt < 2) {
                            stepRestartAttempts.set(index, restartAttempt + 1);
                            appendProgress(
                                `WARNING: Throughput appears stagnant after limit change at ${formatLimitWithVariant(step.targetMbps)} (Δ${dataPlaneValidation.deltaWireMbps.toFixed(2)} Mbps over ${dataPlaneValidation.sampleCount}s). Restarting this step and reapplying limit (attempt ${restartAttempt + 1}/2).`,
                                'warn'
                            );
                            index -= 1;
                            continue;
                        }
                        appendProgress(
                            `ERROR: Throughput still stagnant after retries at ${formatLimitWithVariant(step.targetMbps)} (Δ${dataPlaneValidation.deltaWireMbps.toFixed(2)} Mbps over ${dataPlaneValidation.sampleCount}s). Proceeding with caution.`,
                            'error'
                        );
                    }
                    runState.stepTiming[index] = {
                        limitAppliedAt: Date.now(),
                        targetMbps: step.targetMbps,
                        direction: step.direction
                    };
                    statusEl.textContent = `Step ${index + 1}/${schedule.length} @ ${step.targetMbps.toFixed(2)} Mbps`;
                    const emergencyModeSuffix = step.mode === 'emergency-downshift'
                        ? ` [cycle=${step.cycleIndex || '-'} kind=${step.stepKind || '-'}]`
                        : '';
                    appendProgress(`Throughput target set to ${formatLimitWithVariant(step.targetMbps)} (${step.direction})${emergencyModeSuffix}.`, 'info');

                    const settleTimeoutMs = maxStepDurationMs;
                    const settleStart = Date.now();
                    let lastSettleLogAt = 0;
                    let inRangeConsecutive = 0;
                    let stabilized = false;
                    const settleMode = step.settleOn === 'variant'
                        ? 'variant'
                        : (step.settleOn === 'rendition-match' ? 'rendition-match' : 'throughput');
                    const settleVariantIndex = Number.isFinite(Number(step.settleVariantIndex))
                        ? Number(step.settleVariantIndex)
                        : -1;
                    const settleTargetRenditionMbps = toNumber(step.settleTargetRenditionMbps, null);
                    const settleToleranceRatio = Math.max(0, Math.min(0.5, (toNumber(step.settleTolerancePct, 0) || 0) / 100));

                    if (step.skipSettle) {
                        stabilized = true;
                        if (!trackingStarted) {
                            trackingStarted = true;
                            runState.startedAt = Date.now();
                            appendProgress('Tracking enabled at step start because settle gating is disabled for this step.', 'info');
                        }
                        appendProgress(
                            `Skipping settle gate for ${step.stepKind || 'custom'}; applying fixed hold at ${formatLimitWithVariant(step.targetMbps)}.`,
                            'info'
                        );
                    }

                    while (!step.skipSettle && Date.now() - settleStart < settleTimeoutMs) {
                        if (cancelRequested) break;
                        const latest = await fetchSessionById(runState.sessionId);
                        if (!latest) {
                            await sleep(1000);
                            continue;
                        }
                        const throughput = extractThroughput(latest);
                        const wireThroughput = toNumber(latest.mbps_wire_throughput, null);
                        const playerEstimate = toNumber(latest.player_metrics_network_bitrate_mbps, null);
                        const variant = extractVariant(latest);
                        const bufferSeconds = extractBufferDepth(latest);
                        latestBufferDepthSeconds = bufferSeconds;
                        if (bufferSeconds !== null && bufferSeconds <= 0.01) {
                            zeroBufferSampleCount += 1;
                        } else {
                            zeroBufferSampleCount = 0;
                        }
                        if (trackingStarted) {
                            recordSample(latest, index, step);
                        } else {
                            updateRuler({
                                limitMbps: step.targetMbps,
                                throughputMbps: throughput,
                                wireThroughputMbps: wireThroughput,
                                playerEstimateMbps: playerEstimate,
                                currentVariantMbps: variant
                            });
                        }
                        const now = Date.now();
                        maybeRestartForPlaybackFailure(latest, now, index, step);
                        const low = step.targetMbps * (1 - settleToleranceRatio);
                        const high = step.targetMbps * (1 + settleToleranceRatio);
                        const currentVariantIndex = nearestVariantIndex(runState.ladderMbps || [], variant);
                        const renditionLow = settleTargetRenditionMbps !== null ? (settleTargetRenditionMbps * (1 - settleToleranceRatio)) : null;
                        const renditionHigh = settleTargetRenditionMbps !== null ? (settleTargetRenditionMbps * (1 + settleToleranceRatio)) : null;
                        const inRange = settleMode === 'variant'
                            ? (variant !== null && currentVariantIndex === settleVariantIndex)
                            : (settleMode === 'rendition-match'
                                ? (variant !== null && renditionLow !== null && renditionHigh !== null && variant >= renditionLow && variant <= renditionHigh)
                                : (throughput !== null && throughput >= low && throughput <= high));
                        inRangeConsecutive = inRange ? (inRangeConsecutive + 1) : 0;

                        if (inRangeConsecutive >= 2) {
                            stabilized = true;
                            if (!trackingStarted) {
                                trackingStarted = true;
                                runState.startedAt = Date.now();
                                appendProgress('Initial throughput has reached active limit tolerance; tracking is now enabled.', 'success');
                            }
                            if (settleMode === 'variant') {
                                const variantTargetLabel = settleVariantIndex >= 0
                                    ? `V${settleVariantIndex + 1}`
                                    : 'target variant';
                                appendProgress(
                                    `Rendition reached ${variantTargetLabel} for target ${formatLimitWithVariant(step.targetMbps)}. Starting hold timer (30s).`,
                                    'success'
                                );
                            } else if (settleMode === 'rendition-match') {
                                appendProgress(
                                    `Rendition matched target ${settleTargetRenditionMbps !== null ? settleTargetRenditionMbps.toFixed(2) : '—'} Mbps (±${Math.round(settleToleranceRatio * 100)}%) for limit ${formatLimitWithVariant(step.targetMbps)}. Starting hold timer (30s).`,
                                    'success'
                                );
                            } else {
                                appendProgress(
                                    `Throughput settled near target ${formatLimitWithVariant(step.targetMbps)} (current ${throughput.toFixed(2)} Mbps, wire_throughput ${wireThroughput !== null ? wireThroughput.toFixed(2) : '—'} Mbps, player_est_network ${playerEstimate !== null ? playerEstimate.toFixed(2) : '—'} Mbps, ±25%). Starting hold timer.`,
                                    'success'
                                );
                            }
                            break;
                        }

                        if (now - lastSettleLogAt > 5000) {
                            if (settleMode === 'variant') {
                                const variantText = variant !== null
                                    ? `${formatVariantWithLabel(variant, runState.ladderMbps || []).text}`
                                    : '—';
                                const variantTargetLabel = settleVariantIndex >= 0
                                    ? `V${settleVariantIndex + 1}`
                                    : 'target variant';
                                appendProgress(
                                    `Waiting for rendition to reach ${variantTargetLabel} before hold. Current rendition ${variantText}.`,
                                    'info'
                                );
                            } else if (settleMode === 'rendition-match') {
                                appendProgress(
                                    `Waiting for rendition match near ${settleTargetRenditionMbps !== null ? settleTargetRenditionMbps.toFixed(2) : '—'} Mbps (±${Math.round(settleToleranceRatio * 100)}%). Current rendition ${variant !== null ? variant.toFixed(2) : '—'} Mbps.`,
                                    'info'
                                );
                            } else {
                                appendProgress(
                                    `Waiting for throughput to settle near ${formatLimitWithVariant(step.targetMbps)} (±25%). Current ${throughput !== null ? throughput.toFixed(2) : '—'} Mbps, wire_throughput ${wireThroughput !== null ? wireThroughput.toFixed(2) : '—'} Mbps, player_est_network ${playerEstimate !== null ? playerEstimate.toFixed(2) : '—'} Mbps.`,
                                    'info'
                                );
                            }
                            lastSettleLogAt = now;
                        }
                        await sleep(1000);
                    }

                    if (!stabilized && !cancelRequested && !step.skipSettle) {
                        appendProgress(
                            `Settle timeout after ${Math.round((Date.now() - settleStart) / 1000)}s for target ${formatLimitWithVariant(step.targetMbps)}; proceeding with hold anyway.`,
                            'warn'
                        );
                    }

                    if ((stabilized || step.forceHoldWithoutSettle) && trackingStarted && !cancelRequested) {
                        const elapsedSinceStepStart = Date.now() - stepStartedAt;
                        const remainingBudgetMs = Math.max(0, maxStepDurationMs - elapsedSinceStepStart);
                        const holdMs = Math.min(requiredHoldAfterSettleMs, remainingBudgetMs);
                        const holdStart = Date.now();
                        let lastHeartbeatAt = 0;

                        appendProgress(
                            `Starting post-settle hold for ${(holdMs / 1000).toFixed(0)}s (step budget remaining ${(remainingBudgetMs / 1000).toFixed(0)}s).`,
                            'info'
                        );

                        while (Date.now() - holdStart < holdMs) {
                            if (cancelRequested) break;
                            const latest = await fetchSessionById(runState.sessionId);
                            if (!latest) {
                                await sleep(1000);
                                continue;
                            }
                            const { throughput, variant, wireThroughput, playerEstimate } = recordSample(latest, index, step);
                            const now = Date.now();
                            maybeRestartForPlaybackFailure(latest, now, index, step);

                            if (Date.now() - lastHeartbeatAt > 5000) {
                                appendProgress(
                                    `Holding ${formatLimitWithVariant(step.targetMbps)}; current throughput ${throughput !== null ? throughput.toFixed(2) : '—'} Mbps, wire_throughput ${wireThroughput !== null ? wireThroughput.toFixed(2) : '—'} Mbps, player_est_network ${playerEstimate !== null ? playerEstimate.toFixed(2) : '—'} Mbps, rendition ${variant !== null ? variant.toFixed(2) : '—'} Mbps.`,
                                    'info'
                                );
                                lastHeartbeatAt = Date.now();
                            }

                            await sleep(1000);
                        }
                    }

                    const stepElapsedMs = Date.now() - stepStartedAt;
                    if (stepElapsedMs >= maxStepDurationMs && !cancelRequested) {
                        appendProgress(
                            `Step capped at ${(maxStepDurationMs / 1000).toFixed(0)}s for target ${formatLimitWithVariant(step.targetMbps)}; advancing.`,
                            'warn'
                        );
                    }
                }

                const emergencyDownshift = buildEmergencyDownshiftResults(runState);
                const transientShockResults = buildTransientShockResults(runState);
                const startupCapsResults = buildStartupCapsResults(runState);
                const downshiftSeverityResults = buildDownshiftSeverityResults(runState);
                const hysteresisGapResults = buildHysteresisGapResults(runState);

                runState.emergencyDownshiftResults = emergencyDownshift;
                runState.transientShockResults = transientShockResults;
                runState.startupCapsResults = startupCapsResults;
                runState.downshiftSeverityResults = downshiftSeverityResults;
                runState.hysteresisGapResults = hysteresisGapResults;

                const summary = buildSummary(runState);
                if (emergencyDownshift) {
                    summary.emergencyDownshift = emergencyDownshift;
                }
                const periodThroughputStats = buildPeriodThroughputStats(runState);
                const periodBufferDepthStats = buildPeriodBufferDepthStats(runState);
                const limitResponseLatency = buildLimitResponseLatencies(runState);
                latestSummary = {
                    session_id: runState.sessionId,
                    master_url: runState.masterUrl,
                    started_at: new Date(runState.startedAt).toISOString(),
                    finished_at: new Date().toISOString(),
                    steps: runState.steps,
                    summary,
                    period_throughput_stats: periodThroughputStats,
                    period_buffer_depth_stats: periodBufferDepthStats,
                    limit_response_latency: limitResponseLatency,
                    emergency_downshift: emergencyDownshift,
                    transient_shock: transientShockResults,
                    startup_caps: startupCapsResults,
                    downshift_severity: downshiftSeverityResults,
                    hysteresis_gap: hysteresisGapResults,
                    switch_events: runState.switchEvents,
                    shaping_warnings: runState.shapingWarnings,
                    sample_count: runState.samples.length
                };
                latestReport = buildReportMarkdown(runState, summary);
                summaryEl.textContent = renderSummary(summary);
                if (reportEl) {
                    reportEl.textContent = latestReport;
                    reportEl.style.display = '';
                }
                downloadsEl.style.display = '';
                statusEl.textContent = 'Completed';
                appendProgress('Characterization completed.', 'success');
            } catch (error) {
                console.error(`${ABRCHAR_LOG_TAG} ABR characterization failed`, error);
                statusEl.textContent = 'Failed';
                summaryEl.textContent = `Run failed: ${error && error.message ? error.message : 'unknown error'}`;
                if (reportEl) {
                    reportEl.style.display = 'none';
                    reportEl.textContent = '';
                }
                appendProgress(`Run failed: ${error && error.message ? error.message : 'unknown error'}`, 'error');
            } finally {
                if (runLockAcquired) {
                    const lockSessionIdFinal = runState?.sessionId || (session && session.session_id ? String(session.session_id) : '');
                    if (lockSessionIdFinal) {
                        const unlocked = await setCharacterizationRunLock(lockSessionIdFinal, false, runOwnerToken);
                        if (!unlocked) {
                            appendProgress('WARNING: Failed to clear characterization run lock; use Save Settings to refresh control state.', 'warn');
                        }
                    }
                }
                if (runState?.sessionId) {
                    try {
                        await applyRate(runState.sessionId, 0);
                        const clearConfirmation = await confirmRateApplied(runState.sessionId, 0, 12000);
                        if (!clearConfirmation.ok) {
                            appendProgress(
                                `WARNING: Could not confirm unlimited shaping state (observed ${clearConfirmation.observedRateMbps !== null ? `${clearConfirmation.observedRateMbps.toFixed(2)} Mbps` : '—'}).`,
                                'warn'
                            );
                        }
                        appendProgress('Cleared throughput limit; network shaping is now unlimited.', 'info');
                    } catch (cleanupError) {
                        appendProgress(
                            `WARNING: Failed to clear throughput limit at end of run: ${cleanupError && cleanupError.message ? cleanupError.message : 'unknown error'}`,
                            'warn'
                        );
                    }
                }
                updateRuler({ limitMbps: null });
                runState = null;
                startButton.disabled = false;
                stopButton.disabled = true;
            }
        }

        async function forceClearCharacterizationState() {
            const currentSession = options.getSession ? options.getSession() : null;
            const sessionId = currentSession && currentSession.session_id ? String(currentSession.session_id) : '';
            if (!sessionId) {
                appendProgress('Force clear failed: no active session selected.', 'error');
                statusEl.textContent = 'No active session';
                return;
            }

            forceClearButton.disabled = true;
            const wasRunning = !!runState;
            if (wasRunning) {
                cancelRequested = true;
                appendProgress('Force clear requested: stopping active characterization loop.', 'warn');
            }
            statusEl.textContent = 'Force clearing…';

            try {
                const lockCleared = await patchSessionControlFields(sessionId, {
                    abrchar_run_lock: false,
                    abrchar_run_owner: '',
                    abrchar_run_started_at: ''
                });
                if (lockCleared) {
                    appendProgress('Characterization run lock cleared.', 'success');
                } else {
                    appendProgress('WARNING: Failed to confirm lock clear through session patch.', 'warn');
                }

                await applyRate(sessionId, 0);
                const clearConfirmation = await confirmRateApplied(sessionId, 0, 12000);
                if (!clearConfirmation.ok) {
                    appendProgress(
                        `WARNING: Force clear could not confirm unlimited shaping state (observed ${clearConfirmation.observedRateMbps !== null ? `${clearConfirmation.observedRateMbps.toFixed(2)} Mbps` : '—'}).`,
                        'warn'
                    );
                } else {
                    appendProgress('Force clear set shaping to unlimited.', 'success');
                }

                updateRuler({ limitMbps: null });
                statusEl.textContent = wasRunning ? 'Stopping…' : 'Idle';
            } catch (error) {
                appendProgress(`Force clear failed: ${error && error.message ? error.message : 'unknown error'}`, 'error');
                statusEl.textContent = 'Force clear failed';
            } finally {
                forceClearButton.disabled = false;
            }
        }

        startButton.addEventListener('click', () => {
            runCharacterization();
        });

        stopButton.addEventListener('click', () => {
            cancelRequested = true;
            statusEl.textContent = 'Stopping…';
        });

        forceClearButton.addEventListener('click', () => {
            forceClearCharacterizationState();
        });

        downloadJsonButton.addEventListener('click', () => {
            if (!latestSummary) return;
            downloadFile('summary.json', JSON.stringify(latestSummary, null, 2), 'application/json');
        });

        downloadMdButton.addEventListener('click', () => {
            if (!latestReport) return;
            downloadFile('report.md', latestReport, 'text/markdown');
        });
    }

    window.PlayerCharacterization = {
        mount
    };
})();
