package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

func findOutputDirectories(cfg map[string]interface{}, outputDir string) []string {
	outputName, _ := cfg["output_name"].(string)
	if outputName == "" {
		outputName = "output"
	}
	selection, _ := cfg["codec_selection"].(string)
	paths := []string{}
	if selection == "" || selection == "both" || selection == "hevc" {
		if dirExists(filepath.Join(outputDir, outputName+"_hevc")) {
			paths = append(paths, outputName+"_hevc")
		}
	}
	if selection == "" || selection == "both" || selection == "h264" || selection == "av1" {
		suffix := "_h264"
		if selection == "av1" {
			suffix = "_av1"
		}
		if dirExists(filepath.Join(outputDir, outputName+suffix)) {
			paths = append(paths, outputName+suffix)
		}
	}
	return paths
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
