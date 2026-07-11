// D2 adversarial fixture: union containing a predefined_type keyword (number).
// type Mixed = 'read' | 'write' | number — the 'number' keyword is a
// predefined_type node in the AST, not a literal_type. The structural whitelist
// must reject this as a non-pure union.
// Expected: 0 leads.

type Action = 'read' | 'write' | number;

function dispatch(action: Action): void {
  // Even if this were flaggable, structural whitelist must exclude Action.
  switch (action as string) {
    case 'read':
      break;
    case 'notwrite':  // would be a typo if Action were a closed string union
      break;
  }
}
