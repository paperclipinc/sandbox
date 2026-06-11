package cas

import (
	"fmt"
)

// VerifyFilesAgainstManifest re-derives the chunk digests of the supplied
// on-disk files and checks them against the recorded manifest. It is the shared
// integrity primitive used by both the forkd build-time verify (which verifies
// the WHOLE snapshot via the manifest digest) and the husk activate-time verify
// (which has only the loaded mem+vmstate files mounted, not the rootfs, so it
// verifies the present subset against the same recorded manifest).
//
// files maps a manifest logical name (e.g. "mem", "vmstate") to its on-disk
// path. For every supplied name the file MUST appear in the manifest, its size
// and ordered chunk digests MUST match exactly, or an error is returned. A
// supplied name absent from the manifest is an error: a file that is not part of
// the recorded snapshot identity must not be loaded.
//
// This re-hashes each supplied file once (streaming, bounded memory). It does
// NOT require every manifest file to be present: a caller that mounts only a
// subset (the husk pod mounts mem+vmstate but not rootfs) passes exactly the
// files it has. The recorded manifest itself is bound to the recorded content
// address by the caller comparing manifest.Digest() to the expected digest, so
// the unmounted files are still pinned by the manifest's own integrity.
func VerifyFilesAgainstManifest(m Manifest, files map[string]string) error {
	byName := make(map[string]FileEntry, len(m.Files))
	for _, fe := range m.Files {
		byName[fe.Name] = fe
	}
	for name, path := range files {
		fe, ok := byName[name]
		if !ok {
			return fmt.Errorf("file %q is not part of the recorded snapshot manifest", name)
		}
		chunks, err := chunkFile(path)
		if err != nil {
			return fmt.Errorf("re-hash %q: %w", name, err)
		}
		if err := compareChunks(name, fe.Chunks, chunks); err != nil {
			return err
		}
	}
	return nil
}

// compareChunks reports a mismatch between the recorded and re-derived chunk
// lists for a file. A differing chunk count or any differing chunk digest/size
// is a tamper signal. The digests are content addresses and safe to log.
func compareChunks(name string, want, got []ChunkRef) error {
	if len(want) != len(got) {
		return fmt.Errorf("file %q failed integrity verification: recorded %d chunks but on-disk content has %d", name, len(want), len(got))
	}
	for i := range want {
		if want[i].Digest != got[i].Digest || want[i].Size != got[i].Size {
			return fmt.Errorf("file %q failed integrity verification: chunk %d recorded %s/%d does not match on-disk %s/%d", name, i, want[i].Digest, want[i].Size, got[i].Digest, got[i].Size)
		}
	}
	return nil
}

// DecodeManifest decodes canonical manifest bytes (the bytes a Store writes to
// <root>/manifests/<digest>, identical to Manifest.Canonical) back into a
// Manifest. It is exported so a process that receives a manifest out-of-band
// (the husk pod, which has the manifest mounted but not the whole CAS store) can
// decode it, recompute its digest, and verify files against it without a Store.
func DecodeManifest(data []byte) (Manifest, error) {
	return decodeManifest(data)
}
