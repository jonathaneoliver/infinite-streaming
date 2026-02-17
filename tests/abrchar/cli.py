#!/usr/bin/env python3
"""ABR Characterization CLI Tool

Usage:
    python -m tests.abrchar.cli analyze --data <dir> --hls-url <url> [--output <dir>]
    python -m tests.abrchar.cli run --config <file>
"""

import argparse
import os
import sys
from pathlib import Path

# Add parent directory to path so we can import from tests.abrchar
sys.path.insert(0, str(Path(__file__).parent.parent.parent))

from tests.abrchar.playlist import parse_hls_master
from tests.abrchar.telemetry import load_telemetry_events, load_segment_downloads
from tests.abrchar.analysis import Analyzer
from tests.abrchar.output import write_json_summary, write_markdown_report


def analyze_command(args):
    """Execute the analyze command"""
    print(f"Analyzing telemetry data from: {args.data}")
    
    # Parse HLS master playlist
    if not args.hls_url:
        print("Error: --hls-url is required for analysis")
        return 1
    
    print(f"Parsing HLS master playlist: {args.hls_url}")
    try:
        ladder = parse_hls_master(args.hls_url)
        print(f"Loaded bitrate ladder with {len(ladder.variants)} variants")
    except Exception as e:
        print(f"Error parsing HLS playlist: {e}")
        return 1
    
    # Load telemetry events
    events_path = os.path.join(args.data, "telemetry_events.jsonl")
    print(f"Loading telemetry events from: {events_path}")
    try:
        events = load_telemetry_events(events_path)
        print(f"Loaded {len(events)} telemetry events")
    except Exception as e:
        print(f"Error loading telemetry events: {e}")
        return 1
    
    # Load segment downloads (optional)
    downloads_path = os.path.join(args.data, "segment_downloads.jsonl")
    print(f"Loading segment downloads from: {downloads_path}")
    try:
        downloads = load_segment_downloads(downloads_path)
        print(f"Loaded {len(downloads)} segment downloads")
    except Exception as e:
        print(f"Warning: failed to load segment downloads: {e}")
        downloads = []
    
    # Perform analysis
    print("\nAnalyzing variant switches and computing metrics...")
    analyzer = Analyzer(ladder, throughput_window_size=5)
    result = analyzer.analyze(events, downloads)
    
    print(f"Found {result.total_switches} variant switches")
    print(f"Identified {len(result.boundary_metrics)} boundaries with metrics")
    
    # Set output directory
    output_dir = args.output or os.path.join(args.data, "analysis")
    os.makedirs(output_dir, exist_ok=True)
    
    # Write JSON summary
    json_path = os.path.join(output_dir, "summary.json")
    print(f"\nWriting JSON summary to: {json_path}")
    write_json_summary(result, json_path)
    
    # Write Markdown report
    report_path = os.path.join(output_dir, "report.md")
    print(f"Writing Markdown report to: {report_path}")
    write_markdown_report(result, report_path)
    
    print(f"\n✓ Analysis complete!")
    print(f"Results written to: {output_dir}")
    return 0


def run_command(args):
    """Execute the run command (stub for now)"""
    print("Run command - not yet fully implemented")
    print("This would:")
    print("1. Parse HLS multivariant playlist")
    print("2. Start player automation")
    print("3. Execute throttling schedule")
    print("4. Collect telemetry")
    print("5. Analyze results")
    print("\nFor now, use 'analyze' command on existing telemetry data.")
    return 0


def main():
    parser = argparse.ArgumentParser(
        description="ABR Characterization Tool for HLS Players",
        formatter_class=argparse.RawDescriptionHelpFormatter
    )
    
    subparsers = parser.add_subparsers(dest='command', help='Command to execute')
    
    # Analyze command
    analyze_parser = subparsers.add_parser('analyze', help='Analyze existing telemetry data')
    analyze_parser.add_argument('--data', required=True, help='Directory containing telemetry logs')
    analyze_parser.add_argument('--hls-url', required=True, help='HLS multivariant playlist URL')
    analyze_parser.add_argument('--output', help='Output directory for analysis results')
    
    # Run command
    run_parser = subparsers.add_parser('run', help='Execute an ABR characterization experiment')
    run_parser.add_argument('--config', required=True, help='Path to experiment config file')
    
    # Version command
    version_parser = subparsers.add_parser('version', help='Show version information')
    
    args = parser.parse_args()
    
    if not args.command:
        parser.print_help()
        return 1
    
    if args.command == 'analyze':
        return analyze_command(args)
    elif args.command == 'run':
        return run_command(args)
    elif args.command == 'version':
        print("abrchar version 0.1.0")
        return 0
    else:
        parser.print_help()
        return 1


if __name__ == '__main__':
    sys.exit(main())
