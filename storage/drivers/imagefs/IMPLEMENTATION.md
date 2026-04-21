# imagefs Storage Driver - Implementation Guide

## Overview

The `imagefs` driver provides efficient container storage using immutable EROFS image files layered via OverlayFS. This approach minimizes the number of files on the host disk and leverages the efficiency of read-only compressed filesystems.

## Architecture

### Storage Layout

```
storage/imagefs/<layer_id>/
├── layer.erofs           # EROFS image file
├── layer.erofs.tar       # Original tarball (for tar-split reconstruction)
├── parent             # Parent layer ID (if applicable)
├── upper/             # Writable overlay layer
├── work/              # OverlayFS working directory
└── merged/            # Final mounted root filesystem
```

### Mount Stack

The driver creates container filesystems through a multi-stage mounting process:

1. **Layer Resolution**: Traverse the image hierarchy to identify all required layers
2. **Base Mounts**: Mount each immutable EROFS image into a volatile directory (`/run/user/<uid>/containers/imagefs/...`)
   - **Privileged Mode**: Uses kernel mounts (`mount -t erofs`)
   - **Rootless Mode**: Falls back to FUSE (`erofsfuse`) when kernel mount fails with EPERM
   - **Security**: All mounts use `nodev` and `nosuid` flags
3. **Overlay Composition**: Create an OverlayFS mount with:
   - `lowerdir`: Sequence of mounted EROFS image layers (topmost first)
   - `upperdir`: Local directory for container writes
   - `workdir`: OverlayFS internal operations directory

### EROFS Features

The driver leverages EROFS (Enhanced Read-Only File System) capabilities:
- **Legacy Compression**: Uses `-E legacy-compress` for broad compatibility
- **Tar Input**: Creates EROFS images directly from tarballs using `--tar=i`
- **Whiteout Handling**: Uses `--aufs` flag to convert `.wh.*` files to overlayfs character devices
- **Metadata Storage**: Stores device files in separate `.tar` file for FUSE mounting

## Core Implementation

### Pulling Images (`ApplyDiff`)

When pulling layers, the driver:

1. **Saves Original Tarball**: Writes the incoming diff stream to `layer.erofs.tar`
   - This preserves the exact tar structure for tar-split reconstruction
   - Critical for maintaining correct digests when pushing images
   
2. **Creates EROFS Image**: Runs `mkfs.erofs --tar=i --aufs` on the tarball
   - `--tar=i`: Use tarball as filesystem source
   - `--aufs`: Automatically convert `.wh.*` whiteout files to character devices (c 0 0)
   - No extraction/re-tarring needed - single pass operation

```go
func (d *Driver) ApplyDiff(id string, options graphdriver.ApplyDiffOpts) (int64, error) {
    // Save original tarball for tar-split
    writeToFile(options.Diff, tarballPath)
    
    // Create EROFS with automatic whiteout conversion
    mkfs.erofs --tar=i --aufs -E legacy-compress layer.erofs tarball.tar
}
```

### Mounting Layers (`Get`)

The driver has two mount modes:

#### Read-Only Mounts
Used for tar-split reconstruction during image push:
- Detects `"ro"` in `options.Options`
- Mounts the EROFS image directly (no overlay)
- Returns the mount point for file access

#### Read-Write Mounts
Used for container execution:
- Mounts parent layers as read-only lowerdirs
- Mounts the requested layer's EROFS (if committed)
- Creates overlay with upperdir/workdir for writes

#### Merged EROFS Strategy
For EROFS-only layer stacks on kernel 5.14+:
- Merges multiple EROFS images into a single image
- Reduces mount count and improves performance
- Falls back to separate mounts for mixed layer types

### Exporting Layers (`Diff`)

The driver implements optimized diff logic based on layer type:

```go
func (d *Driver) Diff(id, parent string) (io.ReadCloser, error) {
    // For committed image layers with no parent:
    // Return the original tarball to preserve digests
    if parent == "" && isCommittedLayer {
        return os.Open(imagePath + ".tar")
    }
    
    // For working container layers (no layer.erofs):
    // Tar the upperdir directly to avoid device/inode mismatches
    if !isCommittedLayer {
        return archive.TarWithOptions(upperdir, &archive.TarOptions{
            WhiteoutFormat: archive.OverlayWhiteoutFormat,
        })
    }
    
    // For committed layers with parents: use naive diff
    return d.naiveDiff.Diff(...)
}
```

## Key Features & Fixes

### Whiteout/Deletion Handling

**Problem**: Files deleted in a layer (via `rm` command) were still appearing in the final container.

**Root Cause**: EROFS images created without proper whiteout conversion stored `.wh.*` files as regular files instead of character devices that overlayfs recognizes.

**Solution**: Use `mkfs.erofs --aufs` flag to automatically convert `.wh.*` files to overlayfs whiteout character devices (c 0 0) during EROFS creation.

**Example**:
```dockerfile
FROM busybox:latest
RUN touch /root/a.txt
RUN rm /root/a.txt     # ← Now properly deleted
RUN touch /root/b.txt
```

Result: Only `b.txt` exists in the final container.

### Digest Preservation for Image Push

**Problem**: Pushing images to a registry failed with digest mismatch errors.

**Root Cause**: 
1. Re-tarring extracted content changed file ordering and metadata
2. Tar-split reconstruction read from mounted EROFS, producing different content

**Solution**: 
1. Save the original tarball to `layer.erofs.tar` (never modified)
2. Use read-only direct mounts for tar-split file access
3. Return original tarball in `Diff()` when exporting full layer

This ensures the exact original tar structure is preserved for digest verification.

### Bloated Diff Tarballs for Working Layers

**Problem**: When building images with multiple RUN steps, intermediate layers became hundreds of megabytes instead of a few kilobytes, causing image sizes to balloon (e.g., a simple busybox-based image showing 422 MB instead of 5 MB).

**Root Cause**: 
1. Working container layers (during `RUN` steps) don't have a `layer.erofs` yet - changes are only in the overlay upperdir
2. When computing diffs via `naiveDiff.Diff()`, the driver called `Get()` for both the working layer and its parent
3. Both `Get()` calls created overlay mounts, causing same files to appear with different device/inode numbers
4. `ChangesDirs()` inode-based optimization marked ALL files as "changed" (not just the actual modifications)
5. `ExportChanges()` included full file contents for all "changed" files
6. Result: 398 MB tarball containing duplicate busybox binaries instead of a 3 KB tarball with just the whiteout file

**Solution**: 
Implement custom `Diff()` logic for working container layers that tars the `upperdir` directly instead of using `naiveDiff`:

```go
if !isCommittedLayer {
    upperdir := filepath.Join(d.dir(id), "upper")
    return archive.TarWithOptions(upperdir, &archive.TarOptions{
        WhiteoutFormat: archive.OverlayWhiteoutFormat,
    })
}
```

**How it works:**
- The overlay `upperdir` contains only the actual changes made in that layer
- `TarWithOptions` with `OverlayWhiteoutFormat` converts whiteout character devices (created by `rm`) to `.wh.*` files
- When `ApplyDiff` processes the tarball, `mkfs.erofs --aufs` converts `.wh.*` files back to character devices
- No overlay mounts are created during diff, avoiding device/inode mismatches
- Only actual changes are included in the tarball

**Result:**
- Layer from `RUN touch /root/a.txt`: ~3 KB tarball
- Layer from `RUN rm /root/a.txt`: ~3 KB tarball (just the whiteout)
- Layer from `RUN touch /root/b.txt`: ~3 KB tarball
- Total image size: ~5-6 MB (not 422 MB)

### Committed vs Working Layers

The driver distinguishes between two layer types:

**Committed Image Layers** (have `layer.erofs`):
- Immutable EROFS filesystem
- Content visible via EROFS mount
- Whiteouts stored as character devices
- Original tarball preserved for export
- `Diff()` returns original tarball (for base layers) or uses naiveDiff (for derived layers)

**Working Container Layers** (no `layer.erofs`):
- No EROFS image yet
- Changes captured in overlay upperdir
- Created during container builds (e.g., during `RUN` steps)
- Converted to EROFS when committed via `ApplyDiff()`
- `Diff()` tars the upperdir directly with whiteout format conversion

The `Get()` method checks for `layer.erofs` existence to determine layer type and mount accordingly. The `Diff()` method uses this same check to determine the optimal diff strategy.

### User Namespace Support (`CreateFromTemplate`)

For user namespace ID mapping (e.g., `podman run --userns=keep-id`), the driver implements `CreateFromTemplate`:

```go
func (d *Driver) CreateFromTemplate(id, template string, ...) error {
    // For read-write layers, create with upperdir
    if readWrite {
        return d.CreateReadWrite(id, template, opts)
    }
    
    // For read-only layers, create symlinks to template's EROFS files
    // This avoids copying immutable data
    d.Create(id, template, opts)
    os.Symlink(templateImagePath, newLayerImagePath)
    os.Symlink(templateTarballPath, newLayerTarballPath)
}
```

**How it works:**
1. **Read-Only Template Layers**: Creates symlinks to the template's `layer.erofs` and `layer.erofs.tar` files
   - No disk space wasted (symlinks are tiny)
   - EROFS images are immutable, so sharing is safe
   - Kernel handles UID/GID mapping at mount time via user namespace
   
2. **Read-Write Template Layers**: Creates normal layer with upperdir for modifications

This approach leverages EROFS immutability - since the image content never changes and ID mapping happens at the kernel level during mount, we can safely share EROFS images across user namespaces via symlinks.

### Synchronization Mode (`SyncMode`)

The driver supports filesystem synchronization configuration via the `SyncMode()` method:

```go
func (d *Driver) SyncMode() graphdriver.SyncMode {
    return d.syncMode  // Defaults to SyncModeNone
}
```

**Sync Modes:**
- **SyncModeNone** (default): No explicit synchronization, relies on OS buffering
- **SyncModeFilesystem**: Uses `syncfs()` before marking layers as present

For imagefs, `SyncModeNone` is appropriate because EROFS images are immutable once created, and the mkfs.erofs tool handles proper data integrity during image creation.

## Mount Manager

The `MountManager` abstracts mounting complexity:
- Handles both kernel and FUSE mounts
- Automatic fallback from kernel → FUSE on EPERM
- Manages volatile `rundir` lifecycle
- Device path management for EROFS metadata

## Testing

The test suite verifies:
- **Merged EROFS Strategy**: Multi-layer EROFS merging on supported kernels
- **Separate Layers Strategy**: Individual layer mounting fallback
- **Empty Layers**: Base images with no parent layers
- **Diff Operations**: Export and change detection
- **Working Container Layers**: Layers without committed EROFS images
- **Dockerfile Scenarios**: Multi-step builds with deletions

All tests use `MockMounter` to verify mount call sequences without requiring root privileges.

## Requirements

### Required Tools
- `mkfs.erofs` (v1.7+): EROFS image creation with `--aufs` support
- `erofsfuse` or `squashfuse`: Rootless FUSE mounting

### Kernel Support
- **Basic**: Linux 5.4+ (EROFS support)
- **Merged Strategy**: Linux 5.14+ (multi-image EROFS merging)

## Performance Characteristics

### Advantages
- **Fewer Inodes**: One file per layer vs. thousands
- **Better Caching**: Compressed read-only images
- **Fast Mounting**: Direct EROFS mounts
- **Space Efficient**: Compression + deduplication

### Trade-offs
- **Build Time**: EROFS creation adds overhead during pulls
- **FUSE Performance**: Rootless mode slower than kernel mounts
- **Kernel Dependency**: Requires modern kernel for best performance

## Future Enhancements

Potential improvements:
- **Composefs Integration**: Leverage composefs for better content-addressed storage
- **Zstd Compression**: Use modern compression (requires newer erofs-utils)
- **Chunk-based Deduplication**: Block-level deduplication across layers
- **Direct Tar Push**: Stream tar-split reconstruction without mounting
