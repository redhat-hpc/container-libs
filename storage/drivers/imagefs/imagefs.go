//go:build linux

package imagefs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	home      string
	runRoot   string
	options   Options
	backend   Backend
	mm        *MountManager
	syncMode  graphdriver.SyncMode
	naiveDiff graphdriver.DiffDriver
}

func Init(home string, options graphdriver.Options) (graphdriver.Driver, error) {
	opts, err := parseOptions(options.DriverOptions)
	if err != nil {
		return nil, err
	}

	// Create the appropriate backend based on the format
	var backend Backend

	switch opts.Format {
	case FormatEROFS:
		backend = NewErofsBackend(opts.Compression)
	case FormatSquashFS:
		backend = NewSquashfsBackend(opts.Compression)
	default:
		return nil, fmt.Errorf("unsupported format: %s", opts.Format)
	}

	d := &Driver{
		home:     home,
		runRoot:  options.RunRoot,
		options:  *opts,
		backend:  backend,
		mm:       NewMountManager(options.RunRoot, nil),
		syncMode: graphdriver.SyncModeNone, // Default to no sync
	}
	d.naiveDiff = graphdriver.NewNaiveDiffDriver(d, graphdriver.NewNaiveLayerIDMapUpdater(d))
	return d, nil
}

func init() {
	graphdriver.MustRegister("imagefs", Init)
}

// --- ProtoDriver implementation ---

func (d *Driver) String() string {
	return "imagefs"
}

func (d *Driver) SyncMode() graphdriver.SyncMode {
	return d.syncMode
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
	// CreateFromTemplate creates a new layer based on a template layer with different ID mappings.
	// For imagefs, since EROFS images are immutable and already have fixed UIDs/GIDs,
	// we create symlinks to the template's image files rather than copying them.
	if readWrite {
		return d.CreateReadWrite(id, template, opts)
	}

	// Create the layer directory and parent file
	if err := d.Create(id, template, opts); err != nil {
		return err
	}

	// For read-only template layers, create symlinks to the template's EROFS image files.
	// This avoids copying immutable data while allowing the layer to reference the template.
	templateImagePath := d.getImagePath(template)
	if templateImagePath != "" {
		layerDir := d.dir(id)
		ext := filepath.Ext(templateImagePath)

		// Symlink the EROFS image
		targetImage := filepath.Join(layerDir, "layer"+ext)
		if err := os.Symlink(templateImagePath, targetImage); err != nil {
			return fmt.Errorf("failed to symlink template image: %w", err)
		}

		// Symlink the tarball if it exists
		templateTarball := templateImagePath + ".tar"
		if fileutils.Exists(templateTarball) == nil {
			targetTarball := targetImage + ".tar"
			if err := os.Symlink(templateTarball, targetTarball); err != nil {
				return fmt.Errorf("failed to symlink template tarball: %w", err)
			}
		}
	}

	return nil
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

	// Check if the requested layer itself has a committed image (layer.erofs).
	// If it does, it's a committed image layer and its own EROFS image must be
	// included as the topmost lowerdir so that its content is visible in the
	// merged view. If it doesn't have a layer.erofs, it's a working container
	// layer whose writes are captured by the overlay upperdir.
	idImagePath := d.getImagePath(id)
	isCommittedLayer := idImagePath != ""

	isReadOnly := slices.Contains(options.Options, "ro")
	logrus.Debugf("[imagefs] Get(%s): layers=%d, isCommitted=%v, isReadOnly=%v, options=%v", id, len(layers), isCommittedLayer, isReadOnly, options.Options)

	// If this is a read-only mount request for a committed layer, just mount
	// the layer directly without overlay. This is important for tar-split
	// reconstruction which needs to read the exact original layer content.
	if isReadOnly && isCommittedLayer {
		var devicePaths []string
		if filepath.Ext(idImagePath) == ".erofs" {
			devicePath := idImagePath + ".tar"
			if fileutils.Exists(devicePath) == nil {
				devicePaths = append(devicePaths, devicePath)
			}
		}
		isRoot := os.Getuid() == 0
		mountPoint, _, err := d.mm.MountLayerWithDevices(containerID, id, idImagePath, isRoot, devicePaths, options.MountLabel)
		if err != nil {
			return "", fmt.Errorf("failed to mount layer %s read-only: %w", id, err)
		}
		success = true
		return mountPoint, nil
	}

	// Separate the requested layer (id) from its parent layers.
	// The parent layers (everything below id) are always mounted as EROFS lowerdirs.
	if len(layers) > 0 {
		layers = layers[:len(layers)-1]
	}

	// Track whether any layer used FUSE mounts
	var anyLayerUsedFuse bool

	// Mount parent layers as lowerdirs.
	if len(layers) > 0 {
		// Determine strategy based on whether backend supports merging
		useMerged, err := d.canUseMergedLayers(layers)
		if err != nil {
			return "", err
		}

		var layersUsedFuse bool
		if useMerged {
			logrus.Debugf("[imagefs] Using merged layers strategy for container %s", containerID)
			lowerDirs, layersUsedFuse, err = d.mountMergedLayers(containerID, layers, options.MountLabel)
		} else {
			logrus.Debugf("[imagefs] Using separate layers strategy for container %s", containerID)
			lowerDirs, layersUsedFuse, err = d.mountLayersSeparately(containerID, layers, options.MountLabel)
		}

		if err != nil {
			return "", err
		}

		if layersUsedFuse {
			anyLayerUsedFuse = true
		}

		// Lowerdir for OverlayFS is top-to-bottom (most recent first)
		// If we used separate mounts, they were mounted bottom-to-top, so we reverse.
		// If we used merged, there is only one mount point, so reverse does nothing.
		if !useMerged {
			for i, j := 0, len(lowerDirs)-1; i < j; i, j = i+1, j-1 {
				lowerDirs[i], lowerDirs[j] = lowerDirs[j], lowerDirs[i]
			}
		}
	}

	// If the requested layer is a committed image layer, mount its own EROFS
	// image and prepend it as the topmost lowerdir. This ensures that files
	// added or deleted (via whiteout devices) by this layer are visible in
	// the overlay merged view.
	if isCommittedLayer {
		var devicePaths []string
		if filepath.Ext(idImagePath) == ".erofs" {
			devicePath := idImagePath + ".tar"
			if fileutils.Exists(devicePath) == nil {
				devicePaths = append(devicePaths, devicePath)
			}
		}

		isRoot := os.Getuid() == 0
		mountPoint, layerUsedFuse, err := d.mm.MountLayerWithDevices(containerID, id, idImagePath, isRoot, devicePaths, options.MountLabel)
		if err != nil {
			return "", fmt.Errorf("failed to mount layer %s: %w", id, err)
		}
		if layerUsedFuse {
			anyLayerUsedFuse = true
		}
		// Prepend: this layer's content sits on top of its parents.
		lowerDirs = append([]string{mountPoint}, lowerDirs...)
	}

	if len(lowerDirs) == 0 {
		return "", fmt.Errorf("no lower directories found for layer %s", id)
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

	// OverlayFS requires workdir to be empty. Clean it up if it exists.
	if fileutils.Exists(workdir) == nil {
		if err := os.RemoveAll(workdir); err != nil {
			return "", fmt.Errorf("failed to clean workdir %s: %w", workdir, err)
		}
	}
	if err := os.MkdirAll(workdir, 0755); err != nil {
		return "", err
	}

	if err := os.MkdirAll(mergedDir, 0755); err != nil {
		return "", err
	}

	// Final Overlay Mount
	// Use fuse-overlayfs when any layer is FUSE-mounted for better compatibility
	if anyLayerUsedFuse {
		// FUSE lowerdirs require fuse-overlayfs for proper xattr/copy-up support
		if _, err := exec.LookPath("fuse-overlayfs"); err != nil {
			return "", fmt.Errorf("fuse-overlayfs is required when layers are FUSE-mounted but not found in PATH")
		}
		logrus.Debugf("[imagefs] Using fuse-overlayfs for container %s (FUSE lowerdirs detected)", containerID)
		err = mountFuseOverlay(lowerdirString, upperdir, workdir, mergedDir)
	} else {
		// All layers are kernel-mounted, use kernel overlayfs
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerdirString, upperdir, workdir)
		logrus.Debugf("[imagefs] Using kernel overlayfs for container %s with options: %s", containerID, opts)
		err = mountOverlayFrom(d.home, "overlay", mergedDir, "overlay", 0, opts)
	}
	if err != nil {
		return "", fmt.Errorf("failed to mount overlay: %w", err)
	}

	// Verify the overlay mount has content
	entries, err := os.ReadDir(mergedDir)
	if err != nil {
		return "", fmt.Errorf("failed to read merged dir %s: %w", mergedDir, err)
	}
	logrus.Debugf("[imagefs] Overlay mounted successfully: %s has %d entries", mergedDir, len(entries))
	if len(entries) == 0 {
		return "", fmt.Errorf("overlay mount succeeded but merged directory %s is empty", mergedDir)
	}

	success = true
	return mergedDir, nil
}

func (d *Driver) canUseMergedLayers(layers []string) (bool, error) {
	// Get image paths for all layers
	imagePaths := make([]string, 0, len(layers))
	for _, layerID := range layers {
		path := d.getImagePath(layerID)
		if path == "" {
			return false, fmt.Errorf("no image file found for layer %s", layerID)
		}
		imagePaths = append(imagePaths, path)
	}

	// Ask the backend if it can merge these layers
	return d.backend.CanMergeLayers(imagePaths), nil
}

// mountFuseOverlay mounts an overlay filesystem using fuse-overlayfs.
func mountFuseOverlay(lowerdir, upperdir, workdir, target string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerdir, upperdir, workdir)
	logrus.Debugf("[imagefs] Mounting overlay with fuse-overlayfs: target=%s, options=%s", target, opts)
	cmd := exec.Command("fuse-overlayfs", "-o", opts, target)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fuse-overlayfs failed: %w: %s", err, stderr.String())
	}
	logrus.Debugf("[imagefs] fuse-overlayfs mount successful: %s", target)
	return nil
}

func (d *Driver) mountLayersSeparately(containerID string, layers []string, mountLabel string) ([]string, bool, error) {
	var lowerDirs []string
	var usedFuse bool
	for _, layerID := range layers {
		imagePath := d.getImagePath(layerID)
		if imagePath == "" {
			return nil, false, fmt.Errorf("no image file found for layer %s", layerID)
		}

		// For EROFS layers, we also need the .tar device file
		var devicePaths []string
		if filepath.Ext(imagePath) == ".erofs" {
			devicePath := imagePath + ".tar"
			if fileutils.Exists(devicePath) == nil {
				devicePaths = append(devicePaths, devicePath)
			}
		}

		isRoot := os.Getuid() == 0
		mountPoint, layerUsedFuse, err := d.mm.MountLayerWithDevices(containerID, layerID, imagePath, isRoot, devicePaths, mountLabel)
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

func (d *Driver) mountMergedLayers(containerID string, layers []string, mountLabel string) ([]string, bool, error) {
	var imagePaths []string
	var devicePaths []string
	for _, layerID := range layers {
		path := d.getImagePath(layerID)
		if path == "" {
			return nil, false, fmt.Errorf("no image file found for layer %s", layerID)
		}
		imagePaths = append(imagePaths, path)

		// The device path is the .tar file associated with each image (EROFS-specific)
		devicePath := path + ".tar"
		if fileutils.Exists(devicePath) != nil {
			return nil, false, fmt.Errorf("no tar device file found for layer %s at %s", layerID, devicePath)
		}
		devicePaths = append(devicePaths, devicePath)
	}

	rundir := d.mm.GetRundir(containerID)
	os.MkdirAll(rundir, 0o755)

	mergedImagePath := filepath.Join(rundir, "merged_layers"+d.backend.FileExtension())

	// Use the backend to merge the layers
	logrus.Debugf("[imagefs] Merging layers using %s backend", d.backend.Format())
	if err := d.backend.MergeLayers(imagePaths, devicePaths, mergedImagePath); err != nil {
		return nil, false, fmt.Errorf("failed to merge layers: %w", err)
	}

	// Mount the merged image
	isRoot := os.Getuid() == 0
	mountPoint, usedFuse, err := d.mm.MountLayerWithDevices(containerID, "merged-layers", mergedImagePath, isRoot, devicePaths, mountLabel)
	if err != nil {
		return nil, false, fmt.Errorf("failed to mount merged image: %w", err)
	}

	return []string{mountPoint}, usedFuse, nil
}

func (d *Driver) Put(id string) error {
	// Unmount merged dir (only if it exists - read-only mounts don't create it)
	mergedDir := filepath.Join(d.dir(id), "merged")
	if fileutils.Exists(mergedDir) == nil {
		logrus.Debugf("[imagefs] Unmounting overlay at %s", mergedDir)
		if err := d.unmountOverlay(mergedDir); err != nil {
			logrus.Errorf("failed to unmount merged dir %s: %v", mergedDir, err)
		}
	}

	// Unmount layers and cleanup rundir
	return d.mm.CleanupRundir(id)
}

// unmountOverlay unmounts an overlay filesystem, handling both kernel and FUSE overlays.
func (d *Driver) unmountOverlay(target string) error {
	// Try normal unmount first
	if err := d.mm.mounter.Unmount(target); err == nil {
		logrus.Debugf("[imagefs] Successfully unmounted overlay: %s", target)
		return nil
	}

	// If normal unmount fails, try fusermount -u for FUSE mounts
	logrus.Debugf("[imagefs] Normal unmount failed, trying fusermount -u for %s", target)
	cmd := exec.Command("fusermount", "-u", target)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fusermount -u failed: %w: %s", err, stderr.String())
	}
	logrus.Debugf("[imagefs] Successfully unmounted FUSE overlay with fusermount: %s", target)
	return nil
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
	// /home/.../storage/imagefs/<id>/layer.erofs or layer.sqfs
	img := filepath.Join(d.dir(id), "layer"+d.backend.FileExtension())
	if fileutils.Exists(img) == nil {
		return img
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
	status := [][2]string{
		{"driver", "imagefs"},
		{"format", d.backend.Format()},
	}

	// Show compression if specified
	compression := d.options.Compression
	if compression == "" {
		compression = "none"
	}
	status = append(status, [2]string{"compression", compression})

	// Add backend-specific status fields
	status = append(status, d.backend.StatusFields()...)

	return status
}

func (d *Driver) Metadata(id string) (map[string]string, error) {
	path := d.getImagePath(id)
	if path == "" {
		return nil, fmt.Errorf("no image or directory found for layer %s", id)
	}

	meta := make(map[string]string)
	meta["path"] = path
	meta["format"] = d.backend.Format()

	logrus.Debugf("[imagefs] Metadata for layer identified: %v", meta)
	return meta, nil
}

func (d *Driver) ReadWriteDiskUsage(id string) (*directory.DiskUsage, error) {
	return nil, fmt.Errorf("ReadWriteDiskUsage not implemented")
}

func (d *Driver) Cleanup() error {
	// Cleanup all FUSE mounts when storage is being shutdown (e.g., on Ctrl+C)
	logrus.Debugf("[imagefs] Cleaning up all mounts")

	// First, unmount all overlay mounts (merged directories)
	homeEntries, err := os.ReadDir(d.home)
	if err != nil && !os.IsNotExist(err) {
		logrus.Errorf("[imagefs] Failed to read home directory %s: %v", d.home, err)
	}
	if err == nil {
		for _, entry := range homeEntries {
			if entry.IsDir() {
				mergedDir := filepath.Join(d.home, entry.Name(), "merged")
				if fileutils.Exists(mergedDir) == nil {
					logrus.Debugf("[imagefs] Unmounting overlay at %s", mergedDir)
					_ = d.unmountOverlay(mergedDir)
				}
			}
		}
	}

	// Second, cleanup all FUSE layer mounts in rundir
	runRoot := filepath.Join(d.runRoot, "imagefs")
	runEntries, err := os.ReadDir(runRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read runRoot %s: %w", runRoot, err)
	}

	for _, entry := range runEntries {
		if entry.IsDir() {
			containerID := entry.Name()
			logrus.Debugf("[imagefs] Cleaning up FUSE mounts for container %s", containerID)
			if err := d.mm.CleanupRundir(containerID); err != nil {
				logrus.Errorf("[imagefs] Failed to cleanup rundir for container %s: %v", containerID, err)
			}
		}
	}

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
	// Check if this is a committed image layer or a working container layer
	imagePath := d.getImagePath(id)
	isCommittedLayer := imagePath != ""

	// For committed layers with no parent, try to get the diff from the backend
	if parent == "" && isCommittedLayer {
		if rc, err := d.backend.GetDiffForBaseLayer(imagePath); err != nil {
			return nil, err
		} else if rc != nil {
			return rc, nil
		}
		// If backend returns nil, fall through to naiveDiff
	}

	// For working container layers (no layer image), tar the upperdir directly.
	// This avoids the device/inode mismatch problem when naiveDiff creates
	// overlay mounts for both the layer and its parent.
	if !isCommittedLayer {
		upperdir := filepath.Join(d.dir(id), "upper")
		logrus.Debugf("[imagefs] Tarring upperdir for working layer %s: %s", id, upperdir)

		if idMappings == nil {
			idMappings = &idtools.IDMappings{}
		}

		return archive.TarWithOptions(upperdir, &archive.TarOptions{
			Compression:    archive.Uncompressed,
			UIDMaps:        idMappings.UIDs(),
			GIDMaps:        idMappings.GIDs(),
			WhiteoutFormat: archive.OverlayWhiteoutFormat,
		})
	}

	// For committed layers with parents, fall back to naive diff
	return d.naiveDiff.Diff(id, idMappings, parent, parentIDMappings, mountLabel)
}

func (d *Driver) Changes(id string, idMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, mountLabel string) ([]archive.Change, error) {
	return d.naiveDiff.Changes(id, idMappings, parent, parentIDMappings, mountLabel)
}

func (d *Driver) ApplyDiff(id string, options graphdriver.ApplyDiffOpts) (int64, error) {
	layerDir := d.dir(id)
	imagePath := filepath.Join(layerDir, "layer"+d.backend.FileExtension())

	var tarballPath string
	var size int64
	var err error

	if d.backend.ShouldPreserveTarball() {
		// Save the original tarball (EROFS needs this for --device= mounting)
		tarballPath = imagePath + ".tar"
		size, err = writeToFile(options.Diff, tarballPath)
		if err != nil {
			return 0, fmt.Errorf("failed to save tarball: %w", err)
		}
	} else {
		// Save to temporary file (SquashFS doesn't need persistent tarball)
		tmpDir := d.GetTempDirRootDirs()[0]
		td, err := tempdir.NewTempDir(tmpDir)
		if err != nil {
			return 0, fmt.Errorf("failed to create temp dir: %w", err)
		}
		defer td.Cleanup()

		sa, err := td.StageAddition()
		if err != nil {
			return 0, fmt.Errorf("failed to stage temp file: %w", err)
		}

		tarballPath = sa.Path
		size, err = writeToFile(options.Diff, tarballPath)
		if err != nil {
			return 0, fmt.Errorf("failed to write to temp file: %w", err)
		}
	}

	// Create the filesystem image using the backend
	if _, err := d.backend.CreateImage(tarballPath, imagePath); err != nil {
		return 0, fmt.Errorf("failed to create %s image: %w", d.backend.Format(), err)
	}

	logrus.Debugf("[imagefs] layer %s: %s created, size %d bytes", id, d.backend.Format(), size)
	return size, nil
}



func writeToFile(r io.Reader, dstPath string) (int64, error) {
	// Create (or truncate) the destination file with appropriate permissions.
	f, err := os.Create(dstPath)
	if err != nil {
		return 0, err
	}
	// Ensure the file is closed when we’re done.
	defer f.Close()

	// Copy the contents from the reader to the file and return size.
	size, err := io.Copy(f, r)
	return size, err
}

func (d *Driver) DiffSize(id string, idMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, mountLabel string) (int64, error) {
	return d.naiveDiff.DiffSize(id, idMappings, parent, parentIDMappings, mountLabel)
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
