package imagefs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	graphdriver "go.podman.io/storage/drivers"
)

type MockMounter struct {
	mock.Mock
}

func (m *MockMounter) Mount(source, target, fsType, options string) error {
	args := m.Called(source, target, fsType, options)
	return args.Error(0)
}

func (m *MockMounter) Unmount(target string) error {
	args := m.Called(target)
	return args.Error(0)
}

func (m *MockMounter) RunCommand(name string, args ...string) error {
	// We use a slice for the call to match how we pass arguments in the mock
	argsSlice := make([]string, 0, len(args)+1)
	argsSlice = append(argsSlice, name)
	argsSlice = append(argsSlice, args...)
	
	mCalled := m.Called(argsSlice)
	return mCalled.Error(0)
}

func TestDriver_Get(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()
	
	mockMounter := new(MockMounter)
	d := &Driver{
		home:    tmpDir,
		runRoot: runRoot,
		mm: &MountManager{
			runRoot: runRoot,
			mounter: mockMounter,
		},
	}

	// Setup layers: L1 -> L2 -> L3 (L3 is the top)
	layers := []string{"L1", "L2", "L3"}
	for i, l := range layers {
		dir := filepath.Join(tmpDir, l)
		os.MkdirAll(dir, 0755)
		if i < len(layers)-1 {
			os.WriteFile(filepath.Join(dir, "parent"), []byte(layers[i+1]), 0644)
		}
		// Create dummy image file
		os.WriteFile(filepath.Join(tmpDir, l+".img"), []byte("dummy"), 0644)
	}

	// Mock mount calls for each layer
	// Use mock.Anything for options because they might vary (nodev,nosuid)
	mockMounter.On("Mount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe().Return(nil)
	mockMounter.On("RunCommand", mock.Anything).Maybe().Return(nil)
	
	// Mock the final overlay mount
	mockMounter.On("Mount", "overlay", mock.Anything, "overlay", mock.Anything).Return(nil)

	mergedDir, err := d.Get("L1", graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")
	
	mockMounter.AssertExpectations(t)
}

func TestDriver_Put(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()
	
	mockMounter := new(MockMounter)
	d := &Driver{
		home:    tmpDir,
		runRoot: runRoot,
		mm: &MountManager{
			runRoot: runRoot,
			mounter: mockMounter,
		},
	}

	// Setup dummy merged dir
	mergedDir := filepath.Join(tmpDir, "L1", "merged")
	os.MkdirAll(mergedDir, 0755)

	mockMounter.On("Unmount", mergedDir).Return(nil)

	err := d.Put("L1")
	assert.NoError(t, err)
	
	mockMounter.AssertExpectations(t)
}
