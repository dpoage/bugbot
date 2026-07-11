// D2/D3 adversarial fixture: type alias whose value is an object_type, not a union.
// The properties themselves have union types, but those are nested inside an
// object_type — the structural whitelist must exclude the outer alias entirely
// (it's not a union_type, so isPureStringUnion returns false for the value node).
// Expected: 0 leads.

type Config = {
  mode: 'a' | 'b';
  other: 'c' | 'd';
};

function run(cfg: Config): void {
  switch (cfg.mode) {
    case 'a':
      break;
    case 'x':  // would be a FP if 'a'|'b'|'c'|'d' were treated as a closed union
      break;
  }
}
