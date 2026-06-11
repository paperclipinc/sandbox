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
// Pulls are anonymous, which covers public images. Private registries and
// credentialed pulls are a follow-up: we deliberately avoid the docker
// config keychain here because it drags in docker/cli and its credential
// helpers, a heavy and version-fragile dependency tree.
func PullImage(ctx context.Context, ref string) (v1.Image, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("ociroot: parse image ref %q: %w", ref, err)
	}

	img, err := remote.Image(parsed,
		remote.WithContext(ctx),
		remote.WithAuth(authn.Anonymous),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
	)
	if err != nil {
		return nil, fmt.Errorf("ociroot: pull image %q: %w", ref, err)
	}
	return img, nil
}
