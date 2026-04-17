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

const parentFileName = "parent"

type Driver struct {
	home    string
	runRoot string
	options Options
	mm      *MountManager
}

func Init(home string, options graphdriver.Options) (graphdriver.Driver, error) {
	opts, err := parseOptions(options.DriverOptions)
	if err != nil {
		return nil, err
	}

	return &Driver{
		home:    home,
		runRoot: options.RunRoot,
		options: *opts,
		mm:      NewMountManager(options.RunRoot, nil),
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
	return d.createLayer(id, parent)
}

func (d *Driver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	return d.createLayer(id, parent)
}

func (d *Driver) createLayer(id, parent string) error {
	dir := d.dir(id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	d.relabel(dir)

	if parent != "" {
		if err := os.WriteFile(filepath.Join(dir, parentFileName), []byte(parent), 0644); err != nil {
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

	// Use a defer to clean up mounts if an error occurs during the setup process.
	var success bool
	defer func() {
		if !success {
			d.cleanupMounts(containerID, lowerDirs)
		}
	}()

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
			return "", fmt.Errorf("failed to mount layer %s: %w", layerID, err)
		}
		lowerDirs = append(lowerDirs, mountPoint)
	}

	// Lowerdir for OverlayFS is top-to-bottom (most recent first)
	// Reverse the slice of mounted directories
	for i, j := 0, len(lowerDirs)-1; i < j; i, j = i+1, j-1 {
		lowerDirs[i], lowerDirs[j] = lowerDirs[j], lowerDirs[i]
	}
	lowerdirString := strings.Join(lowerDirs, ":")

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
		return "", fmt.Errorf("failed to mount overlay: %w", err)
	}

	success = true
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
		parentFile := filepath.Join(d.dir(current), parentFileName)
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

	extensions := []string{".img", ".sqsh"}
	for _, ext := range extensions {
		img := filepath.Join(d.dir(id), "data"+ext)
		if fileutils.Exists(img) == nil {
			return img
		}
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

	logrus.Debugf("[imagefs] Metadata for layer identified: %v", meta)
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
	// To create an immutable image file from a diff stream, we pipe the tarball
	// stream directly into mkfs.erofs via stdin.
	imagePath := filepath.Join(d.dir(id), "data.img")

	if err := d.createImageFile(options.Diff, imagePath); err != nil {
		return 0, fmt.Errorf("failed to create image file %s: %w", imagePath, err)
	}

	info, err := os.Stat(imagePath)
	if err != nil {
		return 0, fmt.Errorf("failed to stat resulting image file: %w", err)
	}

	logrus.Debugf("[imagefs] layer %s wrote %d bytes", imagePath, info.Size())
	return info.Size(), nil
}

func (d *Driver) createImageFile(r io.Reader, destFile string) error {
	return d.runMkfsErofs(r, destFile)
}

func (d *Driver) runMkfsErofs(r io.Reader, dest string) error {
	// mkfs.erofs -t tar <dest_image> - <source_tarball_on_stdin>
	cmd := exec.Command("mkfs.erofs", "--tar=f", "-zlz4", dest)
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	logrus.Debugf("[imagefs] Creating the layer: %v", cmd.Args)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.erofs failed: %w: %s", err, stderr.String())
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
	return filepath.Join(d.home, id)
}

func (d *Driver) relabel(path string) {
	// Attempt to relabel the path to container_file_t for SELinux.
	// This is necessary for rootless containers to access files in the home directory.
	// We ignore errors here because chcon might not be installed or SELinux might be disabled.
	_ = exec.Command("chcon", "-t", "container_file_t", path).Run()
}
