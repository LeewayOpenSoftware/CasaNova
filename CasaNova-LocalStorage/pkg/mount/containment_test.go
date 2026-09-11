package mount

import "testing"

// OLA 3 — single source for the mount-containment decision route/v1 and
// route/v2 used to each reimplement.
func TestValidatePointAllowsPathsUnderAllowedRoots(t *testing.T) {
	for _, mp := range []string{"/mnt/usb1", "/media/backup", "/DATA/shares"} {
		if err := ValidatePoint(mp); err != nil {
			t.Errorf("ValidatePoint(%q) = %v, want nil", mp, err)
		}
	}
}

func TestValidatePointRejectsTheRootsThemselves(t *testing.T) {
	for _, mp := range []string{"/mnt", "/media", "/DATA"} {
		if err := ValidatePoint(mp); err == nil {
			t.Errorf("ValidatePoint(%q) = nil, want an error — mounting over an allowed "+
				"root itself would hide every other mount under it", mp)
		}
	}
}

func TestValidatePointRejectsOutsideAllowedRoots(t *testing.T) {
	for _, mp := range []string{"", "relative/path", "/etc/passwd", "/mnt/../etc", "/mnt/usb/../../etc"} {
		if err := ValidatePoint(mp); err == nil {
			t.Errorf("ValidatePoint(%q) = nil, want an error", mp)
		}
	}
}
