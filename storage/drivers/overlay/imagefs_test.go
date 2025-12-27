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

	driver := graphtest.GetDriver(t, driverName, `image_fs_type=erofs`, `image_fs_create_command=mkfs.erofs`)
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
