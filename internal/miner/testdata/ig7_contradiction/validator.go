// Package config validates the user-supplied runtime configuration. The
// validator in this file REJECTS the very values the doc side (widgetconfig)
// documents as meaningful — that is the contradiction the miner flags.
package config

import "fmt"

// Validate checks the user's configuration. A successful call returns nil;
// any rejected field produces an error naming the field by its snake_case
// config key so the user can fix it in their YAML.
func Validate(c any) error {
	type widgetish interface {
		GetWidgetLimit() int
	}
	// The if-guard pattern the miner recognizes: `if X <= 0 { return err }`.
	// The error string carries the snake_case config key the user sees.
	if w, ok := c.(widgetish); ok {
		if w.GetWidgetLimit() <= 0 {
			return fmt.Errorf("config: widget_limit must be > 0")
		}
	}
	return nil
}
