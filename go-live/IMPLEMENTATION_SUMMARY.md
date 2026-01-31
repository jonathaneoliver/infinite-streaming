# Go-Live Implementation Summary

**Status (Jan 2026):** Go-live is the active runtime implementation for LL-HLS and LL-DASH generation.

## Scope
- LL-HLS + LL-DASH generation
- 2s and 6s variant manifests
- Unified per-content worker

## Entry Points
- `/go-live/{content}/master.m3u8`
- `/go-live/{content}/master_2s.m3u8`
- `/go-live/{content}/master_6s.m3u8`
- `/go-live/{content}/manifest.mpd`
- `/go-live/{content}/manifest_2s.mpd`
- `/go-live/{content}/manifest_6s.mpd`
