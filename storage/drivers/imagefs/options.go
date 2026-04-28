package imagefs

import (
	"fmt"
	"strings"
)

// Options represents the configuration for the imagefs storage driver.
type Options struct {
	Format      string
	Compression string // squashfs compression algorithm (gzip, xz, lz4, zstd)
}

const (
	FormatEROFS    = "erofs"
	FormatSquashFS = "squashfs"
)

// parseOptions processes driver options from the storage configuration.
func parseOptions(options []string) (*Options, error) {
	opts := &Options{
		Format:      FormatEROFS,
		Compression: "gzip", // default compression for squashfs
	}

	for _, opt := range options {
		if strings.HasPrefix(opt, "imagefs_format=") {
			format := strings.TrimPrefix(opt, "imagefs_format=")
			if format != FormatEROFS && format != FormatSquashFS {
				return nil, fmt.Errorf("imagefs: invalid format %q, must be %s or %s", format, FormatEROFS, FormatSquashFS)
			}
			opts.Format = format
		} else if strings.HasPrefix(opt, "imagefs_compression=") {
			compression := strings.TrimPrefix(opt, "imagefs_compression=")
			// Validate compression algorithm
			validCompressors := []string{"gzip", "lzma", "lzo", "xz", "lz4", "zstd"}
			valid := false
			for _, c := range validCompressors {
				if compression == c {
					valid = true
					break
				}
			}
			if !valid {
				return nil, fmt.Errorf("imagefs: invalid compression %q, must be one of: %v", compression, validCompressors)
			}
			opts.Compression = compression
		}
	}
	return opts, nil
}
