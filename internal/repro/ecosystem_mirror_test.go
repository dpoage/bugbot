package repro

import (
	"reflect"
	"strings"
	"testing"

	eco "github.com/dpoage/bugbot/internal/ecosystem"
)

// TestEcosystemRulesMirrorsInterpRules guards the ecosystemRules mirror
// against silent field drift (bugbot-ds90 discovered hazard): a field added
// to ecosystem.InterpRules but not mirrored here is invisible to the real
// interpret() pipeline while all internal/ecosystem tests still pass —
// exactly how LineAnchoredRanMarkers was nearly lost. Every exported
// InterpRules field must have a same-named (lowercased first rune)
// counterpart in ecosystemRules, and fromInterpRules must copy each one.
func TestEcosystemRulesMirrorsInterpRules(t *testing.T) {
	src := reflect.TypeOf(eco.InterpRules{})
	dst := reflect.TypeOf(ecosystemRules{})

	dstFields := make(map[string]reflect.Type, dst.NumField())
	for i := 0; i < dst.NumField(); i++ {
		f := dst.Field(i)
		dstFields[f.Name] = f.Type
	}

	for i := 0; i < src.NumField(); i++ {
		f := src.Field(i)
		mirror := strings.ToLower(f.Name[:1]) + f.Name[1:]
		dt, ok := dstFields[mirror]
		if !ok {
			t.Errorf("ecosystem.InterpRules.%s has no mirror field %q in repro.ecosystemRules — add it to the struct AND fromInterpRules, or interpret() silently ignores it", f.Name, mirror)
			continue
		}
		// name is sandbox.Ecosystem (a string alias) mirroring
		// ecosystem.Ecosystem; both are string kinds. All others must match
		// kinds exactly.
		if dt.Kind() != f.Type.Kind() {
			t.Errorf("mirror field %q kind = %v, want %v", mirror, dt.Kind(), f.Type.Kind())
		}
	}

	// The copy check: populate every source field with a distinct non-zero
	// value via reflection, convert, and require every mirror field of the
	// result to be non-zero. Catches a struct field added to both types but
	// forgotten in fromInterpRules.
	in := reflect.New(src).Elem()
	for i := 0; i < src.NumField(); i++ {
		f := in.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Slice:
			f.Set(reflect.Append(reflect.MakeSlice(f.Type(), 0, 1), reflect.ValueOf("x").Convert(f.Type().Elem())))
		default:
			t.Fatalf("ecosystem.InterpRules.%s has unhandled kind %v — extend this test", src.Field(i).Name, f.Kind())
		}
	}
	out := reflect.ValueOf(fromInterpRules(in.Interface().(eco.InterpRules)))
	for i := 0; i < out.NumField(); i++ {
		if out.Field(i).IsZero() {
			t.Errorf("fromInterpRules left ecosystemRules.%s zero — field is declared but never copied", dst.Field(i).Name)
		}
	}
}
