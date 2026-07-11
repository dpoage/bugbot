// Positive fixture: 'pending' is a member of Direction but has no case arm.
// Expected: 1 type-B lead (missing arm) at the switch line.
// No default clause so type-B suppression does not fire.

type Direction = 'north' | 'south' | 'east' | 'west';

function move(dir: Direction): string {
  switch (dir) {
    case 'north':
      return 'going north';
    case 'south':
      return 'going south';
    case 'east':
      return 'going east';
    // 'west' is missing — type-B lead
  }
  return 'unknown direction';
}
