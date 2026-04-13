package imagefs

import (
	"os"
	"path/filepath"
	"testing"

	graphdriver "go.podman.io/storage/drivers"
)

// TestImageFSInitFailure tests that the imagefs driver fails to initialize when tools are missing.
func TestImageFSInitFailure(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "imagefs-init-fail-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	opts := graphdriver.Options{
		Root:    tempDir,
		RunRoot: filepath.Join(tempDir, "run"),
	}

	// Since mkfs.erofs/tar2sqfs are likely missing in the test environment,
	// this should fail.
	driver, err := Init(filepath.Join(tempDir, "imagefs"), opts)
	if err == nil {
		t.Errorf("Expected error due to missing tools, but got nil. Driver: %v", driver)
	}
}

// TestImageFSBasicOperations tests the logic of the driver without requiring external tools.
func TestImageFSBasicOperations(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "imagefs-basic-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// We create a manual ImageFS instance to bypass the tool check in Init.
	d := &ImageFS{
		name:    "imagefs",
		home:    filepath.Join(tempDir, "imagefs"),
		options: &Options{Format: FormatEROFS},
	}
	os.MkdirAll(filepath.Join(d.home, "layers"), 0o700)

	layerID := "test-layer"
	layerPath := filepath.Join(d.home, "layers", layerID+".erofs")

	// Test Exists (should be false)
	if d.Exists(layerID) {
		t.Errorf("Expected layer %s to not exist", layerID)
	}

	// Create a dummy file to simulate a layer
	if err := os.WriteFile(layerPath, []byte("dummy content"), 0644); err != nil {
		t.Fatalf("Failed to create dummy layer file: %v", err)
	}

	// Test Exists (should be true)
	if !d.Exists(layerID) {
		t.Errorf("Expected layer %s to exist", layerID)
	}

	// Test ListLayers
	layers, err := d.ListLayers()
	if err != nil {
		t.Fatalf("ListLayers failed: %v", err)
	}
	if len(layers) != 1 || layers[0] != layerID {
		t.Errorf("Expected layers [%s], got %v", layerID, layers)
	}

	// Test Metadata
	meta, err := d.Metadata(layerID)
	if err != nil {
		t.Fatalf("Metadata failed: %v", err)
	}
	if meta["format"] != FormatEROFS {
		t.Errorf("Expected format %s, got %s", FormatEROFS, meta["format"])
	}

	// Test Remove
	if err := d.Remove(layerID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if d.Exists(layerID) {
		t.Errorf("Expected layer %s to be removed", layerID)
	}
}
