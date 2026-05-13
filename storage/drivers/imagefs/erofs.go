//go:build linux

package imagefs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/fileutils"
	"go.podman.io/storage/pkg/parsers/kernel"
)

// ErofsBackend implements the Backend interface for EROFS (Enhanced Read-Only File System).
type ErofsBackend struct {
	info        BackendInfo
	compression string
}

// NewErofsBackend creates a new EROFS backend.
func NewErofsBackend(compression string) *ErofsBackend {
	// Determine if we should preserve tarball based on compression mode:
	// - No compression → metadata-only EROFS → preserve .tar for device mounting
	// - With compression → full compressed EROFS → no .tar needed, use tar-split
	preserveTarball := compression == ""

	return &ErofsBackend{
		info: BackendInfo{
			Format:          FormatEROFS,
			FileExtension:   ".erofs",
			PreserveTarball: preserveTarball,
		},
		compression: compression,
	}
}

func (b *ErofsBackend) Info() BackendInfo {
	return b.info
}

func (b *ErofsBackend) CreateImage(tarballPath, destImagePath string) (int64, error) {
	var args []string

	if b.compression == "" {
		// Metadata-only mode: create metadata-only EROFS that references .tar as device
		args = []string{"--tar=i", "--aufs", "-E", "legacy-compress"}
		logrus.Debugf("[imagefs] Creating metadata-only EROFS: %s -> %s", tarballPath, destImagePath)
	} else {
		// Full image mode: create compressed EROFS with actual data
		// Compression support varies by erofs-utils version:
		// - 1.7.x: lz4, lz4hc, deflate, libdeflate
		// - 1.8+:  lz4, lz4hc, deflate, lzma, zstd
		// Note: If the specified compressor is not available, mkfs.erofs will fail
		// with a clear error message about unsupported compression algorithm
		args = []string{"--aufs", "-z", b.compression}
		logrus.Debugf("[imagefs] Creating full compressed EROFS with %s: %s -> %s", b.compression, tarballPath, destImagePath)
	}

	args = append(args, destImagePath, tarballPath)
	cmd := exec.Command("mkfs.erofs", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("mkfs.erofs failed: %w: %s", err, stderr.String())
	}

	// Get the size of the tarball for return value
	info, err := os.Stat(tarballPath)
	if err != nil {
		return 0, fmt.Errorf("failed to stat tarball: %w", err)
	}

	return info.Size(), nil
}

func (b *ErofsBackend) MountLayers(ctx MountContext) ([]string, bool, error) {
	if len(ctx.LayerIDs) == 0 {
		return nil, false, fmt.Errorf("no layers to mount")
	}

	// Collect image paths and device paths
	var imagePaths []string
	var devicePaths []string
	for _, layerID := range ctx.LayerIDs {
		imagePath := ctx.GetImagePath(layerID)
		if imagePath == "" {
			return nil, false, fmt.Errorf("no image file found for layer %s", layerID)
		}
		if filepath.Ext(imagePath) != ".erofs" {
			return nil, false, fmt.Errorf("layer %s is not an EROFS image", layerID)
		}
		imagePaths = append(imagePaths, imagePath)

		// Collect device paths based on compression mode:
		// - No compression (metadata mode): use .tar files as devices
		// - With compression (full image mode): use .erofs files as devices
		if b.compression == "" {
			// Metadata mode: look for .tar device files
			devicePath := imagePath + ".tar"
			if fileutils.Exists(devicePath) == nil {
				devicePaths = append(devicePaths, devicePath)
			}
		} else {
			// Full image mode: use .erofs files themselves as devices
			devicePaths = append(devicePaths, imagePath)
		}
	}

	// Check if we can use the merged strategy
	canMerge := kernel.CheckKernelVersion(5, 14, 0) && len(imagePaths) > 1

	if !canMerge {
		// Fall back to mounting layers separately
		return b.mountLayersSeparately(ctx, imagePaths)
	}

	// Use merged strategy
	compressionMode := "metadata-only"
	if b.compression != "" {
		compressionMode = fmt.Sprintf("compressed (%s)", b.compression)
	}
	logrus.Debugf("[imagefs/erofs] Using merged layers strategy for %d %s layers", len(imagePaths), compressionMode)
	rundir := ctx.MountManager.GetRundir(ctx.ContainerID)
	if err := os.MkdirAll(rundir, 0o755); err != nil {
		return nil, false, fmt.Errorf("failed to create rundir: %w", err)
	}

	mergedImagePath := filepath.Join(rundir, "merged_layers.erofs")

	// Create merged EROFS image
	args := append([]string{mergedImagePath}, imagePaths...)
	cmd := exec.Command("mkfs.erofs", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	logrus.Debugf("[imagefs/erofs] Creating merged EROFS: mkfs.erofs %v", args)
	if err := cmd.Run(); err != nil {
		return nil, false, fmt.Errorf("mkfs.erofs merge failed: %w: %s", err, stderr.String())
	}

	// Mount the merged image
	// The merged EROFS is metadata-only and needs device paths from all original layers
	spec := MountSpec{
		ImagePath:   mergedImagePath,
		FsType:      "erofs",
		FuseCommand: "erofsfuse",
		KernelFlags: []string{"noacl"},
		DevicePaths: devicePaths, // Device paths (.tar in metadata mode, .erofs in full mode)
	}
	// Add FUSE arguments for device paths
	for _, devicePath := range devicePaths {
		spec.FuseArgs = append(spec.FuseArgs, "--device="+devicePath)
	}
	logrus.Debugf("[imagefs/erofs] Mounting merged EROFS with %d device paths: %v", len(devicePaths), devicePaths)

	isRoot := os.Getuid() == 0
	mountPoint, usedFuse, err := ctx.MountManager.MountLayer(
		ctx.ContainerID,
		"merged-layers",
		spec,
		isRoot,
		ctx.MountLabel,
	)
	if err != nil {
		return nil, false, fmt.Errorf("failed to mount merged image: %w", err)
	}

	return []string{mountPoint}, usedFuse, nil
}

func (b *ErofsBackend) mountLayersSeparately(ctx MountContext, imagePaths []string) ([]string, bool, error) {
	var lowerDirs []string
	var usedFuse bool

	for i, imagePath := range imagePaths {
		layerID := ctx.LayerIDs[i]

		// Create EROFS-specific mount spec
		spec := b.CreateMountSpec(imagePath)

		isRoot := os.Getuid() == 0
		mountPoint, layerUsedFuse, err := ctx.MountManager.MountLayer(
			ctx.ContainerID,
			layerID,
			spec,
			isRoot,
			ctx.MountLabel,
		)
		if err != nil {
			return nil, false, fmt.Errorf("failed to mount layer %s: %w", layerID, err)
		}
		if layerUsedFuse {
			usedFuse = true
		}
		lowerDirs = append(lowerDirs, mountPoint)
	}

	return lowerDirs, usedFuse, nil
}

// CreateMountSpec creates an EROFS-specific mount specification.
func (b *ErofsBackend) CreateMountSpec(imagePath string) MountSpec {
	spec := MountSpec{
		ImagePath:   imagePath,
		FsType:      "erofs",
		FuseCommand: "erofsfuse",
		KernelFlags: []string{"noacl"}, // EROFS-specific: container images don't use ACLs
	}

	// For metadata-only EROFS (no compression), add .tar device file
	// For full compressed EROFS, no device needed (data is in the .erofs)
	if b.compression == "" {
		devicePath := imagePath + ".tar"
		if fileutils.Exists(devicePath) == nil {
			spec.DevicePaths = append(spec.DevicePaths, devicePath)
			// FUSE arguments for device paths
			spec.FuseArgs = append(spec.FuseArgs, "--device="+devicePath)
		}
	}

	return spec
}

func (b *ErofsBackend) DiffForBaseLayer(imagePath string) (io.ReadCloser, error) {
	// For metadata-only EROFS (no compression), return the original tarball to preserve exact digests
	// For full compressed EROFS, use tar-split reconstruction (return ErrNotSupported)
	if b.compression != "" {
		logrus.Debugf("[imagefs/erofs] Using tar-split for compressed EROFS layer: %s", imagePath)
		return nil, ErrNotSupported
	}

	tarballPath := imagePath + ".tar"
	if fileutils.Exists(tarballPath) != nil {
		return nil, ErrNotSupported // Tarball doesn't exist, use naiveDiff
	}

	logrus.Debugf("[imagefs/erofs] Returning original tarball for metadata-only EROFS layer: %s", tarballPath)
	f, err := os.Open(tarballPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open tarball %s: %w", tarballPath, err)
	}
	return f, nil
}

func (b *ErofsBackend) StatusFields() [][2]string {
	return [][2]string{
		{"erofs-utils", b.getVersion()},
	}
}

func (b *ErofsBackend) getVersion() string {
	cmd := exec.Command("mkfs.erofs", "-V")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// we don't care about exit codes because we just need to find the version somewhere
	cmd.Run()

	output := stdout.String()
	re := regexp.MustCompile(`(\d+\.\d+\.\d+)`)
	matches := re.FindStringSubmatch(output)
	if len(matches) > 1 {
		return matches[1]
	}

	// Try stderr if stdout doesn't have the version
	output = stderr.String()
	matches = re.FindStringSubmatch(output)
	if len(matches) > 1 {
		return matches[1]
	}

	logrus.Debugf("mkfs.erofs output: %s", stdout.String())
	logrus.Debugf("mkfs.erofs error: %s", stderr.String())

	return "unknown"
}
