# imagefs Storage Driver

## Overview

Efficient container storage using immutable compressed filesystem images (EROFS or SquashFS) layered via OverlayFS. Minimizes file count and leverages compressed read-only filesystems.

**Formats:**
- **EROFS** (default): Enhanced Read-Only File System with legacy compression
- **SquashFS**: Alternative with configurable compression (gzip, xz, lz4, zstd, lzo, lzma)

**Configuration:**
- `imagefs_format=erofs|squashfs` - Select format
- `imagefs_compression=<algorithm>` - SquashFS compression (default: gzip)

## Architecture

### Storage Layout

```
storage/imagefs/<layer_id>/
├── layer.erofs           # EROFS image (format=erofs)
├── layer.erofs.tar       # Original tarball (EROFS only, for device mounting)
├── layer.sqfs            # SquashFS image (format=squashfs)
├── parent                # Parent layer ID
├── upper/                # Writable overlay layer
├── work/                 # OverlayFS work directory
└── merged/               # Final mounted filesystem
```

**Note:** EROFS saves original tarball for device mounting and digest preservation. SquashFS does not.

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

**EROFS:**
1. Save original tarball as `layer.erofs.tar`
2. Create EROFS with `mkfs.erofs --tar=i --aufs -E legacy-compress`

**SquashFS:**
1. Save to temporary tarball
2. Create SquashFS with `sqfstar` or `tar2sqfs`
3. Delete temporary file

### Exporting Layers (`Diff`)

**Strategy by layer type:**
- **EROFS base layers:** Return original `.tar` file (digest preservation)
- **SquashFS base layers:** Use naiveDiff
- **Working layers:** Tar the `upperdir` directly (avoids inode mismatch)
- **Derived layers:** Use naiveDiff

### User Namespace Support

For `--userns` mappings, creates symlinks to template image files instead of copying:
- EROFS: Links `layer.erofs` and `layer.erofs.tar`
- SquashFS: Links `layer.sqfs`
- UID/GID mapping handled by kernel at mount time

## Critical Fixes

### Whiteout Handling
**Problem:** Deleted files reappeared in containers  
**Solution:** `mkfs.erofs --aufs` converts `.wh.*` files to character devices overlayfs recognizes

### Digest Preservation
**Problem:** Image push failed with digest mismatches  
**Solution:** Save original tarball, return it on export for EROFS base layers

### Bloated Working Layer Diffs
**Problem:** Intermediate build layers were 400MB instead of 3KB  
**Solution:** Tar `upperdir` directly instead of using naiveDiff (avoids inode mismatch from double overlay mounts)

## Requirements

### Tools

**EROFS:**
- `mkfs.erofs` (v1.7+)
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
| Tarball saved | Yes (`.tar`) | No |
| Whiteout handling | Automatic (`--aufs`) | TODO |
| Compression | Legacy only | Configurable |
| Merged strategy | Yes (5.14+) | No |
| Kernel support | 5.4+ | 2.6.29+ |
| Tools | mkfs.erofs, erofsfuse | sqfstar/tar2sqfs, squashfuse |

## Future Work

- Implement SquashFS whiteout conversion
- Composefs integration
- Zstd compression for EROFS
- Performance instrumentation
- Direct tar-split streaming
