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
	"sync"

	"github.com/opencontainers/selinux/go-selinux/label"
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
	home       string
	imageStore string // Additional image store directory
	runRoot    string
	options    Options
	backend    Backend
	mm         *MountManager
	syncMode   graphdriver.SyncMode
	naiveDiff  graphdriver.DiffDriver

	// Track active mounts for cleanup
	activeMountsMu sync.Mutex
	activeMounts   map[string]bool // containerID -> true
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
		home:         home,
		imageStore:   options.ImageStore,
		runRoot:      options.RunRoot,
		options:      *opts,
		backend:      backend,
		mm:           NewMountManager(options.RunRoot, nil),
		syncMode:     graphdriver.SyncModeNone, // Default to no sync
		activeMounts: make(map[string]bool),
	}
	d.naiveDiff = graphdriver.NewNaiveDiffDriver(d, graphdriver.NewNaiveLayerIDMapUpdater(d))

	// Clean up any orphaned FUSE processes from previous interrupted builds
	d.cleanupOrphanedProcesses()

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
	// Writable layers always go in the regular home directory
	return d.createLayer(id, parent, false)
}

func (d *Driver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	// Read-only image layers go in the image store if configured
	return d.createLayer(id, parent, true)
}

func (d *Driver) createLayer(id, parent string, useImageStore bool) error {
	var dir string
	if useImageStore {
		dir = d.dirForImageStore(id)
	} else {
		dir = d.dir(id)
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

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
		// New layer goes in image store if configured
		layerDir := d.dirForImageStore(id)
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
	// Find the layer in either location
	var dir string
	if d.imageStore != "" {
		imageStoreDir := d.dirForImageStore(id)
		if fileutils.Exists(imageStoreDir) == nil {
			dir = imageStoreDir
		}
	}
	if dir == "" {
		dir = d.dir(id)
	}

	return func() error {
		return os.RemoveAll(dir)
	}, nil
}

func (d *Driver) GetTempDirRootDirs() []string {
	tempDirs := []string{filepath.Join(d.home, "tmp")}
	// Include imageStore temp directory if it's configured
	// Writable layers can only be in d.home or d.imageStore, not in read-only additional image stores
	if d.imageStore != "" {
		tempDirs = append(tempDirs, filepath.Join(d.homeDirForImageStore(), "tmp"))
	}
	return tempDirs
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

		// Track this container as actively mounted
		d.activeMountsMu.Lock()
		d.activeMounts[containerID] = true
		d.activeMountsMu.Unlock()

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
		// Backend handles all mounting logic (with optimizations like EROFS merge if available)
		ctx := MountContext{
			LayerIDs:     layers,
			ContainerID:  containerID,
			MountLabel:   options.MountLabel,
			MountManager: d.mm,
			GetImagePath: d.getImagePath,
		}
		var layersUsedFuse bool
		lowerDirs, layersUsedFuse, err = d.backend.MountLayers(ctx)
		if err != nil {
			return "", err
		}

		if layersUsedFuse {
			anyLayerUsedFuse = true
		}

		// Lowerdir for OverlayFS is top-to-bottom (most recent first)
		// Backend returns bottom-to-top, so reverse for overlay
		for i, j := 0, len(lowerDirs)-1; i < j; i, j = i+1, j-1 {
			lowerDirs[i], lowerDirs[j] = lowerDirs[j], lowerDirs[i]
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
		err = mountFuseOverlay(lowerdirString, upperdir, workdir, mergedDir, options.MountLabel)
	} else {
		// All layers are kernel-mounted, use kernel overlayfs
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerdirString, upperdir, workdir)
		opts = label.FormatMountLabel(opts, options.MountLabel)
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

	// Track this container as actively mounted
	d.activeMountsMu.Lock()
	d.activeMounts[containerID] = true
	d.activeMountsMu.Unlock()

	return mergedDir, nil
}

// mountFuseOverlay mounts an overlay filesystem using fuse-overlayfs.
func mountFuseOverlay(lowerdir, upperdir, workdir, target, mountLabel string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerdir, upperdir, workdir)
	opts = label.FormatMountLabel(opts, mountLabel)
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
func (d *Driver) Put(id string) error {
	// Unmount merged dir (only if it exists - read-only mounts don't create it)
	mergedDir := filepath.Join(d.dir(id), "merged")
	if fileutils.Exists(mergedDir) == nil {
		logrus.Debugf("[imagefs] Unmounting overlay at %s", mergedDir)
		if err := d.unmountOverlay(mergedDir); err != nil {
			logrus.Errorf("[imagefs] Failed to unmount merged dir %s: %v", mergedDir, err)
		}
	}

	// Unmount layers and cleanup rundir
	logrus.Debugf("[imagefs] Cleaning up rundir for container %s", id)
	err := d.mm.CleanupRundir(id)

	// Remove from active mounts tracking
	d.activeMountsMu.Lock()
	delete(d.activeMounts, id)
	d.activeMountsMu.Unlock()

	return err
}

// unmountOverlay unmounts an overlay filesystem, handling both kernel and FUSE overlays.
func (d *Driver) unmountOverlay(target string) error {
	// Try normal unmount first (works for kernel overlay)
	err := d.mm.mounter.Unmount(target)
	if err == nil {
		logrus.Debugf("[imagefs] Successfully unmounted overlay: %s", target)
		return nil
	}

	logrus.Debugf("[imagefs] Normal unmount failed for %s, trying lazy unmount: %v", target, err)
	err = d.mm.mounter.LazyUnmount(target)
	if err == nil {
		logrus.Debugf("[imagefs] Successfully lazy unmounted overlay: %s", target)
		return nil
	}

	// If normal and lazy unmount fail, try fusermount -u for FUSE overlays
	logrus.Debugf("[imagefs] Lazy unmount failed for %s, trying fusermount -u: %v", target, err)
	cmd := exec.Command("fusermount", "-u", target)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to unmount overlay %s (tried unmount, lazy unmount, and fusermount): %w: %s", target, err, stderr.String())
	}
	logrus.Debugf("[imagefs] Successfully unmounted FUSE overlay with fusermount: %s", target)
	return nil
}

func (d *Driver) getLayerStack(id string) ([]string, error) {
	var stack []string
	current := id
	for current != "" {
		stack = append([]string{current}, stack...)

		// Try to read parent file from either location
		var parentFile string
		if d.imageStore != "" {
			imageStoreParent := filepath.Join(d.dirForImageStore(current), parentFileName)
			if fileutils.Exists(imageStoreParent) == nil {
				parentFile = imageStoreParent
			}
		}
		if parentFile == "" {
			parentFile = filepath.Join(d.dir(current), parentFileName)
		}

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
	// Check both the image store location and regular home location
	info := d.backend.Info()

	// Try image store location first (for read-only image layers)
	if d.imageStore != "" {
		img := filepath.Join(d.dirForImageStore(id), "layer"+info.FileExtension)
		if fileutils.Exists(img) == nil {
			return img
		}
	}

	// Try regular home location (for writable layers or when no image store is configured)
	img := filepath.Join(d.dir(id), "layer"+info.FileExtension)
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
	// Check both image store and regular home locations
	if d.imageStore != "" {
		if fileutils.Exists(d.dirForImageStore(id)) == nil {
			return true
		}
	}
	return fileutils.Exists(d.dir(id)) == nil
}

func (d *Driver) ListLayers() ([]string, error) {
	return nil, fmt.Errorf("ListLayers not implemented")
}

func (d *Driver) Status() [][2]string {
	info := d.backend.Info()
	status := [][2]string{
		{"driver", "imagefs"},
		{"format", info.Format},
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

	info := d.backend.Info()
	meta := make(map[string]string)
	meta["path"] = path
	meta["format"] = info.Format

	logrus.Debugf("[imagefs] Metadata for layer identified: %v", meta)
	return meta, nil
}

func (d *Driver) ReadWriteDiskUsage(id string) (*directory.DiskUsage, error) {
	return nil, fmt.Errorf("ReadWriteDiskUsage not implemented")
}

func (d *Driver) Cleanup() error {
	// Cleanup all FUSE mounts when storage is being shutdown
	// Note: During Ctrl+C, podman kills us before this runs, so orphaned processes
	// will be cleaned up on next podman invocation by cleanupOrphanedProcesses()
	logrus.Debugf("[imagefs] Starting cleanup of all active mounts")

	// Get list of containers that need cleanup (only those actively mounted)
	// Do this as fast as possible - no logging, no checks, just fire fusermount commands
	d.activeMountsMu.Lock()

	// Immediately fire all unmount commands while holding the lock (to prevent race)
	for containerID := range d.activeMounts {
		// Fire and forget - lazy unmount overlay (merged directory)
		mergedDir := filepath.Join(d.home, containerID, "merged")
		exec.Command("fusermount", "-uz", mergedDir).Start()

		// Fire and forget - lazy unmount all FUSE layer mounts in rundir
		rundir := d.mm.GetRundir(containerID)
		if entries, err := os.ReadDir(rundir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					path := filepath.Join(rundir, entry.Name())
					exec.Command("fusermount", "-uz", path).Start()
				}
			}
		}
	}

	// Clear the map
	clear(d.activeMounts)
	d.activeMountsMu.Unlock()

	logrus.Debugf("[imagefs] Cleanup complete, all unmount commands issued")

	return nil
}

// cleanupOrphanedProcesses cleans up any FUSE processes left over from previous interrupted builds.
// Called during Init() to ensure clean state.
func (d *Driver) cleanupOrphanedProcesses() {
	logrus.Debugf("[imagefs] Checking for orphaned FUSE mounts from previous builds...")

	// Clean up any layer mounts in runRoot/imagefs
	runRoot := filepath.Join(d.runRoot, "imagefs")
	if runEntries, err := os.ReadDir(runRoot); err == nil {
		for _, containerEntry := range runEntries {
			if containerEntry.IsDir() {
				containerDir := filepath.Join(runRoot, containerEntry.Name())
				if layerEntries, err := os.ReadDir(containerDir); err == nil {
					for _, layerEntry := range layerEntries {
						if layerEntry.IsDir() {
							layerPath := filepath.Join(containerDir, layerEntry.Name())
							// Try to unmount - if it's not mounted, this will fail silently
							exec.Command("fusermount", "-uz", layerPath).Run()
						}
					}
				}
				// Try to remove the container directory
				os.RemoveAll(containerDir)
			}
		}
	}

	// Clean up any overlay mounts in home/*/merged
	homes := []string{d.home}
	if d.imageStore != "" {
		homes = append(homes, d.homeDirForImageStore())
	}
	// Also check additional read-only image stores
	for _, store := range d.options.imageStores {
		homes = append(homes, filepath.Join(store, "imagefs"))
	}

	for _, home := range homes {
		if homeEntries, err := os.ReadDir(home); err == nil {
			for _, entry := range homeEntries {
				if entry.IsDir() {
					mergedDir := filepath.Join(home, entry.Name(), "merged")
					// Try to unmount - if it's not mounted, this will fail silently
					exec.Command("fusermount", "-uz", mergedDir).Run()
				}
			}
		}
	}

	logrus.Debugf("[imagefs] Orphaned FUSE mount cleanup complete")
}

func (d *Driver) AdditionalImageStores() []string {
	return d.options.imageStores
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
		rc, err := d.backend.DiffForBaseLayer(imagePath)
		if err == nil {
			return rc, nil
		}
		// If backend returns ErrNotSupported, fall through to other methods
		if err != ErrNotSupported {
			return nil, err
		}
	}

	// For working container layers (no layer image), tar the upperdir directly.
	// This avoids the device/inode mismatch problem when naiveDiff creates
	// overlay mounts for both the layer and its parent.
	if !isCommittedLayer {
		// Find the layer directory (should be in regular home for writable layers)
		var layerDir string
		if d.imageStore != "" {
			imageStoreDir := d.dirForImageStore(id)
			if fileutils.Exists(imageStoreDir) == nil {
				layerDir = imageStoreDir
			}
		}
		if layerDir == "" {
			layerDir = d.dir(id)
		}

		upperdir := filepath.Join(layerDir, "upper")
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
	info := d.backend.Info()

	// Find the correct layer directory (could be in image store or regular home)
	var layerDir string
	if d.imageStore != "" {
		imageStoreDir := d.dirForImageStore(id)
		if fileutils.Exists(imageStoreDir) == nil {
			layerDir = imageStoreDir
		}
	}
	if layerDir == "" {
		layerDir = d.dir(id)
	}

	imagePath := filepath.Join(layerDir, "layer"+info.FileExtension)

	var tarballPath string
	var size int64
	var err error

	if info.PreserveTarball {
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
		return 0, fmt.Errorf("failed to create %s image: %w", info.Format, err)
	}

	// Clean up upperdir and workdir since this layer is now committed.
	// Any previous content in upperdir is obsolete - the committed content is in the EROFS image.
	upperdir := filepath.Join(layerDir, "upper")
	workdir := filepath.Join(layerDir, "work")
	if fileutils.Exists(upperdir) == nil {
		if err := os.RemoveAll(upperdir); err != nil {
			logrus.Warnf("[imagefs] Failed to clean upperdir for committed layer %s: %v", id, err)
		}
	}
	if fileutils.Exists(workdir) == nil {
		if err := os.RemoveAll(workdir); err != nil {
			logrus.Warnf("[imagefs] Failed to clean workdir for committed layer %s: %v", id, err)
		}
	}

	logrus.Debugf("[imagefs] layer %s: %s created, size %d bytes", id, info.Format, size)
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

// homeDirForImageStore returns the home directory to use when an image store is configured.
// This is used for read-only image layers.
func (d *Driver) homeDirForImageStore() string {
	if d.imageStore != "" {
		return filepath.Join(d.imageStore, "imagefs")
	}
	// If there is not an image store configured, use the same store
	return d.home
}

// dirForImageStore returns the directory for a layer that should be in the image store.
// It checks the writable image store, then additional read-only stores, then the regular home location.
func (d *Driver) dirForImageStore(id string) string {
	// First check the writable image store (if configured)
	if d.imageStore != "" {
		homedir := d.homeDirForImageStore()
		newpath := filepath.Join(homedir, id)
		if fileutils.Exists(newpath) == nil {
			return newpath
		}
	}

	// Then check additional read-only image stores
	for _, store := range d.options.imageStores {
		storePath := filepath.Join(store, "imagefs", id)
		if fileutils.Exists(storePath) == nil {
			return storePath
		}
	}

	// Finally check the regular home location (for migration/compatibility)
	regularPath := filepath.Join(d.home, id)
	if fileutils.Exists(regularPath) == nil {
		return regularPath
	}

	// Default to writable image store location for new layers (if configured)
	if d.imageStore != "" {
		return filepath.Join(d.homeDirForImageStore(), id)
	}

	// Otherwise default to regular home
	return regularPath
}
