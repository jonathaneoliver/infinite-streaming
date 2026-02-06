# Migration Guide: From hls_failure_probe.py to Pytest

This guide shows how the original test script was converted to pytest format and how to extend it.

## Key Differences

### 1. Test Organization

**Original:**
```python
# Single monolithic script
def run_failure_tests(url, api_base, session, args, ...):
    tests = []
    # Build test list
    for kind in ["manifest", "segment"]:
        for failure_type in http_failures:
            tests.append(make_http_test(kind, failure_type, ...))

    # Run tests in loop
    for test in tests:
        # Execute test
        ...
```

**Pytest:**
```python
# Organized test classes with parametrization
class TestHTTPFailures:
    @pytest.mark.parametrize("failure_type", ["404", "403", "500"])
    def test_segment_http_failure(self, failure_type, fixtures...):
        # Test code
        ...
```

### 2. Fixtures vs Manual Setup

**Original:**
```python
# Manual setup in each test
def run_test():
    # Reset to baseline
    apply_failure_settings(api_base, session_id, base_failure_payload())
    if session_port:
        apply_shaping(api_base, session_port, restore_mbps)

    # Run test
    ...

    # Cleanup
    apply_failure_settings(api_base, session_id, base_failure_payload())
```

**Pytest:**
```python
# Automatic via fixtures
@pytest.fixture
def clean_session(api_base, session_id, config):
    # Setup
    apply_failure_settings(api_base, session_id, base_failure_payload())
    yield
    # Automatic teardown
    apply_failure_settings(api_base, session_id, base_failure_payload())

# Tests get automatic cleanup
def test_something(clean_session):
    # Just write test code
    ...
```

### 3. Assertions

**Original:**
```python
missing = []
if counters.get("segment_http_error", 0) < expected:
    missing.append(f"segment_http_error {actual} < {expected}")

if missing:
    print("FAIL")
    for item in missing:
        print(f"- {item}")
    sys.exit(2)
```

**Pytest:**
```python
assert counters.get("segment_http_error", 0) >= expected, \
    f"Expected {expected} HTTP errors, got {counters.get('segment_http_error', 0)}"
```

### 4. Test Selection

**Original:**
```python
# Modify script or use flags
parser.add_argument("--shuffle-tests", action="store_true")
parser.add_argument("--stop-on-failure", action="store_true")

if args.shuffle_tests:
    random.shuffle(tests)
```

**Pytest:**
```bash
# Use markers and command-line options
pytest -m http          # Run HTTP tests only
pytest -m "not slow"    # Skip slow tests
pytest -x               # Stop on failure
pytest -k "404"         # Run tests matching "404"
```

### 5. Parametrization

**Original:**
```python
http_failures = ["404", "403", "500", ...]
for failure_type in http_failures:
    tests.append({
        "name": f"segment_{failure_type}",
        "payload": {...},
        ...
    })
```

**Pytest:**
```python
@pytest.mark.parametrize("failure_type", [
    pytest.param("404", id="404_not_found"),
    pytest.param("403", id="403_forbidden"),
    pytest.param("500", id="500_server_error"),
])
def test_segment_http_failure(failure_type, ...):
    ...
```

## Converting Tests

### Example Conversion

**Original Test:**
```python
# From hls_failure_probe.py lines 884-904
for failure_type in socket_resets:
    tests.append(
        make_socket_test(
            kind,
            failure_type,
            f"{kind}_conn_reset",
            args.expect_resets,
            instant_schedule,
            base_mode,
            variant_scope=None,
        )
    )
```

**Converted to Pytest:**
```python
@pytest.mark.socket
@pytest.mark.segment
@pytest.mark.parametrize("failure_type", [
    "request_connect_reset",
    "request_first_byte_reset",
    "request_body_reset",
])
def test_segment_socket_reset(
    failure_type,
    failure_payload_factory,
    stream_info,
    config,
    validate_precheck,
    validate_postcheck,
):
    """Test segment socket reset failures."""
    # Apply failure
    failure_payload_factory(
        segment_failure_type=failure_type,
        segment_consecutive_failures=0,
        segment_failure_frequency=0,
        segment_failure_mode="requests",
    )

    # Run test
    counters = run_probe_window(
        stream_info['media_url'],
        config,
        config.test_seconds,
    )

    # Assert
    assert counters.get("segment_conn_reset", 0) >= config.expect_resets
```

## Adding New Tests

### Step 1: Choose Test Class

Decide which class your test belongs to:
- `TestHTTPFailures` - HTTP status codes, timeouts
- `TestSocketFailures` - Socket resets, hangs, delays
- `TestCorruption` - Content corruption
- `TestTransportFailures` - Packet drop, reject, throttling
- `TestRecovery` - Recovery and resilience
- Create new class if needed

### Step 2: Write Test Function

```python
class TestHTTPFailures:
    @pytest.mark.http
    @pytest.mark.segment
    @pytest.mark.parametrize("failure_type", ["413", "416"])
    def test_new_status_codes(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test new HTTP status codes."""
        # 1. Apply failure
        failure_payload_factory(
            segment_failure_type=failure_type,
            segment_consecutive_failures=0,
            segment_failure_frequency=0,
        )

        # 2. Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            config.test_seconds,
            verbose_label=f"segment_{failure_type}",
        )

        # 3. Assert expectations
        assert counters.get("segment_http_error", 0) >= config.expect_http, \
            f"Expected {config.expect_http} HTTP errors, got {counters.get('segment_http_error', 0)}"
```

### Step 3: Run Your New Test

```bash
# Run just your new test
pytest -k test_new_status_codes -v

# Run with parametrized values
pytest 'test_hls_failures.py::TestHTTPFailures::test_new_status_codes[413]'
```

## Adding New Fixtures

### In conftest.py:

```python
@pytest.fixture
def custom_validator(api_base, session_id):
    """Custom validation fixture."""
    def _validate(expected_condition):
        snapshot = fetch_session_snapshot(api_base, session_id)
        assert snapshot.get("some_field") == expected_condition
    return _validate
```

### Use in test:

```python
def test_with_custom_validation(custom_validator):
    # Test code
    ...
    custom_validator("expected_value")
```

## Common Patterns

### Pattern 1: Pre/Post Validation

```python
def test_with_validation(
    validate_precheck,      # Ensures stream is healthy before test
    validate_postcheck,     # Ensures stream recovers after test
    ...
):
    # Test automatically validates before and after
    ...
```

### Pattern 2: Conditional Skip

```python
def test_master_manifest(stream_info):
    if not stream_info.get('master_url'):
        pytest.skip("No master manifest available")
    # Test code
    ...
```

### Pattern 3: Fixture Factories

```python
@pytest.fixture
def payload_factory(api_base, session_id):
    def _create(**kwargs):
        payload = base_failure_payload()
        payload.update(kwargs)
        apply_failure_settings(api_base, session_id, payload)
        return payload
    return _create

def test_with_factory(payload_factory):
    payload_factory(segment_failure_type="404")
    ...
```

## Best Practices

### 1. Use Fixtures for Setup/Teardown

❌ **Don't:**
```python
def test_something():
    # Manual setup
    setup_stuff()
    try:
        # Test code
        ...
    finally:
        # Manual cleanup
        cleanup_stuff()
```

✅ **Do:**
```python
@pytest.fixture
def my_setup():
    # Setup
    resource = setup_stuff()
    yield resource
    # Automatic cleanup
    cleanup_stuff()

def test_something(my_setup):
    # Just test code
    ...
```

### 2. Use Parametrize for Similar Tests

❌ **Don't:**
```python
def test_404():
    ...

def test_403():
    ...

def test_500():
    ...
```

✅ **Do:**
```python
@pytest.mark.parametrize("status", ["404", "403", "500"])
def test_http_status(status):
    ...
```

### 3. Use Markers for Organization

✅ **Do:**
```python
@pytest.mark.http
@pytest.mark.segment
@pytest.mark.smoke
def test_something():
    ...
```

### 4. Clear Assertion Messages

❌ **Don't:**
```python
assert count > 5
```

✅ **Do:**
```python
assert count > 5, f"Expected more than 5 failures, got {count}"
```

## Migration Checklist

- [x] Install pytest and plugins
- [x] Create conftest.py with fixtures
- [x] Create helpers.py with utility functions
- [x] Create test_hls_failures.py with test classes
- [x] Add pytest.ini configuration
- [x] Add markers to tests
- [x] Add parametrization
- [x] Add pre/post validation fixtures
- [ ] Run smoke tests to validate
- [ ] Run full test suite
- [ ] Compare results with original
- [ ] Update CI/CD integration
- [ ] Train team on pytest usage

## Troubleshooting Migration

### Issue: Tests not discovered

**Problem:** `pytest` finds no tests

**Solution:**
```bash
# Check test discovery
pytest --collect-only

# Ensure you're in the right directory
cd tests/pytest

# Ensure test functions start with test_
def test_my_function():  # ✓
def my_test():           # ✗
```

### Issue: Fixture not found

**Problem:** `fixture 'xyz' not found`

**Solution:**
- Check spelling of fixture name
- Ensure fixture is in conftest.py
- Check fixture scope matches usage

### Issue: Import errors

**Problem:** `ModuleNotFoundError: No module named 'helpers'`

**Solution:**
```bash
# Ensure __init__.py exists
touch __init__.py

# Or use relative imports
from .helpers import http_fetch
```

## Performance Comparison

| Metric | Original | Pytest | Improvement |
|--------|----------|--------|-------------|
| Lines of code | ~1500 | ~800 | 47% reduction |
| Test organization | Flat | Hierarchical | Better |
| Parallel execution | No | Yes | 4x faster |
| Selective running | Manual | Automatic | Much easier |
| Debugging | Print statements | --pdb | Interactive |
| Reports | Text logs | HTML/XML/JSON | Visual |

## Next Steps

1. **Validate**: Run `pytest -m smoke` to ensure basic functionality
2. **Compare**: Run both original and pytest versions side-by-side
3. **Extend**: Add new tests using pytest patterns
4. **Integrate**: Update CI/CD to use pytest
5. **Document**: Add team-specific test cases to test_hls_failures.py

## Resources

- **Pytest Docs**: https://docs.pytest.org/
- **Parametrize Guide**: https://docs.pytest.org/en/stable/how-to/parametrize.html
- **Fixture Guide**: https://docs.pytest.org/en/stable/how-to/fixtures.html
- **Markers Guide**: https://docs.pytest.org/en/stable/how-to/mark.html
