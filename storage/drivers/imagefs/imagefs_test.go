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

func TestDriver_Get_EmptyLayers(t *testing.T) {
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

	// Setup a single layer with no parent (like a base image)
	layerID := "base"
	dir := filepath.Join(tmpDir, layerID)
	os.MkdirAll(dir, 0755)
	// No parent file - this is the base layer
	os.WriteFile(filepath.Join(dir, "data.img"), []byte("dummy"), 0644)
	os.WriteFile(filepath.Join(dir, "data.img.tar"), []byte("dummy-tar"), 0644)

	// Mock all RunCommand calls
	mockMounter.On("RunCommand", mock.Anything, mock.Anything).Return(nil)

	// Mock EROFS mount for the base layer
	mockMounter.On("Mount", mock.Anything, mock.Anything, "erofs", mock.Anything).Return(nil)
	
	// Mock overlay mount with the base layer as lowerdir
	mockMounter.On("Mount", "overlay", mock.Anything, "overlay", mock.Anything).Return(nil)
	
	// Mock cleanup calls
	mockMounter.On("LazyUnmount", mock.Anything).Return(nil)

	mergedDir, err := d.Get(layerID, graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")
	
	// Verify that the overlay mount was called with a valid lowerdir
	mockMounter.AssertCalled(t, "Mount", "overlay", mock.Anything, "overlay", mock.Anything)
}

func TestDriver_Get_Diff(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()

	mockMounter := new(MockMounter)
	
	// Create the driver using Init so naiveDiff is properly set up
	d, err := Init(tmpDir, graphdriver.Options{
		RunRoot: runRoot,
		DriverOptions: []string{},
	})
	assert.NoError(t, err)
	
	// Replace the MountManager with our mock
	imagefsDriver := d.(*Driver)
	imagefsDriver.mm = &MountManager{
		runRoot: runRoot,
		mounter: mockMounter,
	}
	
	// Reset naiveDiff to use the mock MountManager
	imagefsDriver.naiveDiff = graphdriver.NewNaiveDiffDriver(imagefsDriver, graphdriver.NewNaiveLayerIDMapUpdater(imagefsDriver))

	// Setup layers: L1 -> L2 (L2 is the top)
	layers := []string{"L1", "L2"}
	for i, l := range layers {
		dir := filepath.Join(tmpDir, l)
		os.MkdirAll(dir, 0755)
		if i < len(layers)-1 {
			os.WriteFile(filepath.Join(dir, "parent"), []byte(layers[i+1]), 0644)
		}
		os.WriteFile(filepath.Join(dir, "data.img"), []byte("dummy"), 0644)
		os.WriteFile(filepath.Join(dir, "data.img.tar"), []byte("dummy-tar"), 0644)
	}

	// Mock all RunCommand calls
	mockMounter.On("RunCommand", mock.Anything, mock.Anything).Return(nil)

	// Mock EROFS mounts
	mockMounter.On("Mount", mock.Anything, mock.Anything, "erofs", mock.Anything).Return(nil)
	
	// Mock overlay mounts
	mockMounter.On("Mount", "overlay", mock.Anything, "overlay", mock.Anything).Return(nil)
	
	// Mock cleanup calls
	mockMounter.On("LazyUnmount", mock.Anything).Return(nil)
	mockMounter.On("Unmount", mock.Anything).Return(nil)

	// Test Diff method - should delegate to naiveDiff
	// This tests that the Diff method is properly implemented
	arch, err := d.Diff("L2", nil, "L1", nil, "")
	assert.NoError(t, err)
	assert.NotNil(t, arch)
	
	// Close the archive
	arch.Close()
}

func TestDriver_Get_WorkingContainerLayer(t *testing.T) {
	// Tests that a working container layer (no data.img) uses the overlay
	// upperdir for writes, while still seeing committed parent layers.
	// This is the case during a RUN step in podman build.
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

	// Setup: base layer (committed, has data.img) and a container layer (no data.img)
	baseLayer := "base"
	baseDir := filepath.Join(tmpDir, baseLayer)
	os.MkdirAll(baseDir, 0755)
	os.WriteFile(filepath.Join(baseDir, "data.img"), []byte("busybox"), 0644)
	os.WriteFile(filepath.Join(baseDir, "data.img.tar"), []byte("busybox-tar"), 0644)

	containerLayer := "container1"
	containerDir := filepath.Join(tmpDir, containerLayer)
	os.MkdirAll(containerDir, 0755)
	os.WriteFile(filepath.Join(containerDir, "parent"), []byte(baseLayer), 0644)
	// NO data.img — this is a working container layer

	mockMounter.On("RunCommand", mock.Anything, mock.Anything).Return(nil)
	mockMounter.On("Mount", mock.Anything, mock.Anything, "erofs", mock.Anything).Return(nil)
	mockMounter.On("Mount", "overlay", mock.Anything, "overlay", mock.Anything).Return(nil)
	mockMounter.On("LazyUnmount", mock.Anything).Return(nil)

	mergedDir, err := d.Get(containerLayer, graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")

	// Verify that the overlay mount was called.
	// The container layer should NOT have its own EROFS mount (no data.img),
	// so only the base layer's EROFS is a lowerdir, and container1/upper is the upperdir.
	overlayCall := mockMounter.Calls[len(mockMounter.Calls)-1]
	assert.Equal(t, "Mount", overlayCall.Method)
	overlayOpts := overlayCall.Arguments.Get(3).(string)
	// The overlay options should have upperdir=container1/upper
	assert.Contains(t, overlayOpts, filepath.Join(containerDir, "upper"))
	// The lowerdir should NOT contain a mount for containerLayer itself
	assert.NotContains(t, overlayOpts, filepath.Join(runRoot, "imagefs", containerLayer, containerLayer))
}

func TestDriver_Get_DockerfileScenario(t *testing.T) {
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

	// Simulate the Dockerfile scenario:
	// FROM busybox:latest  -> creates base layer (no parent)
	// RUN touch /root/a.txt -> creates layer1 (parent: base)
	// RUN rm /root/a.txt   -> creates layer2 (parent: layer1)
	// RUN touch /root/b.txt -> creates layer3 (parent: layer2)

	// Layer 1: base image (busybox) - no parent
	baseLayer := "base"
	baseDir := filepath.Join(tmpDir, baseLayer)
	os.MkdirAll(baseDir, 0755)
	// getImagePath looks for data.img or data.sqsh
	os.WriteFile(filepath.Join(baseDir, "data.img"), []byte("busybox-dummy"), 0644)
	os.WriteFile(filepath.Join(baseDir, "data.img.tar"), []byte("busybox-tar"), 0644)

	// Layer 2: after first RUN
	layer1 := "layer1"
	layer1Dir := filepath.Join(tmpDir, layer1)
	os.MkdirAll(layer1Dir, 0755)
	os.WriteFile(filepath.Join(layer1Dir, "parent"), []byte(baseLayer), 0644)
	os.WriteFile(filepath.Join(layer1Dir, "data.img"), []byte("layer1-dummy"), 0644)
	os.WriteFile(filepath.Join(layer1Dir, "data.img.tar"), []byte("layer1-tar"), 0644)

	// Layer 3: after second RUN
	layer2 := "layer2"
	layer2Dir := filepath.Join(tmpDir, layer2)
	os.MkdirAll(layer2Dir, 0755)
	os.WriteFile(filepath.Join(layer2Dir, "parent"), []byte(layer1), 0644)
	os.WriteFile(filepath.Join(layer2Dir, "data.img"), []byte("layer2-dummy"), 0644)
	os.WriteFile(filepath.Join(layer2Dir, "data.img.tar"), []byte("layer2-tar"), 0644)

	// Layer 4: after third RUN (the final layer we want to mount)
	layer3 := "layer3"
	layer3Dir := filepath.Join(tmpDir, layer3)
	os.MkdirAll(layer3Dir, 0755)
	os.WriteFile(filepath.Join(layer3Dir, "parent"), []byte(layer2), 0644)
	os.WriteFile(filepath.Join(layer3Dir, "data.img"), []byte("layer3-dummy"), 0644)
	os.WriteFile(filepath.Join(layer3Dir, "data.img.tar"), []byte("layer3-tar"), 0644)

	// Mock all RunCommand calls
	mockMounter.On("RunCommand", mock.Anything, mock.Anything).Return(nil)

	// Mock EROFS mounts
	mockMounter.On("Mount", mock.Anything, mock.Anything, "erofs", mock.Anything).Return(nil)
	
	// Mock overlay mounts
	mockMounter.On("Mount", "overlay", mock.Anything, "overlay", mock.Anything).Return(nil)
	
	// Mock cleanup calls
	mockMounter.On("LazyUnmount", mock.Anything).Return(nil)

	// Test mounting the final layer (layer3)
	// This should:
	// 1. Get layer stack: [base, layer1, layer2, layer3]
	// 2. Remove top layer: [base, layer1, layer2]
	// 3. Mount base layer directly (empty layers case)
	// 4. Mount layer1 and layer2 as separate layers
	// 5. Final overlay mount with all lowerdirs
	mergedDir, err := d.Get(layer3, graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")
	
	// Verify overlay mount was called
	mockMounter.AssertCalled(t, "Mount", "overlay", mock.Anything, "overlay", mock.Anything)
}
