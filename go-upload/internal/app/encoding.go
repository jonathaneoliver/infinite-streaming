package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func buildAbrCommand(scriptPath, outputDir, inputFile string, cfg map[string]interface{}) ([]string, error) {
	if scriptPath == "" {
		return nil, fmt.Errorf("ABR script path not configured")
	}
	cmd := []string{"nice", "-n", "19", "bash", scriptPath, "--input", inputFile}

	if outputName, ok := cfg["output_name"].(string); ok && outputName != "" {
		cmd = append(cmd, "--output", outputName)
	}
	cmd = append(cmd, "--output-dir", outputDir)

	if codecSelection, ok := cfg["codec_selection"].(string); ok && codecSelection != "" && codecSelection != "both" {
		cmd = append(cmd, "--codec", codecSelection)
	}
	if maxRes, ok := cfg["max_resolution"].(string); ok && maxRes != "" {
		cmd = append(cmd, "--max-res", maxRes)
	}
	if hlsFormat, ok := cfg["hls_format"].(string); ok && hlsFormat != "" && hlsFormat != "fmp4" {
		cmd = append(cmd, "--hls-format", hlsFormat)
	}
	if force, ok := cfg["force_software"].(bool); ok && force {
		cmd = append(cmd, "--force-software")
	}
	if padding, ok := cfg["padding"].(string); ok {
		switch padding {
		case "black":
			cmd = append(cmd, "--padding")
		case "pink":
			cmd = append(cmd, "--padding-pink")
		case "none":
			cmd = append(cmd, "--no-padding")
		}
	}
	if val, ok := toInt(cfg["duration_limit"]); ok {
		cmd = append(cmd, "--time", strconv.Itoa(val))
	}
	if val, ok := toInt(cfg["segment_duration"]); ok {
		cmd = append(cmd, "--segment-duration", strconv.Itoa(val))
	}
	if pdur, ok := toFloat(cfg["partial_duration"]); ok {
		if pdur > 10 {
			pdur = pdur / 1000.0
		}
		cmd = append(cmd, "--partial-duration", fmt.Sprintf("%g", pdur))
	}
	if gdur, ok := toFloat(cfg["gop_duration"]); ok {
		if gdur > 10 {
			gdur = gdur / 1000.0
		}
		cmd = append(cmd, "--gop-duration", fmt.Sprintf("%g", gdur))
	}
	if keep, ok := cfg["keep_mezzanine"].(bool); ok && keep {
		cmd = append(cmd, "--keep-mezzanine")
	}
	return cmd, nil
}

// findOutputDirectories reports the content directories the ABR script just
// produced, as bare names relative to outputDir.
//
// It DISCOVERS them rather than reconstructing the name. The script owns the
// naming convention and has grown two suffixes Go cannot predict: a profile tag
// appended after the codec (`_xs` on the apple-uniq-live-xs ladder, chosen by
// the script's --ladder default) and a `_YYYYMMDD_HHMMSS` disambiguator added
// when the target directory already exists. Rebuilding `<name>_<codec>` by
// concatenation missed both, and the failure was silent: no output paths
// recorded on the job, and warmGoLiveWorkers never called, so the first play of
// the new content paid a cold start with nothing in the log to explain it.
//
// Globbing keeps the script as the single source of truth for naming, so a
// future tag needs no matching change here.
func findOutputDirectories(cfg map[string]interface{}, outputDir string) []string {
	outputName, _ := cfg["output_name"].(string)
	if outputName == "" {
		outputName = "output"
	}
	selection, _ := cfg["codec_selection"].(string)

	// Which codecs this run was asked to produce. "" and "both" mean
	// hevc+h264; "all" means every codec — that case previously matched none
	// of the branches and returned an empty list.
	var codecs []string
	switch selection {
	case "", "both":
		codecs = []string{"hevc", "h264"}
	case "all":
		codecs = []string{"hevc", "h264", "av1"}
	default:
		codecs = []string{selection}
	}

	paths := []string{}
	for _, codec := range codecs {
		if name := newestPackageDir(outputDir, outputName, codec); name != "" {
			paths = append(paths, name)
		}
	}
	return paths
}

// newestPackageDir finds the fMP4 package directory for one codec: the entry
// named `<outputName>_<codec>` optionally followed by `_<tag>` and/or a
// `_<timestamp>`. When more than one matches (a re-encode that collided and got
// a timestamp), the most recently modified wins — that is the one this run just
// wrote. Returns "" when nothing matches.
func newestPackageDir(outputDir, outputName, codec string) string {
	prefix := outputName + "_" + codec
	matches, err := filepath.Glob(filepath.Join(outputDir, prefix+"*"))
	if err != nil {
		return ""
	}

	var best string
	var bestMod time.Time
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		name := filepath.Base(path)
		// Reject a longer codec name matched by the prefix, and skip the
		// parallel MPEG-TS package — it is a separate artifact, not this
		// content's fMP4 output, and was never reported before.
		rest := strings.TrimPrefix(name, prefix)
		if rest != "" && !strings.HasPrefix(rest, "_") {
			continue
		}
		if rest == "_ts" || strings.HasPrefix(rest, "_ts_") {
			continue
		}
		if best == "" || info.ModTime().After(bestMod) {
			best, bestMod = name, info.ModTime()
		}
	}
	return best
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func toInt(val interface{}) (int, bool) {
	switch v := val.(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	case string:
		if v == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(v)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func toFloat(val interface{}) (float64, bool) {
	switch v := val.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		if v == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(v, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
