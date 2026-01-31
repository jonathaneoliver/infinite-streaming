package util

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
)

type VideoMetadata struct {
	Raw        map[string]interface{}
	Duration   float64
	Resolution string
	Codec      string
}

func ValidateVideo(path string) bool {
	cmd := exec.Command("ffprobe", "-v", "error", path)
	return cmd.Run() == nil
}

func GetVideoMetadata(path string) VideoMetadata {
	cmd := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path)
	out, err := cmd.Output()
	if err != nil {
		return VideoMetadata{Raw: map[string]interface{}{}}
	}
	var data map[string]interface{}
	if err := json.Unmarshal(out, &data); err != nil {
		return VideoMetadata{Raw: map[string]interface{}{}}
	}

	meta := VideoMetadata{Raw: map[string]interface{}{}}
	streams, _ := data["streams"].([]interface{})
	for _, stream := range streams {
		vs, _ := stream.(map[string]interface{})
		if vs["codec_type"] == "video" {
			if codec, ok := vs["codec_name"].(string); ok {
				meta.Codec = codec
				meta.Raw["codec"] = codec
			}
			width, _ := toInt(vs["width"])
			height, _ := toInt(vs["height"])
			if width > 0 && height > 0 {
				meta.Resolution = strconv.Itoa(width) + "x" + strconv.Itoa(height)
				meta.Raw["resolution"] = meta.Resolution
				meta.Raw["width"] = width
				meta.Raw["height"] = height
			}
			if fpsStr, ok := vs["r_frame_rate"].(string); ok && fpsStr != "" {
				meta.Raw["fps"] = parseFraction(fpsStr)
			}
			break
		}
	}
	if format, ok := data["format"].(map[string]interface{}); ok {
		if durationStr, ok := format["duration"].(string); ok {
			if d, err := strconv.ParseFloat(durationStr, 64); err == nil {
				meta.Duration = d
				meta.Raw["duration"] = d
			}
		}
		if bitrateStr, ok := format["bit_rate"].(string); ok {
			if b, err := strconv.Atoi(bitrateStr); err == nil {
				meta.Raw["bitrate"] = b
			}
		}
		if formatName, ok := format["format_name"].(string); ok {
			meta.Raw["format"] = formatName
		}
	}
	return meta
}

func FloatPtr(val float64) *float64 {
	if val == 0 {
		return nil
	}
	return &val
}

func StringPtr(val string) *string {
	if val == "" {
		return nil
	}
	return &val
}

func parseFraction(val string) float64 {
	parts := strings.Split(val, "/")
	if len(parts) != 2 {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
		return 0
	}
	n, _ := strconv.ParseFloat(parts[0], 64)
	d, _ := strconv.ParseFloat(parts[1], 64)
	if d == 0 {
		return 0
	}
	return n / d
}
