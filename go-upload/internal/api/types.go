package api

import (
	"encoding/json"
	"os"
)

type UploadMetadata struct {
	SourceID       string   `json:"source_id"`
	JobID          string   `json:"job_id"`
	JobIDs         []string `json:"job_ids,omitempty"`
	Filename       string   `json:"filename"`
	HybridFilename string   `json:"hybrid_filename"`
	ExpectedSize   int64    `json:"expected_size"`
	ReceivedSize   int64    `json:"received_size"`
	OutputName     string   `json:"output_name"`
}

func (m *UploadMetadata) Save(path string) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func LoadUploadMetadata(path string) (*UploadMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta UploadMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

type BatchReencodeRequest struct {
	SourceIDs       []string `json:"source_ids"`
	CodecSelection  string   `json:"codec_selection"`
	MaxResolution   *string  `json:"max_resolution"`
	HlsFormat       string   `json:"hls_format"`
	ForceSoftware   bool     `json:"force_software"`
	Padding         string   `json:"padding"`
	DurationLimit   *int     `json:"duration_limit"`
	PartialDurations []int   `json:"partial_durations"`
	SegmentDuration int      `json:"segment_duration"`
	GopDuration     float64  `json:"gop_duration"`
	KeepMezzanine   bool     `json:"keep_mezzanine"`
}
