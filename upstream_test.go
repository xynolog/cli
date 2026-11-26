package cli

import "testing"

// The launcher writes Xynolog's images at the images release the lock names,
// and, while it names images by commit alone, at local: no registry holds an
// image of an unreleased commit.
func TestTheImagesTagIsTheLockedImagesRelease(t *testing.T) {
	for _, c := range []struct {
		lock, want string
	}{
		{`{"schema": "xynolog-upstream-lock/v1", "sdk": {"commit": "548e4fb7238693be7f21d7977a814441fc090bcd"}, "images": {"version": "26.11.0", "commit": "0ddc65aaf522f056d69ac08d91773889b76f8cef"}}`, "26.11.0"},
		{`{"schema": "xynolog-upstream-lock/v1", "sdk": {"commit": "548e4fb7238693be7f21d7977a814441fc090bcd"}, "images": {"commit": "0ddc65aaf522f056d69ac08d91773889b76f8cef"}}`, "local"},
	} {
		got, err := lockedImagesTag([]byte(c.lock))
		if err != nil || got != c.want {
			t.Errorf("lockedImagesTag(%s) = %q, %v; want %q", c.lock, got, err, c.want)
		}
	}
}

// A lock that is not one, or names no images, is refused: there is no tag the
// launcher would otherwise write.
func TestALockThatNamesNoImagesIsRefused(t *testing.T) {
	for _, lock := range []string{
		`{"schema": "xynolog-upstream-lock/v1", "sdk": {"commit": "548e4fb7238693be7f21d7977a814441fc090bcd"}}`,
		`{"schema": "xynolog-sdk-pin/v2", "commit": "548e4fb7238693be7f21d7977a814441fc090bcd"}`,
		`not json`,
	} {
		if tag, err := lockedImagesTag([]byte(lock)); err == nil {
			t.Errorf("lockedImagesTag(%s) = %q, want a refusal", lock, tag)
		}
	}
}
