package runner

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
)

// WriteChart renders an HTML file with the report's samples plotted as
// four traces against time: applied rate cap, measured network bitrate,
// fetched variant (step function), and buffer depth. Chart.js loaded from
// cdnjs — opens in any browser, no install.
//
// Returns the path to the written .html. Designed to be called from a
// mode test immediately after WriteReport so the operator gets three
// artifacts per run: the JSON (machine), the MD (table view), the HTML
// (visual).
func WriteChart(outdir, basename string, r *Report) (string, error) {
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", outdir, err)
	}
	htmlPath := filepath.Join(outdir, basename+".html")

	// Convert samples to compact JS arrays. We want ms epoch for the X
	// axis (Chart.js time scale accepts millis directly).
	type point struct {
		X int64   `json:"x"`
		Y float64 `json:"y"`
	}
	applied := make([]point, 0, len(r.Samples))
	network := make([]point, 0, len(r.Samples))
	bitrate := make([]point, 0, len(r.Samples))
	buffer := make([]point, 0, len(r.Samples))
	variant := make([]point, 0, len(r.Samples))
	for _, s := range r.Samples {
		x := s.Ts.UnixMilli()
		applied = append(applied, point{x, s.AppliedRateMbps})
		network = append(network, point{x, s.NetworkBitrateMbps})
		bitrate = append(bitrate, point{x, s.VideoBitrateMbps})
		buffer = append(buffer, point{x, s.BufferDepthS})
		// Plot variant idx as descending-quality numeric: 0 = top, N-1 = bottom.
		// We render the Y axis reversed so "higher quality is up."
		if s.VariantIdx >= 0 {
			variant = append(variant, point{x, float64(s.VariantIdx)})
		}
	}

	// Variant tick labels (top→bottom = idx 0 → N-1)
	type variantTick struct {
		Idx        int    `json:"idx"`
		Resolution string `json:"resolution"`
	}
	ticks := make([]variantTick, 0, len(r.Variants))
	for i, v := range r.Variants {
		ticks = append(ticks, variantTick{Idx: i, Resolution: v.Resolution})
	}

	// Step boundaries — vertical lines so the operator can see where
	// each cap change happened.
	type stepMark struct {
		X      int64   `json:"x"`
		Label  string  `json:"label"`
		Cap    float64 `json:"cap"`
	}
	stepMarks := make([]stepMark, 0, len(r.Steps))
	for _, st := range r.Steps {
		label := ""
		if st.Variant != nil {
			label = fmt.Sprintf("%s %+d%%", st.Variant.Resolution, st.Variant.MarginPct)
		}
		stepMarks = append(stepMarks, stepMark{
			X:     st.StartedAt.UnixMilli(),
			Label: label,
			Cap:   st.RateMbps,
		})
	}

	data := struct {
		Title    string
		Applied  string
		Network  string
		Bitrate  string
		Buffer   string
		Variant  string
		Ticks    string
		Steps    string
		Subtitle string
	}{
		Title:    fmt.Sprintf("%s — %s", r.Mode, r.Device.String()),
		Applied:  mustJSON(applied),
		Network:  mustJSON(network),
		Bitrate:  mustJSON(bitrate),
		Buffer:   mustJSON(buffer),
		Variant:  mustJSON(variant),
		Ticks:    mustJSON(ticks),
		Steps:    mustJSON(stepMarks),
		Subtitle: fmt.Sprintf("player %s · %d samples · %d steps", r.PlayerID, len(r.Samples), len(r.Steps)),
	}

	tpl := template.Must(template.New("chart").Parse(chartHTML))
	f, err := os.Create(htmlPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := tpl.Execute(f, data); err != nil {
		return "", err
	}
	return htmlPath, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

const chartHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<style>
  body { font: 14px/1.4 -apple-system, system-ui, sans-serif; margin: 24px; color: #222; }
  h1 { margin: 0; font-size: 18px; }
  h2 { font-size: 13px; color: #555; font-weight: 500; margin: 4px 0 16px; }
  .chart-wrap { position: relative; height: 70vh; }
  .legend { font-size: 12px; color: #555; margin-top: 8px; }
</style>
<script src="https://cdnjs.cloudflare.com/ajax/libs/Chart.js/4.4.1/chart.umd.min.js"></script>
<script src="https://cdnjs.cloudflare.com/ajax/libs/moment.js/2.29.4/moment.min.js"></script>
<script src="https://cdnjs.cloudflare.com/ajax/libs/chartjs-adapter-moment/1.0.1/chartjs-adapter-moment.min.js"></script>
<script src="https://cdnjs.cloudflare.com/ajax/libs/chartjs-plugin-annotation/3.0.1/chartjs-plugin-annotation.min.js"></script>
</head>
<body>
<h1>{{.Title}}</h1>
<h2>{{.Subtitle}}</h2>
<div class="chart-wrap"><canvas id="c"></canvas></div>
<div class="legend">
  <strong>Left axis (Mbps):</strong> applied cap (orange step), network throughput (green), video bitrate / variant peak (blue dots).
  <strong>Right axis #1 (seconds):</strong> buffer depth (purple area).
  <strong>Right axis #2 (variant):</strong> fetched variant rung (red step — top of chart is highest quality).
</div>
<script>
const applied = {{.Applied}};
const network = {{.Network}};
const bitrate = {{.Bitrate}};
const buffer = {{.Buffer}};
const variant = {{.Variant}};
const ticks = {{.Ticks}};
const steps = {{.Steps}};

Chart.register(window['chartjs-plugin-annotation']);

const stepLines = steps.map(s => ({
  type: 'line', xMin: s.x, xMax: s.x, borderColor: 'rgba(0,0,0,0.12)', borderWidth: 1, borderDash: [2,2],
  label: { display: false }
}));

const ctx = document.getElementById('c');
new Chart(ctx, {
  type: 'line',
  data: {
    datasets: [
      { label: 'applied cap (Mbps)', data: applied, yAxisID: 'y',
        borderColor: '#d97706', backgroundColor: '#d97706',
        stepped: 'after', pointRadius: 0, borderWidth: 2 },
      { label: 'network bitrate (Mbps)', data: network, yAxisID: 'y',
        borderColor: '#059669', backgroundColor: '#059669',
        pointRadius: 0, borderWidth: 1.5 },
      { label: 'video bitrate (Mbps)', data: bitrate, yAxisID: 'y',
        borderColor: '#2563eb', backgroundColor: '#2563eb',
        pointRadius: 2, showLine: false },
      { label: 'buffer depth (s)', data: buffer, yAxisID: 'y_buf',
        borderColor: '#7c3aed', backgroundColor: 'rgba(124,58,237,0.10)',
        fill: true, pointRadius: 0, borderWidth: 1.5 },
      { label: 'fetched variant', data: variant, yAxisID: 'y_var',
        borderColor: '#dc2626', backgroundColor: '#dc2626',
        stepped: 'after', pointRadius: 0, borderWidth: 2 }
    ]
  },
  options: {
    animation: false,
    responsive: true,
    maintainAspectRatio: false,
    interaction: { mode: 'nearest', axis: 'x', intersect: false },
    scales: {
      x: { type: 'time', time: { tooltipFormat: 'HH:mm:ss' }, title: { display: true, text: 'time' } },
      y: { type: 'logarithmic', position: 'left', title: { display: true, text: 'Mbps (log)' } },
      y_buf: { position: 'right', title: { display: true, text: 'buffer (s)' }, grid: { drawOnChartArea: false } },
      y_var: { position: 'right', reverse: true,
               min: -0.5, max: ticks.length - 0.5,
               title: { display: true, text: 'variant' },
               grid: { drawOnChartArea: false },
               ticks: { stepSize: 1, callback: v => (ticks[v] ? ticks[v].resolution : '') },
               offset: true }
    },
    plugins: {
      legend: { position: 'top' },
      annotation: { annotations: stepLines },
      tooltip: { mode: 'index', intersect: false }
    }
  }
});
</script>
</body>
</html>
`
