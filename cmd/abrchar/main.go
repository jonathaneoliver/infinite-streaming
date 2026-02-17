package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/boss/abrchar/pkg/analysis"
	"github.com/boss/abrchar/pkg/config"
	"github.com/boss/abrchar/pkg/output"
	"github.com/boss/abrchar/pkg/playlist"
	"github.com/boss/abrchar/pkg/telemetry"
	"github.com/boss/abrchar/pkg/throttle"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runCmd := flag.NewFlagSet("run", flag.ExitOnError)
		configFile := runCmd.String("config", "", "Path to config file (YAML or JSON)")
		hlsURL := runCmd.String("hls-url", "", "HLS multivariant playlist URL")
		outputDir := runCmd.String("output", "", "Output directory for results")
		runCmd.Parse(os.Args[2:])

		if *configFile == "" && *hlsURL == "" {
			fmt.Fprintln(os.Stderr, "Error: Either --config or --hls-url is required")
			runCmd.Usage()
			os.Exit(1)
		}

		if err := runExperiment(*configFile, *hlsURL, *outputDir); err != nil {
			fmt.Fprintf(os.Stderr, "Error running experiment: %v\n", err)
			os.Exit(1)
		}

	case "analyze":
		analyzeCmd := flag.NewFlagSet("analyze", flag.ExitOnError)
		dataDir := analyzeCmd.String("data", "", "Directory containing telemetry logs")
		hlsURL := analyzeCmd.String("hls-url", "", "HLS multivariant playlist URL for ladder parsing")
		outputDir := analyzeCmd.String("output", "", "Output directory for analysis results")
		analyzeCmd.Parse(os.Args[2:])

		if *dataDir == "" {
			fmt.Fprintln(os.Stderr, "Error: --data is required")
			analyzeCmd.Usage()
			os.Exit(1)
		}

		if err := analyzeData(*dataDir, *hlsURL, *outputDir); err != nil {
			fmt.Fprintf(os.Stderr, "Error analyzing data: %v\n", err)
			os.Exit(1)
		}

	case "version":
		fmt.Printf("abrchar version %s\n", version)

	case "help", "-h", "--help":
		printUsage()

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `abrchar - ABR Characterization Tool for HLS Players

USAGE:
  abrchar <command> [options]

COMMANDS:
  run       Execute an ABR characterization experiment
  analyze   Analyze existing telemetry logs
  version   Print version information
  help      Show this help message

RUN OPTIONS:
  --config <file>     Path to experiment config file (YAML or JSON)
  --hls-url <url>     HLS multivariant playlist URL
  --output <dir>      Output directory for results

ANALYZE OPTIONS:
  --data <dir>        Directory containing telemetry logs
  --hls-url <url>     HLS multivariant playlist URL
  --output <dir>      Output directory for analysis results

EXAMPLES:
  # Run experiment with config file
  abrchar run --config experiment.yaml

  # Run experiment with command-line options
  abrchar run --hls-url https://example.com/master.m3u8 --output ./results

  # Analyze existing telemetry data
  abrchar analyze --data ./telemetry --hls-url https://example.com/master.m3u8 --output ./analysis

For more information, see cmd/abrchar/README.md
`)
}

func runExperiment(configFile, hlsURL, outputDir string) error {
	var cfg *config.Config
	var err error
	
	// Load config if provided
	if configFile != "" {
		cfg, err = config.LoadConfig(configFile)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
	} else {
		// Create minimal config from command-line args
		cfg = &config.Config{
			HLSURL:    hlsURL,
			OutputDir: outputDir,
		}
		cfg.Throttle.Method = "http"
		cfg.Throttle.HTTPURL = "http://localhost:8080"
		cfg.Throttle.Port = 30081
		
		// Apply defaults
		if cfg.OutputDir == "" {
			cfg.OutputDir = "./abrchar-output"
		}
	}
	
	fmt.Printf("Running ABR characterization experiment\n")
	fmt.Printf("HLS URL: %s\n", cfg.HLSURL)
	fmt.Printf("Output directory: %s\n", cfg.OutputDir)
	fmt.Printf("\nNOTE: This is a skeleton implementation.\n")
	fmt.Printf("To fully implement the 'run' command, you would need to:\n")
	fmt.Printf("1. Parse the HLS multivariant playlist\n")
	fmt.Printf("2. Start a player instance (browser automation or native player)\n")
	fmt.Printf("3. Execute the throttling schedule via the throttler\n")
	fmt.Printf("4. Collect telemetry events from the player\n")
	fmt.Printf("5. Record segment download metrics\n")
	fmt.Printf("6. Save telemetry data to output directory\n")
	fmt.Printf("\nFor now, you can use the 'analyze' command on existing telemetry data.\n")
	
	// Parse the ladder to validate HLS URL
	fmt.Printf("\nValidating HLS URL and parsing bitrate ladder...\n")
	ladder, err := playlist.ParseHLSMaster(cfg.HLSURL)
	if err != nil {
		return fmt.Errorf("failed to parse HLS master playlist: %w", err)
	}
	
	fmt.Printf("Successfully parsed bitrate ladder with %d variants:\n", len(ladder.Variants))
	for i, variant := range ladder.Variants {
		fmt.Printf("  %d: %s\n", i, variant.String())
	}
	
	// Create throttler instance
	var throttler throttle.Throttler
	if cfg.Throttle.Method == "http" {
		throttler = throttle.NewHTTPThrottler(cfg.Throttle.HTTPURL, cfg.Throttle.Port)
		fmt.Printf("\nThrottler: HTTP API at %s (port %d)\n", cfg.Throttle.HTTPURL, cfg.Throttle.Port)
	} else {
		throttler = throttle.NewShellThrottler(cfg.Throttle.Command, cfg.Throttle.ResetCmd)
		fmt.Printf("\nThrottler: Shell command\n")
	}
	
	// Generate schedule
	schedule := throttle.GenerateStairStepSchedule(
		cfg.Throttle.WarmupBandwidth,
		cfg.Throttle.MinBandwidth,
		cfg.Throttle.StepPercent,
		cfg.Throttle.HoldDuration,
		cfg.Throttle.Direction,
	)
	
	fmt.Printf("\nGenerated throttle schedule with %d steps:\n", len(schedule.Steps))
	for i, step := range schedule.Steps {
		fmt.Printf("  Step %d: %.2f Mbps for %.0fs\n", i, step.BandwidthMbps, step.Duration.Seconds())
		if i >= 4 {
			fmt.Printf("  ... and %d more steps\n", len(schedule.Steps)-5)
			break
		}
	}
	
	// Note about experiment execution
	_ = throttler // Use the throttler variable
	fmt.Printf("\n✓ Configuration validated successfully\n")
	fmt.Printf("\nTo execute a real experiment:\n")
	fmt.Printf("1. Ensure a player is running and sending telemetry to go-proxy\n")
	fmt.Printf("2. Ensure go-proxy is running and exposing the throttling API\n")
	fmt.Printf("3. The tool would execute the schedule and collect data\n")
	
	return nil
}

func analyzeData(dataDir, hlsURL, outputDir string) error {
	fmt.Printf("Analyzing telemetry data from: %s\n", dataDir)
	
	// Set default output directory
	if outputDir == "" {
		outputDir = filepath.Join(dataDir, "analysis")
	}
	
	// Create output directory
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}
	
	// Parse HLS master playlist
	if hlsURL == "" {
		return fmt.Errorf("HLS URL is required for analysis (use --hls-url)")
	}
	
	fmt.Printf("Parsing HLS master playlist: %s\n", hlsURL)
	ladder, err := playlist.ParseHLSMaster(hlsURL)
	if err != nil {
		return fmt.Errorf("failed to parse HLS master playlist: %w", err)
	}
	
	fmt.Printf("Loaded bitrate ladder with %d variants\n", len(ladder.Variants))
	
	// Load telemetry events
	eventsPath := filepath.Join(dataDir, "telemetry_events.jsonl")
	fmt.Printf("Loading telemetry events from: %s\n", eventsPath)
	events, err := telemetry.LoadEvents(eventsPath)
	if err != nil {
		return fmt.Errorf("failed to load telemetry events: %w", err)
	}
	fmt.Printf("Loaded %d telemetry events\n", len(events))
	
	// Load segment downloads
	segmentsPath := filepath.Join(dataDir, "segment_downloads.jsonl")
	fmt.Printf("Loading segment downloads from: %s\n", segmentsPath)
	downloads, err := telemetry.LoadSegmentDownloads(segmentsPath)
	if err != nil {
		// Segment downloads may be optional
		fmt.Printf("Warning: failed to load segment downloads: %v\n", err)
		downloads = []telemetry.SegmentDownload{}
	} else {
		fmt.Printf("Loaded %d segment downloads\n", len(downloads))
	}
	
	// Create analyzer
	analyzer := analysis.NewAnalyzer(ladder, 5)
	
	// Perform analysis
	fmt.Printf("\nAnalyzing variant switches and computing metrics...\n")
	result, err := analyzer.Analyze(events, downloads)
	if err != nil {
		return fmt.Errorf("analysis failed: %w", err)
	}
	
	fmt.Printf("Found %d variant switches\n", result.TotalSwitches)
	fmt.Printf("Identified %d boundaries with metrics\n", len(result.BoundaryMetrics))
	
	// Write JSON summary
	jsonPath := filepath.Join(outputDir, "summary.json")
	fmt.Printf("\nWriting JSON summary to: %s\n", jsonPath)
	if err := output.WriteJSONSummary(result, jsonPath); err != nil {
		return fmt.Errorf("failed to write JSON summary: %w", err)
	}
	
	// Write Markdown report
	reportPath := filepath.Join(outputDir, "report.md")
	fmt.Printf("Writing Markdown report to: %s\n", reportPath)
	if err := output.WriteMarkdownReport(result, reportPath); err != nil {
		return fmt.Errorf("failed to write Markdown report: %w", err)
	}
	
	fmt.Printf("\n✓ Analysis complete!\n")
	fmt.Printf("Results written to: %s\n", outputDir)
	
	return nil
}
