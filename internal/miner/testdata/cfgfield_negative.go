// Package fixture is a test fixture for the config-field miner (negative case).
package fixture

import "fmt"

// WorkerConfig holds worker pool configuration.
type WorkerConfig struct {
	// Workers is the number of worker goroutines. defaults to 4
	Workers int

	// QueueSize must be set before calling Start; it is required for proper
	// back-pressure behavior.
	QueueSize int
}

// Validate validates the config.
func (c *WorkerConfig) Validate() error {
	if c.Workers <= 0 {
		return fmt.Errorf("Workers must be > 0")
	}
	return nil
}

// Start starts the worker pool. QueueSize is read here.
func Start(cfg WorkerConfig) {
	_ = cfg.QueueSize
	_ = cfg.Workers
}
