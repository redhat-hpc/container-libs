//go:build linux

package imagefs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/loopback"
	"go.podman.io/storage/pkg/mount"
	"golang.org/x/sys/unix"
)

type Mounter interface {
	Mount(source, target, fsType, options string) error
	Unmount(target string) error
	LazyUnmount(target string) error
	RunCommand(name string, args ...string) error
}

var (
	// skipMountViaFile tracks whether direct file mounting is supported (kernel 6.12+).
	// If false, we try direct file mounts first. If true, we skip directly to loopback device mounts.
	skipMountViaFile atomic.Bool
)

type RealMounter struct{}

func (r *RealMounter) Mount(source, target, fsType, options string) error {
	return mount.Mount(source, target, fsType, options)
}

func (r *RealMounter) Unmount(target string) error {
	return mount.Unmount(target)
}

func (r *RealMounter) LazyUnmount(target string) error {
	return r.RunCommand("umount", "-l", target)
}

func (r *RealMounter) RunCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command %s %v failed: %w: %s", name, args, err, stderr.String())
	}
	return nil
}

type MountManager struct {
	runRoot string
	mounter Mounter
}

func NewMountManager(runRoot string, mounter Mounter) *MountManager {
	if mounter == nil {
		mounter = &RealMounter{}
	}
	return &MountManager{
		runRoot: runRoot,
		mounter: mounter,
	}
}

func (m *MountManager) GetRundir(containerID string) string {
	return filepath.Join(m.runRoot, "imagefs", containerID)
}

func (m *MountManager) MountLayer(containerID, layerID, imagePath string, isRoot bool) (string, bool, error) {
	return m.MountLayerWithDevices(containerID, layerID, imagePath, isRoot, nil, "")
}

func (m *MountManager) MountLayerWithDevices(containerID, layerID, imagePath string, isRoot bool, devices []string, mountLabel string) (string, bool, error) {
	rundir := m.GetRundir(containerID)
	layerDir := filepath.Join(rundir, layerID)

	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return "", false, fmt.Errorf("failed to create layer dir %s: %w", layerDir, err)
	}

	// Attempt to mount the layer.
	// If we think we are root, try the kernel mount first.
	if isRoot {
		logrus.Debugf("[imagefs] Attempting kernel mount for layer %s (rootful mode)", layerID)
		err := m.mountRoot(imagePath, layerDir, devices, mountLabel)
		if err == nil {
			return layerDir, false, nil // Successfully used kernel mount
		}

		// If the kernel mount failed due to permissions (EPERM),
		// we might be in a user namespace where we are "root" but not
		// privileged enough to use syscall.Mount. Fall back to FUSE.
		//
		// Also fall back to FUSE if the kernel requires a block device (ENOTBLK).
		// On kernels < 6.12, EROFS/squashfs cannot be mounted directly from files.
		if strings.Contains(err.Error(), "operation not permitted") {
			logrus.Debugf("[imagefs] Kernel mount failed with EPERM, falling back to FUSE for layer %s", layerID)
		} else if errors.Is(err, unix.ENOTBLK) || strings.Contains(err.Error(), "block device") {
			logrus.Debugf("[imagefs] Kernel mount requires block device, falling back to FUSE for layer %s", layerID)
		} else {
			return "", false, err
		}
	} else {
		logrus.Debugf("[imagefs] Using FUSE mount for layer %s (rootless mode)", layerID)
	}

	// Rootless path: Use FUSE mounts.
	if err := m.mountRootlessWithDevices(imagePath, layerDir, devices); err != nil {
		return "", false, fmt.Errorf("rootless mount failed for layer %s: %w", layerID, err)
	}

	return layerDir, true, nil // Used FUSE mount
}

// openBlobFile mounts an EROFS or squashfs image file using the new mount API.
// It supports both direct file mounting (kernel 6.12+) and loopback device mounting (kernel 5.14-6.11).
// For metadata-only EROFS images, device paths can be provided for external data.
// Returns a file descriptor for the mounted filesystem which must be closed by the caller.
func openBlobFile(blobFile, fsType string, useLoopDevice bool, devices []string, mountLabel string) (int, error) {
	var loop *os.File

	if useLoopDevice {
		var err error
		loop, err = loopback.AttachLoopDeviceRO(blobFile)
		if err != nil {
			return -1, err
		}
		defer loop.Close()
		blobFile = loop.Name()
	}

	// Use new mount API
	fsfd, err := unix.Fsopen(fsType, 0)
	if err != nil {
		return -1, fmt.Errorf("failed to open %s filesystem: %w", fsType, err)
	}
	defer unix.Close(fsfd)

	if err := unix.FsconfigSetString(fsfd, "source", blobFile); err != nil {
		return -1, fmt.Errorf("failed to set source for %s: %w", fsType, err)
	}

	// For metadata-only EROFS images, set external device paths
	if fsType == "erofs" && len(devices) > 0 {
		for _, device := range devices {
			if err := unix.FsconfigSetString(fsfd, "device", device); err != nil {
				return -1, fmt.Errorf("failed to set device %s for erofs: %w", device, err)
			}
		}
	}

	// Apply SELinux context if provided
	if mountLabel != "" {
		if err := unix.FsconfigSetString(fsfd, "context", mountLabel); err != nil {
			return -1, fmt.Errorf("failed to set SELinux context for %s: %w", fsType, err)
		}
	}

	if err := unix.FsconfigSetFlag(fsfd, "ro"); err != nil {
		return -1, fmt.Errorf("failed to set %s read-only: %w", fsType, err)
	}

	// Container images don't use ACLs - always set noacl for EROFS
	if fsType == "erofs" {
		if err := unix.FsconfigSetFlag(fsfd, "noacl"); err != nil {
			return -1, fmt.Errorf("failed to set noacl for erofs: %w", err)
		}
	}

	if err := unix.FsconfigCreate(fsfd); err != nil {
		return -1, fmt.Errorf("failed to create %s filesystem: %w", fsType, err)
	}

	mfd, err := unix.Fsmount(fsfd, 0, unix.MOUNT_ATTR_RDONLY)
	if err != nil {
		return -1, fmt.Errorf("failed to mount %s filesystem: %w", fsType, err)
	}

	return mfd, nil
}

func (m *MountManager) mountRoot(source, target string, devices []string, mountLabel string) error {
	// Detect filesystem type
	fsType := "erofs"
	if filepath.Ext(source) == ".sqsh" {
		fsType = "squashfs"
	}

	// Tier 1: Try direct file mount (kernel 6.12+)
	if !skipMountViaFile.Load() {
		mfd, err := openBlobFile(source, fsType, false, devices, mountLabel)
		if err == nil {
			defer unix.Close(mfd)
			if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
				return fmt.Errorf("failed to move mount to %q: %w", target, err)
			}
			logrus.Debugf("[imagefs] Kernel mounted %s via direct file: %s -> %s (devices: %v, label: %s)", fsType, source, target, devices, mountLabel)
			return nil
		}

		// If direct mount failed due to block device requirement, remember and fall through
		if errors.Is(err, unix.ENOTBLK) {
			logrus.Debugf("[imagefs] Direct file mounting not supported, using loopback device")
			skipMountViaFile.Store(true)
		} else {
			// Real error - return it
			return fmt.Errorf("kernel mount failed: %w", err)
		}
	}

	// Tier 2: Try loopback device mount (kernel 5.14-6.11)
	mfd, err := openBlobFile(source, fsType, true, devices, mountLabel)
	if err != nil {
		return fmt.Errorf("loopback mount failed: %w", err)
	}
	defer unix.Close(mfd)

	if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("failed to move mount to %q: %w", target, err)
	}

	logrus.Debugf("[imagefs] Kernel mounted %s via loopback: %s -> %s (devices: %v, label: %s)", fsType, source, target, devices, mountLabel)
	return nil
}

func (m *MountManager) mountRootlessWithDevices(source, target string, devices []string) error {
	var cmdName string
	if filepath.Ext(source) == ".sqsh" {
		cmdName = "squashfuse"
	} else {
		cmdName = "erofsfuse"
	}

	// FUSE mount options for overlay compatibility:
	// - allow_other: allow other users to access the mount
	// - default_permissions: enable kernel permission checking
	// Note: Removed direct_io and use_ino as they may interfere with overlay copy-up
	args := []string{"-o", "allow_other,default_permissions", source, target}

	// Add device arguments before the source
	if len(devices) > 0 {
		deviceArgs := []string{}
		for _, device := range devices {
			deviceArgs = append(deviceArgs, "--device="+device)
		}
		args = append(deviceArgs, args...)
	}
	if err := m.mounter.RunCommand(cmdName, args...); err != nil {
		return fmt.Errorf("rootless mount failed: %w", err)
	}
	return nil
}

func (m *MountManager) UnmountLayer(target string) error {
	// Try normal unmount first
	err := m.mounter.Unmount(target)
	if err == nil {
		return nil
	}

	logrus.Debugf("[imagefs] Normal unmount failed for %s, trying lazy umount: %v", target, err)
	err = m.mounter.LazyUnmount(target)
	if err == nil {
		return nil
	}

	// Try fusermount -u for FUSE mounts
	logrus.Debugf("[imagefs] Lazy unmount failed for %s, trying fusermount -u: %v", target, err)
	if err := m.mounter.RunCommand("fusermount", "-u", target); err != nil {
		return fmt.Errorf("failed to unmount %s (tried unmount, lazy unmount, and fusermount): %w", target, err)
	}
	return nil
}

func (m *MountManager) CleanupRundir(containerID string) error {
	rundir := m.GetRundir(containerID)

	entries, err := os.ReadDir(rundir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read rundir %s: %w", rundir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			path := filepath.Join(rundir, entry.Name())
			logrus.Debugf("[imagefs] Unmounting layer at %s", path)

			// Try lazy unmount first
			if err := m.mounter.LazyUnmount(path); err != nil {
				// If lazy unmount fails, try fusermount -u for FUSE mounts
				logrus.Debugf("[imagefs] Lazy unmount failed for %s, trying fusermount -u: %v", path, err)
				_ = m.mounter.RunCommand("fusermount", "-u", path)
			}
		}
	}

	if err := os.RemoveAll(rundir); err != nil {
		return fmt.Errorf("failed to remove rundir %s: %w", rundir, err)
	}
	return nil
}
