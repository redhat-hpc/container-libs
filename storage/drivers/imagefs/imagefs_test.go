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

func (m *MockMounter) LazyUnmount(target string) error {
	args := m.Called(target)
	return args.Error(0)
}

func (m *MockMounter) RunCommand(name string, args ...string) error {
	argsSlice := make([]string, 0, len(args)+1)
	argsSlice = append(argsSlice, name)
	argsSlice = append(argsSlice, args...)
	mCalled := m.Called(argsSlice)
	return mCalled.Error(0)
}

func TestDriver_Get(t *testing.T) {
	t.Run("MergedErofsStrategy", func(t *testing.T) {
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
			os.WriteFile(filepath.Join(dir, "data.img"), []byte("dummy"), 0644)
			os.WriteFile(filepath.Join(dir, "data.img.tar"), []byte("dummy-tar"), 0644)
		}

		// Mock all RunCommand calls including mkfs.erofs
		mockMounter.On("RunCommand", mock.Anything, mock.Anything).Return(nil)

		mockMounter.On("Mount", mock.Anything, mock.Anything, "erofs", mock.Anything).Return(nil)
		mockMounter.On("Mount", "overlay", mock.Anything, "overlay", mock.Anything).Return(nil)
		
		// Mock cleanup calls
		mockMounter.On("LazyUnmount", mock.Anything).Return(nil)

		mergedDir, err := d.Get("L1", graphdriver.MountOpts{})
		assert.NoError(t, err)
		assert.Contains(t, mergedDir, "merged")
	})

	t.Run("SeparateLayersStrategy", func(t *testing.T) {
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

		// Setup layers: L1 (.img) -> L2 (.sqsh) -> L3 (.img)
		layers := []string{"L1", "L2", "L3"}
		exts := []string{".img", ".sqsh", ".img"}
		for i, l := range layers {
			dir := filepath.Join(tmpDir, l)
			os.MkdirAll(dir, 0755)
			if i < len(layers)-1 {
				os.WriteFile(filepath.Join(dir, "parent"), []byte(layers[i+1]), 0644)
			}
			os.WriteFile(filepath.Join(dir, "data"+exts[i]), []byte("dummy"), 0644)
		}

		// Mock FUSE mounts for separate layers
		mockMounter.On("RunCommand", mock.MatchedBy(func(args []string) bool {
			return args[0] == "erofsfuse" || args[0] == "squashfuse"
		})).Return(nil)
		
		mockMounter.On("Mount", "overlay", mock.Anything, "overlay", mock.Anything).Return(nil)
		
		// Mock cleanup calls
		mockMounter.On("LazyUnmount", mock.Anything).Return(nil)

		mergedDir, err := d.Get("L1", graphdriver.MountOpts{})
		assert.NoError(t, err)
		assert.Contains(t, mergedDir, "merged")
	})
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
