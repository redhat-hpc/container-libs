# Implementation Summary: `imagefs` Storage Driver

## Overview
The `imagefs` driver provides a way to run containers using a stack of immutable image files (`.img` for EROFS or `.sqsh` for SquashFS) layered via OverlayFS. This approach minimizes the number of files on the host disk and leverages the efficiency of read-only compressed filesystems.

## Architecture

### 1. Storage Layout
The driver organizes data to keep metadata and image files together:
- **Layer Directory**: `storage/imagefs/<layer_id>/`
- **Image File**: `storage/imagefs/<layer_id>/data.img` (or `data.sqsh`)
- **Writable Layer**: `storage/imagefs/<container_id>/upper` and `work`
- **Final RootFS**: `storage/imagefs/<container_id>/merged`

### 2. The Mount Stack
The driver follows a multi-stage mounting process to create the container root filesystem:

1.  **Layer Resolution**: The driver traverses the image hierarchy to identify all required layers.
2.  **Base Mounts**: Each immutable image file is mounted into a volatile directory in the `rundir` (e.g., `/run/user/1001/containers/imagefs/...`).
    - **Privileged Mode**: Uses kernel mounts (`mount -t erofs` or `mount -t squashfs`).
    - **Rootless Mode**: Uses FUSE wrappers (`erofsfuse` or `squashfuse`).
    - **Security**: Mounts are applied with `nodev` and `nosuid` flags.
3.  **Overlay Composition**: An OverlayFS mount is created where:
    - `lowerdir`: The sequence of mounted image layers in the rundir.
    - `upperdir`: A local directory to capture container writes.
    - `workdir`: A required directory for OverlayFS internal operations.

### 3. Pulling Images (`ApplyDiff`)
To support `podman pull`, the driver implements a conversion pipeline:
- **Extraction**: Layer blobs are extracted to a temporary directory.
- **Conversion**: The driver uses `mkfs.erofs` (primary) or `mksquashfs` (fallback) to convert the extracted directory into a single immutable image file.
- **Persistence**: The resulting file is stored as `data.img` in the layer's directory.

## Key Implementation Details

### Mount Manager
The `MountManager` abstracts the complexity of kernel vs. FUSE mounts and handles the lifecycle of the `rundir`. It includes a fallback mechanism: if a kernel mount fails with `EPERM` (common in user namespaces), it automatically attempts a FUSE mount.

### Lifecycle Management
- **`Get()`**: Handles the sequential mounting of layers and the final OverlayFS assembly.
- **`Put()`**: Performs a reverse teardown, unmounting the OverlayFS mount and cleaning up the volatile `rundir`.

## Verification & Testing
- **Unit Tests**: A comprehensive test suite using a `MockMounter` verifies the correct sequence of mount calls and the construction of OverlayFS options.
- **Integration**: Verified through `podman pull` (testing image creation) and `podman run` (testing the mount stack and execution).

## Requirements
- `mkfs.erofs` or `mksquashfs` (for pulling images)
- `erofsfuse` or `squashfuse` (for rootless mounting)
