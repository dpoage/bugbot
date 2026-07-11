// D1 adversarial fixture: untyped inner closure param shadows outer typed union param.
// items.forEach((event) => { switch(event) { case 'clickk': ... } })
// The inner arrow-function parameter 'event' has NO type annotation.
// The nearest binding of 'event' is the untyped arrow-function param.
// Expected: 0 leads (precision gate: untyped inner binding shadows typed outer).

type Event = 'click' | 'hover' | 'focus';

function handleEvents(event: Event, items: string[]): void {
  // Outer typed binding of 'event' exists above.
  // Now we shadow it in the inner forEach callback:
  items.forEach((event) => {
    // 'event' here is untyped (plain string from items array).
    // The switch must NOT be flagged — the miner cannot resolve the union here.
    switch (event) {
      case 'clickk':   // would be a typo if typed, but inner event is untyped
        break;
      case 'hover':
        break;
    }
  });
}
