# ABR Characterization Report

## Experiment Summary

- **Start:** 2024-01-01 12:00:00
- **End:** 2024-01-01 12:02:07
- **Duration:** 127 seconds
- **Total Switches:** 5

## Bitrate Ladder

| Index | Bandwidth | Avg Bandwidth | Resolution |
|-------|-----------|---------------|------------|
| 0 | 1.28 Mbps | 1.00 Mbps | 640x360 |
| 1 | 2.56 Mbps | 2.00 Mbps | 960x540 |
| 2 | 3.84 Mbps | 3.00 Mbps | 1280x720 |
| 3 | 6.40 Mbps | 5.00 Mbps | 1920x1080 |

## Boundary Metrics

### Boundary: Variant 2 ↔ Variant 3

**Bitrates:** 3.00 Mbps ↔ 5.00 Mbps

#### Downswitch (High → Low)

**Count:** 1

**Throughput Thresholds:**

- Mean: 18.50 Mbps
- Median: 18.50 Mbps
- StdDev: 0.00 Mbps
- Range: 18.50 - 18.50 Mbps

**Safety Factors (α = variant_bw / throughput):**

- Mean: 0.27
- Median: 0.27
- StdDev: 0.00
- Range: 0.27 - 0.27

---

### Boundary: Variant 1 ↔ Variant 2

**Bitrates:** 2.00 Mbps ↔ 3.00 Mbps

#### Downswitch (High → Low)

**Count:** 1

**Throughput Thresholds:**

- Mean: 9.60 Mbps
- Median: 9.60 Mbps
- StdDev: 0.00 Mbps
- Range: 9.60 - 9.60 Mbps

**Safety Factors (α = variant_bw / throughput):**

- Mean: 0.31
- Median: 0.31
- StdDev: 0.00
- Range: 0.31 - 0.31

#### Upswitch (Low → High)

**Count:** 1

**Throughput Thresholds:**

- Mean: 8.90 Mbps
- Median: 8.90 Mbps
- StdDev: 0.00 Mbps
- Range: 8.90 - 8.90 Mbps

**Safety Factors (α = variant_bw / throughput):**

- Mean: 0.34
- Median: 0.34
- StdDev: 0.00
- Range: 0.34 - 0.34

#### Hysteresis

- Downswitch median: 9.60 Mbps
- Upswitch median: 8.90 Mbps
- **Hysteresis:** -0.70 Mbps

---

### Boundary: Variant 0 ↔ Variant 1

**Bitrates:** 1.00 Mbps ↔ 2.00 Mbps

#### Downswitch (High → Low)

**Count:** 1

**Throughput Thresholds:**

- Mean: 8.90 Mbps
- Median: 8.90 Mbps
- StdDev: 0.00 Mbps
- Range: 8.90 - 8.90 Mbps

**Safety Factors (α = variant_bw / throughput):**

- Mean: 0.22
- Median: 0.22
- StdDev: 0.00
- Range: 0.22 - 0.22

#### Upswitch (Low → High)

**Count:** 1

**Throughput Thresholds:**

- Mean: 8.90 Mbps
- Median: 8.90 Mbps
- StdDev: 0.00 Mbps
- Range: 8.90 - 8.90 Mbps

**Safety Factors (α = variant_bw / throughput):**

- Mean: 0.22
- Median: 0.22
- StdDev: 0.00
- Range: 0.22 - 0.22

#### Hysteresis

- Downswitch median: 8.90 Mbps
- Upswitch median: 8.90 Mbps
- **Hysteresis:** 0.00 Mbps

---

## Key Conclusions

### Safety Factor Summary

| Boundary | Direction | Median α | Mean α | StdDev α |
|----------|-----------|----------|--------|----------|
| 2→3 | Down | 0.27 | 0.27 | 0.00 |
| 1→2 | Down | 0.31 | 0.31 | 0.00 |
| 1→2 | Up | 0.34 | 0.34 | 0.00 |
| 0→1 | Down | 0.22 | 0.22 | 0.00 |
| 0→1 | Up | 0.22 | 0.22 | 0.00 |

