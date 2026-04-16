package imagefs

import (
	"bytes"
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
	"go.podman.io/storage/pkg/fileutils"
	"go.podman.io/storage/pkg/idtools"
)

type Driver struct {
	home    string
	runRoot string
	options Options
	mm      *MountManager
}

func Init(home string, options graphdriver.Options) (graphdriver.Driver, error) {
	return &Driver{
		home:    home,
		runRoot: options.RunRoot,
		options: Options{Format: FormatEROFS},
		mm:      NewMountManager(options.RunRoot),
	}, nil
}

func init() {
	graphdriver.MustRegister("imagefs", Init)
}

// --- ProtoDriver implementation ---

func (d *Driver) String() string {
	return "imagefs"
}

func (d *Driver) CreateReadWrite(id, parent string, opts *graphdriver.CreateOpts) error {
	// For imagefs, we just need to ensure the directory for the writable layer exists.
	// The actual layering happens at mount time.
	dir := d.dir(id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	if parent != "" {
		if err := os.WriteFile(filepath.Join(dir, "parent"), []byte(parent), 0644); err != nil {
			return fmt.Errorf("failed to write parent file: %w", err)
		}
	}
	return nil
}

func (d *Driver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	// In imagefs, RO layers are assumed to be .img or .sqsh files in the home directory.
	// We create a directory for metadata.
	dir := d.dir(id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	if parent != "" {
		if err := os.WriteFile(filepath.Join(dir, "parent"), []byte(parent), 0644); err != nil {
			return fmt.Errorf("failed to write parent file: %w", err)
		}
	}
	return nil
}

func (d *Driver) CreateFromTemplate(id, template string, templateIDMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, opts *graphdriver.CreateOpts, readWrite bool) error {
	return fmt.Errorf("CreateFromTemplate not implemented")
}

func (d *Driver) Remove(id string) error {
	cleanup, err := d.DeferredRemove(id)
	if err != nil {
		return err
	}
	return cleanup()
}

func (d *Driver) DeferredRemove(id string) (tempdir.CleanupTempDirFunc, error) {
	dir := d.dir(id)
	return func() error {
		return os.RemoveAll(dir)
	}, nil
}

func (d *Driver) GetTempDirRootDirs() []string {
	return []string{filepath.Join(d.home, "tmp")}
}

func (d *Driver) Get(id string, options graphdriver.MountOpts) (string, error) {
	layers, err := d.getLayerStack(id)
	if err != nil {
		return "", err
	}

	containerID := id
	var lowerDirs []string

	// Phase 2: Mount each layer in the rundir from bottom to top.
	// The top-most layer (id) is the writable layer and should not be mounted as a lowerdir.
	if len(layers) > 0 {
		layers = layers[:len(layers)-1]
	}

	for _, layerID := range layers {
		imagePath := d.getImagePath(layerID)
		if imagePath == "" {
			return "", fmt.Errorf("no image file found for layer %s", layerID)
		}

		// Check if we are root or rootless (simplified check)
		isRoot := os.Getuid() == 0

		mountPoint, err := d.mm.MountLayer(containerID, layerID, imagePath, isRoot)
		if err != nil {
			d.cleanupMounts(containerID, lowerDirs)
			return "", fmt.Errorf("failed to mount layer %s: %w", layerID, err)
		}
		lowerDirs = append(lowerDirs, mountPoint)
	}

	// Lowerdir for OverlayFS is top-to-bottom (most recent first)
	var lowerdirString string
	for i := len(lowerDirs) - 1; i >= 0; i-- {
		if lowerdirString != "" {
			lowerdirString += ":"
		}
		lowerdirString += lowerDirs[i]
	}

	// Setup upperdir and workdir
	layerDir := d.dir(id)
	upperdir := filepath.Join(layerDir, "upper")
	workdir := filepath.Join(layerDir, "work")
	mergedDir := filepath.Join(layerDir, "merged")

	if err := os.MkdirAll(upperdir, 0755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(workdir, 0755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(mergedDir, 0755); err != nil {
		return "", err
	}

	// Final Overlay Mount
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerdirString, upperdir, workdir)
	logrus.Debugf("[imagefs] Final Overlay Mount: target=%s, opts=%s", mergedDir, opts)
	err = d.mm.mounter.Mount("overlay", mergedDir, "overlay", opts)
	if err != nil {
		d.cleanupMounts(containerID, lowerDirs)
		return "", fmt.Errorf("failed to mount overlay: %w", err)
	}

	return mergedDir, nil
}

func (d *Driver) Put(id string) error {
	// Unmount merged dir
	mergedDir := filepath.Join(d.dir(id), "merged")
	if err := d.mm.mounter.Unmount(mergedDir); err != nil && !os.IsNotExist(err) {
		logrus.Errorf("failed to unmount merged dir %s: %v", mergedDir, err)
	}

	// Unmount layers and cleanup rundir
	return d.mm.CleanupRundir(id)
}

func (d *Driver) getLayerStack(id string) ([]string, error) {
	var stack []string
	current := id
	for current != "" {
		stack = append([]string{current}, stack...)
		parentFile := filepath.Join(d.dir(current), "parent")
		data, err := os.ReadFile(parentFile)
		if err != nil {
			if os.IsNotExist(err) {
				current = ""
			} else {
				return nil, err
			}
		} else {
			current = strings.TrimSpace(string(data))
		}
	}
	return stack, nil
}

func (d *Driver) getImagePath(id string) string {
	// In our updated layout, the image file is stored inside the layer directory:
	// /home/.../storage/imagefs/<id>/data.img

	img := filepath.Join(d.dir(id), "data.img")
	if fileutils.Exists(img) == nil {
		return img
	}

	// Fallback to .sqsh if we used squashfs
	sqsh := filepath.Join(d.dir(id), "data.sqsh")
	if fileutils.Exists(sqsh) == nil {
		return sqsh
	}

	return ""
}

func (d *Driver) cleanupMounts(containerID string, mountedDirs []string) {
	for _, dir := range mountedDirs {
		d.mm.UnmountLayer(dir)
	}
	d.mm.CleanupRundir(containerID)
}

func (d *Driver) Exists(id string) bool {
	return fileutils.Exists(d.dir(id)) == nil
}

func (d *Driver) ListLayers() ([]string, error) {
	return nil, fmt.Errorf("ListLayers not implemented")
}

func (d *Driver) Status() [][2]string {
	return [][2]string{{"driver", "imagefs"}}
}

func (d *Driver) Metadata(id string) (map[string]string, error) {
	path := d.getImagePath(id)
	if path == "" {
		return nil, fmt.Errorf("no image or directory found for layer %s", id)
	}

	meta := make(map[string]string)
	meta["path"] = path

	// Determine the format
	if filepath.Ext(path) == ".img" {
		meta["format"] = "erofs"
	} else if filepath.Ext(path) == ".sqsh" {
		meta["format"] = "squashfs"
	} else {
		meta["format"] = "directory"
	}

	return meta, nil
}

func (d *Driver) ReadWriteDiskUsage(id string) (*directory.DiskUsage, error) {
	return nil, fmt.Errorf("ReadWriteDiskUsage not implemented")
}

func (d *Driver) Cleanup() error {
	return nil
}

func (d *Driver) AdditionalImageStores() []string {
	return nil
}

func (d *Driver) Dedup(args graphdriver.DedupArgs) (graphdriver.DedupResult, error) {
	return graphdriver.DedupResult{}, fmt.Errorf("Dedup not implemented")
}

// --- DiffDriver implementation ---

func (d *Driver) Diff(id string, idMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, mountLabel string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("Diff not implemented")
}

func (d *Driver) Changes(id string, idMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, mountLabel string) ([]archive.Change, error) {
	return nil, fmt.Errorf("Changes not implemented")
}

func (d *Driver) ApplyDiff(id string, options graphdriver.ApplyDiffOpts) (int64, error) {
	// To create an immutable image file from a diff stream, we must:
	// 1. Extract the stream to a temporary directory.
	// 2. Use a tool like mkfs.erofs to create the image file from that directory.
	// 3. Store the resulting image file inside the layer's directory.

	tempDir, err := os.MkdirTemp("", "imagefs-apply-diff-")
	if err != nil {
		return 0, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// Extract the layer blob to the temp directory
	bytesWritten, err := archive.ApplyLayer(tempDir, options.Diff)
	if err != nil {
		return 0, fmt.Errorf("failed to extract layer to temp dir: %w", err)
	}

	// Create the image file inside the layer's metadata directory
	// This keeps the storage layout clean: /home/.../storage/imagefs/<id>/data.img
	imagePath := filepath.Join(d.dir(id), "data.img")
	if err := d.createImageFile(tempDir, imagePath); err != nil {
		return 0, fmt.Errorf("failed to create image file %s: %w", imagePath, err)
	}

	return bytesWritten, nil
}

func (d *Driver) createImageFile(srcDir, destFile string) error {
	// We prefer erofs, fallback to squashfs
	err := d.runMkfsErofs(srcDir, destFile)
	if err == nil {
		return nil
	}

	logrus.Warnf("[imagefs] mkfs.erofs failed or not found, trying mksquashfs: %v", err)

	// If erofs fails, try squashfs (adjusting extension)
	sqshFile := strings.TrimSuffix(destFile, filepath.Ext(destFile)) + ".sqsh"
	if err := d.runMkfsSquashfs(srcDir, sqshFile); err != nil {
		return fmt.Errorf("both mkfs.erofs and mksquashfs failed: %w", err)
	}

	return nil
}

func (d *Driver) runMkfsErofs(src, dest string) error {
	// Correct syntax: mkfs.erofs <dest_image> <source_dir>
	cmd := exec.Command("mkfs.erofs", dest, src)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.erofs failed: %w: %s", err, stderr.String())
	}
	return nil
}

func (d *Driver) runMkfsSquashfs(src, dest string) error {
	// mksquashfs <dir> <dest> -noappend
	cmd := exec.Command("mksquashfs", src, dest, "-noappend")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mksquashfs failed: %w: %s", err, stderr.String())
	}
	return nil
}

func (d *Driver) DiffSize(id string, idMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, mountLabel string) (int64, error) {
	return 0, fmt.Errorf("DiffSize not implemented")
}

// --- LayerIDMapUpdater implementation ---

func (d *Driver) UpdateLayerIDMap(id string, toContainer, toHost *idtools.IDMappings, mountLabel string) error {
	return nil
}

func (d *Driver) SupportsShifting(uidmap, gidmap []idtools.IDMap) bool {
	return false
}

// --- Helpers ---

func (d *Driver) dir(id string) string {
	path := filepath.Join(d.home, id)
	d.relabel(path)
	return path
}

func (d *Driver) relabel(path string) {
	// Attempt to relabel the path to container_file_t for SELinux.
	// This is necessary for rootless containers to access files in the home directory.
	// We ignore errors here because chcon might not be installed or SELinux might be disabled.
	_ = exec.Command("chcon", "-t", "container_file_t", path).Run()
}
