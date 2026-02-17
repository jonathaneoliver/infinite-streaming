package throttle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Throttler defines the interface for controlling network throttling
type Throttler interface {
	// SetBandwidth sets the bandwidth limit in Mbps
	SetBandwidth(bandwidthMbps float64) error
	// Reset removes all throttling
	Reset() error
	// GetCurrentLimit returns the current bandwidth limit in Mbps
	GetCurrentLimit() (float64, error)
}

// HTTPThrottler implements Throttler using HTTP API calls
type HTTPThrottler struct {
	BaseURL string // Base URL of the throttling API (e.g., "http://localhost:8080")
	Port    int    // Port to throttle
	Timeout time.Duration
}

// NewHTTPThrottler creates a new HTTP-based throttler
func NewHTTPThrottler(baseURL string, port int) *HTTPThrottler {
	return &HTTPThrottler{
		BaseURL: baseURL,
		Port:    port,
		Timeout: 10 * time.Second,
	}
}

// SetBandwidth sets the bandwidth limit via HTTP API
func (t *HTTPThrottler) SetBandwidth(bandwidthMbps float64) error {
	url := fmt.Sprintf("%s/api/nft/bandwidth/%d", t.BaseURL, t.Port)
	
	payload := map[string]interface{}{
		"rate": bandwidthMbps,
	}
	
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	
	client := &http.Client{Timeout: t.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP error: %d", resp.StatusCode)
	}
	
	return nil
}

// Reset removes all throttling
func (t *HTTPThrottler) Reset() error {
	// Set to a very high bandwidth (100 Gbps = 100000 Mbps) to effectively disable throttling
	return t.SetBandwidth(100000)
}

// GetCurrentLimit returns the current bandwidth limit
func (t *HTTPThrottler) GetCurrentLimit() (float64, error) {
	// This would require a GET endpoint on the API
	// For now, we'll return 0 to indicate unknown
	return 0, fmt.Errorf("not implemented")
}

// ShellThrottler implements Throttler using shell commands
type ShellThrottler struct {
	Command     string   // Command template (e.g., "tc qdisc change dev eth0 root tbf rate {{.Rate}}mbit")
	ResetCmd    string   // Command to reset throttling
	CommandArgs []string // Additional arguments
}

// NewShellThrottler creates a new shell-based throttler
func NewShellThrottler(command string, resetCmd string) *ShellThrottler {
	return &ShellThrottler{
		Command:  command,
		ResetCmd: resetCmd,
	}
}

// SetBandwidth sets the bandwidth limit via shell command
func (t *ShellThrottler) SetBandwidth(bandwidthMbps float64) error {
	// Replace {{.Rate}} placeholder with actual rate
	cmd := strings.ReplaceAll(t.Command, "{{.Rate}}", fmt.Sprintf("%.2f", bandwidthMbps))
	
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}
	
	execCmd := exec.Command(parts[0], parts[1:]...)
	output, err := execCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("command failed: %w, output: %s", err, string(output))
	}
	
	return nil
}

// Reset removes all throttling
func (t *ShellThrottler) Reset() error {
	if t.ResetCmd == "" {
		return nil
	}
	
	parts := strings.Fields(t.ResetCmd)
	if len(parts) == 0 {
		return nil
	}
	
	cmd := exec.Command(parts[0], parts[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("reset command failed: %w, output: %s", err, string(output))
	}
	
	return nil
}

// GetCurrentLimit returns the current bandwidth limit
func (t *ShellThrottler) GetCurrentLimit() (float64, error) {
	return 0, fmt.Errorf("not implemented")
}

// Schedule represents a throttling schedule
type Schedule struct {
	Steps []Step
}

// Step represents a single step in the throttling schedule
type Step struct {
	BandwidthMbps float64       // Target bandwidth for this step
	Duration      time.Duration // How long to hold this bandwidth
	Description   string        // Human-readable description
}

// Execute executes the throttling schedule
func (s *Schedule) Execute(throttler Throttler, callback func(step Step, index int) error) error {
	for i, step := range s.Steps {
		if err := throttler.SetBandwidth(step.BandwidthMbps); err != nil {
			return fmt.Errorf("failed to set bandwidth for step %d: %w", i, err)
		}
		
		if callback != nil {
			if err := callback(step, i); err != nil {
				return fmt.Errorf("callback failed for step %d: %w", i, err)
			}
		}
		
		if step.Duration > 0 {
			time.Sleep(step.Duration)
		}
	}
	
	return nil
}

// GenerateStairStepSchedule creates a schedule that steps down or up through bandwidth levels
func GenerateStairStepSchedule(
	startBandwidth float64,
	endBandwidth float64,
	stepPercent float64,
	holdDuration time.Duration,
	direction string, // "down", "up", or "down-up"
) *Schedule {
	schedule := &Schedule{
		Steps: []Step{},
	}
	
	if direction == "down" || direction == "down-up" {
		// Generate steps going down
		current := startBandwidth
		for current >= endBandwidth {
			schedule.Steps = append(schedule.Steps, Step{
				BandwidthMbps: current,
				Duration:      holdDuration,
				Description:   fmt.Sprintf("Hold at %.2f Mbps", current),
			})
			current = current * (1 - stepPercent/100.0)
		}
	}
	
	if direction == "up" || direction == "down-up" {
		// Generate steps going up
		current := endBandwidth
		for current <= startBandwidth {
			schedule.Steps = append(schedule.Steps, Step{
				BandwidthMbps: current,
				Duration:      holdDuration,
				Description:   fmt.Sprintf("Hold at %.2f Mbps", current),
			})
			current = current * (1 + stepPercent/100.0)
		}
	}
	
	return schedule
}
