package imagefs

import graphdriver "go.podman.io/storage/drivers"

// Options represents the configuration for the imagefs storage driver.
// This struct is currently a placeholder and does not define any driver-specific options.
// Driver-specific options can be added as the implementation evolves.
type Options struct {
	// Add driver-specific configuration fields here in future
}

// parseOptions processes driver options from the storage configuration.
// For now, it simply returns a default Options struct as no driver-specific
// options are implemented.
func parseOptions(options []string) (*Options, error) {
	opts := &Options{}
	// Process driver options if needed in the future
	// For now, just return default options
	return opts, nil
}

// ToDriverOptions converts Options to the graphdriver.Options format
// for compatibility with the storage driver framework.
func (o *Options) ToDriverOptions() graphdriver.Options {
	return graphdriver.Options{
		DriverOptions: []string{},
	}
}
