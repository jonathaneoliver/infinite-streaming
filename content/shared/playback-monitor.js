/**
 * Playback Quality Monitor
 * Tracks video playback metrics to detect stalls, skips, loops, and dropped frames
 */
class PlaybackQualityMonitor {
  constructor(videoElement, options = {}) {
    this.video = videoElement;
    this.options = {
      expectedLoopTime: options.expectedLoopTime || 210.28,
      sampleInterval: options.sampleInterval || 100,
      skipTolerance: options.skipTolerance || 0.15,
      ...options
    };
    
    this.metrics = {
      stalls: [],           // [{time, duration, reason, videoTime}]
      skips: [],            // [{time, from, to, skipAmount}]
      loops: [],            // [{time, successful, fromTime, toTime, issue}]
      droppedFrames: [],    // sampled periodically
      bufferEvents: [],     // [{time, videoTime, bufferLength, bufferRanges}]
      timelineLog: [],      // [{realTime, videoTime, buffered, readyState}]
      events: []            // All video events
    };
    
    this.state = {
      lastTime: 0,
      lastRealTime: 0,
      stallStart: null,
      monitorInterval: null,
      frameInterval: null,
      startTime: performance.now(),
      isMonitoring: false
    };
    
    this.setupEventListeners();
  }

  setupEventListeners() {
    const events = [
      'loadedmetadata', 'loadeddata', 'canplay', 'canplaythrough',
      'playing', 'pause', 'ended', 'waiting', 'stalled', 'suspend',
      'seeking', 'seeked', 'timeupdate', 'progress', 'error'
    ];
    
    events.forEach(eventName => {
      this.video.addEventListener(eventName, (e) => {
        this.metrics.events.push({
          time: performance.now() - this.state.startTime,
          type: eventName,
          videoTime: this.video.currentTime,
          readyState: this.video.readyState,
          networkState: this.video.networkState,
          paused: this.video.paused
        });
      });
    });
  }

  /**
   * Detect stalls: video.currentTime not advancing when it should be
   */
  checkStall() {
    const now = performance.now();
    const currentTime = this.video.currentTime;
    const realTimeDelta = now - this.state.lastRealTime;
    
    // Only check if enough time has passed and video should be playing
    if (!this.video.paused && !this.video.seeking && realTimeDelta > 50) {
      const timeDelta = currentTime - this.state.lastTime;
      
      // Video time should advance roughly in sync with real time (accounting for playback rate)
      const expectedDelta = (realTimeDelta / 1000) * this.video.playbackRate;
      
      if (timeDelta < expectedDelta * 0.5 && this.video.readyState < 4) {
        // Video is not advancing as expected - likely stalling
        if (!this.state.stallStart) {
          this.state.stallStart = {
            realTime: now,
            videoTime: currentTime,
            readyState: this.video.readyState
          };
          console.warn(`[STALL START] at video time ${currentTime.toFixed(2)}s, readyState=${this.video.readyState}`);
        }
      } else if (this.state.stallStart) {
        // Stall ended
        const duration = now - this.state.stallStart.realTime;
        this.metrics.stalls.push({
          time: now - this.state.startTime,
          videoTime: this.state.stallStart.videoTime,
          duration: duration,
          reason: this.state.stallStart.readyState < 4 ? 'buffering' : 'unknown',
          readyState: this.state.stallStart.readyState
        });
        console.warn(`[STALL END] duration: ${duration.toFixed(0)}ms`);
        this.state.stallStart = null;
      }
    }
    
    this.state.lastTime = currentTime;
    this.state.lastRealTime = now;
  }

  /**
   * Detect skips: currentTime jumped unexpectedly forward
   */
  checkSkip() {
    const currentTime = this.video.currentTime;
    const timeDiff = currentTime - this.state.lastTime;
    
    // Allow for normal playback advancement + tolerance, but detect large jumps
    if (timeDiff > this.options.skipTolerance && !this.video.seeking && !this.video.paused) {
      // Could be a loop, check that separately
      const isLoop = currentTime < this.state.lastTime - 10;
      
      if (!isLoop) {
        this.metrics.skips.push({
          time: performance.now() - this.state.startTime,
          from: this.state.lastTime,
          to: currentTime,
          skipAmount: timeDiff
        });
        console.error(`[SKIP] from ${this.state.lastTime.toFixed(2)}s to ${currentTime.toFixed(2)}s (skipped ${timeDiff.toFixed(2)}s)`);
      }
    }
  }

  /**
   * Detect loop wrap: time goes from end back to beginning
   * Also detect by frame counter reset
   */
  checkLoop() {
    const currentTime = this.video.currentTime;
    const timeDiff = currentTime - this.state.lastTime;
    
    // Debug logging around potential loop times
    if (currentTime > 200 || currentTime < 20) {
      console.log(`[LOOP CHECK] currentTime=${currentTime.toFixed(2)}s, lastTime=${this.state.lastTime.toFixed(2)}s, diff=${timeDiff.toFixed(2)}s, backward=${timeDiff < -10}`);
    }
    
    // Primary detection: time went backwards significantly (reduced threshold from -10 to -5)
    if (currentTime < this.state.lastTime - 5) {
      const loopSuccess = (
        this.state.lastTime > this.options.expectedLoopTime - 20 &&  // Was near end (within 20s)
        currentTime < 20 &&                                          // Now near beginning (within 20s)
        !this.state.stallStart                                       // No active stall
      );
      
      this.metrics.loops.push({
        time: performance.now() - this.state.startTime,
        successful: loopSuccess,
        fromTime: this.state.lastTime,
        toTime: currentTime,
        detectionMethod: 'time_backwards',
        issue: loopSuccess ? null : 'unexpected_loop_or_seek'
      });
      
      const status = loopSuccess ? '✓ SUCCESS' : '✗ ISSUE';
      console.log(`[LOOP] ${status} - ${this.state.lastTime.toFixed(2)}s → ${currentTime.toFixed(2)}s (detected by time jump)`);
      
      // Mark that we just looped - used to track decoder recovery
      this.state.lastLoopTime = performance.now();
      this.state.framesAtLoop = this.metrics.droppedFrames.length > 0 
        ? this.metrics.droppedFrames[this.metrics.droppedFrames.length - 1].total 
        : 0;
    }
    
    // Secondary detection: frame counter reset (Safari might reset decoder at loop)
    // This catches loops even if time sampling misses the exact transition moment
    if (this.metrics.droppedFrames.length > 1) {
      const currentFrames = this.metrics.droppedFrames[this.metrics.droppedFrames.length - 1].total;
      const previousFrames = this.metrics.droppedFrames[this.metrics.droppedFrames.length - 2].total;
      const frameDiff = currentFrames - previousFrames;
      
      // Debug: log when we're near beginning and checking frame counts
      if (currentTime < 30 && previousFrames > 0) {
        console.log(`[FRAME CHECK] currentTime=${currentTime.toFixed(2)}s, frames: ${previousFrames}→${currentFrames} (diff=${frameDiff}), backward=${frameDiff < 0}`);
      }
      
      // If frames went DOWN (or reset to 0/low), and currentTime is near beginning, this is a loop
      if (frameDiff < -50 && currentTime < 60) {
        // Check if we already logged this loop recently (within last 5 seconds)
        const recentLoop = this.metrics.loops.find(l => 
          (performance.now() - this.state.startTime - l.time) < 5000
        );
        
        if (!recentLoop) {
          const loopSuccess = currentTime < 10 && !this.state.stallStart;
          
          this.metrics.loops.push({
            time: performance.now() - this.state.startTime,
            successful: loopSuccess,
            fromTime: this.state.lastTime,
            toTime: currentTime,
            detectionMethod: 'frame_counter_reset',
            frameCountDrop: previousFrames - currentFrames,
            issue: loopSuccess ? null : 'decoder_reset_detected'
          });
          
          const status = loopSuccess ? '✓ SUCCESS' : '✗ ISSUE';
          console.log(`[LOOP] ${status} - detected by frame counter reset (${previousFrames}→${currentFrames} frames) at time ${currentTime.toFixed(2)}s`);
          
          // Mark that we just looped
          this.state.lastLoopTime = performance.now();
          this.state.framesAtLoop = currentFrames;
        }
      }
      
      // Track decoder recovery after loop
      if (this.state.lastLoopTime && (performance.now() - this.state.lastLoopTime) < 10000) {
        const timeSinceLoop = performance.now() - this.state.lastLoopTime;
        const framesGained = currentFrames - this.state.framesAtLoop;
        
        // If frames are increasing again, decoder recovered!
        if (framesGained > 30 && !this.state.decoderRecoveryLogged) {
          console.log(`[DECODER RECOVERY] ✓ Decoder restarted successfully! ${framesGained} frames decoded in ${(timeSinceLoop/1000).toFixed(1)}s after loop`);
          this.state.decoderRecoveryLogged = true;
          
          // Mark the last loop as successful recovery
          if (this.metrics.loops.length > 0) {
            const lastLoop = this.metrics.loops[this.metrics.loops.length - 1];
            lastLoop.decoderRecovered = true;
            lastLoop.recoveryTime = timeSinceLoop;
          }
        }
        
        // If >5 seconds after loop and still at 0 frames, decoder failed to recover
        if (timeSinceLoop > 5000 && framesGained === 0 && !this.state.decoderFailureLogged) {
          console.error(`[DECODER FAILURE] ✗ Decoder did NOT restart after loop! Still at ${currentFrames} frames after ${(timeSinceLoop/1000).toFixed(1)}s`);
          this.state.decoderFailureLogged = true;
          
          // Mark the last loop as failed recovery
          if (this.metrics.loops.length > 0) {
            const lastLoop = this.metrics.loops[this.metrics.loops.length - 1];
            lastLoop.decoderRecovered = false;
            lastLoop.successful = false;
            lastLoop.issue = 'decoder_failed_to_restart';
          }
        }
      }
      
      // Reset recovery tracking flags when we're far enough from last loop
      if (this.state.lastLoopTime && (performance.now() - this.state.lastLoopTime) > 10000) {
        this.state.lastLoopTime = null;
        this.state.decoderRecoveryLogged = false;
        this.state.decoderFailureLogged = false;
      }
    }
    
    // Tertiary detection: simple time wrap-around check
    // If we were in the last 30 seconds and now we're in the first 30 seconds, it's a loop
    if (this.state.lastTime > this.options.expectedLoopTime - 30 && currentTime < 30 && timeDiff < -50) {
      // Check if we already detected this loop
      const recentLoop = this.metrics.loops.find(l => 
        (performance.now() - this.state.startTime - l.time) < 3000
      );
      
      if (!recentLoop) {
        const loopSuccess = !this.state.stallStart;
        
        this.metrics.loops.push({
          time: performance.now() - this.state.startTime,
          successful: loopSuccess,
          fromTime: this.state.lastTime,
          toTime: currentTime,
          detectionMethod: 'time_wraparound',
          issue: loopSuccess ? null : 'stalled_during_loop'
        });
        
        const status = loopSuccess ? '✓ SUCCESS' : '✗ ISSUE';
        console.log(`[LOOP] ${status} - wrap-around detected: ${this.state.lastTime.toFixed(2)}s → ${currentTime.toFixed(2)}s`);
        
        // Mark that we just looped
        this.state.lastLoopTime = performance.now();
        if (this.metrics.droppedFrames.length > 0) {
          this.state.framesAtLoop = this.metrics.droppedFrames[this.metrics.droppedFrames.length - 1].total;
        }
      }
    }
  }

  /**
   * Sample frame drop statistics and detect rendering failures
   * Also tracks audio/video separately to diagnose which decoder failed
   */
  sampleFrameDrops() {
    if (typeof this.video.getVideoPlaybackQuality === 'function') {
      const quality = this.video.getVideoPlaybackQuality();
      
      // Collect audio/video frame stats (browser-specific APIs)
      const audioStats = {
        decodedBytes: this.video.webkitAudioDecodedByteCount || 0,
        // Some browsers expose audio frame count
        decodedFrames: this.video.mozDecodedAudioFrames || this.video.webkitDecodedAudioFrames || null
      };
      
      const videoStats = {
        decodedBytes: this.video.webkitVideoDecodedByteCount || 0,
        decodedFrames: quality.totalVideoFrames || 0,
        droppedFrames: quality.droppedVideoFrames || 0,
        // WebKit exposes additional frame counters
        webkitDecodedFrames: this.video.webkitDecodedFrameCount || null,
        webkitDroppedFrames: this.video.webkitDroppedFrameCount || null
      };
      
      const currentSample = {
        time: performance.now() - this.state.startTime,
        videoTime: this.video.currentTime,
        // Standard API
        total: quality.totalVideoFrames,
        dropped: quality.droppedVideoFrames,
        corrupted: quality.corruptedVideoFrames,
        dropRate: quality.totalVideoFrames > 0 
          ? (quality.droppedVideoFrames / quality.totalVideoFrames * 100).toFixed(2)
          : 0,
        // Separate audio/video tracking
        audio: audioStats,
        video: videoStats
      };
      
      // CRITICAL: Detect video rendering failure
      // If video is playing but totalVideoFrames hasn't increased, decoder may have failed
      if (this.metrics.droppedFrames.length > 0) {
        const lastSample = this.metrics.droppedFrames[this.metrics.droppedFrames.length - 1];
        const timeDiff = currentSample.time - lastSample.time;
        const frameDiff = currentSample.total - lastSample.total;
        const audioBytesDiff = currentSample.audio.decodedBytes - (lastSample.audio?.decodedBytes || 0);
        const videoBytesDiff = currentSample.video.decodedBytes - (lastSample.video?.decodedBytes || 0);
        
        // Debug logging to help diagnose
        if (!this.video.paused && this.video.currentTime > 0) {
          console.log(`[FRAME SAMPLE] time=${this.video.currentTime.toFixed(2)}s, videoFrames=${currentSample.total} (+${frameDiff}), audioBytes=${currentSample.audio.decodedBytes} (+${audioBytesDiff}), videoBytes=${currentSample.video.decodedBytes} (+${videoBytesDiff}), paused=${this.video.paused}`);
          
          // CRITICAL: Detect decoder reset (frame counter went DOWN)
          if (frameDiff < 0) {
            console.warn(`[DECODER RESET] Frame counter decreased from ${lastSample.total} to ${currentSample.total} at time ${this.video.currentTime.toFixed(2)}s - decoder may have reinitialized!`);
          }
        }
        
        // Detect complete video rendering failure
        if (!this.video.paused && 
            !this.video.seeking && 
            timeDiff > 1500 && 
            frameDiff === 0 && 
            this.video.currentTime > 0) {
          
          if (!this.metrics.renderingFailures) {
            this.metrics.renderingFailures = [];
          }
          
          // Determine what's failing
          const diagnosis = audioBytesDiff > 0 ? 'VIDEO decoder failed (audio still working)' : 'BOTH audio and video decoders failed';
          
          // Only log once per failure period (every 5 seconds)
          const lastFailure = this.metrics.renderingFailures[this.metrics.renderingFailures.length - 1];
          if (!lastFailure || currentSample.time - lastFailure.time > 5000) {
            this.metrics.renderingFailures.push({
              time: currentSample.time,
              videoTime: this.video.currentTime,
              duration: timeDiff,
              totalFrames: currentSample.total,
              audioWorking: audioBytesDiff > 0,
              videoWorking: videoBytesDiff > 0,
              diagnosis: diagnosis,
              readyState: this.video.readyState,
              networkState: this.video.networkState
            });
            
            console.error(`[RENDERING FAILURE] ${diagnosis} - NO VIDEO FRAMES for ${(timeDiff/1000).toFixed(1)}s at ${this.video.currentTime.toFixed(2)}s (videoFrames stuck at ${currentSample.total}, audioBytes +${audioBytesDiff})`);
          }
        }
      }
      
      this.metrics.droppedFrames.push(currentSample);
    }
  }

  /**
   * Check buffer health
   */
  checkBuffer() {
    const buffered = this.video.buffered;
    const currentTime = this.video.currentTime;
    let bufferLength = 0;
    const ranges = [];
    
    if (buffered.length > 0) {
      // Find buffer range containing current time
      for (let i = 0; i < buffered.length; i++) {
        const start = buffered.start(i);
        const end = buffered.end(i);
        ranges.push([start.toFixed(2), end.toFixed(2)]);
        
        if (currentTime >= start && currentTime <= end) {
          bufferLength = end - currentTime;
        }
      }
    }
    
    // Detect buffer starvation (low buffer during playback)
    let issue = null;
    if (bufferLength === 0 && !this.video.paused) {
      issue = 'no_buffer';
    } else if (bufferLength < 1.0 && !this.video.paused && this.video.readyState < 4) {
      issue = 'low_buffer';
    } else if (ranges.length > 1) {
      issue = 'fragmented_buffer'; // Multiple non-contiguous buffer ranges
    }
    
    // CRITICAL: Detect buffer drop at loop boundary
    // If near beginning of video (likely just looped) and buffer is zero, segments after DISCONTINUITY aren't loading
    if (currentTime < 30 && bufferLength === 0 && !this.video.paused) {
      // Check if we recently looped (within last 10 seconds)
      if (this.state.lastLoopTime && (performance.now() - this.state.lastLoopTime) < 10000) {
        issue = 'buffer_starvation_after_loop';
        console.error(`[BUFFER STARVATION] No buffer at ${currentTime.toFixed(2)}s after loop! Segments after DISCONTINUITY may not be loading.`);
      }
    }
    
    // Log significant buffer changes
    if (this.metrics.bufferEvents.length > 0) {
      const lastBuffer = this.metrics.bufferEvents[this.metrics.bufferEvents.length - 1];
      const bufferDrop = lastBuffer.bufferLength - bufferLength;
      
      // Detect sudden buffer drop (>5 seconds lost)
      if (bufferDrop > 5.0 && !this.video.seeking) {
        console.warn(`[BUFFER DROP] Buffer dropped by ${bufferDrop.toFixed(1)}s (${lastBuffer.bufferLength.toFixed(1)}s → ${bufferLength.toFixed(1)}s) at ${currentTime.toFixed(2)}s`);
      }
    }
    
    this.metrics.bufferEvents.push({
      time: performance.now() - this.state.startTime,
      videoTime: currentTime,
      bufferLength: bufferLength,
      bufferRanges: ranges,
      issue: issue
    });
    
    // Log critical buffer issues
    if (issue === 'no_buffer') {
      console.warn(`[BUFFER] No buffer at ${currentTime.toFixed(2)}s - stall imminent`);
    } else if (issue === 'fragmented_buffer') {
      console.warn(`[BUFFER] Fragmented buffer (${ranges.length} ranges) at ${currentTime.toFixed(2)}s: ${JSON.stringify(ranges)}`);
    }
  }
  
  /**
   * Check for bitrate/quality changes (if videoWidth/videoHeight change)
   */
  checkQualityChange() {
    if (!this.state.lastVideoWidth) {
      this.state.lastVideoWidth = this.video.videoWidth;
      this.state.lastVideoHeight = this.video.videoHeight;
      return;
    }
    
    if (this.video.videoWidth !== this.state.lastVideoWidth || 
        this.video.videoHeight !== this.state.lastVideoHeight) {
      const change = {
        time: performance.now() - this.state.startTime,
        from: `${this.state.lastVideoWidth}x${this.state.lastVideoHeight}`,
        to: `${this.video.videoWidth}x${this.video.videoHeight}`,
        videoTime: this.video.currentTime
      };
      
      if (!this.metrics.qualityChanges) {
        this.metrics.qualityChanges = [];
      }
      this.metrics.qualityChanges.push(change);
      
      console.log(`[QUALITY] Resolution changed: ${change.from} → ${change.to}`);
      
      this.state.lastVideoWidth = this.video.videoWidth;
      this.state.lastVideoHeight = this.video.videoHeight;
    }
  }
  
  /**
   * Check playback rate anomalies
   */
  checkPlaybackRate() {
    if (!this.state.lastPlaybackRate) {
      this.state.lastPlaybackRate = this.video.playbackRate;
      return;
    }
    
    // Detect unexpected playback rate changes (not user-initiated speed changes)
    if (this.video.playbackRate !== this.state.lastPlaybackRate) {
      const change = {
        time: performance.now() - this.state.startTime,
        from: this.state.lastPlaybackRate,
        to: this.video.playbackRate,
        videoTime: this.video.currentTime
      };
      
      if (!this.metrics.playbackRateChanges) {
        this.metrics.playbackRateChanges = [];
      }
      this.metrics.playbackRateChanges.push(change);
      
      console.log(`[PLAYBACK RATE] Changed: ${change.from}x → ${change.to}x`);
      
      this.state.lastPlaybackRate = this.video.playbackRate;
    }
  }

  /**
   * Main monitoring loop
   */
  startMonitoring(intervalMs) {
    if (this.state.isMonitoring) {
      console.warn('Already monitoring');
      return;
    }
    
    this.state.isMonitoring = true;
    this.state.startTime = performance.now();
    this.state.lastTime = this.video.currentTime;
    this.state.lastRealTime = performance.now();
    
    const interval = intervalMs || this.options.sampleInterval;
    
    this.state.monitorInterval = setInterval(() => {
      this.checkStall();
      this.checkSkip();
      this.checkLoop();
      this.checkBuffer();
      this.checkQualityChange();
      this.checkPlaybackRate();
      
      // Log timeline snapshot for detailed analysis
      this.metrics.timelineLog.push({
        realTime: performance.now() - this.state.startTime,
        videoTime: this.video.currentTime,
        buffered: this.getBufferedRanges(),
        readyState: this.video.readyState,
        paused: this.video.paused
      });
      
      // Debug: log current time periodically to help diagnose loop detection
      if (Math.floor(this.video.currentTime) % 30 === 0) {
        console.log(`[TIME CHECK] currentTime=${this.video.currentTime.toFixed(2)}s, lastTime=${this.state.lastTime.toFixed(2)}s, diff=${(this.video.currentTime - this.state.lastTime).toFixed(2)}s`);
      }
    }, interval);
    
    // Sample frame drops every second
    this.state.frameInterval = setInterval(() => {
      this.sampleFrameDrops();
      this.checkLoop(); // Also check for loops during frame sampling to catch frame counter resets
    }, 1000);
    
    console.log(`[MONITOR] Started monitoring (interval: ${interval}ms, expectedLoopTime: ${this.options.expectedLoopTime}s)`);
  }

  stopMonitoring() {
    if (!this.state.isMonitoring) {
      return;
    }
    
    clearInterval(this.state.monitorInterval);
    clearInterval(this.state.frameInterval);
    this.state.isMonitoring = false;
    
    console.log('[MONITOR] Stopped monitoring');
  }

  /**
   * Get stream start date from #EXT-X-PROGRAM-DATE-TIME (Safari/WebKit specific)
   */
  getStreamStartDate() {
    // Safari/WebKit specific API
    if (typeof this.video.getStartDate === 'function') {
      try {
        const startDate = this.video.getStartDate();
        console.log('[START DATE] getStartDate() returned:', startDate);
        return startDate;
      } catch (e) {
        console.warn('[START DATE] getStartDate() failed:', e);
        return null;
      }
    }
    console.log('[START DATE] getStartDate() not available in this browser');
    return null;
  }

  /**
   * Generate comprehensive report
   */
  generateReport() {
    const totalStallTime = this.metrics.stalls.reduce((sum, s) => sum + s.duration, 0);
    const totalSkipTime = this.metrics.skips.reduce((sum, s) => sum + s.skipAmount, 0);
    const successfulLoops = this.metrics.loops.filter(l => l.successful).length;
    const failedLoops = this.metrics.loops.filter(l => !l.successful).length;
    
    let finalQuality = { totalVideoFrames: 0, droppedVideoFrames: 0, corruptedVideoFrames: 0 };
    if (typeof this.video.getVideoPlaybackQuality === 'function') {
      finalQuality = this.video.getVideoPlaybackQuality();
    }
    
    const dropRate = finalQuality.totalVideoFrames > 0
      ? (finalQuality.droppedVideoFrames / finalQuality.totalVideoFrames * 100).toFixed(2)
      : 0;
    
    const testDuration = (performance.now() - this.state.startTime) / 1000;
    
    const summary = {
      testDurationSec: testDuration.toFixed(1),
      videoCurrentTime: this.video.currentTime.toFixed(2),
      totalStalls: this.metrics.stalls.length,
      totalStallTimeMs: totalStallTime.toFixed(0),
      stallTimePercent: ((totalStallTime / (testDuration * 1000)) * 100).toFixed(2),
      totalSkips: this.metrics.skips.length,
      totalSkipTimeSec: totalSkipTime.toFixed(2),
      successfulLoops: successfulLoops,
      failedLoops: failedLoops,
      renderingFailures: this.metrics.renderingFailures ? this.metrics.renderingFailures.length : 0,
      droppedFrames: finalQuality.droppedVideoFrames,
      totalFrames: finalQuality.totalVideoFrames,
      corruptedFrames: finalQuality.corruptedVideoFrames,
      frameDropRate: `${dropRate}%`,
      averageBufferLength: this.calculateAverageBuffer()
    };
    
    const verdict = this.calculateVerdict(totalStallTime, this.metrics.skips.length, successfulLoops, parseFloat(dropRate), testDuration);
    
    // Get timing information
    const streamStartDate = this.getStreamStartDate();
    const currentBufferedRanges = this.getBufferedRanges();
    
    return {
      summary,
      verdict,
      streamStartDate: streamStartDate,
      currentTime: this.video.currentTime,
      bufferedRanges: currentBufferedRanges,
      details: {
        stalls: this.metrics.stalls,
        skips: this.metrics.skips,
        loops: this.metrics.loops,
        renderingFailures: this.metrics.renderingFailures || [],
        qualityChanges: this.metrics.qualityChanges || [],
        playbackRateChanges: this.metrics.playbackRateChanges || [],
        droppedFramesSamples: this.metrics.droppedFrames.slice(-10), // Last 10 samples
        bufferEventsSamples: this.metrics.bufferEvents.slice(-20),   // Last 20 samples
        eventsSummary: this.summarizeEvents()
      },
      rawData: {
        timeline: this.metrics.timelineLog,
        allEvents: this.metrics.events
      }
    };
  }

  calculateVerdict(stallTime, skipCount, successfulLoops, dropRate, testDuration) {
    const issues = [];
    let status = 'PASS';
    
    // CRITICAL: Check for rendering failures first
    if (this.metrics.renderingFailures && this.metrics.renderingFailures.length > 0) {
      const totalFailureTime = this.metrics.renderingFailures.reduce((sum, f) => sum + (f.duration || 0), 0);
      issues.push(`CRITICAL: video rendering failed for ${(totalFailureTime/1000).toFixed(1)}s (${this.metrics.renderingFailures.length} failures)`);
      status = 'FAIL';
    }
    
    if (skipCount > 0) {
      issues.push(`${skipCount} video skip(s) detected`);
      status = 'FAIL';
    }
    
    if (stallTime > 5000) {
      issues.push(`excessive stalling (${(stallTime/1000).toFixed(1)}s > 5s)`);
      status = 'FAIL';
    }
    
    if (successfulLoops === 0 && testDuration > 200) {
      issues.push('no successful loops detected');
      status = 'FAIL';
    }
    
    if (dropRate > 5) {
      issues.push(`high frame drop rate (${dropRate}% > 5%)`);
      if (status !== 'FAIL') status = 'WARN';
    }
    
    if (stallTime > 1000 && stallTime <= 5000) {
      issues.push(`minor stalling detected (${(stallTime/1000).toFixed(1)}s)`);
      if (status !== 'FAIL') status = 'WARN';
    }
    
    if (status === 'PASS') {
      return {
        status: 'PASS',
        message: 'Smooth playback - no significant issues detected',
        issues: []
      };
    }
    
    return {
      status,
      message: `${status} - ${issues.join(', ')}`,
      issues
    };
  }

  calculateAverageBuffer() {
    if (this.metrics.bufferEvents.length === 0) return 0;
    const sum = this.metrics.bufferEvents.reduce((s, e) => s + e.bufferLength, 0);
    return (sum / this.metrics.bufferEvents.length).toFixed(2);
  }

  summarizeEvents() {
    const counts = {};
    this.metrics.events.forEach(e => {
      counts[e.type] = (counts[e.type] || 0) + 1;
    });
    return counts;
  }

  getBufferedRanges() {
    const buffered = this.video.buffered;
    const ranges = [];
    for (let i = 0; i < buffered.length; i++) {
      ranges.push([
        parseFloat(buffered.start(i).toFixed(2)),
        parseFloat(buffered.end(i).toFixed(2))
      ]);
    }
    return ranges;
  }

  /**
   * Get current live stats for UI display
   */
  getLiveStats() {
    const buffered = this.video.buffered;
    const currentTime = this.video.currentTime;
    let bufferAhead = 0;
    
    if (buffered.length > 0) {
      for (let i = 0; i < buffered.length; i++) {
        if (currentTime >= buffered.start(i) && currentTime <= buffered.end(i)) {
          bufferAhead = buffered.end(i) - currentTime;
          break;
        }
      }
    }
    
    let quality = { totalVideoFrames: 0, droppedVideoFrames: 0 };
    if (typeof this.video.getVideoPlaybackQuality === 'function') {
      quality = this.video.getVideoPlaybackQuality();
    }
    
    return {
      currentTime: currentTime.toFixed(2),
      bufferAhead: bufferAhead.toFixed(2),
      stalls: this.metrics.stalls.length,
      skips: this.metrics.skips.length,
      loops: this.metrics.loops.filter(l => l.successful).length,
      droppedFrames: quality.droppedVideoFrames,
      totalFrames: quality.totalVideoFrames,
      readyState: this.video.readyState,
      networkState: this.video.networkState,
      paused: this.video.paused
    };
  }
}

// Export for use in other scripts
if (typeof module !== 'undefined' && module.exports) {
  module.exports = PlaybackQualityMonitor;
}
