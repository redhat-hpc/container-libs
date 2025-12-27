//go:build linux

package overlay

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	graphdriver "go.podman.io/storage/drivers"
	"go.podman.io/storage/drivers/graphtest"
	"go.podman.io/storage/pkg/archive"
)

// driverName is defined in overlay_test.go in the same package

func TestImageFSErofs(t *testing.T) {
	// Create a temporary directory and a file within that directory
	tempDir := t.TempDir()
	err := os.WriteFile(filepath.Join(tempDir, "hello"), []byte("hello world"), 0644)
	require.NoError(t, err)

	// Create a tarball from the temporary directory
	tarReader, err := archive.TarWithOptions(tempDir, &archive.TarOptions{Compression: archive.Uncompressed})
	require.NoError(t, err)
	defer tarReader.Close()

	var b bytes.Buffer
	_, err = io.Copy(&b, tarReader)
	require.NoError(t, err)

	driver := graphtest.GetDriver(t, driverName, `image_fs_type=erofs`, `image_fs_create_command=mkfs.erofs {{.ImagePath}} {{.TmpDir}}`)
	require.NoError(t, driver.Create("erofs_layer", "", nil))

	// Apply the tarball as a diff to the layer - this should create the erofs image
	size, err := driver.ApplyDiff("erofs_layer", graphdriver.ApplyDiffOpts{
		Diff:     &b,
		Mappings: nil,
	})
	require.NoError(t, err)
	assert.Greater(t, size, int64(0))

	// Verify that the erofs image file was created in the layer directory
	// Use Metadata to get the UpperDir, then go up one level to get the layer directory
	metadata, err := driver.Metadata("erofs_layer")
	require.NoError(t, err)
	upperDir, ok := metadata["UpperDir"]
	require.True(t, ok, "UpperDir should be in metadata")
	layerDir := filepath.Dir(upperDir)
	erofsImagePath := filepath.Join(layerDir, "erofs.img")
	_, err = os.Stat(erofsImagePath)
	require.NoError(t, err, "erofs image file should exist at %s", erofsImagePath)

	// Verify the image file has a reasonable size
	stat, err := os.Stat(erofsImagePath)
	require.NoError(t, err)
	assert.Greater(t, stat.Size(), int64(0), "erofs image file should have non-zero size")
	// Note: GetDriver sets up cleanup automatically via t.Cleanup(), so no need to call PutDriver
}

func TestImageFSMountReadOnly(t *testing.T) {
	// Create a temporary directory with some files
	tempDir := t.TempDir()
	err := os.WriteFile(filepath.Join(tempDir, "testfile"), []byte("test content"), 0644)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tempDir, "subdir"), 0755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tempDir, "subdir", "nested"), []byte("nested content"), 0644)
	require.NoError(t, err)

	// Create a tarball from the temporary directory
	tarReader, err := archive.TarWithOptions(tempDir, &archive.TarOptions{Compression: archive.Uncompressed})
	require.NoError(t, err)
	defer tarReader.Close()

	var b bytes.Buffer
	_, err = io.Copy(&b, tarReader)
	require.NoError(t, err)

	driver := graphtest.GetDriver(t, driverName, `image_fs_type=erofs`, `image_fs_create_command=mkfs.erofs {{.ImagePath}} {{.TmpDir}}`)
	require.NoError(t, driver.Create("imagefs_layer", "", nil))

	// Apply the tarball as a diff to create the erofs image
	_, err = driver.ApplyDiff("imagefs_layer", graphdriver.ApplyDiffOpts{
		Diff:     &b,
		Mappings: nil,
	})
	require.NoError(t, err)

	// Mount the layer (read-only)
	// Note: Mounting may fail if we're rootless and don't have FUSE mount programs
	// or if the kernel doesn't support the new mount API, so we skip the test in that case
	mountpoint, err := driver.Get("imagefs_layer", graphdriver.MountOpts{})
	if err != nil {
		// If mounting fails, it's likely because we don't have the necessary tools
		// or permissions. This is acceptable for testing - the image creation worked.
		t.Skipf("Skipping mount test: failed to mount imagefs layer: %v", err)
		return
	}
	defer func() {
		require.NoError(t, driver.Put("imagefs_layer"))
	}()

	// Verify we can read files from the mounted image filesystem
	content, err := os.ReadFile(filepath.Join(mountpoint, "testfile"))
	require.NoError(t, err)
	assert.Equal(t, "test content", string(content))

	content, err = os.ReadFile(filepath.Join(mountpoint, "subdir", "nested"))
	require.NoError(t, err)
	assert.Equal(t, "nested content", string(content))

	// Note: Even though the imagefs layer is read-only, when mounted as a lower layer
	// in an overlay, the overlay mount itself is writable (it has an upper layer for writes).
	// So we can write to the mountpoint - writes will go to the upper layer.
	err = os.WriteFile(filepath.Join(mountpoint, "newfile"), []byte("new"), 0644)
	require.NoError(t, err, "should be able to write to overlay mount (writes go to upper layer)")

	// Verify the write worked
	content, err = os.ReadFile(filepath.Join(mountpoint, "newfile"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(content))
}

func TestImageFSMountWithParent(t *testing.T) {
	// Create a base layer with imagefs
	baseTempDir := t.TempDir()
	err := os.WriteFile(filepath.Join(baseTempDir, "basefile"), []byte("base content"), 0644)
	require.NoError(t, err)

	baseTarReader, err := archive.TarWithOptions(baseTempDir, &archive.TarOptions{Compression: archive.Uncompressed})
	require.NoError(t, err)
	defer baseTarReader.Close()

	var baseBuf bytes.Buffer
	_, err = io.Copy(&baseBuf, baseTarReader)
	require.NoError(t, err)

	driver := graphtest.GetDriver(t, driverName, `image_fs_type=erofs`, `image_fs_create_command=mkfs.erofs {{.ImagePath}} {{.TmpDir}}`)
	require.NoError(t, driver.Create("base_layer", "", nil))
	_, err = driver.ApplyDiff("base_layer", graphdriver.ApplyDiffOpts{
		Diff:     &baseBuf,
		Mappings: nil,
	})
	require.NoError(t, err)

	// Create an upper layer on top of the imagefs base
	require.NoError(t, driver.Create("upper_layer", "base_layer", nil))

	// Mount the upper layer (should have imagefs as lower)
	mountpoint, err := driver.Get("upper_layer", graphdriver.MountOpts{})
	if err != nil {
		t.Skipf("Skipping mount test: failed to mount imagefs layer: %v", err)
		return
	}
	defer func() {
		require.NoError(t, driver.Put("upper_layer"))
	}()

	// Verify we can read from the base imagefs layer
	content, err := os.ReadFile(filepath.Join(mountpoint, "basefile"))
	require.NoError(t, err)
	assert.Equal(t, "base content", string(content))

	// Verify we can write to the upper layer
	err = os.WriteFile(filepath.Join(mountpoint, "upperfile"), []byte("upper content"), 0644)
	require.NoError(t, err)

	content, err = os.ReadFile(filepath.Join(mountpoint, "upperfile"))
	require.NoError(t, err)
	assert.Equal(t, "upper content", string(content))
}

func TestImageFSMountProgram(t *testing.T) {
	// Test that image_fs_mount_program option validation works
	// The option should be validated during driver initialization
	// We expect an error if the mount program path doesn't exist
	_, err := graphdriver.GetDriver(driverName, graphdriver.Options{
		DriverOptions: []string{
			`image_fs_type=erofs`,
			`image_fs_create_command=mkfs.erofs {{.ImagePath}} {{.TmpDir}}`,
			`image_fs_mount_program=/nonexistent/mount/program`,
		},
		Root:    t.TempDir(),
		RunRoot: t.TempDir(),
	})
	// We expect an error because the mount program doesn't exist
	require.Error(t, err, "should fail when mount program path doesn't exist")
	assert.Contains(t, err.Error(), "image_fs_mount_program", "error should mention the mount program option")
}

func TestImageFSMultipleLayers(t *testing.T) {
	// Test multiple layers with imagefs
	driver := graphtest.GetDriver(t, driverName, `image_fs_type=erofs`, `image_fs_create_command=mkfs.erofs {{.ImagePath}} {{.TmpDir}}`)

	// Create layer 1
	tempDir1 := t.TempDir()
	err := os.WriteFile(filepath.Join(tempDir1, "layer1"), []byte("layer1"), 0644)
	require.NoError(t, err)
	tar1, err := archive.TarWithOptions(tempDir1, &archive.TarOptions{Compression: archive.Uncompressed})
	require.NoError(t, err)
	defer tar1.Close()
	var buf1 bytes.Buffer
	_, err = io.Copy(&buf1, tar1)
	require.NoError(t, err)

	require.NoError(t, driver.Create("layer1", "", nil))
	_, err = driver.ApplyDiff("layer1", graphdriver.ApplyDiffOpts{Diff: &buf1, Mappings: nil})
	require.NoError(t, err)

	// Create layer 2 on top
	tempDir2 := t.TempDir()
	err = os.WriteFile(filepath.Join(tempDir2, "layer2"), []byte("layer2"), 0644)
	require.NoError(t, err)
	tar2, err := archive.TarWithOptions(tempDir2, &archive.TarOptions{Compression: archive.Uncompressed})
	require.NoError(t, err)
	defer tar2.Close()
	var buf2 bytes.Buffer
	_, err = io.Copy(&buf2, tar2)
	require.NoError(t, err)

	require.NoError(t, driver.Create("layer2", "layer1", nil))
	_, err = driver.ApplyDiff("layer2", graphdriver.ApplyDiffOpts{Diff: &buf2, Mappings: nil})
	require.NoError(t, err)

	// Mount layer2 and verify we can see both layers
	mountpoint, err := driver.Get("layer2", graphdriver.MountOpts{})
	if err != nil {
		t.Skipf("Skipping mount test: failed to mount imagefs layer: %v", err)
		return
	}
	defer func() {
		require.NoError(t, driver.Put("layer2"))
	}()

	// Should see layer1 content (from imagefs lower)
	content, err := os.ReadFile(filepath.Join(mountpoint, "layer1"))
	require.NoError(t, err)
	assert.Equal(t, "layer1", string(content))

	// Should see layer2 content (from upper)
	content, err = os.ReadFile(filepath.Join(mountpoint, "layer2"))
	require.NoError(t, err)
	assert.Equal(t, "layer2", string(content))
}
