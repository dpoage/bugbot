// Package cleancfg holds a runtime configuration whose documentation and
// validators agree: there is no sentinel-vs-validator contradiction here, so
// the miner must post zero leads.
package cleancfg

// Config holds the user-facing runtime knobs. The struct field comments
// document preconditions that the validator in Validate enforces.
type Config struct {
	// MaxRetries is the maximum retry count. Must be > 0.
	MaxRetries int
	// DefaultPort is the TCP port the service binds to. Must be non-empty.
	DefaultPort string
}

// Validate enforces the preconditions documented above.
func Validate(c Config) error {
	if c.MaxRetries <= 0 {
		return fmt.Errorf("max_retries must be > 0")
	}
	if c.DefaultPort == "" {
		return fmt.Errorf("default_port must not be empty")
	}
	return nil
}
