package llm

import "context"

// UsageEvent is a single accounting record emitted after a completion. It binds
// the spend to the role/provider/model that incurred it so callers can ledger
// per-dimension.
type UsageEvent struct {
	// Role is the pipeline role that made the call (e.g. "finder"), if known.
	// Empty when the client was built outside a role context.
	Role string
	// Provider is the config provider name (the key in config.Providers).
	Provider string
	// Model is the model identifier.
	Model string
	// Usage is the token consumption for this call.
	Usage Usage
}

// Recorder is a callback hook callers implement to ledger spend. Record is
// invoked once per successful Complete. Implementations must be safe for
// concurrent use and must not block for long, since they run on the request
// path.
type Recorder interface {
	Record(ev UsageEvent)
}

// RecorderFunc adapts a plain function to the Recorder interface.
type RecorderFunc func(ev UsageEvent)

// Record implements Recorder.
func (f RecorderFunc) Record(ev UsageEvent) { f(ev) }

// recordingClient wraps a Client, reporting usage to a Recorder after each
// successful completion.
type recordingClient struct {
	inner    Client
	rec      Recorder
	role     string
	provider string
	model    string
}

// WithRecorder wraps c so that every successful Complete reports its usage to
// rec, tagged with role/provider/model. A nil rec returns c unchanged.
func WithRecorder(c Client, rec Recorder, role, provider, model string) Client {
	if rec == nil {
		return c
	}
	return &recordingClient{inner: c, rec: rec, role: role, provider: provider, model: model}
}

func (r *recordingClient) Capabilities() Capabilities { return r.inner.Capabilities() }

func (r *recordingClient) Complete(ctx context.Context, req Request) (Response, error) {
	resp, err := r.inner.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	r.rec.Record(UsageEvent{
		Role:     r.role,
		Provider: r.provider,
		Model:    r.model,
		Usage:    resp.Usage,
	})
	return resp, nil
}
