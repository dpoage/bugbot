// Negative fixture: various mixed union forms that must all be excluded.
// None of these should produce leads. Expected: 0 leads.

// Numeric literal in literal_type — excluded.
type StringOrNum = 'hello' | 42;

// Boolean literal in literal_type — excluded.
type StringOrBool = 'yes' | true | false;

// null/undefined literal in literal_type — excluded.
type StringOrNull = 'value' | null | undefined;

// predefined_type keyword as direct union branch — excluded.
type StringOrNumber = 'alpha' | 'beta' | number;

function handle(v: StringOrNum): string {
  switch (v as unknown as string) {
    case 'hello':
      return 'greeting';
    case 'goodbye':  // not in any of the above unions (all excluded)
      return 'farewell';
  }
  return 'other';
}
