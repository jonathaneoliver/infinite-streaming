# Pytest Test Suite - Quick Start Guide

## Installation (One Time Setup)

```bash
# Navigate to integration tests directory
cd tests/integration

# Install dependencies
pip install -r requirements.txt

# Verify installation
pytest --version
```

## Running Tests

### Fastest Way to Get Started

```bash
# Run smoke tests (fastest, validates basic functionality)
pytest -m smoke

# Should see output like:
# ==================== test session starts ====================
# collected 15 items / 12 deselected / 3 selected
#
# test_hls_failures.py::TestHTTPFailures::test_segment_http_failure_instant[404_not_found] PASSED
# test_hls_failures.py::TestHTTPFailures::test_manifest_http_failure_instant[404_not_found] PASSED
# test_hls_failures.py::TestCorruption::test_segment_corrupted_zero_fill PASSED
#
# ==================== 3 passed in 45.2s ====================
```

### Common Commands

```bash
# Run all tests
pytest

# Run with verbose output
pytest -v

# Run HTTP tests only
pytest -m http

# Run segment tests only
pytest -m segment

# Run everything except slow tests
pytest -m "not slow"

# Run tests in parallel (faster)
pytest -n auto

# Generate HTML report
pytest --html=report.html --self-contained-html
```

## Understanding Test Output

```
test_hls_failures.py::TestHTTPFailures::test_segment_http_failure_instant[404_not_found] PASSED [33%]
                     │                 │                                     │              │
                     │                 │                                     │              └─ Test status
                     │                 │                                     └─ Parameter value
                     │                 └─ Test function name
                     └─ Test class name
```

## Test Structure

```
tests/integration/
├── conftest.py              # Fixtures and configuration
├── helpers.py               # Utility functions
├── test_hls_failures.py     # Main test file
├── pytest.ini               # Pytest configuration
├── requirements.txt         # Dependencies
├── README.md                # Full documentation
└── examples.sh              # Example commands
```

## Test Categories (Markers)

| Marker | Description | Example |
|--------|-------------|---------|
| `smoke` | Quick validation tests | `pytest -m smoke` |
| `http` | HTTP-level failures | `pytest -m http` |
| `socket` | Socket-level failures | `pytest -m socket` |
| `transport` | Transport faults | `pytest -m transport` |
| `corruption` | Content corruption | `pytest -m corruption` |
| `segment` | Segment failures | `pytest -m segment` |
| `manifest` | Manifest failures | `pytest -m manifest` |
| `slow` | Slow tests (>30s) | `pytest -m "not slow"` |

## Configuration Options

### Server Settings

```bash
# Default (uses lenovo:30000/30081)
pytest

# Custom server
pytest --host=my-server

# Custom ports
pytest --api-port=8000 --hls-port=8080

# Test specific URL
pytest --url=http://cdn.example.com/stream/master.m3u8
```

### Test Duration

```bash
# Default (12 seconds per test)
pytest

# Longer tests (more thorough)
pytest --test-seconds=30

# Shorter tests (faster)
pytest --test-seconds=5
```

## Troubleshooting

### "No module named pytest"
```bash
pip install pytest
```

### "Collection error" or "Import error"
```bash
# Make sure you're in the pytest directory
cd tests/integration
pytest
```

### Tests fail to find stream
```bash
# Check server is running
curl http://lenovo:30000/api/content

# Specify correct host
pytest --host=your-server-name
```

### Need to see what's happening
```bash
# Maximum verbosity
pytest -vv -s

# Show local variables on failure
pytest -l

# Drop into debugger on first failure
pytest -x --pdb
```

## Next Steps

1. ✅ **Run smoke tests**: `pytest -m smoke`
2. ✅ **Run HTTP tests**: `pytest -m http -v`
3. ✅ **Generate report**: `pytest --html=report.html`
4. 📖 **Read full docs**: See `README.md`
5. 🔧 **Customize**: Edit `pytest.ini` for your needs

## Comparison: Original vs Pytest

| Task | Original | Pytest |
|------|----------|--------|
| Run all tests | `python hls_failure_probe.py` | `pytest` |
| Run specific test | Modify script | `pytest -k test_name` |
| Run HTTP tests only | Modify script | `pytest -m http` |
| Parallel execution | Not supported | `pytest -n auto` |
| HTML report | Not built-in | `pytest --html=report.html` |
| Stop on first failure | Add flag | `pytest -x` |
| Debug failure | Print statements | `pytest --pdb` |

## Example Workflow

```bash
# 1. Quick validation
pytest -m smoke

# 2. Run specific category
pytest -m "http and segment" -v

# 3. Generate report for team
pytest -m "not slow" --html=report.html --self-contained-html

# 4. Debug a failure
pytest -x --pdb -vv test_hls_failures.py::TestHTTPFailures::test_segment_http_failure_instant

# 5. Full CI run
pytest -n auto --html=report.html --junitxml=results.xml
```

## Getting Help

```bash
# List all command-line options
pytest --help

# List all available markers
pytest --markers

# List all tests without running
pytest --collect-only

# Run examples script to see all commands
./examples.sh
```

## Resources

- **Full Documentation**: [README.md](README.md)
- **Player Characterization Matrix**: [PLAYER_CHARACTERIZATION_PYTEST.md](PLAYER_CHARACTERIZATION_PYTEST.md)
- **Example Commands**: [examples.sh](examples.sh)
- **Test Improvement Plan**: [../testing_improvement_plan.md](../testing_improvement_plan.md)
- **Pytest Docs**: https://docs.pytest.org/
