// D1 adversarial fixture: block-scoped const/let shadows outer typed union param.
// Inside the if block, a new const 'mode' is declared as plain string.
// The nearest binding of 'mode' at the switch is the block-scoped const.
// Expected: 0 leads (precision gate: block-const shadow hides typed outer param).

type Mode = 'fast' | 'slow' | 'auto';

function process(mode: Mode): string {
  if (true) {
    // 'mode' re-declared as a plain string — shadows the Mode-typed param.
    const mode = 'whatever';  // block-scoped const, untyped (inferred string)
    switch (mode) {
      case 'notamode':   // would be a typo if Mode-typed, but const is plain string
        return 'x';
      case 'slow':
        return 'y';
    }
  }
  return mode; // outer typed 'mode' unaffected
}
