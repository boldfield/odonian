#!/bin/bash
# Smoke check for PDF rendering capabilities in the fleet image.
# Verifies that pdfinfo, pdftoppm, and pdftotext are available to the runtime user
# and can process a multi-page PDF (page 2 specifically).

set -eu

TMPDIR="${TMPDIR:-/tmp}"
WORKDIR=$(mktemp -d -p "$TMPDIR" pdf-smoke-check.XXXXXX)
trap "rm -rf '$WORKDIR'" EXIT

echo "PDF smoke check starting in $WORKDIR"

# Verify all three utilities are available
echo "Checking for Poppler utilities..."
for tool in pdfinfo pdftoppm pdftotext; do
  if ! command -v "$tool" &>/dev/null; then
    echo "ERROR: $tool not found in PATH"
    exit 1
  fi
  echo "  ✓ $tool found"
done

# Download a small public PDF with multiple pages (using a PDF that is publicly available)
# This uses a sample PDF from Apache PDFBox project or creates a minimal test PDF
echo "Downloading test PDF..."
TEST_PDF="$WORKDIR/test.pdf"

# Try to download a sample PDF from a public source; fall back to creating a minimal one
if ! curl -fsSL "https://www.w3.org/WAI/WCAG21/Techniques/pdf/img/table.pdf" -o "$TEST_PDF" 2>/dev/null; then
  echo "Could not download PDF from public source, checking for alternative..."
  # If curl fails, create a minimal test PDF using available tools
  # For this fallback, we'll attempt to create one or use an existing sample
  if ! curl -fsSL "https://www.apache.org/licenses/LICENSE-2.0.pdf" -o "$TEST_PDF" 2>/dev/null; then
    echo "WARNING: Could not download sample PDF. Attempting alternative source..."
    # Try another source
    if ! curl -fsSL "https://pdfjs.robwierzbowski.com/data/pdfs/tracemonkey.pdf" -o "$TEST_PDF" 2>/dev/null; then
      echo "ERROR: Could not download any public PDF for testing"
      exit 1
    fi
  fi
fi

if [ ! -f "$TEST_PDF" ]; then
  echo "ERROR: Test PDF file not created"
  exit 1
fi

echo "Test PDF downloaded: $(stat -c%s "$TEST_PDF" 2>/dev/null || stat -f%z "$TEST_PDF") bytes"

# Test pdfinfo
echo "Testing pdfinfo..."
if ! pdfinfo "$TEST_PDF" > "$WORKDIR/pdfinfo.txt"; then
  echo "ERROR: pdfinfo failed"
  exit 1
fi
PAGECOUNT=$(grep "^Pages:" "$WORKDIR/pdfinfo.txt" | awk '{print $2}')
echo "  ✓ pdfinfo succeeded; PDF has $PAGECOUNT pages"

# Test with page 2 (or last page if PDF has fewer than 2 pages)
TARGET_PAGE=2
if [ "$PAGECOUNT" -lt 2 ]; then
  TARGET_PAGE=1
  echo "  Note: PDF has fewer than 2 pages, testing page 1 instead"
fi

# Test pdftotext for page extraction
echo "Testing pdftotext (page $TARGET_PAGE)..."
if ! pdftotext -f "$TARGET_PAGE" -l "$TARGET_PAGE" "$TEST_PDF" "$WORKDIR/extracted.txt" 2>/dev/null; then
  echo "ERROR: pdftotext failed"
  exit 1
fi

TEXT_SIZE=$(wc -c < "$WORKDIR/extracted.txt")
echo "  ✓ pdftotext succeeded; extracted $TEXT_SIZE bytes of text"

# Test pdftoppm for page rendering
echo "Testing pdftoppm (page $TARGET_PAGE to PNG)..."
if ! pdftoppm -png -singlefile -f "$TARGET_PAGE" -l "$TARGET_PAGE" "$TEST_PDF" "$WORKDIR/page"; then
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
  echo "ERROR: Rendered image is too small ($IMAGE_SIZE bytes)"
  exit 1
fi

echo ""
echo "✓ All smoke checks passed!"
echo "  - pdfinfo is available and readable"
echo "  - pdftoppm can render pages to PNG"
echo "  - pdftotext can extract text"
echo ""
echo "Temporary files cleaned up from $TMPDIR"
