package hosts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteExpiry(t *testing.T) {
	dir := newRegistryRepo(t)
	d := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	changed, err := WriteExpiry(dir, "vm-a", d)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "hosts", "vm-a", "expiry"))
	if err != nil || string(b) != "2026-09-30\n" {
		t.Errorf("%q %v", b, err)
	}
	reg, _ := ReadRegistry(dir)
	if !reg["vm-a"].Expiry.Equal(d) {
		t.Errorf("read back %v", reg["vm-a"].Expiry)
	}
	before := headCount(t, dir)
	if changed, err := WriteExpiry(dir, "vm-a", d); err != nil || changed || headCount(t, dir) != before {
		t.Errorf("republishing the same date must not commit: %v %v", changed, err)
	}
	if changed, err := WriteExpiry(dir, "vm-a", d.AddDate(0, 0, 7)); err != nil || !changed {
		t.Errorf("a new date is committed: %v %v", changed, err)
	}
	if _, err := WriteExpiry(dir, "../up", d); err == nil {
		t.Error("a bad label is refused")
	}
}
