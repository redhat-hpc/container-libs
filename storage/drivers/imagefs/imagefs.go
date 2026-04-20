package imagefs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
	graphdriver "go.podman.io/storage/drivers"
	"go.podman.io/storage/internal/tempdir"
	"go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/directory"
	"go.podman.io/storage/pkg/fileutils"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/parsers/kernel"
)

const parentFileName = "parent"

type Driver struct {
	home     string
	runRoot  string
	options  Options
	mm       *MountManager
	naiveDiff graphdriver.DiffDriver
}

func Init(home string, options graphdriver.Options) (graphdriver.Driver, error) {
	opts, err := parseOptions(options.DriverOptions)
	if err != nil {
		return nil, err
	}

	d := &Driver{
		home:     home,
		runRoot:  options.RunRoot,
		options:  *opts,
		mm:       NewMountManager(options.RunRoot, nil),
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

	// Separate the requested layer (id) from its parent layers.
	// The parent layers (everything below id) are always mounted as EROFS lowerdirs.
	if len(layers) > 0 {
		layers = layers[:len(layers)-1]
	}

	// Check if the requested layer itself has a committed image (data.img).
	// If it does, it's a committed image layer and its own EROFS image must be
	// included as the topmost lowerdir so that its content is visible in the
	// merged view. If it doesn't have a data.img, it's a working container
	// layer whose writes are captured by the overlay upperdir.
	idImagePath := d.getImagePath(id)
	isCommittedLayer := idImagePath != ""

	// Mount parent layers as EROFS lowerdirs.
	if len(layers) > 0 {
		// Determine strategy based on whether all layers are EROFS
		useMerged, err := d.canUseMergedErofs(layers)
		if err != nil {
			return "", err
		}

		if useMerged {
			logrus.Debugf("[imagefs] Using merged EROFS strategy for container %s", containerID)
			lowerDirs, err = d.mountErofsMerged(containerID, layers)
		} else {
			logrus.Debugf("[imagefs] Using separate layers strategy for container %s", containerID)
			lowerDirs, err = d.mountLayersSeparately(containerID, layers)
		}

		if err != nil {
			return "", err
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
		if filepath.Ext(idImagePath) == ".img" {
			devicePath := idImagePath + ".tar"
			if fileutils.Exists(devicePath) == nil {
				devicePaths = append(devicePaths, devicePath)
			}
		}

		isRoot := os.Getuid() == 0
		mountPoint, err := d.mm.MountLayerWithDevices(containerID, id, idImagePath, isRoot, devicePaths)
		if err != nil {
			return "", fmt.Errorf("failed to mount layer %s: %w", id, err)
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

func (d *Driver) canUseMergedErofs(layers []string) (bool, error) {
	for _, layerID := range layers {
		path := d.getImagePath(layerID)
		if !kernel.CheckKernelVersion(5, 14, 0) {
			return false, nil
		}
		if path == "" {
			return false, fmt.Errorf("no image file found for layer %s", layerID)
		}
		if filepath.Ext(path) != ".img" {
			return false, nil // At least one layer is not EROFS
		}
		if len(layers) == 1 {
			return false, nil
		}
	}
	return true, nil
}

func (d *Driver) mountLayersSeparately(containerID string, layers []string) ([]string, error) {
	var lowerDirs []string
	for _, layerID := range layers {
		imagePath := d.getImagePath(layerID)
		if imagePath == "" {
			return nil, fmt.Errorf("no image file found for layer %s", layerID)
		}

		// For EROFS layers, we also need the .tar device file
		var devicePaths []string
		if filepath.Ext(imagePath) == ".img" {
			devicePath := imagePath + ".tar"
			if fileutils.Exists(devicePath) == nil {
				devicePaths = append(devicePaths, devicePath)
			}
		}

		isRoot := os.Getuid() == 0
		mountPoint, err := d.mm.MountLayerWithDevices(containerID, layerID, imagePath, isRoot, devicePaths)
		if err != nil {
			return nil, fmt.Errorf("failed to mount layer %s: %w", layerID, err)
		}
		lowerDirs = append(lowerDirs, mountPoint)
	}
	return lowerDirs, nil
}

func (d *Driver) mountErofsMerged(containerID string, layers []string) ([]string, error) {
	var imagePaths []string
	var devicePaths []string
	for _, layerID := range layers {
		path := d.getImagePath(layerID)
		if path == "" {
			return nil, fmt.Errorf("no image file found for layer %s", layerID)
		}
		imagePaths = append(imagePaths, path)

		// The device path is the .tar file associated with each image
		devicePath := path + ".tar"
		if fileutils.Exists(devicePath) != nil {
			return nil, fmt.Errorf("no tar device file found for layer %s at %s", layerID, devicePath)
		}
		devicePaths = append(devicePaths, devicePath)
	}

	rundir := d.mm.GetRundir(containerID)
	os.MkdirAll(rundir, 0o755)

	mergedImagePath := filepath.Join(rundir, "merged_layers.img")

	// mkfs.erofs <dest> <src1> <src2> ...
	args := append([]string{mergedImagePath}, imagePaths...)
	if err := d.mm.mounter.RunCommand("mkfs.erofs", args...); err != nil {
		return nil, fmt.Errorf("failed to merge EROFS layers: %w", err)
	}

	isRoot := os.Getuid() == 0
	mountPoint, err := d.mm.MountLayerWithDevices(containerID, "merged-layers", mergedImagePath, isRoot, devicePaths)
	if err != nil {
		return nil, fmt.Errorf("failed to mount merged EROFS image: %w", err)
	}

	return []string{mountPoint}, nil
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

func (d *Driver) getMkfsErofsVersion() string {
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

func (d *Driver) Status() [][2]string {
	return [][2]string{
		{"driver", "imagefs"},
		{"erofs-utils", d.getMkfsErofsVersion()},
	}
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
	return d.naiveDiff.Diff(id, idMappings, parent, parentIDMappings, mountLabel)
}

func (d *Driver) Changes(id string, idMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, mountLabel string) ([]archive.Change, error) {
	return d.naiveDiff.Changes(id, idMappings, parent, parentIDMappings, mountLabel)
}

func (d *Driver) ApplyDiff(id string, options graphdriver.ApplyDiffOpts) (int64, error) {
	// To properly handle file deletions in EROFS images, we need to extract
	// the diff tarball to a temporary directory first. This ensures that 
	// deletion operations (represented as whiteout files in the tar) are
	// properly preserved in the final EROFS image.
	layerDir := d.dir(id)
	tempDir, err := os.MkdirTemp(layerDir, "diff-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tempDir)

	// Extract the diff tarball to temp directory to properly handle deletions
	// This ensures that whiteout files (representing deletions) are correctly 
	// processed and that deleted files don't appear in the final EROFS image
	tarOptions := &archive.TarOptions{
		// Use overlay whiteout format to ensure proper deletion handling
		WhiteoutFormat: archive.OverlayWhiteoutFormat,
	}
	
	// Set ID mappings if provided
	if options.Mappings != nil {
		tarOptions.UIDMaps = options.Mappings.UIDs()
		tarOptions.GIDMaps = options.Mappings.GIDs()
	}
	
	if err := archive.Untar(options.Diff, tempDir, tarOptions); err != nil {
		return 0, err
	}

	// Create image file from the properly extracted directory structure
	// This preserves the correct filesystem state including deletions
	imagePath := filepath.Join(layerDir, "data.img")
	
	// Create a tar from the directory and pass it to mkfs.erofs for image creation
	// This preserves the directory structure with deletions properly represented
	archive, err := archive.TarWithOptions(tempDir, &archive.TarOptions{
		Compression: archive.Uncompressed,
	})
	if err != nil {
		return 0, err
	}
	defer archive.Close()
	
	if err := d.runMkfsErofs(archive, imagePath); err != nil {
		return 0, fmt.Errorf("failed to create image file %s: %w", imagePath, err)
	}

	info, err := os.Stat(imagePath)
	if err != nil {
		return 0, fmt.Errorf("failed to stat resulting image file: %w", err)
	}

	logrus.Debugf("[imagefs] layer %s wrote %d bytes", imagePath, info.Size())
	return info.Size(), nil
}

func (d *Driver) runMkfsErofs(r io.Reader, dest string) error {
	// Write the tar stream to a file so mkfs.erofs can use it as both the
	// filesystem source (--tar=i) and the backing data device (.tar file).
	tarball := dest + ".tar"
	if err := writeToFile(r, tarball); err != nil {
		return fmt.Errorf("failed to write tarball %s: %w", tarball, err)
	}

	cmd := exec.Command("mkfs.erofs", "--tar=i", "-E", "legacy-compress", dest, tarball)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	logrus.Debugf("[imagefs] Creating the layer: %v", cmd.Args)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.erofs failed: %w: %s", err, stderr.String())
	}
	return nil
}

func writeToFile(r io.Reader, dstPath string) error {
	// Create (or truncate) the destination file with appropriate permissions.
	f, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	// Ensure the file is closed when we’re done.
	defer f.Close()

	// Copy the contents from the reader to the file.
	_, err = io.Copy(f, r)
	return err
}

// func (d *Driver) runMkfsErofsWithExtraction(r io.Reader, dest string) error {
// 	// Create a temporary directory for extraction
// 	rootDir := d.GetTempDirRootDirs()[0]
// 	td, err := tempdir.NewTempDir(rootDir)
// 	if err != nil {
// 		return fmt.Errorf("failed to create temp dir for extraction: %w", err)
// 	}
// 	defer td.Cleanup()

// 	// Extract the tarball stream into the temporary directory
// 	if err := archive.Untar(r, td.Path(), &archive.TarOptions{}); err != nil {
// 		return fmt.Errorf("failed to extract layer to temp dir: %w", err)
// 	}

// 	// Run mkfs.erofs on the extracted directory
// 	// mkfs.erofs <dest_image> <source_dir>
// 	cmd := exec.Command("mkfs.erofs", dest, td.Path())
// 	var stderr bytes.Buffer
// 	cmd.Stderr = &stderr

// 	logrus.Debugf("[imagefs] Creating the layer from directory: %v", cmd.Args)
// 	if err := cmd.Run(); err != nil {
// 		return fmt.Errorf("mkfs.erofs failed: %w: %s", err, stderr.String())
// 	}

// 	return nil
// }

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
