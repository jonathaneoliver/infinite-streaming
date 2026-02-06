# Pytest-based HLS Failure Injection Tests

Modern pytest-based test suite for comprehensive HLS player resilience testing under various failure conditions.

## Features

- **Parametrized Tests**: Extensive use of `@pytest.mark.parametrize` for comprehensive coverage
- **Fixtures**: Clean session management, automatic setup/teardown
- **Markers**: Organize and run specific test categories
- **Detailed Assertions**: Clear, informative failure messages
- **Pre/Post Validation**: Automatic health checks before and after tests
- **Parallel Execution**: Support for pytest-xdist
- **HTML Reports**: Integration with pytest-html
- **Coverage**: Compatible with pytest-cov

## Installation

```bash
# Install pytest and recommended plugins
pip install pytest pytest-xdist pytest-html pytest-timeout pytest-cov

# Or install from requirements
pip install -r requirements.txt
```

## Quick Start

```bash
# Run all tests
pytest

# Run with verbose output
pytest -v

# Run smoke tests only (fast)
pytest -m smoke

# Run specific test class
pytest test_hls_failures.py::TestHTTPFailures

# Run specific test
pytest test_hls_failures.py::TestHTTPFailures::test_segment_http_failure_instant
```

## Test Organization

### Test Classes

- **TestHTTPFailures**: HTTP-level failures (404, 500, timeout, DNS)
- **TestSocketFailures**: Socket-level failures (reset, hang, delay)
- **TestCorruption**: Content corruption scenarios
- **TestTransportFailures**: Transport-level faults (drop, reject, throttle)
- **TestVariantScoping**: Variant-specific failure tests
- **TestRecovery**: Recovery and resilience tests

### Test Markers

Run tests by category using markers:

```bash
# HTTP failures only
pytest -m http

# Socket failures only
pytest -m socket

# Transport failures only
pytest -m transport

# Corruption tests only
pytest -m corruption

# Segment tests only
pytest -m segment

# Manifest tests only
pytest -m manifest

# Slow tests (skip for quick runs)
pytest -m "not slow"

# Smoke tests (quick validation)
pytest -m smoke

# Combine markers
pytest -m "http and segment"
pytest -m "http or socket"
pytest -m "segment and not slow"
```

## Command-Line Options

### Server Configuration

```bash
# Specify server
pytest --host=my-server --api-port=30000 --hls-port=30081

# Use HTTPS
pytest --scheme=https

# Override base URLs
pytest --api-base=http://api.example.com --hls-base=http://cdn.example.com

# Test specific URL
pytest --url=http://cdn.example.com/stream/master.m3u8
```

### Test Behavior

```bash
# Adjust test duration
pytest --test-seconds=20

# Adjust timeouts
pytest --timeout=30

# Set expectations (minimum counts)
pytest --expect-http=2 --expect-timeouts=3 --expect-resets=2
```

### Output and Reporting

```bash
# Verbose output
pytest -v

# Very verbose (show test details)
pytest -vv

# Quiet mode
pytest -q

# Show local variables on failure
pytest -l

# Show test durations
pytest --durations=10

# Stop on first failure
pytest -x

# Stop after N failures
pytest --maxfail=3
```

## Advanced Usage

### Parallel Execution

Run tests in parallel using pytest-xdist:

```bash
# Auto-detect CPU count
pytest -n auto

# Specify number of workers
pytest -n 4

# Distribute by test class
pytest --dist=loadfile
```

### HTML Reports

Generate HTML test reports:

```bash
# Basic HTML report
pytest --html=report.html --self-contained-html

# With captured output
pytest --html=report.html --self-contained-html --capture=sys
```

### Code Coverage

Measure test coverage:

```bash
# Basic coverage
pytest --cov=.

# HTML coverage report
pytest --cov=. --cov-report=html

# Coverage with missing lines
pytest --cov=. --cov-report=term-missing

# Branch coverage
pytest --cov=. --cov-branch
```

### Test Selection

```bash
# Run by test name pattern
pytest -k "http and segment"
pytest -k "not slow"

# Run specific test file
pytest test_hls_failures.py

# Run specific class
pytest test_hls_failures.py::TestHTTPFailures

# Run specific test
pytest test_hls_failures.py::TestHTTPFailures::test_segment_http_failure_instant

# Run parametrized test variant
pytest 'test_hls_failures.py::TestHTTPFailures::test_segment_http_failure_instant[404_not_found]'
```

### Debugging

```bash
# Drop into debugger on failure
pytest --pdb

# Drop into debugger on first failure
pytest -x --pdb

# Show stdout/stderr
pytest -s

# Show full traceback
pytest --tb=long

# Show short traceback
pytest --tb=short

# Show only failed tests
pytest --lf

# Run failed tests first
pytest --ff
```

## Fixtures

### Session Fixtures

Available to all tests (set up once per test session):

- `config`: Test configuration from command-line options
- `api_base`: API base URL
- `hls_base`: HLS base URL
- `player_id`: Unique player ID for this session
- `stream_info`: Auto-selected stream information
- `session_id`: Test session ID
- `session_port`: Session port for network shaping

### Function Fixtures

Set up before each test:

- `clean_session`: Resets failure settings before/after test
- `validate_precheck`: Validates stream health before test
- `validate_postcheck`: Validates recovery after test
- `failure_payload_factory`: Factory for creating failure payloads
- `snapshot`: Current session snapshot

### Example Usage

```python
def test_my_failure(
    failure_payload_factory,
    stream_info,
    config,
    validate_precheck,
    validate_postcheck,
):
    """Test with automatic validation."""
    # Apply failure
    failure_payload_factory(segment_failure_type="404")

    # Test code here
    ...
```

## Configuration File

Customize behavior in `pytest.ini`:

```ini
[pytest]
# Adjust these as needed
addopts = -v --strict-markers
testpaths = .
markers =
    smoke: Quick smoke tests
    slow: Slow tests (>30s)
```

## Best Practices

### Writing Tests

1. **Use fixtures**: Let fixtures handle setup/teardown
2. **Use markers**: Tag tests appropriately
3. **Clear assertions**: Provide descriptive assertion messages
4. **Parametrize**: Use `@pytest.mark.parametrize` for similar tests
5. **Skip when needed**: Use `pytest.skip()` for conditional tests

### Running Tests

1. **Start with smoke**: `pytest -m smoke` for quick validation
2. **Use markers**: Target specific failure types
3. **Parallel execution**: Use `-n auto` for faster runs
4. **Generate reports**: Use `--html` for shareable results

### Example Test Session

```bash
# 1. Quick smoke test
pytest -m smoke

# 2. Run HTTP failures only
pytest -m http -v

# 3. Run all tests except slow ones
pytest -m "not slow"

# 4. Full test run with report
pytest --html=report.html --self-contained-html

# 5. Parallel execution
pytest -n auto --html=report.html
```

## Extending Tests

### Adding New Test Cases

```python
@pytest.mark.http
@pytest.mark.segment
@pytest.mark.parametrize("failure_type", ["413", "416"])
def test_new_http_status_codes(
    failure_type,
    failure_payload_factory,
    stream_info,
    config,
    validate_precheck,
    validate_postcheck,
):
    """Test new HTTP status codes."""
    failure_payload_factory(segment_failure_type=failure_type)

    counters = run_probe_window(
        stream_info['media_url'],
        config,
        config.test_seconds,
    )

    assert counters.get("segment_http_error", 0) >= 1
```

### Adding New Fixtures

In `conftest.py`:

```python
@pytest.fixture
def custom_fixture(config):
    """Custom fixture for special testing needs."""
    # Setup
    value = setup_something(config)

    yield value

    # Teardown
    cleanup_something(value)
```

### Adding Custom Markers

In `conftest.py`:

```python
def pytest_configure(config):
    config.addinivalue_line(
        "markers",
        "custom: Custom marker for special tests"
    )
```

## Troubleshooting

### Tests Failing to Find Stream

```bash
# Ensure server is running
curl http://lenovo:30000/api/content

# Check connectivity
pytest --host=your-server --api-port=30000
```

### Session Not Found

```bash
# Increase timeout
pytest --timeout=30

# Check session endpoint
curl http://lenovo:30000/api/sessions
```

### Import Errors

```bash
# Ensure you're in the pytest directory
cd tests/pytest

# Run pytest
pytest
```

### Parallel Execution Issues

```bash
# Some tests may not be parallel-safe
# Run sequentially if issues occur
pytest -n 0
```

## Comparison with Original Test Suite

| Feature | Original | Pytest |
|---------|----------|--------|
| Organization | Single file | Multiple files with classes |
| Test discovery | Manual | Automatic |
| Parametrization | Manual loops | `@pytest.mark.parametrize` |
| Fixtures | Manual setup/teardown | Automatic fixtures |
| Markers | None | Comprehensive markers |
| Parallel execution | No | Yes (with pytest-xdist) |
| HTML reports | No | Yes (with pytest-html) |
| Code coverage | No | Yes (with pytest-cov) |
| Selective runs | Manual filtering | Markers and -k |
| Assertions | Manual checks | pytest assertions |
| Debugging | Print statements | --pdb integration |

## Next Steps

1. **Run smoke tests**: `pytest -m smoke` to validate setup
2. **Explore markers**: `pytest --markers` to see all markers
3. **Generate report**: `pytest --html=report.html` for visual results
4. **Add custom tests**: Extend test classes for specific scenarios
5. **Integrate CI/CD**: Add to your CI pipeline

## Resources

- [Pytest Documentation](https://docs.pytest.org/)
- [Pytest-xdist (parallel execution)](https://pytest-xdist.readthedocs.io/)
- [Pytest-html (HTML reports)](https://pytest-html.readthedocs.io/)
- [Pytest-cov (code coverage)](https://pytest-cov.readthedocs.io/)

## Support

For issues or questions:
1. Check `pytest --help` for all options
2. Review test logs in `test_run.log`
3. Run with `-vv` for maximum verbosity
4. Use `--pdb` to debug failures interactively
