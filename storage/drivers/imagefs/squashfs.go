//go:build linux

package imagefs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"

	"github.com/sirupsen/logrus"
)

// SquashfsBackend implements the Backend interface for SquashFS.
type SquashfsBackend struct {
	info        BackendInfo
	compression string
}

// NewSquashfsBackend creates a new SquashFS backend.
func NewSquashfsBackend(compression string) *SquashfsBackend {
	return &SquashfsBackend{
		info: BackendInfo{
			Format:          FormatSquashFS,
			FileExtension:   ".sqfs",
			PreserveTarball: false, // Storage layer handles tar-split
		},
		compression: compression,
	}
}

func (b *SquashfsBackend) Info() BackendInfo {
	return b.info
}

func (b *SquashfsBackend) CreateImage(tarballPath, destImagePath string) (int64, error) {
	// Open the tarball
	tarFile, err := os.Open(tarballPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open tarball: %w", err)
	}
	defer tarFile.Close()

	// Get tarball size for return value
	info, err := tarFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to stat tarball: %w", err)
	}
	size := info.Size()

	// Detect which tool to use: prefer sqfstar (RHEL 10+), fallback to tar2sqfs (RHEL 9)
	toolType := b.getToolType()
	if toolType == "" {
		return 0, fmt.Errorf("neither sqfstar nor tar2sqfs found in PATH")
	}

	var cmd *exec.Cmd
	var args []string

	switch toolType {
	case "sqfstar":
		// Use sqfstar (squashfs-tools 4.6.1+ on RHEL 10)
		if b.compression != "" {
			args = append(args, "-comp", b.compression)
		}
		args = append(args, destImagePath)
		cmd = exec.Command("sqfstar", args...)
		logrus.Debugf("[imagefs] Creating squashfs with sqfstar: %v < %s", args, tarballPath)

	case "tar2sqfs":
		// Use tar2sqfs (squashfs-tools-ng on RHEL 9)
		if b.compression != "" {
			args = append(args, "--compressor", b.compression)
		}
		args = append(args, destImagePath)
		cmd = exec.Command("tar2sqfs", args...)
		logrus.Debugf("[imagefs] Creating squashfs with tar2sqfs: %v < %s", args, tarballPath)
	}

	cmd.Stdin = tarFile

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("%s failed: %w: %s", toolType, err, stderr.String())
	}

	return size, nil
}

func (b *SquashfsBackend) MountLayers(ctx MountContext) ([]string, bool, error) {
	// SquashFS doesn't have optimizations like EROFS merge, so just mount each layer separately
	var lowerDirs []string
	var usedFuse bool

	for _, layerID := range ctx.LayerIDs {
		imagePath := ctx.GetImagePath(layerID)
		if imagePath == "" {
			return nil, false, fmt.Errorf("no image file found for layer %s", layerID)
		}

		// SquashFS doesn't need device paths like EROFS
		isRoot := os.Getuid() == 0
		mountPoint, layerUsedFuse, err := ctx.MountManager.MountLayerWithDevices(
			ctx.ContainerID,
			layerID,
			imagePath,
			isRoot,
			nil, // no device paths for SquashFS
			ctx.MountLabel,
		)
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

func (b *SquashfsBackend) DiffForBaseLayer(imagePath string) (io.ReadCloser, error) {
	return nil, ErrNotSupported // Signal to use naiveDiff
}

func (b *SquashfsBackend) StatusFields() [][2]string {
	fields := [][2]string{}

	toolType := b.getToolType()
	if toolType != "" {
		fields = append(fields, [2]string{"squashfs-backend", toolType})
	}
	fields = append(fields, [2]string{"squashfs-tools", b.getVersion()})

	return fields
}

func (b *SquashfsBackend) getToolType() string {
	if _, err := exec.LookPath("sqfstar"); err == nil {
		return "sqfstar"
	}
	if _, err := exec.LookPath("tar2sqfs"); err == nil {
		return "tar2sqfs"
	}
	return ""
}

func (b *SquashfsBackend) getVersion() string {
	toolType := b.getToolType()

	switch toolType {
	case "sqfstar":
		cmd := exec.Command("sqfstar", "-version")
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		cmd.Run()

		// sqfstar outputs version to stderr
		output := stdout.String() + stderr.String()
		re := regexp.MustCompile(`(\d+\.\d+\.\d+)`)
		matches := re.FindStringSubmatch(output)
		if len(matches) > 1 {
			return "sqfstar " + matches[1]
		}
		return "sqfstar (unknown version)"

	case "tar2sqfs":
		cmd := exec.Command("tar2sqfs", "--version")
		var stdout bytes.Buffer
		cmd.Stdout = &stdout

		cmd.Run()

		output := stdout.String()
		re := regexp.MustCompile(`(\d+\.\d+\.\d+)`)
		matches := re.FindStringSubmatch(output)
		if len(matches) > 1 {
			return "tar2sqfs " + matches[1]
		}
		return "tar2sqfs (unknown version)"

	default:
		return "not found"
	}
}
