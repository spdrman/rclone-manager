package classifytransport

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/internal/transport"
	"github.com/spdrman/rclone-manager/internal/transport/rclone"
)

// TestWrap_ClassifiesWhatTheRealAdapterLeavesUnclassified is the positive
// control for this package's whole reason for existing: proof that the
// same real adapter call that internal/transport/rclone's own
// error_classification_gap_a213_test.go shows returns an unclassified
// error does return a classified one once passed through Wrap.
func TestWrap_ClassifiesWhatTheRealAdapterLeavesUnclassified(t *testing.T) {
	root := t.TempDir()
	raw := rclone.New()
	wrapped := Wrap(raw)
	src := transport.Source{ID: "classify-wrap-probe", Type: "local", Root: root}
	ctx := context.Background()

	_, rawErr := raw.Stat(ctx, src, "missing.txt")
	if _, ok := transport.CategoryOf(rawErr); ok {
		t.Fatalf("test assumption broken: the raw adapter now classifies its own errors (got a category for %v); "+
			"the known defect this package works around appears fixed, see the PR description", rawErr)
	}

	_, wrappedErr := wrapped.Stat(ctx, src, "missing.txt")
	category, ok := transport.CategoryOf(wrappedErr)
	if !ok {
		t.Fatalf("Wrap(adapter).Stat on a missing object: CategoryOf ok=false, want a classified NotFound: %v", wrappedErr)
	}
	if category != transport.NotFound {
		t.Fatalf("Wrap(adapter).Stat on a missing object: category = %v, want NotFound", category)
	}
}
