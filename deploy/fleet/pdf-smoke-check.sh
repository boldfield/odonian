#!/bin/bash
# Smoke check for PDF rendering capabilities in the fleet image.
# Verifies that pdfinfo, pdftoppm, and pdftotext are available to the runtime user
# and can process page 2 of a multi-page PDF.
#
# Environment:
#   PDF_URL: URL of the PDF to test (defaults to a known public multi-page PDF).
#            For testing with the NIC PDF, set PDF_URL to its URL before running.

set -eu

TMPDIR="${TMPDIR:-/tmp}"
WORKDIR=$(mktemp -d -p "$TMPDIR" pdf-smoke-check.XXXXXX)
trap "rm -rf '$WORKDIR'" EXIT

# Use the specified PDF URL, or default to a known public multi-page PDF.
# The NIC PDF can be tested by setting PDF_URL before running this script.
PDF_URL="${PDF_URL:-https://pubs.usgs.gov/fs/2008/3014/fs2008-3014.pdf}"

echo "PDF smoke check starting in $WORKDIR"
echo "Testing PDF from: $PDF_URL"

# Verify all three utilities are available
echo "Checking for Poppler utilities..."
for tool in pdfinfo pdftoppm pdftotext; do
  if ! command -v "$tool" &>/dev/null; then
    echo "ERROR: $tool not found in PATH"
    exit 1
  fi
  echo "  ✓ $tool found"
done

# Download the test PDF
echo "Downloading test PDF..."
TEST_PDF="$WORKDIR/test.pdf"

if ! curl -fsSL "$PDF_URL" -o "$TEST_PDF"; then
  echo "ERROR: Could not download PDF from $PDF_URL"
  exit 1
fi

if [ ! -f "$TEST_PDF" ] || [ ! -s "$TEST_PDF" ]; then
  echo "ERROR: Test PDF file is empty or missing"
  exit 1
fi

FILESIZE=$(stat -c%s "$TEST_PDF" 2>/dev/null || stat -f%z "$TEST_PDF")
echo "Test PDF downloaded: $FILESIZE bytes"

# Test pdfinfo
echo "Testing pdfinfo..."
if ! pdfinfo "$TEST_PDF" > "$WORKDIR/pdfinfo.txt" 2>&1; then
  echo "ERROR: pdfinfo failed"
  cat "$WORKDIR/pdfinfo.txt"
  exit 1
fi
PAGECOUNT=$(grep "^Pages:" "$WORKDIR/pdfinfo.txt" | awk '{print $2}')
echo "  ✓ pdfinfo succeeded; PDF has $PAGECOUNT pages"

# Ensure we test page 2 if it exists
TARGET_PAGE=2
if [ "$PAGECOUNT" -lt 2 ]; then
  echo "ERROR: PDF must have at least 2 pages for this smoke check; found $PAGECOUNT"
  exit 1
fi

# Test pdftotext for page extraction
echo "Testing pdftotext (page $TARGET_PAGE)..."
if ! pdftotext -f "$TARGET_PAGE" -l "$TARGET_PAGE" "$TEST_PDF" "$WORKDIR/extracted.txt" 2>&1; then
  echo "ERROR: pdftotext failed"
  exit 1
fi

TEXT_SIZE=$(wc -c < "$WORKDIR/extracted.txt")
echo "  ✓ pdftotext succeeded; extracted $TEXT_SIZE bytes from page $TARGET_PAGE"

# Test pdftoppm for page rendering
echo "Testing pdftoppm (page $TARGET_PAGE to PNG)..."
if ! pdftoppm -png -singlefile -f "$TARGET_PAGE" -l "$TARGET_PAGE" "$TEST_PDF" "$WORKDIR/page" 2>&1; then
  echo "ERROR: pdftoppm failed"
  exit 1
fi

if [ ! -f "$WORKDIR/page.png" ]; then
  echo "ERROR: pdftoppm did not create output PNG"
  exit 1
fi

IMAGE_SIZE=$(stat -c%s "$WORKDIR/page.png" 2>/dev/null || stat -f%z "$WORKDIR/page.png")
echo "  ✓ pdftoppm succeeded; rendered image is $IMAGE_SIZE bytes"

# Verify image is nonempty
if [ "$IMAGE_SIZE" -lt 100 ]; then
  echo "ERROR: Rendered image is too small ($IMAGE_SIZE bytes) — check PDF validity"
  exit 1
fi

echo ""
echo "✓ All smoke checks passed!"
echo "  - pdfinfo is available and can read PDF metadata"
echo "  - pdftoppm can render page $TARGET_PAGE to a nonempty PNG image"
echo "  - pdftotext can extract text from page $TARGET_PAGE"
echo ""
echo "Temporary files cleaned up from $TMPDIR"
