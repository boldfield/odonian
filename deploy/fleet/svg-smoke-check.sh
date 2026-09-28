#!/bin/bash
# Smoke check for SVG-to-PNG rendering capabilities in the fleet image.
# Verifies that rsvg-convert is available to the runtime user and can render
# a synthetic SVG containing geometry and text to PNG.

set -eu

TMPDIR="${TMPDIR:-/tmp}"
WORKDIR=$(mktemp -d -p "$TMPDIR" svg-smoke-check.XXXXXX)
trap "rm -rf '$WORKDIR'" EXIT

echo "SVG smoke check starting in $WORKDIR"

# Verify rsvg-convert is available
echo "Checking for rsvg-convert..."
if ! command -v rsvg-convert &>/dev/null; then
  echo "ERROR: rsvg-convert not found in PATH"
  exit 1
fi
echo "  ✓ rsvg-convert found"

# Create a synthetic test SVG with geometry and text
TEST_SVG="$WORKDIR/test.svg"
cat > "$TEST_SVG" << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200" viewBox="0 0 200 200">
  <!-- Background -->
  <rect width="200" height="200" fill="#ffffff"/>

  <!-- Rectangle -->
  <rect x="10" y="10" width="80" height="80" fill="#0066cc" stroke="#000000" stroke-width="2"/>

  <!-- Circle -->
  <circle cx="150" cy="50" r="30" fill="#ff6600" stroke="#000000" stroke-width="2"/>

  <!-- Text -->
  <text x="100" y="140" font-size="18" text-anchor="middle" font-family="sans-serif" fill="#000000">
    SVG Test
  </text>

  <!-- Line -->
  <line x1="10" y1="160" x2="190" y2="160" stroke="#000000" stroke-width="2"/>
</svg>
EOF

if [ ! -f "$TEST_SVG" ] || [ ! -s "$TEST_SVG" ]; then
  echo "ERROR: Failed to create test SVG file"
  exit 1
fi

SVG_SIZE=$(stat -c%s "$TEST_SVG" 2>/dev/null || stat -f%z "$TEST_SVG")
echo "Test SVG created: $SVG_SIZE bytes"

# Render SVG to PNG
echo "Testing rsvg-convert (SVG to PNG)..."
OUTPUT_PNG="$WORKDIR/output.png"

if ! rsvg-convert "$TEST_SVG" -o "$OUTPUT_PNG" 2>&1; then
  echo "ERROR: rsvg-convert failed"
  exit 1
fi

if [ ! -f "$OUTPUT_PNG" ] || [ ! -s "$OUTPUT_PNG" ]; then
  echo "ERROR: rsvg-convert did not create output PNG"
  exit 1
fi

PNG_SIZE=$(stat -c%s "$OUTPUT_PNG" 2>/dev/null || stat -f%z "$OUTPUT_PNG")
echo "  ✓ rsvg-convert succeeded; rendered image is $PNG_SIZE bytes"

# Validate PNG signature (magic bytes: 89 50 4E 47 0D 0A 1A 0A)
echo "Validating PNG signature and dimensions..."
PNG_MAGIC=$(xxd -p -l 8 "$OUTPUT_PNG")
EXPECTED_MAGIC="89504e470d0a1a0a"

if [ "$PNG_MAGIC" != "$EXPECTED_MAGIC" ]; then
  echo "ERROR: Invalid PNG signature (got: $PNG_MAGIC, expected: $EXPECTED_MAGIC)"
  exit 1
fi
echo "  ✓ PNG signature valid"

# Extract PNG dimensions (stored in IHDR chunk at bytes 16-24)
# Width and height are 4 bytes each, big-endian
IHDR_HEX=$(xxd -p -s 16 -l 8 "$OUTPUT_PNG")
WIDTH=$((16#${IHDR_HEX:0:8}))
HEIGHT=$((16#${IHDR_HEX:8:8}))

echo "  ✓ PNG dimensions: ${WIDTH}x${HEIGHT}"

# Verify dimensions are reasonable (should be 200x200 or close to it due to scaling)
if [ "$WIDTH" -lt 100 ] || [ "$HEIGHT" -lt 100 ]; then
  echo "ERROR: PNG dimensions too small (${WIDTH}x${HEIGHT})"
  exit 1
fi

if [ "$PNG_SIZE" -lt 100 ]; then
  echo "ERROR: Rendered image is too small ($PNG_SIZE bytes) — check SVG validity"
  exit 1
fi

echo ""
echo "✓ All smoke checks passed!"
echo "  - rsvg-convert is available and working"
echo "  - SVG with geometry and text rendered successfully to PNG"
echo "  - PNG signature is valid"
echo "  - PNG dimensions are ${WIDTH}x${HEIGHT}"
echo ""
echo "Temporary files cleaned up from $TMPDIR"
