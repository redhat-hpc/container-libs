//go:build linux

package imagefs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	graphdriver "go.podman.io/storage/drivers"
)

// MockMounter is a simple mock implementation for testing without external dependencies
type MockMounter struct {
	MountCalls       []MountCall
	UnmountCalls     []string
	LazyUnmountCalls []string
	RunCommandCalls  []RunCommandCall

	MountError      error
	UnmountError    error
	RunCommandError error
}

type MountCall struct {
	Source  string
	Target  string
	FsType  string
	Options string
}

type RunCommandCall struct {
	Name string
	Args []string
}

func (m *MockMounter) Mount(source, target, fsType, options string) error {
	m.MountCalls = append(m.MountCalls, MountCall{
		Source:  source,
		Target:  target,
		FsType:  fsType,
		Options: options,
	})
	return m.MountError
}

func (m *MockMounter) Unmount(target string) error {
	m.UnmountCalls = append(m.UnmountCalls, target)
	return m.UnmountError
}

func (m *MockMounter) LazyUnmount(target string) error {
	m.LazyUnmountCalls = append(m.LazyUnmountCalls, target)
	return m.UnmountError
}

func (m *MockMounter) RunCommand(name string, args ...string) error {
	m.RunCommandCalls = append(m.RunCommandCalls, RunCommandCall{
		Name: name,
		Args: args,
	})
	return m.RunCommandError
}

func TestDriver_Get_MergedErofsStrategy(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()

	mockMounter := &MockMounter{}
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
		os.WriteFile(filepath.Join(dir, "layer.erofs"), []byte("dummy"), 0644)
		os.WriteFile(filepath.Join(dir, "layer.erofs.tar"), []byte("dummy-tar"), 0644)
	}

	mergedDir, err := d.Get("L1", graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")

	// Verify mkfs.erofs was called for merging
	hasRunCommand := false
	for _, call := range mockMounter.RunCommandCalls {
		if call.Name == "mkfs.erofs" {
			hasRunCommand = true
			break
		}
	}
	assert.True(t, hasRunCommand, "Expected mkfs.erofs to be called for merged strategy")
}

func TestDriver_Get_SeparateLayersStrategy(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()

	mockMounter := &MockMounter{}
	d := &Driver{
		home:    tmpDir,
		runRoot: runRoot,
		mm: &MountManager{
			runRoot: runRoot,
			mounter: mockMounter,
		},
	}

	// Setup layers: L1 (.erofs) -> L2 (.sqsh) -> L3 (.erofs)
	// Mixed types should use separate layers strategy
	layers := []string{"L1", "L2", "L3"}
	exts := []string{".erofs", ".sqsh", ".erofs"}
	for i, l := range layers {
		dir := filepath.Join(tmpDir, l)
		os.MkdirAll(dir, 0755)
		if i < len(layers)-1 {
			os.WriteFile(filepath.Join(dir, "parent"), []byte(layers[i+1]), 0644)
		}
		os.WriteFile(filepath.Join(dir, "layer"+exts[i]), []byte("dummy"), 0644)
	}

	mergedDir, err := d.Get("L1", graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")

	// Verify FUSE mounts were attempted (erofsfuse or squashfuse)
	hasFuseMount := false
	for _, call := range mockMounter.RunCommandCalls {
		if call.Name == "erofsfuse" || call.Name == "squashfuse" {
			hasFuseMount = true
			break
		}
	}
	assert.True(t, hasFuseMount, "Expected FUSE mount commands for separate layers")
}

func TestDriver_Put(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()

	mockMounter := &MockMounter{}
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

	err := d.Put("L1")
	assert.NoError(t, err)

	// Verify unmount was called
	assert.True(t, len(mockMounter.UnmountCalls) > 0 || len(mockMounter.LazyUnmountCalls) > 0,
		"Expected unmount to be called during Put")
}

func TestDriver_Get_EmptyLayers(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()

	mockMounter := &MockMounter{}
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
	os.WriteFile(filepath.Join(dir, "layer.erofs"), []byte("dummy"), 0644)
	os.WriteFile(filepath.Join(dir, "layer.erofs.tar"), []byte("dummy-tar"), 0644)

	mergedDir, err := d.Get(layerID, graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")

	// Verify that at least one mount was called
	assert.True(t, len(mockMounter.MountCalls) > 0 || len(mockMounter.RunCommandCalls) > 0,
		"Expected mount operations for base layer")
}

func TestDriver_Get_Diff(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()

	mockMounter := &MockMounter{}

	// Create the driver using Init so naiveDiff is properly set up
	d, err := Init(tmpDir, graphdriver.Options{
		RunRoot:       runRoot,
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
		os.WriteFile(filepath.Join(dir, "layer.erofs"), []byte("dummy"), 0644)
		os.WriteFile(filepath.Join(dir, "layer.erofs.tar"), []byte("dummy-tar"), 0644)
	}

	// Test Diff method - should delegate to naiveDiff
	// This tests that the Diff method is properly implemented
	arch, err := d.Diff("L2", nil, "L1", nil, "")
	assert.NoError(t, err)
	assert.NotNil(t, arch)

	// Close the archive
	arch.Close()
}

func TestDriver_Get_WorkingContainerLayer(t *testing.T) {
	// Tests that a working container layer (no layer.erofs) uses the overlay
	// upperdir for writes, while still seeing committed parent layers.
	// This is the case during a RUN step in podman build.
	tmpDir := t.TempDir()
	runRoot := t.TempDir()

	mockMounter := &MockMounter{}
	d := &Driver{
		home:    tmpDir,
		runRoot: runRoot,
		mm: &MountManager{
			runRoot: runRoot,
			mounter: mockMounter,
		},
	}

	// Setup: base layer (committed, has layer.erofs) and a container layer (no layer.erofs)
	baseLayer := "base"
	baseDir := filepath.Join(tmpDir, baseLayer)
	os.MkdirAll(baseDir, 0755)
	os.WriteFile(filepath.Join(baseDir, "layer.erofs"), []byte("busybox"), 0644)
	os.WriteFile(filepath.Join(baseDir, "layer.erofs.tar"), []byte("busybox-tar"), 0644)

	containerLayer := "container1"
	containerDir := filepath.Join(tmpDir, containerLayer)
	os.MkdirAll(containerDir, 0755)
	os.WriteFile(filepath.Join(containerDir, "parent"), []byte(baseLayer), 0644)
	// NO layer.erofs — this is a working container layer

	mergedDir, err := d.Get(containerLayer, graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")

	// Verify that mount calls happened
	assert.True(t, len(mockMounter.MountCalls) > 0 || len(mockMounter.RunCommandCalls) > 0,
		"Expected mount operations for working container layer")
}

func TestDriver_Get_DockerfileScenario(t *testing.T) {
	tmpDir := t.TempDir()
	runRoot := t.TempDir()

	mockMounter := &MockMounter{}
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
	os.WriteFile(filepath.Join(baseDir, "layer.erofs"), []byte("busybox-dummy"), 0644)
	os.WriteFile(filepath.Join(baseDir, "layer.erofs.tar"), []byte("busybox-tar"), 0644)

	// Layer 2: after first RUN
	layer1 := "layer1"
	layer1Dir := filepath.Join(tmpDir, layer1)
	os.MkdirAll(layer1Dir, 0755)
	os.WriteFile(filepath.Join(layer1Dir, "parent"), []byte(baseLayer), 0644)
	os.WriteFile(filepath.Join(layer1Dir, "layer.erofs"), []byte("layer1-dummy"), 0644)
	os.WriteFile(filepath.Join(layer1Dir, "layer.erofs.tar"), []byte("layer1-tar"), 0644)

	// Layer 3: after second RUN
	layer2 := "layer2"
	layer2Dir := filepath.Join(tmpDir, layer2)
	os.MkdirAll(layer2Dir, 0755)
	os.WriteFile(filepath.Join(layer2Dir, "parent"), []byte(layer1), 0644)
	os.WriteFile(filepath.Join(layer2Dir, "layer.erofs"), []byte("layer2-dummy"), 0644)
	os.WriteFile(filepath.Join(layer2Dir, "layer.erofs.tar"), []byte("layer2-tar"), 0644)

	// Layer 4: after third RUN (the final layer we want to mount)
	layer3 := "layer3"
	layer3Dir := filepath.Join(tmpDir, layer3)
	os.MkdirAll(layer3Dir, 0755)
	os.WriteFile(filepath.Join(layer3Dir, "parent"), []byte(layer2), 0644)
	os.WriteFile(filepath.Join(layer3Dir, "layer.erofs"), []byte("layer3-dummy"), 0644)
	os.WriteFile(filepath.Join(layer3Dir, "layer.erofs.tar"), []byte("layer3-tar"), 0644)

	// Test mounting the final layer (layer3)
	mergedDir, err := d.Get(layer3, graphdriver.MountOpts{})
	assert.NoError(t, err)
	assert.Contains(t, mergedDir, "merged")

	// Verify mount operations happened
	assert.True(t, len(mockMounter.MountCalls) > 0 || len(mockMounter.RunCommandCalls) > 0,
		"Expected mount operations for multi-layer scenario")
}
