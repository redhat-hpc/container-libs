//go:build linux

package overlay

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/fileutils"
)

func (d *Driver) maybeAddImageFSMount(id, dir, lowerID string, i int, readWrite, inAdditionalStore bool) (string, error) {
	if d.options.imageFSType == "" {
		return "", nil
	}
	if readWrite && i == 0 {
		return "", fmt.Errorf("cannot mount an image filesystem layer as writeable")
	}
	logrus.Debugf("overlay: using %s blob for lower %s", d.options.imageFSType, lowerID)
	imageBlob := d.getImageFSData(lowerID)
	if err := fileutils.Exists(imageBlob); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	dest := d.getStorePrivateDirectory(id, dir, fmt.Sprintf("imagefs-layers/%d", i), inAdditionalStore)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	if err := mountImageFSBlob(imageBlob, dest, d.options.imageFSType); err != nil {
		return "", err
	}
	return dest, nil
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

func mountImageFSBlob(imageBlob, dest, fsType string) error {
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
