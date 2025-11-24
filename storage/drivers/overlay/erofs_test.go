//go:build linux
// +build linux

package overlay

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/storage/pkg/unshare"
)

// min returns the smaller of two integers (for older Go versions)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// skipIfEROFSUnavailable skips the test if mkfs.erofs is not available
func skipIfEROFSUnavailable(t *testing.T) {
	if _, err := getEROFSHelper(); err != nil {
		t.Skipf("mkfs.erofs not available: %v", err)
	}
}

// skipIfEROFSFuseUnavailable skips the test if erofsfuse is not available (needed for rootless)
func skipIfEROFSFuseUnavailable(t *testing.T) {
	if unshare.IsRootless() {
		if _, err := getEROFSFuseHelper(); err != nil {
			t.Skipf("erofsfuse not available (required for rootless): %v", err)
		}
	}
}

// createTarStream creates a simple tar stream for testing
func createTarStream(tb testing.TB) io.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Add a simple file
	header := &tar.Header{
		Name: "test.txt",
		Size: 11,
		Mode: 0644,
	}
	require.NoError(tb, tw.WriteHeader(header))
	_, err := tw.Write([]byte("hello world"))
	require.NoError(tb, err)

	// Add a directory
	header = &tar.Header{
		Name:     "testdir/",
		Typeflag: tar.TypeDir,
		Mode:     0755,
	}
	require.NoError(tb, tw.WriteHeader(header))

	// Add a file in directory
	header = &tar.Header{
		Name: "testdir/nested.txt",
		Size: 7,
		Mode: 0644,
	}
	require.NoError(tb, tw.WriteHeader(header))
	_, err = tw.Write([]byte("nested\n"))
	require.NoError(tb, err)

	require.NoError(tb, tw.Close())
	return bytes.NewReader(buf.Bytes())
}

func TestEROFSOptionParsing(t *testing.T) {
	skipIfEROFSUnavailable(t)

	// Test use_erofs=true parsing
	opts, err := parseOptions([]string{"use_erofs=true"})
	require.NoError(t, err)
	assert.True(t, opts.useEROFS)

	// Test use_erofs=false parsing
	opts, err = parseOptions([]string{"use_erofs=false"})
	require.NoError(t, err)
	assert.False(t, opts.useEROFS)

	// Test invalid value
	_, err = parseOptions([]string{"use_erofs=invalid"})
	assert.Error(t, err)

	// Test default (should be false)
	opts, err = parseOptions([]string{})
	require.NoError(t, err)
	assert.False(t, opts.useEROFS)
}

func TestEROFSHelperAvailability(t *testing.T) {
	// Test mkfs.erofs availability
	path, err := getEROFSHelper()
	if err != nil {
		t.Skipf("mkfs.erofs not available: %v", err)
	}
	assert.NotEmpty(t, path)

	// Test erofsfuse availability (if rootless)
	if unshare.IsRootless() {
		path, err := getEROFSFuseHelper()
		if err != nil {
			t.Skipf("erofsfuse not available: %v", err)
		}
		assert.NotEmpty(t, path)
	}
}

func TestEROFSBlobGeneration(t *testing.T) {
	skipIfEROFSUnavailable(t)

	tempDir := t.TempDir()
	blobPath := filepath.Join(tempDir, "test.erofs")

	// Create test tar stream
	tarStream := createTarStream(t)

	// Generate EROFS blob from tar
	err := generateEROFSBlobFromTar(tarStream, blobPath, nil)
	require.NoError(t, err)

	// Verify blob was created
	stat, err := os.Stat(blobPath)
	require.NoError(t, err)
	assert.Greater(t, stat.Size(), int64(0))

	// Verify it's a valid EROFS blob by mounting it and checking contents
	mountPoint := filepath.Join(tempDir, "mnt-gen")
	require.NoError(t, os.MkdirAll(mountPoint, 0o755))

	// Mount the blob
	err = mountEROFSBlob(blobPath, mountPoint)
	require.NoError(t, err)
	defer func() {
		err := unmountEROFSBlob(mountPoint)
		if err != nil {
			t.Errorf("Failed to unmount EROFS blob at %s: %v", mountPoint, err)
		}
	}()

	// Verify mount worked by checking files exist
	testFile := filepath.Join(mountPoint, "test.txt")
	content, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(content))

	nestedFile := filepath.Join(mountPoint, "testdir", "nested.txt")
	content, err = os.ReadFile(nestedFile)
	require.NoError(t, err)
	assert.Equal(t, "nested\n", string(content))
}

func TestEROFSBlobMounting(t *testing.T) {
	skipIfEROFSUnavailable(t)
	skipIfEROFSFuseUnavailable(t)

	tempDir := t.TempDir()
	blobPath := filepath.Join(tempDir, "test.erofs")
	mountPoint := filepath.Join(tempDir, "mount")

	// Create test tar stream
	tarStream := createTarStream(t)

	// Generate EROFS blob
	err := generateEROFSBlobFromTar(tarStream, blobPath, nil)
	require.NoError(t, err)

	// Mount the blob
	err = mountEROFSBlob(blobPath, mountPoint)
	require.NoError(t, err)

	// Verify mount worked by checking files exist
	testFile := filepath.Join(mountPoint, "test.txt")
	content, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(content))

	nestedFile := filepath.Join(mountPoint, "testdir", "nested.txt")
	content, err = os.ReadFile(nestedFile)
	require.NoError(t, err)
	assert.Equal(t, "nested\n", string(content))

	// Clean up - unmount
	err = unmountEROFSBlob(mountPoint)
	require.NoError(t, err)
}
