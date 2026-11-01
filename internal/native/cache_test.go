package native

import (
	"math"
	"testing"
)

func TestCacheByteAccountingRejectsOverflow(t *testing.T) {
	total := int64(math.MaxInt64 - 1)
	if err := addCacheBytes(&total, 1); err != nil || total != math.MaxInt64 {
		t.Fatalf("exact int64 boundary failed: total=%d, error=%v", total, err)
	}
	if err := addCacheBytes(&total, 1); err == nil || total != math.MaxInt64 {
		t.Fatalf("overflow was accepted or changed total: total=%d, error=%v", total, err)
	}
	if err := addCacheBytes(&total, -1); err == nil || total != math.MaxInt64 {
		t.Fatalf("negative file size was accepted or changed total: total=%d, error=%v", total, err)
	}
}
