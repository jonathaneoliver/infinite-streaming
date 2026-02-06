#!/bin/bash
# Example pytest commands for HLS failure injection tests

set -e

echo "=== Pytest HLS Failure Injection Test Examples ==="
echo

# Basic runs
echo "1. Basic test run (all tests):"
echo "   pytest"
echo

echo "2. Verbose output:"
echo "   pytest -v"
echo

echo "3. Very verbose (shows test docstrings):"
echo "   pytest -vv"
echo

# Marker-based selection
echo "4. Run smoke tests only (fast):"
echo "   pytest -m smoke"
echo

echo "5. Run HTTP tests only:"
echo "   pytest -m http"
echo

echo "6. Run socket tests only:"
echo "   pytest -m socket"
echo

echo "7. Run segment tests only:"
echo "   pytest -m segment"
echo

echo "8. Run manifest tests only:"
echo "   pytest -m manifest"
echo

echo "9. Run all tests except slow ones:"
echo "   pytest -m 'not slow'"
echo

echo "10. Combine markers (HTTP AND segment):"
echo "    pytest -m 'http and segment'"
echo

# Test selection
echo "11. Run specific test class:"
echo "    pytest test_hls_failures.py::TestHTTPFailures"
echo

echo "12. Run specific test:"
echo "    pytest test_hls_failures.py::TestHTTPFailures::test_segment_http_failure_instant"
echo

echo "13. Run tests matching pattern:"
echo "    pytest -k 'http and not master'"
echo

# Parallel execution
echo "14. Run tests in parallel (auto CPU detection):"
echo "    pytest -n auto"
echo

echo "15. Run tests with 4 workers:"
echo "    pytest -n 4"
echo

# Reporting
echo "16. Generate HTML report:"
echo "    pytest --html=report.html --self-contained-html"
echo

echo "17. Generate coverage report:"
echo "    pytest --cov=. --cov-report=html"
echo

echo "18. Generate JUnit XML report:"
echo "    pytest --junitxml=results.xml"
echo

# Debugging
echo "19. Stop on first failure:"
echo "    pytest -x"
echo

echo "20. Drop into debugger on failure:"
echo "    pytest --pdb"
echo

echo "21. Show stdout/stderr:"
echo "    pytest -s"
echo

echo "22. Run last failed tests:"
echo "    pytest --lf"
echo

# Server configuration
echo "23. Custom server configuration:"
echo "    pytest --host=my-server --api-port=30000 --hls-port=30081"
echo

echo "24. Test specific URL:"
echo "    pytest --url=http://cdn.example.com/stream/master.m3u8"
echo

# Advanced combinations
echo "25. Full CI pipeline run:"
echo "    pytest -n auto -m 'not slow' --html=report.html --junitxml=results.xml --cov=. --cov-report=term-missing"
echo

echo "26. Quick smoke test with report:"
echo "    pytest -m smoke --html=smoke-report.html --self-contained-html"
echo

echo "27. Debug specific failure:"
echo "    pytest -vv -s -x test_hls_failures.py::TestHTTPFailures::test_segment_http_failure_instant --pdb"
echo

# List available markers and tests
echo "28. List all markers:"
echo "    pytest --markers"
echo

echo "29. Collect tests without running:"
echo "    pytest --collect-only"
echo

echo "30. List tests with verbose info:"
echo "    pytest --collect-only -q"
echo

echo
echo "=== Example Workflows ==="
echo

echo "Quick validation workflow:"
echo "  1. pytest -m smoke                    # Quick smoke test"
echo "  2. pytest -m 'http and segment' -v    # Test HTTP segment failures"
echo "  3. pytest -m http --html=report.html  # Full HTTP test suite with report"
echo

echo "CI/CD workflow:"
echo "  pytest -n auto -m 'not slow' --html=report.html --junitxml=results.xml --cov=. --cov-report=html"
echo

echo "Development workflow:"
echo "  1. pytest -m smoke --lf               # Run last failed smoke tests"
echo "  2. pytest -k my_feature -vv -s        # Debug specific feature"
echo "  3. pytest --collect-only              # Verify test discovery"
echo

echo
echo "Run any command by removing the 'echo' and executing directly"
