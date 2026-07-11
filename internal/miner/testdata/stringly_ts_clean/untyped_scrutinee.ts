// Negative fixture: scrutinee has no local type annotation tracing to a known
// union type. The miner must not emit any leads (precision gate). Expected: 0 leads.

type Status = 'active' | 'inactive' | 'pending';

function handleRaw(s: string): string {
  // s is typed as plain string, not Status — no type binding found.
  switch (s) {
    case 'activ':
      return 'typo branch';
    case 'inactive':
      return 'inactive branch';
  }
  return 'unknown';
}
