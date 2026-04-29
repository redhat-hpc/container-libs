//go:build linux

package imagefs

import (
	"io"
)

// Backend handles format-specific operations for creating and managing filesystem images.
type Backend interface {
	// Format returns the format name ("erofs" or "squashfs")
	Format() string

	// FileExtension returns the file extension for this format (".erofs" or ".sqfs")
	FileExtension() string

	// CreateImage creates a filesystem image from a tarball.
	// Returns the size of the original tarball in bytes.
	CreateImage(tarballPath, destImagePath string) (int64, error)

	// CanMergeLayers returns true if this backend supports merging multiple
	// image files into a single merged image (EROFS-specific optimization).
	CanMergeLayers(imagePaths []string) bool

	// MergeLayers merges multiple image files into a single image.
	// Returns an error if merging is not supported by this backend.
	// Only called if CanMergeLayers returns true.
	MergeLayers(imagePaths []string, devicePaths []string, mergedImagePath string) error

	// ShouldPreserveTarball returns true if this backend needs to keep
	// the original tarball for tar-split reconstruction.
	// EROFS: true (needs .tar for --device= mounting)
	// SquashFS: false (storage layer handles tar-split)
	ShouldPreserveTarball() bool

	// GetDiffForBaseLayer returns a ReadCloser for the layer's diff.
	// For EROFS, this returns the original tarball to preserve digests.
	// For SquashFS, returns nil to indicate naiveDiff should be used.
	GetDiffForBaseLayer(imagePath string) (io.ReadCloser, error)

	// StatusFields returns format-specific status information as key-value pairs.
	// Examples:
	//   EROFS: [["erofs-utils", "1.7.1"]]
	//   SquashFS: [["squashfs-backend", "sqfstar"], ["squashfs-tools", "sqfstar 4.6.1"]]
	StatusFields() [][2]string
}
