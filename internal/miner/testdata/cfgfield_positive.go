// Package fixture is a test fixture for the config-field miner.
package fixture

// ServerConfig holds server configuration options.
type ServerConfig struct {
	// MaxConnections is the maximum number of connections allowed.
	// Default: 0
	MaxConnections int

	// Timeout is the connection timeout in seconds. defaults to 5.
	Timeout int
}

// Validate validates the config.
func (c *ServerConfig) Validate() error {
	if c.MaxConnections <= 0 {
		return fmt.Errorf("MaxConnections must be > 0")
	}
	return nil
}
