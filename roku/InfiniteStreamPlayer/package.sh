#!/bin/bash
# Package the Roku channel for deployment

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
OUTPUT_FILE="${SCRIPT_DIR}/../InfiniteStreamPlayer.zip"

echo "Packaging Roku channel..."

cd "$SCRIPT_DIR"

# Remove old package if it exists
if [ -f "$OUTPUT_FILE" ]; then
    echo "Removing old package: $OUTPUT_FILE"
    rm "$OUTPUT_FILE"
fi

# Create the package
zip -r "$OUTPUT_FILE" \
    manifest \
    source/ \
    components/ \
    images/*.png \
    -x "*.DS_Store" \
    -x "*/.*" \
    -x "images/create_images.py"

echo ""
echo "Package created: $OUTPUT_FILE"
echo ""
echo "To install on your Roku device:"
echo "1. Enable developer mode on your Roku"
echo "2. Go to http://<ROKU_IP> in your browser"
echo "3. Upload $OUTPUT_FILE under 'Development Application Installer'"
echo "4. Click 'Install'"
echo ""
