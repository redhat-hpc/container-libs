package imagefs

import (
	"os"
	"path/filepath"
	"testing"

	graphdriver "go.podman.io/storage/drivers"
)

// TestImageFSInit tests that the imagefs driver can be initialized.
func TestImageFSInit(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "imagefs-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Test initialization with empty options
	opts := graphdriver.Options{
		Root:    tempDir,
		RunRoot: filepath.Join(tempDir, "run"),
	}

	driver, err := Init(filepath.Join(tempDir, "imagefs"), opts)
	if err != nil {
		t.Fatalf("Failed to initialize imagefs driver: %v", err)
	}

	if driver == nil {
		t.Fatal("Driver should not be nil after initialization")
	}

	// Verify the driver is of the correct type
	if _, ok := driver.(*ImageFS); !ok {
		t.Fatalf("Expected *ImageFS, got %T", driver)
	}

	// Verify the driver string representation
	if driver.String() != "imagefs" {
		t.Errorf("Expected driver name 'imagefs', got '%s'", driver.String())
	}
}

// TestImageFSMethodsNotImplemented tests that stubbed methods return appropriate errors.
func TestImageFSMethodsNotImplemented(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "imagefs-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Initialize the driver
	opts := graphdriver.Options{
		Root:    tempDir,
		RunRoot: filepath.Join(tempDir, "run"),
	}

	driver, err := Init(filepath.Join(tempDir, "imagefs"), opts)
	if err != nil {
		t.Fatalf("Failed to initialize imagefs driver: %v", err)
	}

	// Test Create method
	err = driver.Create("test-id", "", nil)
	if err == nil {
		t.Error("Create should return an error for stubbed driver")
	} else if err.Error() != "imagefs: Create not yet implemented" {
		t.Errorf("Expected 'imagefs: Create not yet implemented', got '%v'", err)
	}

	// Test Remove method
	err = driver.Remove("test-id")
	if err == nil {
		t.Error("Remove should return an error for stubbed driver")
	} else if err.Error() != "imagefs: Remove not yet implemented" {
		t.Errorf("Expected 'imagefs: Remove not yet implemented', got '%v'", err)
	}

	// Test Get method
	_, err = driver.Get("test-id", graphdriver.MountOpts{})
	if err == nil {
		t.Error("Get should return an error for stubbed driver")
	} else if err.Error() != "imagefs: Get not yet implemented" {
		t.Errorf("Expected 'imagefs: Get not yet implemented', got '%v'", err)
	}

	// Test Exists method (should return false, not an error)
	if driver.Exists("test-id") {
		t.Error("Exists should return false for stubbed driver")
	}
}

// TestImageFSString tests the String() method.
func TestImageFSString(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "imagefs-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	opts := graphdriver.Options{
		Root:    tempDir,
		RunRoot: filepath.Join(tempDir, "run"),
	}

	driver, err := Init(filepath.Join(tempDir, "imagefs"), opts)
	if err != nil {
		t.Fatalf("Failed to initialize imagefs driver: %v", err)
	}

	if driver.String() != "imagefs" {
		t.Errorf("Expected 'imagefs', got '%s'", driver.String())
	}
}
