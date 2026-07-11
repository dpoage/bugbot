// Negative fixture: discriminated union pattern — common TS idiom.
// All members covered, no default. Expected: 0 leads.

type Shape = 'circle' | 'square' | 'triangle';

function area(shape: Shape, size: number): number {
  switch (shape) {
    case 'circle':
      return Math.PI * size * size;
    case 'square':
      return size * size;
    case 'triangle':
      return (size * size) / 2;
  }
  return 0;
}
