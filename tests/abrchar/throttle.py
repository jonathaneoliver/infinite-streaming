"""Network throttling control"""

import requests
import subprocess
import time
from typing import List, Optional


class Throttler:
    """Base interface for network throttling"""
    
    def set_bandwidth(self, bandwidth_mbps: float):
        """Set the bandwidth limit in Mbps"""
        raise NotImplementedError
    
    def reset(self):
        """Remove all throttling"""
        raise NotImplementedError


class HTTPThrottler(Throttler):
    """HTTP API-based throttler (for go-proxy integration)"""
    
    def __init__(self, base_url: str, port: int, timeout: int = 10):
        self.base_url = base_url.rstrip('/')
        self.port = port
        self.timeout = timeout
    
    def set_bandwidth(self, bandwidth_mbps: float):
        """Set bandwidth via HTTP API"""
        url = f"{self.base_url}/api/nft/bandwidth/{self.port}"
        payload = {"rate": bandwidth_mbps}
        
        response = requests.post(url, json=payload, timeout=self.timeout)
        response.raise_for_status()
    
    def reset(self):
        """Reset to very high bandwidth (100 Gbps = 100000 Mbps)"""
        self.set_bandwidth(100000)


class ShellThrottler(Throttler):
    """Shell command-based throttler"""
    
    def __init__(self, command_template: str, reset_command: Optional[str] = None):
        self.command_template = command_template
        self.reset_command = reset_command
    
    def set_bandwidth(self, bandwidth_mbps: float):
        """Set bandwidth via shell command"""
        cmd = self.command_template.replace("{{.Rate}}", f"{bandwidth_mbps:.2f}")
        subprocess.run(cmd, shell=True, check=True, capture_output=True)
    
    def reset(self):
        """Reset throttling"""
        if self.reset_command:
            subprocess.run(self.reset_command, shell=True, check=True, capture_output=True)


class ThrottleStep:
    """A single step in a throttling schedule"""
    
    def __init__(self, bandwidth_mbps: float, duration_seconds: float, description: str = ""):
        self.bandwidth_mbps = bandwidth_mbps
        self.duration_seconds = duration_seconds
        self.description = description or f"Hold at {bandwidth_mbps:.2f} Mbps"


class ThrottleSchedule:
    """A sequence of throttling steps"""
    
    def __init__(self, steps: List[ThrottleStep]):
        self.steps = steps
    
    def execute(self, throttler: Throttler, callback=None):
        """Execute the throttling schedule
        
        Args:
            throttler: Throttler instance to use
            callback: Optional callback function called after each step
        """
        for i, step in enumerate(self.steps):
            throttler.set_bandwidth(step.bandwidth_mbps)
            
            if callback:
                callback(step, i)
            
            if step.duration_seconds > 0:
                time.sleep(step.duration_seconds)
    
    @staticmethod
    def generate_stair_step(start_bandwidth: float, end_bandwidth: float,
                           step_percent: float, hold_duration: float,
                           direction: str = "down") -> 'ThrottleSchedule':
        """Generate a stair-step throttling schedule
        
        Args:
            start_bandwidth: Starting bandwidth in Mbps
            end_bandwidth: Ending bandwidth in Mbps
            step_percent: Percentage step size (e.g., 10 for 10%)
            hold_duration: Duration to hold each step in seconds
            direction: "down", "up", or "down-up"
            
        Returns:
            ThrottleSchedule with generated steps
        """
        steps = []
        
        if direction in ("down", "down-up"):
            # Generate steps going down
            current = start_bandwidth
            while current >= end_bandwidth:
                steps.append(ThrottleStep(
                    bandwidth_mbps=current,
                    duration_seconds=hold_duration,
                    description=f"Hold at {current:.2f} Mbps"
                ))
                current = current * (1 - step_percent / 100.0)
        
        if direction in ("up", "down-up"):
            # Generate steps going up
            current = end_bandwidth
            while current <= start_bandwidth:
                steps.append(ThrottleStep(
                    bandwidth_mbps=current,
                    duration_seconds=hold_duration,
                    description=f"Hold at {current:.2f} Mbps"
                ))
                current = current * (1 + step_percent / 100.0)
        
        return ThrottleSchedule(steps)
