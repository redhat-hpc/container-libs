//go:build linux

package imagefs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/sirupsen/logrus"
	"go.podman.io/storage/pkg/fileutils"
	"go.podman.io/storage/pkg/parsers/kernel"
)

// ErofsBackend implements the Backend interface for EROFS (Enhanced Read-Only File System).
type ErofsBackend struct {
	compression string
}

// NewErofsBackend creates a new EROFS backend.
func NewErofsBackend(compression string) *ErofsBackend {
	return &ErofsBackend{
		compression: compression,
	}
}

func (b *ErofsBackend) Format() string {
	return FormatEROFS
}

func (b *ErofsBackend) FileExtension() string {
	return ".erofs"
}

func (b *ErofsBackend) CreateImage(tarballPath, destImagePath string) (int64, error) {
	// Create EROFS image from tarball with --aufs flag.
	// The --aufs flag tells mkfs.erofs to convert .wh.* files to overlayfs
	// whiteout character devices automatically during EROFS creation.
	//
	// Compression support varies by erofs-utils version:
	// - 1.7.x: lz4, lz4hc, deflate, libdeflate
	// - 1.8+:  lz4, lz4hc, deflate, lzma, zstd
	args := []string{"--tar=i", "--aufs", "-E", "legacy-compress"}

	// Add compression algorithm if specified
	// Note: If the specified compressor is not available, mkfs.erofs will fail
	// with a clear error message about unsupported compression algorithm
	if b.compression != "" {
		compressor := b.compression
		// EROFS uses "deflate" while SquashFS uses "gzip"
		// Map gzip -> deflate for EROFS since they're the same algorithm
		if compressor == "gzip" {
			compressor = "deflate"
		}
		args = append(args, "-z", compressor)
	}

	args = append(args, destImagePath, tarballPath)
	cmd := exec.Command("mkfs.erofs", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	compressionInfo := b.compression
	if compressionInfo == "" {
		compressionInfo = "none"
	}
	logrus.Debugf("[imagefs] Creating EROFS with compression %s: %v", compressionInfo, cmd.Args)
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("mkfs.erofs failed: %w: %s", err, stderr.String())
	}

	// Get the size of the tarball for return value
	info, err := os.Stat(tarballPath)
	if err != nil {
		return 0, fmt.Errorf("failed to stat tarball: %w", err)
	}

	return info.Size(), nil
}

func (b *ErofsBackend) CanMergeLayers(imagePaths []string) bool {
	// EROFS merge requires:
	// - Kernel 5.14+ for multi-image support
	// - At least 2 layers to merge
	// - All images must be EROFS format
	if !kernel.CheckKernelVersion(5, 14, 0) {
		return false
	}

	if len(imagePaths) <= 1 {
		return false
	}

	for _, path := range imagePaths {
		if filepath.Ext(path) != ".erofs" {
			return false
		}
	}

	return true
}

func (b *ErofsBackend) MergeLayers(imagePaths []string, devicePaths []string, mergedImagePath string) error {
	// mkfs.erofs <dest> <src1> <src2> ...
	args := append([]string{mergedImagePath}, imagePaths...)
	cmd := exec.Command("mkfs.erofs", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	logrus.Debugf("[imagefs] Creating merged EROFS: mkfs.erofs %v", args)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.erofs merge failed: %w: %s", err, stderr.String())
	}

	return nil
}

func (b *ErofsBackend) ShouldPreserveTarball() bool {
	return true // EROFS needs the tarball for --device= mounting
}

func (b *ErofsBackend) GetDiffForBaseLayer(imagePath string) (io.ReadCloser, error) {
	// For EROFS, return the original tarball to preserve exact digests
	tarballPath := imagePath + ".tar"
	if fileutils.Exists(tarballPath) != nil {
		return nil, nil // Tarball doesn't exist, use naiveDiff
	}

	logrus.Debugf("[imagefs] Returning original tarball for EROFS layer: %s", tarballPath)
	f, err := os.Open(tarballPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open tarball %s: %w", tarballPath, err)
	}
	return f, nil
}

func (b *ErofsBackend) StatusFields() [][2]string {
	return [][2]string{
		{"erofs-utils", b.getVersion()},
	}
}

func (b *ErofsBackend) getVersion() string {
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
