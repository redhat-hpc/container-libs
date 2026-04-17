package imagefs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/mount"
)

type Mounter interface {
	Mount(source, target, fsType, options string) error
	Unmount(target string) error
	LazyUnmount(target string) error
	RunCommand(name string, args ...string) error
}

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

func (m *MountManager) MountLayer(containerID, layerID, imagePath string, isRoot bool) (string, error) {
	rundir := m.GetRundir(containerID)
	layerDir := filepath.Join(rundir, layerID)

	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create layer dir %s: %w", layerDir, err)
	}

	// Attempt to mount the layer.
	// If we think we are root, try the kernel mount first.
	if isRoot {
		err := m.mountRoot(imagePath, layerDir)
		if err == nil {
			return layerDir, nil
		}

		// If the kernel mount failed due to permissions (EPERM), 
		// we might be in a user namespace where we are "root" but not 
		// privileged enough to use syscall.Mount. Fall back to FUSE.
		if strings.Contains(err.Error(), "operation not permitted") {
			logrus.Debugf("[imagefs] Kernel mount failed with EPERM, falling back to FUSE for layer %s", layerID)
		} else {
			return "", err
		}
	}

	// Rootless path: Use FUSE mounts.
	if err := m.mountRootless(imagePath, layerDir); err != nil {
		return "", fmt.Errorf("rootless mount failed for layer %s: %w", layerID, err)
	}

	return layerDir, nil
}

func (m *MountManager) mountRoot(source, target string) error {
	fsType := "erofs"
	if filepath.Ext(source) == ".sqsh" {
		fsType = "squashfs"
	}

	// Use secure mount flags via options string. 
	// Removed noexec to allow container binaries to run.
	options := "nodev,nosuid"
	logrus.Debugf("[imagefs] Root mounting layer: %s -> %s (type: %s, opts: %s)", source, target, fsType, options)
	err := m.mounter.Mount(source, target, fsType, options)
	if err != nil {
		return fmt.Errorf("mounter.Mount failed (%s): %w", fsType, err)
	}
	return nil
}

func (m *MountManager) mountRootless(source, target string) error {
	var cmdName string
	if filepath.Ext(source) == ".sqsh" {
		cmdName = "squashfuse"
	} else {
		cmdName = "erofsfuse"
	}

	logrus.Debugf("[imagefs] Rootless mounting layer via %s: %s -> %s", cmdName, source, target)
	// Use -o allow_other,default_permissions to prevent permission denied errors for container processes.
	// 'allow_other' allows other users to access the mount.
	// 'default_permissions' tells FUSE to use the kernel's standard permission checks.
	// Note: This requires /etc/fuse.conf to have 'user_allow_other' enabled.
	if err := m.mounter.RunCommand(cmdName, "-o", "allow_other,default_permissions", source, target); err != nil {
		return fmt.Errorf("rootless mount failed: %w", err)
	}
	return nil
}

func (m *MountManager) UnmountLayer(target string) error {
	err := m.mounter.Unmount(target)
	if err != nil {
		logrus.Debugf("mounter.Unmount failed for %s, trying lazy umount: %v", target, err)
		if err := m.mounter.LazyUnmount(target); err != nil {
			return fmt.Errorf("failed to unmount %s: %w", target, err)
		}
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
			// We use lazy unmount to ensure we break the link even if files are open.
			_ = m.mounter.LazyUnmount(path)
		}
	}

	if err := os.RemoveAll(rundir); err != nil {
		return fmt.Errorf("failed to remove rundir %s: %w", rundir, err)
	}
	return nil
}
