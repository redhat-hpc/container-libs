package imagefs

import (
	"fmt"
	"slices"
	"strings"
)

// Options represents the configuration for the imagefs storage driver.
type Options struct {
	Format      string
	Compression string // Compression algorithm (gzip->deflate for EROFS, lz4, lz4hc, lzma, zstd, etc.)
}

const (
	FormatEROFS    = "erofs"
	FormatSquashFS = "squashfs"
)

// parseOptions processes driver options from the storage configuration.
func parseOptions(options []string) (*Options, error) {
	opts := &Options{
		Format:      FormatEROFS,
		Compression: "", // default: no compression
	}

	for _, opt := range options {
		if format, found := strings.CutPrefix(opt, "imagefs_format="); found {
			if format != FormatEROFS && format != FormatSquashFS {
				return nil, fmt.Errorf("imagefs: invalid format %q, must be %s or %s", format, FormatEROFS, FormatSquashFS)
			}
			opts.Format = format
		} else if compression, found := strings.CutPrefix(opt, "imagefs_compression="); found {
			// Validate compression algorithm
			// EROFS: lz4, lz4hc, deflate, libdeflate (1.7+), lzma, zstd (1.8+)
			// SquashFS: gzip, lzma, lzo, xz, lz4, zstd
			// Note: "gzip" is automatically mapped to "deflate" for EROFS
			validCompressors := []string{"lz4", "lz4hc", "deflate", "libdeflate", "gzip", "lzma", "lzo", "xz", "zstd"}
			if !slices.Contains(validCompressors, compression) {
				return nil, fmt.Errorf("imagefs: invalid compression %q, must be one of: %v", compression, validCompressors)
			}
			opts.Compression = compression
		}
	}
	return opts, nil
}
