// Negative fixture: all union members are handled exactly.
// Expected: 0 leads.

type Color = 'red' | 'green' | 'blue';

function describeColor(c: Color): string {
  switch (c) {
    case 'red':
      return 'red color';
    case 'green':
      return 'green color';
    case 'blue':
      return 'blue color';
  }
  return 'unknown';
}
