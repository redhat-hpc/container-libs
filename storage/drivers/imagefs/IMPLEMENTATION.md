# imagefs Storage Driver

## Overview

Efficient container storage using immutable compressed filesystem images (EROFS or SquashFS) layered via OverlayFS. Minimizes file count and leverages compressed read-only filesystems.

**Formats:**
- **EROFS** (default): Enhanced Read-Only File System with configurable compression
  - **Metadata-only mode** (no compression): Stores metadata in `.erofs`, references original `.tar` as device
  - **Full image mode** (with compression): Creates compressed `.erofs` with all data, uses tar-split for digest preservation
- **SquashFS**: Alternative with configurable compression (gzip, xz, lz4, zstd, lzo, lzma)

## Configuration

### Via storage.conf

Add to `/etc/containers/storage.conf` (system-wide) or `~/.config/containers/storage.conf` (rootless):

```toml
[storage]
driver = "imagefs"

[storage.options.imagefs]
imagefs_format = "squashfs"          # Optional: "erofs" (default) or "squashfs"
imagefs_compression = "zstd"         # Optional: compression algorithm
                                     # EROFS: lz4, lz4hc, deflate, libdeflate, lzma, zstd
                                     # SquashFS: gzip, xz, lz4, zstd, lzo, lzma
```

### Via Command Line

```bash
podman --storage-driver imagefs --storage-opt imagefs_format=squashfs build .
podman --storage-driver imagefs --storage-opt imagefs_compression=zstd run image
```

**Options:**
- `imagefs_format=erofs|squashfs` - Select format (default: erofs)
- `imagefs_compression=<algorithm>` - Compression algorithm:
  - EROFS: lz4, lz4hc, deflate, libdeflate, lzma, zstd (requires erofs-utils 1.7+)
  - SquashFS: gzip (default), xz, lz4, zstd, lzo, lzma

## Architecture

### Storage Layout

```
storage/imagefs/<layer_id>/
├── layer.erofs           # EROFS image (metadata-only or full compressed)
├── layer.erofs.tar       # Original tarball (EROFS metadata-only mode for device mounting)
├── layer.sqfs            # SquashFS image (format=squashfs)
├── parent                # Parent layer ID
├── upper/                # Writable overlay layer
├── work/                 # OverlayFS work directory
└── merged/               # Final mounted filesystem
```

**Note:** EROFS metadata-only mode (no compression) saves `.tar` for device mounting and digest preservation. EROFS full mode (with compression) and SquashFS use tar-split for digest preservation.

### Mount Strategy

**Three-Tier Approach (rootful):**
1. **Tier 1** (kernel 6.12+): Direct file mount via new mount API
2. **Tier 2** (kernel 5.14-6.11): Loopback device mount via new mount API
3. **Tier 3** (fallback): FUSE mount (`erofsfuse`/`squashfuse`)

**Rootless:** Always uses FUSE mounts

**Overlay Selection:**
- All kernel mounts → kernel overlayfs
- Any FUSE mount → fuse-overlayfs (for proper xattr/copy-up)

### New Mount API

Uses modern syscalls for kernel mounts:
- `Fsopen` - Open filesystem context
- `FsconfigSetString` - Set source, device paths, SELinux context
- `FsconfigSetFlag` - Set read-only, noacl flags
- `Fsmount` - Create mount file descriptor
- `MoveMount` - Attach to target directory

**EROFS Features:**
- Metadata-only images with external device paths (`.tar` files)
- SELinux context support
- Automatic whiteout conversion via `--aufs` flag

### Deep Layer Stack Support

For images with 50+ layers, uses reexec subprocess with `/proc/self/fd` file descriptors to shorten mount option strings and avoid kernel page size limits.

### EROFS Modes

EROFS supports two distinct modes based on compression settings:

#### Metadata-Only Mode (no compression)
- **Image creation:** `mkfs.erofs --tar=i --aufs -E legacy-compress layer.erofs layer.tar`
- **Storage:** Keeps original `.tar` file alongside `.erofs`
- **Mounting:** Uses `.tar` as external device via `--device=` flag
- **Multi-device merge:** Merges metadata, references multiple `.tar` devices
- **Diff export:** Returns original `.tar` for exact digest preservation
- **Use case:** Default mode, optimal for digest matching without compression overhead

#### Full Image Mode (with compression)
- **Image creation:** `mkfs.erofs --aufs -z <algorithm> layer.erofs layer.tar`
- **Storage:** No `.tar` preservation, only compressed `.erofs`
- **Mounting:** Mounts `.erofs` directly (contains all data)
- **Multi-device merge:** Merges metadata, references multiple `.erofs` devices
- **Diff export:** Uses tar-split reconstruction (like SquashFS)
- **Use case:** Space optimization with compression (lz4, lz4hc, deflate, lzma, zstd)
- **Requirements:** erofs-utils 1.7+ for compression support

## Backend Architecture

The driver uses a clean backend abstraction to separate format-specific logic from generic infrastructure:

**Backend Interface:**
- `Info()` - Static properties (format name, file extension, tarball preservation)
- `CreateImage()` - Format-specific image creation
- `CreateMountSpec()` - Returns MountSpec with all format-specific details
- `MountLayers()` - Implements format-specific optimizations (e.g., EROFS merge)
- `DiffForBaseLayer()` - Format-specific diff export

**MountSpec:** Describes how to mount an image with all format-specific details (filesystem type, device paths, FUSE command, kernel flags). This keeps MountManager completely generic.

**EROFS Backend:**
- **Dual mode operation:**  
  - Metadata-only mode (no compression): Saves `.tar`, uses `--tar=i`, returns `.tar` for Diff
  - Full image mode (with compression): No `.tar`, uses `-z`, returns ErrNotSupported for Diff (tar-split)
- Uses `mkfs.erofs --aufs` for automatic whiteout conversion (both modes)
- Mount spec includes `noacl` kernel flag and device paths:  
  - Metadata mode: `.tar` files as devices
  - Full mode: `.erofs` files as devices (for multi-device) or direct mount (single layer)
- **Merge optimization (kernel 5.14+):** Combines multiple EROFS images into one metadata-only merged image that references all layer devices
- Falls back to separate mounts on older kernels

**SquashFS Backend:**
- No tarball preservation (storage handles tar-split)
- Uses `sqfstar` or `tar2sqfs` for creation
- Simple mount spec without device paths or special flags
- No merge optimization - each layer mounted separately

**MountManager (Generic):**
- All format-specific details come from MountSpec
- Handles kernel vs FUSE fallback
- No file extension checks or format detection
- Easy to add new formats by implementing Backend interface

## Layer Types

**Committed Layers** (have `layer.erofs` or `layer.sqfs`):
- Immutable compressed filesystem
- Mounted as read-only lowerdir
- Whiteouts stored as character devices (EROFS)

**Working Layers** (no image file):
- Changes captured in overlay `upperdir`
- Created during `RUN` steps in builds
- Converted to image on commit via `ApplyDiff`

## Key Operations

### Pulling Images (`ApplyDiff`)

**EROFS (Metadata-only mode - no compression):**
1. Save original tarball as `layer.erofs.tar`
2. Create EROFS with `mkfs.erofs --tar=i --aufs -E legacy-compress`

**EROFS (Full image mode - with compression):**
1. Save to temporary tarball
2. Create compressed EROFS with `mkfs.erofs --aufs -z <algorithm>`
3. Delete temporary file

**SquashFS:**
1. Save to temporary tarball
2. Create SquashFS with `sqfstar` or `tar2sqfs`
3. Delete temporary file

### Exporting Layers (`Diff`)

**Strategy by layer type:**
- **EROFS metadata-only base layers:** Return original `.tar` file (digest preservation)
- **EROFS full mode base layers:** Use tar-split reconstruction
- **SquashFS base layers:** Use tar-split reconstruction
- **Working layers:** Tar the `upperdir` directly (avoids inode mismatch)
- **Derived layers:** Use naiveDiff

## Requirements

### Tools

**EROFS:**
- `mkfs.erofs` (v1.7+ for compression support; older versions support metadata-only mode)
- `erofsfuse`

**SquashFS:**
- `sqfstar` (RHEL 10+) or `tar2sqfs` (RHEL 9)
- `squashfuse`

**Overlay:**
- `fuse-overlayfs` (when any layer is FUSE-mounted)
- `fusermount` / `fusermount3` (optional)

### Kernel Support

- **EROFS:** 5.4+ (basic), 5.14+ (merged strategy), 6.12+ (direct file mount)
- **SquashFS:** 2.6.29+
- **FUSE:** Any kernel with FUSE support

## Performance

**Advantages:**
- Fewer inodes (1-2 files vs thousands per layer)
- Better caching from compressed images
- Fast kernel mounts
- Format flexibility

**Trade-offs:**
- Image creation overhead during pulls
- FUSE slower than kernel mounts
- Tool dependencies
- SquashFS whiteout conversion not yet implemented

## Format Comparison

| Feature | EROFS | SquashFS |
|---------|-------|----------|
| Tarball saved | Metadata mode only | No |
| Whiteout handling | Automatic (`--aufs`) | TODO |
| Compression | Configurable (lz4, lz4hc, deflate, lzma, zstd) | Configurable (gzip, xz, lz4, zstd, lzo, lzma) |
| Modes | Metadata-only (no compression) / Full (compressed) | Full only |
| Digest preservation | Original `.tar` (metadata) / tar-split (full) | tar-split |
| Merged strategy | Yes (5.14+) | No |
| Kernel support | 5.4+ | 2.6.29+ |
| Tools | mkfs.erofs 1.7+, erofsfuse | sqfstar/tar2sqfs, squashfuse |

## Cleanup and Lifecycle

### Normal Shutdown

When `podman` exits normally (build completes, container stops, etc.):
1. `Put()` is called for each mounted container
2. Overlay is unmounted from `merged/`
3. Layer mounts are unmounted via `MountManager.CleanupRundir()`
4. `Cleanup()` is called on driver shutdown
5. All FUSE processes terminate cleanly

### Interrupted Builds (Ctrl+C)

**Problem:** FUSE processes (`erofsfuse`, `fuse-overlayfs`) outlive the podman process when interrupted.

**Why it happens:**
- Both podman's shutdown handler and driver receive SIGINT simultaneously
- Podman terminates the process before driver cleanup can complete
- Kernel mounts auto-cleanup, but FUSE processes remain

**Solution:** Lazy cleanup on next invocation
- `cleanupOrphanedProcesses()` runs during `Init()`
- Scans for leftover mounts in `runRoot/imagefs/` and `home/*/merged`
- Issues `fusermount -uz` on all found mounts
- Removes empty directories
- Next podman command automatically cleans up orphaned processes

**This matches overlay driver behavior:** Overlay leaves kernel mounts behind on Ctrl+C (cleaned up on next use), imagefs leaves FUSE processes (also cleaned up on next use).

### Active Mount Tracking

Driver tracks which containers are currently mounted:
- `activeMounts` map populated on `Get()` success
- Cleared on `Put()` or during `Cleanup()`
- Only actively mounted containers are cleaned up (not entire storage)

## Known Issues

### Mixed Format Storage

**Problem:** Driver cannot mount layers when the storage format is switched (e.g., pulling with EROFS, then running with `--storage-opt imagefs_format=squashfs`)

**Why it happens:**
- `getImagePath()` only looks for files matching the current backend's extension
- Existing EROFS layers have `layer.erofs`, but SquashFS backend searches for `layer.sqfs`
- Backend's `MountLayers()` assumes all layers are in the same format

**Workaround:** Clear storage (`podman system reset`) when switching formats

**Proper fix would require:**
- Backend-agnostic layer mounting logic in the driver layer
- Format detection from file extensions rather than current backend
- Mixed-format support in mount operations

## Future Work

- Mixed format storage support
- Performance instrumentation
- Direct tar-split streaming
