//go:build linux

package overlay

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/opencontainers/selinux/go-selinux"
	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/loopback"
	"go.podman.io/storage/pkg/unshare"
	"golang.org/x/sys/unix"
)

var (
	erofsHelperOnce sync.Once
	erofsHelperPath string
	erofsHelperErr  error

	erofsFuseHelperOnce sync.Once
	erofsFuseHelperPath string
	erofsFuseHelperErr  error
)

// getEROFSHelper finds the mkfs.erofs tool
func getEROFSHelper() (string, error) {
	erofsHelperOnce.Do(func() {
		erofsHelperPath, erofsHelperErr = exec.LookPath("mkfs.erofs")
	})
	return erofsHelperPath, erofsHelperErr
}

// getEROFSFuseHelper finds the erofsfuse tool for rootless mounting
func getEROFSFuseHelper() (string, error) {
	erofsFuseHelperOnce.Do(func() {
		erofsFuseHelperPath, erofsFuseHelperErr = exec.LookPath("erofsfuse")
	})
	return erofsFuseHelperPath, erofsFuseHelperErr
}

// getEROFSBlob returns the path to the EROFS blob for a layer
func getEROFSBlob(dataDir string) string {
	return filepath.Join(dataDir, "layer.erofs")
}

// generateEROFSBlobFromTar creates an EROFS filesystem from a tar stream
func generateEROFSBlobFromTar(tarReader io.Reader, layerDir string, options *overlayOptions) error {
	mkfsErofs, err := getEROFSHelper()
	if err != nil {
		return fmt.Errorf("failed to find mkfs.erofs: %w", err)
	}

	// Determine compression algorithm, defaulting to lz4
	compressionAlgo := "lz4"
	if options != nil && options.erofsCompressionAlgorithm != "" {
		compressionAlgo = options.erofsCompressionAlgorithm
	}
	logrus.Debugf("EROFS: Using compression algorithm: %s", compressionAlgo)

	// Create EROFS filesystem directly from tar file with compression
	args := []string{
		"-z", compressionAlgo,
		"--tar=f", // Read from tar file
		"--aufs",
	}

	// Add UID/GID squashing if enabled and in rootless mode
	if options != nil && options.erofsForceIDs {
		logrus.Debugf("EROFS: erofsForceIDs is enabled, checking rootless mode: %t", unshare.IsRootless())
		if unshare.IsRootless() {
			// Default to current user's UID/GID if not specified
			forceUID := options.erofsForceUID
			logrus.Debugf("EROFS: Initial erofsForceUID='%s'", forceUID)
			if forceUID == "" {
				forceUID = fmt.Sprintf("%d", unshare.GetRootlessUID())
				logrus.Debugf("EROFS: Set default UID to '%s'", forceUID)
			}
			forceGID := options.erofsForceGID
			logrus.Debugf("EROFS: Initial erofsForceGID='%s'", forceGID)
			if forceGID == "" {
				forceGID = fmt.Sprintf("%d", unshare.GetRootlessGID())
				logrus.Debugf("EROFS: Set default GID to '%s'", forceGID)
			}

			logrus.Debugf("EROFS: Enabling UID/GID squashing to %s:%s for rootless mode", forceUID, forceGID)
			args = append(args, "--force-uid", forceUID, "--force-gid", forceGID)
		} else {
			logrus.Warningf("EROFS UID/GID squashing is only supported in rootless mode and will be ignored in rootful mode")
		}
	}

	// TODO: Implement a real check for reflink support. A proper implementation
	// would attempt a reflink on a temporary file in layerDir. For now, we
	// default to the non-reflink path to ensure it is exercised.
	//
	// This is required because mkfs.erofs uses fallocate which isn't always supported
	reflinkSupported := false

	erofsBlobPath := getEROFSBlob(layerDir)
	var tempBlobPath string
	var cmd *exec.Cmd

	if reflinkSupported {
		args = append(args, erofsBlobPath)
		cmd = exec.Command(mkfsErofs, args...)
		logrus.Debugf("EROFS: Running mkfs.erofs command: %s %v", mkfsErofs, args)
	} else {
		logrus.Debugf("EROFS: reflink not supported on %s, creating blob in temporary location", layerDir)
		tmpFile, err := os.CreateTemp("", "erofs-blob-")
		if err != nil {
			return fmt.Errorf("failed to create temporary file for erofs blob: %w", err)
		}
		if err := tmpFile.Close(); err != nil {
			os.Remove(tmpFile.Name())
			return fmt.Errorf("failed to close temporary file for erofs blob: %w", err)
		}
		tempBlobPath = tmpFile.Name()
		args = append(args, tempBlobPath)
		cmd = exec.Command(mkfsErofs, args...)
		logrus.Debugf("EROFS: Running mkfs.erofs command (via temp file): %s %v", mkfsErofs, args)
	}

	cmd.Stdin = tarReader

	var stderr bytes.Buffer
	var stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		if tempBlobPath != "" {
			os.Remove(tempBlobPath)
		}
		return fmt.Errorf("mkfs.erofs failed: %w: stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}

	if tempBlobPath != "" {
		if err := moveFile(tempBlobPath, erofsBlobPath); err != nil {
			// The moveFile function will attempt to clean up the source on its own
			return fmt.Errorf("failed to move EROFS blob from %s to %s: %w", tempBlobPath, erofsBlobPath, err)
		}
	}

	// Check if the blob was actually created
	stat, err := os.Stat(erofsBlobPath)
	if err != nil {
		return fmt.Errorf("EROFS blob not created at %s: %w", erofsBlobPath, err)
	}
	logrus.Debugf("EROFS: Generated blob size: %d bytes", stat.Size())

	return nil
}

// mountEROFSBlob mounts an EROFS blob to a mount point
func mountEROFSBlob(erofsBlobPath, mountPoint string) error {
	// Create mount point if it doesn't exist
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Check if we're running in rootless mode
	if unshare.IsRootless() {
		return mountEROFSBlobWithFuse(erofsBlobPath, mountPoint)
	}

	// For privileged containers, use kernel EROFS mounting
	return mountEROFSBlobKernel(erofsBlobPath, mountPoint)
}

// mountEROFSBlobWithFuse mounts an EROFS blob using erofsfuse for rootless scenarios
func mountEROFSBlobWithFuse(erofsBlobPath, mountPoint string) error {
	erofsFuse, err := getEROFSFuseHelper()
	if err != nil {
		return fmt.Errorf("erofsfuse not found (required for rootless EROFS): %w", err)
	}

	logrus.Debugf("Mounting EROFS blob %s at %s using erofsfuse", erofsBlobPath, mountPoint)

	// Mount using erofsfuse
	cmd := exec.Command(erofsFuse, erofsBlobPath, mountPoint)
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout

	logrus.Debugf("EROFS: Executing erofsfuse command: %v", cmd.Args)

	if err := cmd.Run(); err != nil {
		logrus.Debugf("EROFS: erofsfuse failed - stdout: %s, stderr: %s", stdout.String(), stderr.String())
		return fmt.Errorf("erofsfuse mount failed: %w: stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}

	return nil
}

// mountEROFSBlobKernel mounts an EROFS blob using kernel EROFS for privileged scenarios
func mountEROFSBlobKernel(erofsBlobPath, mountPoint string) error {
	// Check if blob has ACL support
	hasACLSupport, err := erofsHasACL(erofsBlobPath)
	if err != nil {
		return fmt.Errorf("failed to check ACL support: %w", err)
	}

	// Mount EROFS blob directly or via loopback
	mfd, err := openEROFSBlob(erofsBlobPath, hasACLSupport, false)
	if err != nil {
		if errors.Is(err, unix.ENOTBLK) {
			logrus.Debugf("Kernel doesn't support direct EROFS mount, using loopback for %s", erofsBlobPath)
			mfd, err = openEROFSBlob(erofsBlobPath, hasACLSupport, true)
		}
		if err != nil {
			return fmt.Errorf("failed to open EROFS blob: %w", err)
		}
	}
	defer unix.Close(mfd)

	// Move mount to target location
	if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, mountPoint, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("failed to move mount to %q: %w", mountPoint, err)
	}

	return nil
}

// unmountEROFSBlob unmounts an EROFS mount point
func unmountEROFSBlob(mountPoint string) error {
	// Check if mount point exists
	if _, err := os.Stat(mountPoint); errors.Is(err, os.ErrNotExist) {
		// Mount point doesn't exist, nothing to unmount
		return nil
	}

	// Try regular unmount first (works for both kernel EROFS and FUSE)
	if err := unix.Unmount(mountPoint, 0); err != nil {
		// If the error is ENOENT or EINVAL, the mount point wasn't mounted
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EINVAL) {
			return nil
		}

		// If regular unmount fails and we're rootless, try fusermount -u as fallback
		if unshare.IsRootless() {
			if fusermount, err := exec.LookPath("fusermount"); err == nil {
				cmd := exec.Command(fusermount, "-u", mountPoint)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if fuseErr := cmd.Run(); fuseErr != nil {
					// If fusermount also fails with "not mounted", that's okay
					stderrStr := stderr.String()
					if strings.Contains(stderrStr, "not mounted") || strings.Contains(stderrStr, "No such file or directory") {
						return nil
					}
					return fmt.Errorf("failed to unmount FUSE %s: %w (original error: %v)", mountPoint, fuseErr, err)
				}
				return nil
			}
		}
		return fmt.Errorf("failed to unmount %s: %w", mountPoint, err)
	}
	return nil
}

// erofsHasACL returns true if the EROFS blob has ACLs enabled
func erofsHasACL(path string) (bool, error) {
	const (
		LCFS_EROFS_FLAGS_HAS_ACL = (1 << 0)
		versionNumberSize        = 4
		magicNumberSize          = 4
		flagsSize                = 4
	)

	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	// Read EROFS header to check for ACL flag
	buffer := make([]byte, versionNumberSize+magicNumberSize+flagsSize)
	nread, err := file.Read(buffer)
	if err != nil {
		return false, err
	}
	if nread != len(buffer) {
		return false, fmt.Errorf("failed to read flags from %q", path)
	}
	flags := buffer[versionNumberSize+magicNumberSize:]
	return binary.LittleEndian.Uint32(flags)&LCFS_EROFS_FLAGS_HAS_ACL != 0, nil
}

// openEROFSBlob opens an EROFS blob for mounting
func openEROFSBlob(blobFile string, hasACL, useLoopDevice bool) (int, error) {
	if useLoopDevice {
		loop, err := loopback.AttachLoopDeviceRO(blobFile)
		if err != nil {
			return -1, fmt.Errorf("failed to attach loop device: %w", err)
		}
		defer loop.Close()
		blobFile = loop.Name()
	}

	fsfd, err := unix.Fsopen("erofs", 0)
	if err != nil {
		return -1, fmt.Errorf("failed to open erofs filesystem: %w", err)
	}
	defer unix.Close(fsfd)

	if err := unix.FsconfigSetString(fsfd, "source", blobFile); err != nil {
		return -1, fmt.Errorf("failed to set source for erofs filesystem: %w", err)
	}

	if err := unix.FsconfigSetFlag(fsfd, "ro"); err != nil {
		return -1, fmt.Errorf("failed to set erofs filesystem read-only: %w", err)
	}

	if !hasACL {
		if err := unix.FsconfigSetFlag(fsfd, "noacl"); err != nil {
			return -1, fmt.Errorf("failed to set noacl for erofs filesystem: %w", err)
		}
	}

	if selinux.GetEnabled() {
		if err := unix.FsconfigSetString(fsfd, "context", selinuxLabelTest); err != nil {
			return -1, fmt.Errorf("failed to set selinux context on erofs filesystem: %w", err)
		}
	}

	if err := unix.FsconfigCreate(fsfd); err != nil {
		buffer := make([]byte, 4096)
		if n, _ := unix.Read(fsfd, buffer); n > 0 {
			return -1, fmt.Errorf("failed to create erofs filesystem: %s: %w", strings.TrimSuffix(string(buffer[:n]), "\n"), err)
		}
		return -1, fmt.Errorf("failed to create erofs filesystem: %w", err)
	}

	mfd, err := unix.Fsmount(fsfd, 0, unix.MOUNT_ATTR_RDONLY)
	if err != nil {
		buffer := make([]byte, 4096)
		if n, _ := unix.Read(fsfd, buffer); n > 0 {
			return -1, fmt.Errorf("failed to mount erofs filesystem: %s: %w", string(buffer[:n]), err)
		}
		return -1, fmt.Errorf("failed to mount erofs filesystem: %w", err)
	}
	return mfd, nil
}

// erofsExists checks if an EROFS blob exists for a layer
func erofsExists(layerDir string) bool {
	blobPath := getEROFSBlob(layerDir)
	_, err := os.Stat(blobPath)
	return err == nil
}

// moveFile performs a copy-and-delete to move a file from src to dst,
// which is necessary for cross-device moves.
func moveFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file for move: %w", err)
	}

	dstFile, err := os.Create(dst)
	if err != nil {
		srcFile.Close()
		return fmt.Errorf("failed to create destination file for move: %w", err)
	}

	logrus.Debugf("EROFS: Moving from %s to %s", srcFile.Name(), dstFile.Name())
	_, err = io.Copy(dstFile, srcFile)

	// Close files before removing source
	dstFile.Close()
	srcFile.Close()

	if err != nil {
		os.Remove(dst) // Clean up partial copy
		return fmt.Errorf("failed to copy file contents for move: %w", err)
	}

	if err := os.Remove(src); err != nil {
		logrus.Warnf("Failed to remove source file after copy: %v", err)
	}
	return nil
}
