package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Event represents a single telemetry event from the player
type Event struct {
	Timestamp       time.Time `json:"timestamp"`
	SessionID       string    `json:"session_id,omitempty"`
	SelectedVariant string    `json:"selected_variant,omitempty"`    // Variant URI or identifier
	VariantBitrate  float64   `json:"variant_bitrate_mbps,omitempty"` // Current variant bitrate in Mbps
	BufferDepth     float64   `json:"buffer_depth_s,omitempty"`       // Buffer depth in seconds
	BufferEnd       float64   `json:"buffer_end_s,omitempty"`         // Buffer end position in seconds
	Position        float64   `json:"position_s,omitempty"`           // Playback position in seconds
	StallCount      int       `json:"stall_count,omitempty"`          // Cumulative stall count
	StallTime       float64   `json:"stall_time_s,omitempty"`         // Cumulative stall time in seconds
	EventType       string    `json:"event_type,omitempty"`           // Event type (e.g., "variant_change", "stall")
	PlayerState     string    `json:"player_state,omitempty"`         // Player state (e.g., "playing", "paused")
	NetworkBitrate  float64   `json:"network_bitrate_mbps,omitempty"` // Player's estimated network bitrate in Mbps
}

// SegmentDownload represents metrics for a single segment download
type SegmentDownload struct {
	URL         string    `json:"url"`
	Timestamp   time.Time `json:"timestamp"`
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	Bytes       int64     `json:"bytes"`
	DurationMs  float64   `json:"duration_ms"`
	ThroughputMbps float64 `json:"throughput_mbps"`
	VariantBitrate float64 `json:"variant_bitrate_mbps,omitempty"`
}

// Collector collects and writes telemetry events
type Collector struct {
	outputDir string
	eventsFile *os.File
	segmentsFile *os.File
}

// NewCollector creates a new telemetry collector
func NewCollector(outputDir string) (*Collector, error) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create output directory: %w", err)
	}
	
	eventsPath := fmt.Sprintf("%s/telemetry_events.jsonl", outputDir)
	eventsFile, err := os.Create(eventsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create events file: %w", err)
	}
	
	segmentsPath := fmt.Sprintf("%s/segment_downloads.jsonl", outputDir)
	segmentsFile, err := os.Create(segmentsPath)
	if err != nil {
		eventsFile.Close()
		return nil, fmt.Errorf("failed to create segments file: %w", err)
	}
	
	return &Collector{
		outputDir: outputDir,
		eventsFile: eventsFile,
		segmentsFile: segmentsFile,
	}, nil
}

// RecordEvent writes a telemetry event
func (c *Collector) RecordEvent(event Event) error {
	if c.eventsFile == nil {
		return fmt.Errorf("collector not initialized")
	}
	
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}
	
	if _, err := c.eventsFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write event: %w", err)
	}
	
	return nil
}

// RecordSegmentDownload writes a segment download metric
func (c *Collector) RecordSegmentDownload(download SegmentDownload) error {
	if c.segmentsFile == nil {
		return fmt.Errorf("collector not initialized")
	}
	
	data, err := json.Marshal(download)
	if err != nil {
		return fmt.Errorf("failed to marshal download: %w", err)
	}
	
	if _, err := c.segmentsFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write download: %w", err)
	}
	
	return nil
}

// Close closes all open files
func (c *Collector) Close() error {
	var errors []error
	
	if c.eventsFile != nil {
		if err := c.eventsFile.Close(); err != nil {
			errors = append(errors, err)
		}
	}
	
	if c.segmentsFile != nil {
		if err := c.segmentsFile.Close(); err != nil {
			errors = append(errors, err)
		}
	}
	
	if len(errors) > 0 {
		return fmt.Errorf("errors closing files: %v", errors)
	}
	
	return nil
}

// LoadEvents loads telemetry events from a JSON lines file
func LoadEvents(filepath string) ([]Event, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()
	
	var events []Event
	decoder := json.NewDecoder(file)
	
	for decoder.More() {
		var event Event
		if err := decoder.Decode(&event); err != nil {
			return nil, fmt.Errorf("failed to decode event: %w", err)
		}
		events = append(events, event)
	}
	
	return events, nil
}

// LoadSegmentDownloads loads segment download metrics from a JSON lines file
func LoadSegmentDownloads(filepath string) ([]SegmentDownload, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()
	
	var downloads []SegmentDownload
	decoder := json.NewDecoder(file)
	
	for decoder.More() {
		var download SegmentDownload
		if err := decoder.Decode(&download); err != nil {
			return nil, fmt.Errorf("failed to decode download: %w", err)
		}
		downloads = append(downloads, download)
	}
	
	return downloads, nil
}
