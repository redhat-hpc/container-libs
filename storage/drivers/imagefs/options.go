package imagefs

import (
	"fmt"
	"strings"

	graphdriver "go.podman.io/storage/drivers"
)

// Options represents the configuration for the imagefs storage driver.
// This struct is currently a placeholder and does not define any driver-specific options.
// Driver-specific options can be added as the implementation evolves.
type Options struct {
	Format string
}

const (
	FormatEROFS    = "erofs"
	FormatSquashFS = "squashfs"
)

// parseOptions processes driver options from the storage configuration.
func parseOptions(options []string) (*Options, error) {
	opts := &Options{
		Format: FormatEROFS,
	}

	for _, opt := range options {
		if strings.HasPrefix(opt, "imagefs_format=") {
			format := strings.TrimPrefix(opt, "imagefs_format=")
			if format != FormatEROFS && format != FormatSquashFS {
				return nil, fmt.Errorf("imagefs: invalid format %q, must be %s or %s", format, FormatEROFS, FormatSquashFS)
			}
			opts.Format = format
		}
	}
	return opts, nil
}

// ToDriverOptions converts Options to the graphdriver.Options format
// for compatibility with the storage driver framework.
func (o *Options) ToDriverOptions() graphdriver.Options {
	return graphdriver.Options{
		DriverOptions: []string{},
	}
}
