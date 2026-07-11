// Positive fixture: 'activ' is a typo — should be 'active'.
// Expected: 1 type-A lead at the case 'activ' line.

type Status = 'active' | 'inactive' | 'pending';

function handleStatus(s: Status): string {
  switch (s) {
    case 'activ':       // typo — not a member of Status
      return 'typo branch';
    case 'inactive':
      return 'inactive branch';
    case 'pending':
      return 'pending branch';
    default:
      return 'unknown';
  }
}
