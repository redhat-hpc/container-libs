//go:build linux

package imagefs

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.podman.io/storage/pkg/reexec"
	"golang.org/x/sys/unix"
)

func init() {
	reexec.Register("imagefs-mountfrom", mountOverlayFromMain)
}

func fatal(err error) {
	fmt.Fprint(os.Stderr, err)
	os.Exit(1)
}

type mountOptions struct {
	Device string
	Target string
	Type   string
	Label  string
	Flag   uint32
}

// mountOverlayFrom performs an overlay mount using a reexec subprocess to handle
// long mount option strings that may exceed the kernel page size limit.
// This is critical for images with many layers where the lowerdir string can be very long.
func mountOverlayFrom(dir, device, target, mType string, flags uintptr, label string) error {
	options := &mountOptions{
		Device: device,
		Target: target,
		Type:   mType,
		Flag:   uint32(flags),
		Label:  label,
	}

	cmd := reexec.Command("imagefs-mountfrom", dir)
	w, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mountfrom error on pipe creation: %w", err)
	}

	output := bytes.NewBuffer(nil)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		w.Close()
		return fmt.Errorf("mountfrom error on re-exec cmd: %w", err)
	}

	// Write the options to the pipe for the reexec process to read
	if err := json.NewEncoder(w).Encode(options); err != nil {
		w.Close()
		return fmt.Errorf("mountfrom json encode to pipe failed: %w", err)
	}
	w.Close()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("mountfrom re-exec output: %s: error: %w", output, err)
	}
	return nil
}

// mountOverlayFromMain is the entry-point for imagefs-mountfrom on re-exec.
func mountOverlayFromMain() {
	runtime.LockOSThread()
	flag.Parse()

	var options *mountOptions

	if err := json.NewDecoder(os.Stdin).Decode(&options); err != nil {
		fatal(err)
	}

	// Mount from the specified directory. Some of the paths mentioned in the
	// mount options are relative to this directory.
	homedir := flag.Arg(0)
	if err := os.Chdir(homedir); err != nil {
		fatal(err)
	}

	pageSize := unix.Getpagesize()
	if len(options.Label) < pageSize {
		if err := unix.Mount(options.Device, options.Target, options.Type, uintptr(options.Flag), options.Label); err != nil {
			fatal(err)
		}
		os.Exit(0)
	}

	// The mount options are too long. Open file descriptors for the lower
	// directories and use /proc/self/fd as shorter paths.

	// Split out the various options so we can manipulate the paths
	var upperk, upperv, workk, workv, lowerk, lowerv, others string
	for arg := range strings.SplitSeq(options.Label, ",") {
		key, val, _ := strings.Cut(arg, "=")
		switch key {
		case "upperdir":
			upperk = "upperdir="
			upperv = val
		case "workdir":
			workk = "workdir="
			workv = val
		case "lowerdir":
			lowerk = "lowerdir="
			lowerv = val
		default:
			if others == "" {
				others = arg
			} else {
				others = others + "," + arg
			}
		}
	}

	// Ensure upperdir, workdir, and target are absolute paths
	if upperv != "" && !filepath.IsAbs(upperv) {
		upperv = filepath.Join(homedir, upperv)
	}
	if workv != "" && !filepath.IsAbs(workv) {
		workv = filepath.Join(homedir, workv)
	}
	if !filepath.IsAbs(options.Target) {
		options.Target = filepath.Join(homedir, options.Target)
	}

	// Open a descriptor for each lower directory and use the descriptor's path
	// (via /proc/self/fd) as the new value, which is much shorter.
	if lowerv != "" {
		var newLowers []string
		dataOnly := false
		for lowerPath := range strings.SplitSeq(lowerv, ":") {
			if lowerPath == "" {
				// Empty element indicates the next layer is data-only
				dataOnly = true
				continue
			}
			lowerFd, err := unix.Open(lowerPath, unix.O_RDONLY, 0)
			if err != nil {
				fatal(err)
			}
			var lower string
			if dataOnly {
				lower = fmt.Sprintf(":%d", lowerFd)
				dataOnly = false
			} else {
				lower = fmt.Sprintf("%d", lowerFd)
			}
			newLowers = append(newLowers, lower)
		}
		lowerv = strings.Join(newLowers, ":")
	}

	// Reconstruct the Label field with shorter paths
	options.Label = upperk + upperv + "," + workk + workv + "," + lowerk + lowerv + "," + others
	options.Label = strings.ReplaceAll(options.Label, ",,", ",")

	// Try the mount again with shorter paths, if we managed to make it fit
	var err error
	if len(options.Label) < pageSize {
		if err := os.Chdir("/proc/self/fd"); err != nil {
			fatal(err)
		}
		err = unix.Mount(options.Device, options.Target, options.Type, uintptr(options.Flag), options.Label)
	} else {
		err = fmt.Errorf("cannot mount overlay, mount data %q too large %d >= page size %d", options.Label, len(options.Label), pageSize)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "creating overlay mount to %s, mount_data=%q\n", options.Target, options.Label)
		fatal(err)
	}

	os.Exit(0)
}
