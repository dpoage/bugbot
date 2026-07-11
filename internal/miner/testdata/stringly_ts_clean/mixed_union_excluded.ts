// Negative fixture: mixed union (string | number) must not be treated as a
// closed string enum. Expected: 0 leads.

type StringOrNum = 'hello' | 42;

function handle(v: StringOrNum): string {
  switch (v as unknown as string) {
    case 'hello':
      return 'greeting';
    case 'goodbye':
      return 'farewell';
  }
  return 'other';
}
