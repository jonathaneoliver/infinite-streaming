package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the experiment configuration
type Config struct {
	// HLS configuration
	HLSURL string `yaml:"hls_url" json:"hls_url"`
	
	// Experiment parameters
	ExperimentDuration time.Duration `yaml:"experiment_duration" json:"experiment_duration"`
	PlayerID           string        `yaml:"player_id" json:"player_id"`
	
	// Throttle schedule
	Throttle ThrottleConfig `yaml:"throttle" json:"throttle"`
	
	// Output configuration
	OutputDir string `yaml:"output_dir" json:"output_dir"`
	
	// Analysis parameters
	Analysis AnalysisConfig `yaml:"analysis" json:"analysis"`
}

// ThrottleConfig defines throttling parameters
type ThrottleConfig struct {
	// Control method
	Method string `yaml:"method" json:"method"` // "http" or "shell"
	
	// HTTP method parameters
	HTTPURL string `yaml:"http_url" json:"http_url"`
	Port    int    `yaml:"port" json:"port"`
	
	// Shell method parameters
	Command  string `yaml:"command" json:"command"`
	ResetCmd string `yaml:"reset_cmd" json:"reset_cmd"`
	
	// Schedule parameters
	WarmupBandwidth float64       `yaml:"warmup_bandwidth" json:"warmup_bandwidth"` // Mbps
	StepPercent     float64       `yaml:"step_percent" json:"step_percent"`         // Percentage step (e.g., 10)
	HoldDuration    time.Duration `yaml:"hold_duration" json:"hold_duration"`       // Duration to hold each step
	MinBandwidth    float64       `yaml:"min_bandwidth" json:"min_bandwidth"`       // Mbps
	MaxBandwidth    float64       `yaml:"max_bandwidth" json:"max_bandwidth"`       // Mbps
	Direction       string        `yaml:"direction" json:"direction"`               // "down", "up", or "down-up"
}

// AnalysisConfig defines analysis parameters
type AnalysisConfig struct {
	ThroughputWindowSize int `yaml:"throughput_window_size" json:"throughput_window_size"` // Number of segments for throughput estimation
}

// LoadConfig loads configuration from a file (YAML or JSON)
func LoadConfig(filepath string) (*Config, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	
	config := &Config{}
	
	// Try YAML first
	if err := yaml.Unmarshal(data, config); err != nil {
		// Try JSON
		if jsonErr := json.Unmarshal(data, config); jsonErr != nil {
			return nil, fmt.Errorf("failed to parse config as YAML or JSON: %w (JSON: %v)", err, jsonErr)
		}
	}
	
	// Apply defaults
	config.applyDefaults()
	
	// Validate
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	
	return config, nil
}

// applyDefaults applies default values to unset fields
func (c *Config) applyDefaults() {
	if c.ExperimentDuration == 0 {
		c.ExperimentDuration = 5 * time.Minute
	}
	
	if c.PlayerID == "" {
		c.PlayerID = "abrchar-player"
	}
	
	if c.OutputDir == "" {
		c.OutputDir = "./abrchar-output"
	}
	
	if c.Throttle.Method == "" {
		c.Throttle.Method = "http"
	}
	
	if c.Throttle.HoldDuration == 0 {
		c.Throttle.HoldDuration = 20 * time.Second
	}
	
	if c.Throttle.StepPercent == 0 {
		c.Throttle.StepPercent = 10
	}
	
	if c.Throttle.Direction == "" {
		c.Throttle.Direction = "down"
	}
	
	if c.Analysis.ThroughputWindowSize == 0 {
		c.Analysis.ThroughputWindowSize = 5
	}
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.HLSURL == "" {
		return fmt.Errorf("hls_url is required")
	}
	
	if c.Throttle.Method != "http" && c.Throttle.Method != "shell" {
		return fmt.Errorf("throttle method must be 'http' or 'shell'")
	}
	
	if c.Throttle.Method == "http" {
		if c.Throttle.HTTPURL == "" {
			return fmt.Errorf("http_url is required for HTTP throttle method")
		}
		if c.Throttle.Port == 0 {
			return fmt.Errorf("port is required for HTTP throttle method")
		}
	}
	
	if c.Throttle.Method == "shell" {
		if c.Throttle.Command == "" {
			return fmt.Errorf("command is required for shell throttle method")
		}
	}
	
	if c.Throttle.Direction != "down" && c.Throttle.Direction != "up" && c.Throttle.Direction != "down-up" {
		return fmt.Errorf("direction must be 'down', 'up', or 'down-up'")
	}
	
	if c.Throttle.StepPercent <= 0 || c.Throttle.StepPercent > 100 {
		return fmt.Errorf("step_percent must be between 0 and 100")
	}
	
	if c.Analysis.ThroughputWindowSize <= 0 {
		return fmt.Errorf("throughput_window_size must be positive")
	}
	
	return nil
}

// SaveExample saves an example configuration file
func SaveExample(filepath string) error {
	example := &Config{
		HLSURL:             "https://example.com/master.m3u8",
		ExperimentDuration: 5 * time.Minute,
		PlayerID:           "abrchar-player",
		Throttle: ThrottleConfig{
			Method:          "http",
			HTTPURL:         "http://localhost:8080",
			Port:            30081,
			WarmupBandwidth: 20.0,
			StepPercent:     10.0,
			HoldDuration:    20 * time.Second,
			MinBandwidth:    1.0,
			MaxBandwidth:    20.0,
			Direction:       "down",
		},
		OutputDir: "./abrchar-output",
		Analysis: AnalysisConfig{
			ThroughputWindowSize: 5,
		},
	}
	
	data, err := yaml.Marshal(example)
	if err != nil {
		return fmt.Errorf("failed to marshal example config: %w", err)
	}
	
	if err := os.WriteFile(filepath, data, 0644); err != nil {
		return fmt.Errorf("failed to write example config: %w", err)
	}
	
	return nil
}
