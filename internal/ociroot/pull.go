package ociroot

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// PullImage resolves ref and fetches the linux/amd64 image from a registry.
// Anonymous public pulls work out of the box; the default keychain still picks
// up configured credentials for private images. This is the network path and
// is exercised only by integration tests.
func PullImage(ctx context.Context, ref string) (v1.Image, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("ociroot: parse image ref %q: %w", ref, err)
	}

	img, err := remote.Image(parsed,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
	)
	if err != nil {
		return nil, fmt.Errorf("ociroot: pull image %q: %w", ref, err)
	}
	return img, nil
}
