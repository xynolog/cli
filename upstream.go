// Package cli holds what only the module's root can embed: upstream.lock.json,
// the upstream releases this build of the CLI was built and released with.
package cli

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed upstream.lock.json
var upstreamLock []byte

var imagesTag = func() string {
	tag, err := lockedImagesTag(upstreamLock)
	if err != nil {
		panic(err)
	}
	return tag
}()

// ImagesTag: the tag at which the launcher writes Xynolog's own images into a
// document, the images release upstream.lock.json names.
func ImagesTag() string { return imagesTag }

// lockedImagesTag: the images release a lock names, or `local` while it names
// images by commit alone. Only a development build has such a lock, since a
// release refuses one, and no registry holds an image of an unreleased commit:
// its images are the ones built on this machine, which run unverified.
func lockedImagesTag(lock []byte) (string, error) {
	var parsed struct {
		Schema string `json:"schema"`
		Images *struct {
			Version string `json:"version"`
		} `json:"images"`
	}
	if err := json.Unmarshal(lock, &parsed); err != nil {
		return "", fmt.Errorf("upstream.lock.json: %w", err)
	}
	if parsed.Schema != "xynolog-upstream-lock/v1" {
		return "", fmt.Errorf("upstream.lock.json: schema %q is not xynolog-upstream-lock/v1", parsed.Schema)
	}
	if parsed.Images == nil {
		return "", fmt.Errorf("upstream.lock.json names no images")
	}
	if parsed.Images.Version == "" {
		return "local", nil
	}
	return parsed.Images.Version, nil
}
