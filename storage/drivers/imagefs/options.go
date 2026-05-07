package imagefs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Options represents the configuration for the imagefs storage driver.
type Options struct {
	Format      string
	Compression string   // Compression algorithm (gzip->deflate for EROFS, lz4, lz4hc, lzma, zstd, etc.)
	imageStores []string // Read-only additional image stores
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
		key, val, _ := strings.Cut(opt, "=")
		switch key {
		case "imagefs_format":
			if val != FormatEROFS && val != FormatSquashFS {
				return nil, fmt.Errorf("imagefs: invalid format %q, must be %s or %s", val, FormatEROFS, FormatSquashFS)
			}
			opts.Format = val
		case "imagefs_compression":
			// Validate compression algorithm
			// EROFS: lz4, lz4hc, deflate, libdeflate (1.7+), lzma, zstd (1.8+)
			// SquashFS: gzip, lzma, lzo, xz, lz4, zstd
			// Note: "gzip" is automatically mapped to "deflate" for EROFS
			validCompressors := []string{"lz4", "lz4hc", "deflate", "libdeflate", "gzip", "lzma", "lzo", "xz", "zstd"}
			if !slices.Contains(validCompressors, val) {
				return nil, fmt.Errorf("imagefs: invalid compression %q, must be one of: %v", val, validCompressors)
			}
			opts.Compression = val
		case "imagestore", "additionalimagestore":
			// Additional read-only image stores to use for lower paths
			if val == "" {
				continue
			}
			for store := range strings.SplitSeq(val, ",") {
				store = filepath.Clean(store)
				if !filepath.IsAbs(store) {
					return nil, fmt.Errorf("imagefs: image path %q is not absolute, cannot be relative", store)
				}
				st, err := os.Stat(store)
				if err != nil {
					return nil, fmt.Errorf("imagefs: can't stat imageStore dir %s: %w", store, err)
				}
				if !st.IsDir() {
					return nil, fmt.Errorf("imagefs: image path %q must be a directory", store)
				}
				opts.imageStores = append(opts.imageStores, store)
			}
		}
	}
	return opts, nil
}
