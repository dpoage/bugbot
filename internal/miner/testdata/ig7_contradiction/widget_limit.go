// Package widgetconfig holds the user-facing budget knobs. The struct field
// comments document the sentinel semantics the runtime must honor; the
// validator in the config package is expected to allow those sentinel values.
package widgetconfig

// Config holds the user-facing runtime knobs. The struct field comments
// document the sentinel semantics the runtime must honor.
type Config struct {
	// WidgetLimit is the cap on the number of widget operations per cycle.
	// 0 = unlimited.
	WidgetLimit int
}
