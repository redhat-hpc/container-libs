package imagefs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	graphdriver "go.podman.io/storage/drivers"
	"go.podman.io/storage/internal/tempdir"
	"go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/directory"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/mount"
)

func init() {
	graphdriver.MustRegister("imagefs", Init)
}

// Init returns a new ImageFS driver.
func Init(home string, options graphdriver.Options) (graphdriver.Driver, error) {
	opts, err := parseOptions(options.DriverOptions)
	if err != nil {
		return nil, err
	}

	// Validate that the required conversion tool is present
	var tool string
	switch opts.Format {
	case FormatEROFS:
		tool = "mkfs.erofs"
	case FormatSquashFS:
		tool = "tar2sqfs"
	}

	if _, err := exec.LookPath(tool); err != nil {
		return nil, fmt.Errorf("imagefs: required tool %q not found in PATH: %w", tool, err)
	}
	logrus.Infof("imagefs: driver initialized with format %q using tool %q", opts.Format, tool)

	d := &ImageFS{
		name:       "imagefs",
		home:       home,
		imageStore: options.ImageStore,
		options:    opts,
	}

	// Create the driver home directory structure
	if err := os.MkdirAll(filepath.Join(home, "layers"), 0o700); err != nil {
		return nil, err
	}

	// Initialize the naive diff driver wrapper
	d.updater = graphdriver.NewNaiveLayerIDMapUpdater(d)
	d.naiveDiff = graphdriver.NewNaiveDiffDriver(d, d.updater)

	return d, nil
}

// ImageFS is a storage driver for image file system operations.
type ImageFS struct {
	name              string
	home              string
	imageStore        string
	options           *Options
	naiveDiff         graphdriver.DiffDriver
	updater           graphdriver.LayerIDMapUpdater
	ignoreChownErrors bool
}

// String returns a string representation of this driver.
func (d *ImageFS) String() string {
	return d.name
}

// Status returns status information for the driver.
// Currently returns empty status as no actual storage is managed.
func (d *ImageFS) Status() [][2]string {
	return nil
}

// Metadata returns metadata for the specified layer ID.
func (d *ImageFS) Metadata(id string) (map[string]string, error) {
	var ext string
	switch d.options.Format {
	case FormatEROFS:
		ext = ".erofs"
	case FormatSquashFS:
		ext = ".sqsh"
	default:
		return nil, fmt.Errorf("imagefs: unsupported format %q", d.options.Format)
	}

	path := filepath.Join(d.home, "layers", id+ext)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	return map[string]string{
		"format": d.options.Format,
		"path":   path,
	}, nil
}

// Cleanup performs any necessary cleanup tasks.
// Currently a no-op as the stub driver doesn't hold resources.
func (d *ImageFS) Cleanup() error {
	return nil
}

// CreateReadWrite creates a new read-write layer with the specified ID and parent.
func (d *ImageFS) CreateReadWrite(id, parent string, opts *graphdriver.CreateOpts) error {
	return fmt.Errorf("imagefs: read-write layers are not supported by this driver")
}

// Create creates a new layer with the specified ID and parent.
func (d *ImageFS) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	// For imagefs, the layer file is created during ApplyDiff.
	// We don't need to do anything here.
	return nil
}

// CreateFromTemplate creates a layer with the same contents as a template layer.
func (d *ImageFS) CreateFromTemplate(id, template string, templateIDMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, opts *graphdriver.CreateOpts, readWrite bool) error {
	if readWrite {
		return fmt.Errorf("imagefs: read-write layers are not supported")
	}

	var ext string
	switch d.options.Format {
	case FormatEROFS:
		ext = ".erofs"
	case FormatSquashFS:
		ext = ".sqsh"
	default:
		return fmt.Errorf("imagefs: unsupported format %q", d.options.Format)
	}

	src := filepath.Join(d.home, "layers", template+ext)
	dst := filepath.Join(d.home, "layers", id+ext)

	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("imagefs: template layer %s not found: %w", template, err)
	}

	// Copy the binary image file
	logrus.Debugf("imagefs: creating layer %s from template %s", id, template)
	input, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("imagefs: failed to open template image: %w", err)
	}
	defer input.Close()

	output, err := os.Create(dst + ".tmp")
	if err != nil {
		return fmt.Errorf("imagefs: failed to create tmp image for template: %w", err)
	}
	tmpName := dst + ".tmp"
	defer os.Remove(tmpName)

	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("imagefs: failed to copy template image: %w", err)
	}
	output.Close()

	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("imagefs: failed to rename template image: %w", err)
	}

	return nil
}

// Remove attempts to remove the layer with the specified ID.
func (d *ImageFS) Remove(id string) error {
	var ext string
	switch d.options.Format {
	case FormatEROFS:
		ext = ".erofs"
	case FormatSquashFS:
		ext = ".sqsh"
	default:
		return fmt.Errorf("imagefs: unsupported format %q", d.options.Format)
	}

	path := filepath.Join(d.home, "layers", id+ext)
	logrus.Debugf("imagefs: removing layer image %s", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("imagefs: failed to remove layer %s: %w", id, err)
	}
	return nil
}

// DeferredRemove is used to remove the layer with the specified ID.
func (d *ImageFS) DeferredRemove(id string) (tempdir.CleanupTempDirFunc, error) {
	// For imagefs, removal is simple and doesn't require a complex deferred cleanup
	// like overlay/vfs might. We can just call Remove and return a no-op cleanup.
	if err := d.Remove(id); err != nil {
		return nil, err
	}
	return func() error {
		// No additional cleanup needed for binary image files
		return nil
	}, nil
}

// GetTempDirRootDirs returns the root directories for temporary directories.
// Currently returns an empty slice as no temp directories are managed.
func (d *ImageFS) GetTempDirRootDirs() []string {
	return []string{}
}

// Get returns the mount point for the layered filesystem referred to by the ID.
func (d *ImageFS) Get(id string, options graphdriver.MountOpts) (string, error) {
	var ext, fsType string
	switch d.options.Format {
	case FormatEROFS:
		ext = ".erofs"
		fsType = "erofs"
	case FormatSquashFS:
		ext = ".sqsh"
		fsType = "squashfs"
	default:
		return "", fmt.Errorf("imagefs: unsupported format %q", d.options.Format)
	}

	imagePath := filepath.Join(d.home, "layers", id+ext)
	if !d.Exists(id) {
		return "", fmt.Errorf("imagefs: layer %s does not exist", id)
	}

	mountPoint := filepath.Join(d.home, "mounts", id)
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return "", fmt.Errorf("imagefs: failed to create mount point %s: %w", mountPoint, err)
	}

	// We use the 'mount' command because it handles loop device allocation automatically.
	// The unix.Mount system call does not.
	cmd := exec.Command("mount", "-t", fsType, "-o", "loop", imagePath, mountPoint)
	logrus.Debugf("imagefs: mounting image %s to %s using %s", imagePath, mountPoint, fsType)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("imagefs: failed to mount image %s to %s: %s: %w", imagePath, mountPoint, string(output), err)
	}

	return mountPoint, nil
}

// Put releases the system resources for the specified ID.
func (d *ImageFS) Put(id string) error {
	mountPoint := filepath.Join(d.home, "mounts", id)

	logrus.Debugf("imagefs: unmounting image from %s", mountPoint)
	// Use the package mount helper to unmount
	if err := mount.Unmount(mountPoint); err != nil {
		// If it's not mounted, we can ignore the error
		return nil
	}

	if err := os.RemoveAll(mountPoint); err != nil {
		return fmt.Errorf("imagefs: failed to remove mount point %s: %w", mountPoint, err)
	}

	return nil
}

// Exists checks whether a layer with the specified ID exists.
func (d *ImageFS) Exists(id string) bool {
	var ext string
	switch d.options.Format {
	case FormatEROFS:
		ext = ".erofs"
	case FormatSquashFS:
		ext = ".sqsh"
	default:
		return false
	}

	path := filepath.Join(d.home, "layers", id+ext)
	_, err := os.Stat(path)
	return err == nil
}

// ListLayers returns a list of layer IDs that exist on this driver.
func (d *ImageFS) ListLayers() ([]string, error) {
	var ext string
	switch d.options.Format {
	case FormatEROFS:
		ext = ".erofs"
	case FormatSquashFS:
		ext = ".sqsh"
	default:
		return nil, fmt.Errorf("imagefs: unsupported format %q", d.options.Format)
	}

	layersDir := filepath.Join(d.home, "layers")
	entries, err := os.ReadDir(layersDir)
	if err != nil {
		return nil, fmt.Errorf("imagefs: failed to read layers directory: %w", err)
	}

	var layers []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ext) {
			layers = append(layers, strings.TrimSuffix(entry.Name(), ext))
		}
	}
	return layers, nil
}

// ReadWriteDiskUsage returns the disk usage of the writable directory for the specified ID.
// Currently returns zero usage as no actual storage is managed.
func (d *ImageFS) ReadWriteDiskUsage(id string) (*directory.DiskUsage, error) {
	return &directory.DiskUsage{}, nil
}

// AdditionalImageStores returns additional image stores supported by the driver.
// Currently returns an empty list as no additional image stores are configured.
func (d *ImageFS) AdditionalImageStores() []string {
	return nil
}

// Dedup performs deduplication of the driver's storage.
// Returns an error indicating this method is not yet implemented.
func (d *ImageFS) Dedup(req graphdriver.DedupArgs) (graphdriver.DedupResult, error) {
	return graphdriver.DedupResult{}, fmt.Errorf("imagefs: Dedup not yet implemented")
}

// Diff produces an archive of the changes between the specified layer and its parent.
// Returns an error indicating this method is not yet implemented.
func (d *ImageFS) Diff(id string, idMappings *idtools.IDMappings, parent string, parentMappings *idtools.IDMappings, mountLabel string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("imagefs: Diff not yet implemented")
}

// Changes produces a list of changes between the specified layer and its parent.
// Returns an error indicating this method is not yet implemented.
func (d *ImageFS) Changes(id string, idMappings *idtools.IDMappings, parent string, parentMappings *idtools.IDMappings, mountLabel string) ([]archive.Change, error) {
	return nil, fmt.Errorf("imagefs: Changes not yet implemented")
}

// ApplyDiff extracts the changeset from the given diff into the layer with the specified ID.
func (d *ImageFS) ApplyDiff(id string, options graphdriver.ApplyDiffOpts) (size int64, err error) {
	var ext string
	var tool string
	var args []string

	switch d.options.Format {
	case FormatEROFS:
		ext = ".erofs"
		tool = "mkfs.erofs"
	case FormatSquashFS:
		ext = ".sqsh"
		tool = "tar2sqfs"
	default:
		return 0, fmt.Errorf("imagefs: unsupported format %q", d.options.Format)
	}

	finalPath := filepath.Join(d.home, "layers", id+ext)
	tmpImgPath := finalPath + ".tmp"

	// Prepare tool arguments. Both tools support reading the tarball from stdin
	// if the input file path is omitted.
	if d.options.Format == FormatEROFS {
		// --tar=f tells mkfs.erofs to read from stdin.
		args = []string{"--tar=f", "-zlz4", tmpImgPath}
	} else {
		// For tar2sqfs, omitting the input file makes it read from stdin.
		args = []string{"-f", tmpImgPath}
	}

	logrus.Debugf("imagefs: converting tarball to %s image using %s %v", d.options.Format, tool, args)
	cmd := exec.Command(tool, args...)
	cmd.Stdin = options.Diff

	if output, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpImgPath)
		return 0, fmt.Errorf("imagefs: conversion tool %q failed: %s: %w", tool, string(output), err)
	}

	// Atomic write: rename tmp image to final image
	if err := os.Rename(tmpImgPath, finalPath); err != nil {
		os.Remove(tmpImgPath)
		return 0, fmt.Errorf("imagefs: failed to rename tmp image to final path: %w", err)
	}

	// Get the size of the created image
	fi, err := os.Stat(finalPath)
	if err != nil {
		return 0, fmt.Errorf("imagefs: failed to stat created image: %w", err)
	}

	return fi.Size(), nil
}

// DiffSize calculates the changes between the specified ID and its parent.
// Returns an error indicating this method is not yet implemented.
func (d *ImageFS) DiffSize(id string, idMappings *idtools.IDMappings, parent string, parentMappings *idtools.IDMappings, mountLabel string) (size int64, err error) {
	return 0, fmt.Errorf("imagefs: DiffSize not yet implemented")
}

// SupportsShifting tells whether the driver supports shifting of UIDs/GIDs.
// Currently returns false as ID shifting is not implemented.
func (d *ImageFS) SupportsShifting(uidmap, gidmap []idtools.IDMap) bool {
	return false
}

// UpdateLayerIDMap updates the layer's filesystem tree with new ownership information.
// Returns an error indicating this method is not yet implemented.
func (d *ImageFS) UpdateLayerIDMap(id string, toContainer, toHost *idtools.IDMappings, mountLabel string) error {
	return fmt.Errorf("imagefs: UpdateLayerIDMap not yet implemented")
}
