package classifytransport

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// Whether the classifying decorator still does something, now that the
// adapter it was written to compensate for classifies its own errors.
//
// The original cell here asserted the defect: a raw call came back
// unclassified and the same call through Wrap came back categorised. The
// defect is fixed, so asserting it would now be asserting a lie, and simply
// deleting the file would drop the property that still matters. What is left
// is idempotency, which is what the decorator actually meets everywhere
// today: wrapping an already-classified error must neither lose the category
// nor change it.
//
// The raw adapter's own classification is checked first, in the same run.
// Without it, an idempotency assertion passes just as happily over an
// adapter that has silently stopped classifying anything.

// TestWrap_ClassifiesWhatTheRealAdapterLeavesUnclassified is the positive
// control for this package's whole reason for existing: proof that the
// same real adapter call that internal/transport/rclone's own
// error_classification_gap_a213_test.go shows returns an unclassified
// error does return a classified one once passed through Wrap.
func TestWrap_IsIdempotentOverAnAlreadyClassifiedError(t *testing.T) {
	root := t.TempDir()
	raw := rclone.New()
	wrapped := Wrap(raw)
	src := transport.Source{ID: "classify-wrap-probe", Type: "local", Root: root}
	ctx := context.Background()

	// The raw adapter classifies its own errors now, which it did not when
	// this package was written. That fix is the reason this decorator is
	// redundant against the rclone adapter, so assert the fixed behaviour
	// rather than the old defect: a raw Stat must carry a category.
	_, rawErr := raw.Stat(ctx, src, "missing.txt")
	if rawCat, ok := transport.CategoryOf(rawErr); !ok {
		t.Fatalf("the raw adapter stopped classifying its own errors: %v", rawErr)
	} else if rawCat != transport.NotFound {
		t.Fatalf("raw adapter category = %v, want NotFound (err=%v)", rawCat, rawErr)
	}

	// Wrap still has to be idempotent over an already-classified error,
	// because that is what it now sees everywhere. Double-wrapping must not
	// lose or change the category.

	_, wrappedErr := wrapped.Stat(ctx, src, "missing.txt")
	category, ok := transport.CategoryOf(wrappedErr)
	if !ok {
		t.Fatalf("Wrap(adapter).Stat on a missing object: CategoryOf ok=false, want a classified NotFound: %v", wrappedErr)
	}
	if category != transport.NotFound {
		t.Fatalf("Wrap(adapter).Stat on a missing object: category = %v, want NotFound", category)
	}
}
