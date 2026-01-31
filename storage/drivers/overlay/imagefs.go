//go:build linux

package overlay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/sirupsen/logrus"
	graphdriver "go.podman.io/storage/drivers"
	"go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/fileutils"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/loopback"
	"go.podman.io/storage/pkg/unshare"
	"golang.org/x/sys/unix"
)

// stagingLayerTarballName is the filename used in the staging directory for the
// erofs tarball when applying a diff without untarring (staging path only).
const stagingLayerTarballName = "layer.tar"

// validateImageFSConfig validates imagefs configuration and checks for required tools.
func validateImageFSConfig(opts *overlayOptions) error {
	if opts.imageFSType == "" {
		return nil
	}

	// If create command is not specified, check if default tool exists
	if opts.imageFSCreateCommand == "" {
		var toolName string
		switch opts.imageFSType {
		case "erofs":
			toolName = "mkfs.erofs"
		case "squashfs":
			toolName = "mksquashfs"
		default:
			return fmt.Errorf("image_fs_type %q requires an image_fs_create_command to be set", opts.imageFSType)
		}

		// Check if the default tool exists
		if _, err := exec.LookPath(toolName); err != nil {
			return fmt.Errorf("image_fs_type %q requires %s to be available (or set image_fs_create_command)", opts.imageFSType, toolName)
		}
		logrus.Debugf("overlay: validated default tool %q for image_fs_type %q", toolName, opts.imageFSType)
	}

	return nil
}

func (d *Driver) maybeAddImageFSMount(id, dir, lowerID string, i int, readWrite, inAdditionalStore bool) (string, error) {
	logrus.Debugf("overlay: maybeAddImageFSMount called for id=%q, dir=%q, lowerID=%q, i=%d, readWrite=%v", id, dir, lowerID, i, readWrite)
	if d.options.imageFSType == "" {
		logrus.Debugf("overlay: imageFSType is empty, skipping imagefs mount")
		return "", nil
	}
	// Image filesystems are always read-only. If this is the current layer (i == 0) and writeable
	// is requested, we can still mount it but it will be used as a lower layer with an empty upper.
	// The check for readWrite && i == 0 is handled in the caller.
	logrus.Debugf("overlay: using %s blob for lower %s", d.options.imageFSType, lowerID)
	imageBlob := d.getImageFSData(lowerID)
	logrus.Debugf("overlay: computed image blob path: %q", imageBlob)
	if err := fileutils.Exists(imageBlob); err != nil {
		if os.IsNotExist(err) {
			logrus.Debugf("overlay: image blob %q does not exist, skipping mount", imageBlob)
			return "", nil
		}
		return "", err
	}
	// Always store imagefs mount points in rundir to ensure they're always in a runtime directory
	dest := path.Join(d.runhome, id, fmt.Sprintf("imagefs-layers/%d", i))
	logrus.Debugf("overlay: computed mount destination: %q", dest)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	if err := d.mountImageFSBlob(imageBlob, dest, d.options.imageFSType); err != nil {
		return "", err
	}
	logrus.Debugf("overlay: successfully mounted %s image %q to %q", d.options.imageFSType, imageBlob, dest)
	return dest, nil
}

// unmountImageFSMounts unmounts all imagefs mounts for a given id.
// It handles both rootful (unix.Unmount) and rootless/FUSE (fusermount) unmounts.
func (d *Driver) unmountImageFSMounts(id string) error {
	if d.options.imageFSType == "" {
		return nil
	}

	imagefsLayersDir := path.Join(d.runhome, id, "imagefs-layers")
	if err := fileutils.Exists(imagefsLayersDir); err != nil {
		if os.IsNotExist(err) {
			return nil // No imagefs mounts to unmount
		}
		return err
	}

	entries, err := os.ReadDir(imagefsLayersDir)
	if err != nil {
		return fmt.Errorf("reading imagefs-layers directory: %w", err)
	}

	var unmountErrors []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		mountpoint := path.Join(imagefsLayersDir, entry.Name())
		logrus.Debugf("overlay: unmounting imagefs mount at %q", mountpoint)

		unmounted := false
		// Try FUSE unmount first (for rootless mounts)
		if unshare.IsRootless() {
			unmounted = tryFUSEUnmount(mountpoint)
		}

		// Fallback to regular unmount (for rootful or if FUSE unmount failed)
		if !unmounted {
			if err := unix.Unmount(mountpoint, unix.MNT_DETACH); err != nil {
				if !errors.Is(err, unix.EINVAL) && !os.IsNotExist(err) {
					unmountErrors = append(unmountErrors, fmt.Errorf("unmounting %q: %w", mountpoint, err))
					logrus.Errorf("Failed to unmount imagefs mount %s: %v", mountpoint, err)
				}
			}
		}
	}

	if len(unmountErrors) > 0 {
		return fmt.Errorf("errors unmounting imagefs mounts: %w", errors.Join(unmountErrors...))
	}

	// Clean up the imagefs-layers directory after unmounting
	d.cleanupImageFSMounts(id)

	return nil
}

func (d *Driver) getImageFSData(id string) string {
	dir := d.dir(id)
	// Use a generic name based on the filesystem type, or default to "image.img"
	imageName := fmt.Sprintf("%s.img", d.options.imageFSType)
	if imageName == ".img" {
		imageName = "image.img"
	}
	return path.Join(dir, imageName)
}

// createImageFSFromDirectory creates an image filesystem from the contents of a source directory.
// It uses the image_fs_create_command template with ImagePath and TmpDir variables.
func (d *Driver) createImageFSFromDirectory(layerID, sourceDir, context string) error {
	if d.options.imageFSType == "" {
		return nil
	}

	// Use default command if not specified (for erofs and squashfs)
	createCommand := d.options.imageFSCreateCommand
	if createCommand == "" {
		switch d.options.imageFSType {
		case "erofs":
			createCommand = "mkfs.erofs -z lz4 {{.ImagePath}} {{.TmpDir}}"
			logrus.Debugf("overlay: %s: using default imagefs create command for erofs: %q", context, createCommand)
		case "squashfs":
			createCommand = "mksquashfs {{.TmpDir}} {{.ImagePath}}"
			logrus.Debugf("overlay: %s: using default imagefs create command for squashfs: %q", context, createCommand)
		default:
			return fmt.Errorf("image_fs_type %q requires an image_fs_create_command to be set", d.options.imageFSType)
		}
	}

	imagePath := d.getImageFSData(layerID)
	imageDir := path.Dir(imagePath)
	logrus.Debugf("overlay: %s: creating imagefs, imagePath=%q, imageDir=%q, sourceDir=%q", context, imagePath, imageDir, sourceDir)

	// Ensure the directory for the image file exists
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("creating directory for image file: %w", err)
	}

	// Construct and execute the image creation command using template expansion
	tmpl, err := template.New("image_fs_create_command").Parse(createCommand)
	if err != nil {
		return fmt.Errorf("parsing image_fs_create_command template: %w", err)
	}

	var cmdBuf bytes.Buffer
	templateData := struct {
		ImagePath string
		TmpDir    string
	}{
		ImagePath: imagePath,
		TmpDir:    sourceDir,
	}
	if err = tmpl.Execute(&cmdBuf, templateData); err != nil {
		return fmt.Errorf("executing image_fs_create_command template: %w", err)
	}

	// Parse the expanded command into arguments
	expandedCmd := strings.TrimSpace(cmdBuf.String())
	args := strings.Fields(expandedCmd)
	if len(args) == 0 {
		return fmt.Errorf("invalid image_fs_create_command: template expanded to empty command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err = cmd.Run(); err != nil {
		return fmt.Errorf("executing image create command %q: %s %w", expandedCmd, stderrBuf.String(), err)
	}

	logrus.Debugf("overlay: %s: successfully created image file at %q", context, imagePath)
	// Verify the image file was created
	stat, err := os.Stat(imagePath)
	if err != nil {
		return fmt.Errorf("image file was not created at %q: %w", imagePath, err)
	}
	logrus.Debugf("overlay: %s: verified image file exists, size=%d", context, stat.Size())
	return nil
}

// createImageFSFromTarball creates an image filesystem directly from a tarball.
// Used for erofs (and other types that support tar input) to avoid untarring.
// The template supports ImagePath and Tarball variables.
func (d *Driver) createImageFSFromTarball(layerID, tarballPath, context string) error {
	if d.options.imageFSType == "" {
		return nil
	}

	// For erofs, use a tarball-specific default (mkfs.erofs --tar=f accepts a tarball directly).
	// image_fs_create_command is for directory input; from tarball we use this default.
	var createCommand string
	switch d.options.imageFSType {
	case "erofs":
		createCommand = "mkfs.erofs --tar=f -z lz4 {{.ImagePath}} {{.Tarball}}"
		logrus.Debugf("overlay: %s: using default imagefs create command for erofs from tarball: %q", context, createCommand)
	default:
		return fmt.Errorf("createImageFSFromTarball: image_fs_type %q does not support tarball input (only erofs)", d.options.imageFSType)
	}

	imagePath := d.getImageFSData(layerID)
	imageDir := path.Dir(imagePath)
	logrus.Debugf("overlay: %s: creating imagefs from tarball, imagePath=%q, tarball=%q", context, imagePath, tarballPath)

	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return fmt.Errorf("creating directory for image file: %w", err)
	}

	tmpl, err := template.New("image_fs_create_command_tarball").Parse(createCommand)
	if err != nil {
		return fmt.Errorf("parsing image_fs_create_command template: %w", err)
	}

	var cmdBuf bytes.Buffer
	templateData := struct {
		ImagePath string
		Tarball   string
	}{
		ImagePath: imagePath,
		Tarball:   tarballPath,
	}
	if err = tmpl.Execute(&cmdBuf, templateData); err != nil {
		return fmt.Errorf("executing image_fs_create_command template: %w", err)
	}

	expandedCmd := strings.TrimSpace(cmdBuf.String())
	args := strings.Fields(expandedCmd)
	if len(args) == 0 {
		return fmt.Errorf("invalid image_fs_create_command: template expanded to empty command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err = cmd.Run(); err != nil {
		return fmt.Errorf("executing image create command %q: %s %w", expandedCmd, stderrBuf.String(), err)
	}

	logrus.Debugf("overlay: %s: successfully created image file at %q from tarball", context, imagePath)
	stat, err := os.Stat(imagePath)
	if err != nil {
		return fmt.Errorf("image file was not created at %q: %w", imagePath, err)
	}
	logrus.Debugf("overlay: %s: verified image file exists, size=%d", context, stat.Size())
	return nil
}

// applyDiffForImageFS handles applying a diff when image_fs_type is set.
// It returns (size, true, nil) when it handled the diff (erofs tarball path or created image).
// It returns (0, false, nil) when the caller should fall back to default untar (e.g. staging + squashfs).
func (d *Driver) applyDiffForImageFS(target string, options graphdriver.ApplyDiffOpts) (size int64, handled bool, err error) {
	if d.options.imageFSType == "" {
		return 0, false, nil
	}

	isStagingDir := false
	if targetAbs, absErr := filepath.Abs(target); absErr == nil {
		for _, tempRoot := range d.GetTempDirRootDirs() {
			if strings.HasPrefix(targetAbs, tempRoot) {
				isStagingDir = true
				break
			}
		}
	}

	if isStagingDir {
		if d.options.imageFSType == "erofs" {
			tarballPath := filepath.Join(target, stagingLayerTarballName)
			f, err := os.Create(tarballPath)
			if err != nil {
				return 0, false, fmt.Errorf("creating staging tarball for erofs: %w", err)
			}
			n, err := io.Copy(f, options.Diff)
			if err != nil {
				_ = f.Close()
				_ = os.Remove(tarballPath)
				return 0, false, fmt.Errorf("writing staging tarball for erofs: %w", err)
			}
			if err := f.Close(); err != nil {
				return 0, false, fmt.Errorf("closing staging tarball: %w", err)
			}
			logrus.Debugf("overlay: applyDiffForImageFS: wrote erofs tarball to staging %q (%d bytes)", tarballPath, n)
			return n, true, nil
		}
		return 0, false, nil
	}

	// Direct apply: we have the layer ID. Create the image here.
	layerDir := path.Dir(target)
	layerID := path.Base(layerDir)

	if d.options.imageFSType == "erofs" {
		tarballFile, err := os.CreateTemp(d.home, fmt.Sprintf("%s-apply-diff-*.tar", d.options.imageFSType))
		if err != nil {
			return 0, false, fmt.Errorf("creating temporary tarball for %s diff: %w", d.options.imageFSType, err)
		}
		tarballPath := tarballFile.Name()
		defer os.Remove(tarballPath)
		if _, err := io.Copy(tarballFile, options.Diff); err != nil {
			_ = tarballFile.Close()
			return 0, false, fmt.Errorf("writing diff to temporary tarball for %s: %w", d.options.imageFSType, err)
		}
		if err := tarballFile.Close(); err != nil {
			return 0, false, fmt.Errorf("closing temporary tarball: %w", err)
		}
		if err := d.createImageFSFromTarball(layerID, tarballPath, "applyDiff"); err != nil {
			return 0, false, err
		}
		imagePath := d.getImageFSData(layerID)
		stat, err := os.Stat(imagePath)
		if err != nil {
			return 0, false, fmt.Errorf("getting size of %s image %q: %w", d.options.imageFSType, imagePath, err)
		}
		return stat.Size(), true, nil
	}

	// squashfs etc.: untar into tmp dir, create image from directory
	tmpDir, err := os.MkdirTemp(d.home, fmt.Sprintf("%s-apply-diff-", d.options.imageFSType))
	if err != nil {
		return 0, false, fmt.Errorf("creating temporary directory for %s diff: %w", d.options.imageFSType, err)
	}
	defer os.RemoveAll(tmpDir)
	var uidMaps, gidMaps []idtools.IDMap
	if options.Mappings != nil {
		uidMaps = options.Mappings.UIDs()
		gidMaps = options.Mappings.GIDs()
	}
	if _, err = archive.ApplyUncompressedLayer(tmpDir, options.Diff, &archive.TarOptions{
		UIDMaps:           uidMaps,
		GIDMaps:           gidMaps,
		IgnoreChownErrors: options.IgnoreChownErrors,
		WhiteoutFormat:    archive.OverlayWhiteoutFormat,
	}); err != nil {
		return 0, false, fmt.Errorf("untarring diff to temporary directory for %s: %w", d.options.imageFSType, err)
	}
	if err := d.createImageFSFromDirectory(layerID, tmpDir, "applyDiff"); err != nil {
		return 0, false, err
	}
	imagePath := d.getImageFSData(layerID)
	stat, err := os.Stat(imagePath)
	if err != nil {
		return 0, false, fmt.Errorf("getting size of %s image %q: %w", d.options.imageFSType, imagePath, err)
	}
	return stat.Size(), true, nil
}

// commitStagedLayerForImageFS creates the image filesystem from the staging directory (or tarball)
// and removes the staging directory. Caller must ensure d.options.imageFSType != "".
func (d *Driver) commitStagedLayerForImageFS(id, stagingPath, applyDir string) error {
	tarballPath := filepath.Join(stagingPath, stagingLayerTarballName)
	if d.options.imageFSType == "erofs" {
		if _, err := os.Stat(tarballPath); err == nil {
			if err := d.createImageFSFromTarball(id, tarballPath, "CommitStagedLayer"); err != nil {
				return err
			}
		} else {
			if err := d.createImageFSFromDirectory(id, stagingPath, "CommitStagedLayer"); err != nil {
				return err
			}
		}
	} else {
		if err := d.createImageFSFromDirectory(id, stagingPath, "CommitStagedLayer"); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(stagingPath); err != nil {
		return fmt.Errorf("removing staging directory after image creation: %w", err)
	}
	if err := os.MkdirAll(applyDir, 0o755); err != nil {
		return fmt.Errorf("creating diff directory: %w", err)
	}
	return nil
}

func (d *Driver) mountImageFSBlob(imageBlob, dest, fsType string) error {
	logrus.Debugf("overlay: mounting %s image blob %q to %q", fsType, imageBlob, dest)
	// If rootless, try to find a FUSE mount program for this filesystem type
	if unshare.IsRootless() {
		// Try to find a FUSE mount program for this filesystem type
		// Common ones: fuse2fs (ext2/3/4), erofsfuse, squashfuse (squashfs)
		fusePrograms := map[string]string{
			"erofs":    "erofsfuse",
			"squashfs": "squashfuse",
			"ext2":     "fuse2fs",
			"ext3":     "fuse2fs",
			"ext4":     "fuse2fs",
		}
		if fuseProg, ok := fusePrograms[fsType]; ok {
			if path, err := exec.LookPath(fuseProg); err == nil {
				return d.mountImageFSWithProgram(imageBlob, dest, fsType, path)
			}
		}
		// Fall through to try new mount API (works on newer kernels for rootless)
	}

	// Try new mount API first (works for rootful and newer rootless kernels)
	err := d.mountImageFSWithNewAPI(imageBlob, dest, fsType)
	if err == nil {
		return nil
	}

	// If new mount API fails, try with loop device (for rootful or if direct mount doesn't work)
	if !unshare.IsRootless() {
		// Try new mount API with loop device
		if err2 := d.mountImageFSWithNewAPIAndLoop(imageBlob, dest, fsType); err2 == nil {
			return nil
		}
		// If that also fails and we're rootful, fall back to traditional mount
	} else {
		// For rootless, if new mount API failed, we need FUSE or mount program
		if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("failed to mount %s image with new mount API: %w", fsType, err)
		}
	}

	// Fall back to loop device + mount (rootful only, or if new API fails)
	if unshare.IsRootless() {
		return fmt.Errorf("rootless mount of %s requires a FUSE mount program (e.g., erofsfuse, squashfuse) to be available", fsType)
	}
	return d.mountImageFSWithLoop(imageBlob, dest, fsType)
}

// cleanupImageFSMounts removes the imagefs-layers directory from rundir after unmounting.
func (d *Driver) cleanupImageFSMounts(id string) {
	if d.options.imageFSType == "" {
		return
	}

	imagefsLayersDir := path.Join(d.runhome, id, "imagefs-layers")
	if err := os.RemoveAll(imagefsLayersDir); err != nil && !os.IsNotExist(err) {
		logrus.Debugf("Failed to remove imagefs-layers directory %q: %v", imagefsLayersDir, err)
	}
}

func (d *Driver) mountImageFSWithProgram(imageBlob, dest, fsType, mountProg string) error {
	logrus.Debugf("overlay: mounting %s image %q to %q using mount program %q", fsType, imageBlob, dest, mountProg)
	// The mount program should accept the image file and mount point
	// Format: <mount_program> <image_file> <mount_point>
	cmd := exec.Command(mountProg, imageBlob, dest)
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to mount %s image with %s: %s %w", fsType, mountProg, stderrBuf.String(), err)
	}
	return nil
}

func (d *Driver) mountImageFSWithNewAPI(imageBlob, dest, fsType string) error {
	logrus.Debugf("overlay: mounting %s image %q to %q using new mount API (direct)", fsType, imageBlob, dest)
	// Try mounting directly from file first (Linux 6.12+)
	fsfd, err := unix.Fsopen(fsType, 0)
	if err != nil {
		return fmt.Errorf("failed to open %s filesystem: %w", fsType, err)
	}
	defer unix.Close(fsfd)

	if err := unix.FsconfigSetString(fsfd, "source", imageBlob); err != nil {
		// If source doesn't work, try with loop device
		return d.mountImageFSWithNewAPIAndLoop(imageBlob, dest, fsType)
	}

	if err := unix.FsconfigSetFlag(fsfd, "ro"); err != nil {
		return fmt.Errorf("failed to set %s filesystem read-only: %w", fsType, err)
	}

	if err := unix.FsconfigCreate(fsfd); err != nil {
		buffer := make([]byte, 4096)
		if n, _ := unix.Read(fsfd, buffer); n > 0 {
			return fmt.Errorf("failed to create %s filesystem: %s: %w", fsType, strings.TrimSuffix(string(buffer[:n]), "\n"), err)
		}
		return fmt.Errorf("failed to create %s filesystem: %w", fsType, err)
	}

	mfd, err := unix.Fsmount(fsfd, 0, unix.MOUNT_ATTR_RDONLY)
	if err != nil {
		buffer := make([]byte, 4096)
		if n, _ := unix.Read(fsfd, buffer); n > 0 {
			return fmt.Errorf("failed to mount %s filesystem: %s: %w", fsType, string(buffer[:n]), err)
		}
		return fmt.Errorf("failed to mount %s filesystem: %w", fsType, err)
	}
	defer unix.Close(mfd)

	if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, dest, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("failed to move mount to %q: %w", dest, err)
	}
	return nil
}

func (d *Driver) mountImageFSWithNewAPIAndLoop(imageBlob, dest, fsType string) error {
	logrus.Debugf("overlay: mounting %s image %q to %q using new mount API with loop device", fsType, imageBlob, dest)
	// Use loop device with new mount API
	loop, err := loopback.AttachLoopDeviceRO(imageBlob)
	if err != nil {
		return fmt.Errorf("failed to attach loop device: %w", err)
	}
	defer loop.Close()

	fsfd, err := unix.Fsopen(fsType, 0)
	if err != nil {
		return fmt.Errorf("failed to open %s filesystem: %w", fsType, err)
	}
	defer unix.Close(fsfd)

	if err := unix.FsconfigSetString(fsfd, "source", loop.Name()); err != nil {
		return fmt.Errorf("failed to set source for %s filesystem: %w", fsType, err)
	}

	if err := unix.FsconfigSetFlag(fsfd, "ro"); err != nil {
		return fmt.Errorf("failed to set %s filesystem read-only: %w", fsType, err)
	}

	if err := unix.FsconfigCreate(fsfd); err != nil {
		buffer := make([]byte, 4096)
		if n, _ := unix.Read(fsfd, buffer); n > 0 {
			return fmt.Errorf("failed to create %s filesystem: %s: %w", fsType, strings.TrimSuffix(string(buffer[:n]), "\n"), err)
		}
		return fmt.Errorf("failed to create %s filesystem: %w", fsType, err)
	}

	mfd, err := unix.Fsmount(fsfd, 0, unix.MOUNT_ATTR_RDONLY)
	if err != nil {
		buffer := make([]byte, 4096)
		if n, _ := unix.Read(fsfd, buffer); n > 0 {
			return fmt.Errorf("failed to mount %s filesystem: %s: %w", fsType, string(buffer[:n]), err)
		}
		return fmt.Errorf("failed to mount %s filesystem: %w", fsType, err)
	}
	defer unix.Close(mfd)

	if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, dest, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("failed to move mount to %q: %w", dest, err)
	}
	return nil
}

func (d *Driver) mountImageFSWithLoop(imageBlob, dest, fsType string) error {
	logrus.Debugf("overlay: mounting %s image %q to %q using traditional loop device", fsType, imageBlob, dest)
	// Traditional loop device + mount (rootful only)
	losetup, err := exec.LookPath("losetup")
	if err != nil {
		return err
	}
	mount, err := exec.LookPath("mount")
	if err != nil {
		return err
	}

	out, err := exec.Command(losetup, "-f", imageBlob).Output()
	if err != nil {
		return fmt.Errorf("failed to setup loop device for %s: %w", imageBlob, err)
	}
	loopDevice := strings.TrimSpace(string(out))
	if _, err := exec.Command(mount, "-t", fsType, loopDevice, dest).Output(); err != nil {
		return fmt.Errorf("failed to mount %s image %s on %s: %w", fsType, imageBlob, dest, err)
	}
	return nil
}
