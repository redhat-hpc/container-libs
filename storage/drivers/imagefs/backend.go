//go:build linux

package imagefs

import (
	"errors"
	"io"
)

// ErrNotSupported indicates an operation is not supported by this backend.
var ErrNotSupported = errors.New("operation not supported by this backend")

// BackendInfo contains static properties of a backend.
type BackendInfo struct {
	// Format is the name of the filesystem format ("erofs" or "squashfs")
	Format string

	// FileExtension is the file extension for this format (".erofs" or ".sqfs")
	FileExtension string

	// PreserveTarball indicates whether to keep the original tarball for tar-split.
	// EROFS: true (needs .tar for --device= mounting)
	// SquashFS: false (storage layer handles tar-split)
	PreserveTarball bool
}

// MountContext provides the context needed for backends to mount layers.
type MountContext struct {
	// LayerIDs is the list of layer IDs to mount (in bottom-to-top order)
	LayerIDs []string

	// ContainerID is the container using these layers
	ContainerID string

	// MountLabel is the SELinux mount label
	MountLabel string

	// MountManager handles the actual mounting operations
	MountManager *MountManager

	// GetImagePath returns the image file path for a given layer ID
	GetImagePath func(id string) string
}

// MountSpec describes how to mount a single filesystem image.
type MountSpec struct {
	// ImagePath is the path to the image file
	ImagePath string

	// FsType is the filesystem type ("erofs" or "squashfs")
	FsType string

	// DevicePaths are external device files (EROFS-specific for metadata-only images)
	DevicePaths []string

	// FuseCommand is the FUSE mount command to use ("erofsfuse" or "squashfuse")
	FuseCommand string

	// FuseArgs are additional arguments for the FUSE command (e.g., "--device=/path")
	FuseArgs []string

	// KernelFlags are format-specific kernel mount flags (e.g., "noacl" for EROFS)
	KernelFlags []string
}

// Backend handles format-specific operations for creating and managing filesystem images.
type Backend interface {
	// Info returns static backend properties.
	Info() BackendInfo

	// CreateImage creates a filesystem image from a tarball.
	// Returns the size of the original tarball in bytes.
	CreateImage(tarballPath, destImagePath string) (int64, error)

	// CreateMountSpec creates a format-specific mount specification for an image.
	CreateMountSpec(imagePath string) MountSpec

	// MountLayers mounts layers using backend-specific optimizations.
	// Returns mount points (in bottom-to-top order), whether FUSE was used, and error.
	MountLayers(ctx MountContext) ([]string, bool, error)

	// DiffForBaseLayer returns a ReadCloser for the layer's diff.
	// For EROFS, this returns the original tarball to preserve digests.
	// For SquashFS, returns ErrNotSupported to indicate naiveDiff should be used.
	DiffForBaseLayer(imagePath string) (io.ReadCloser, error)

	// StatusFields returns format-specific status information as key-value pairs.
	// Examples:
	//   EROFS: [["erofs-utils", "1.7.1"]]
	//   SquashFS: [["squashfs-backend", "sqfstar"], ["squashfs-tools", "sqfstar 4.6.1"]]
	StatusFields() [][2]string
}
