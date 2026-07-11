// D1 round-2 adversarial fixture: paren-LESS single-param arrow shadows outer typed param.
// xs.forEach(event => { switch(event)... })  — 'event' has NO parentheses.
// The inner arrow param 'event' is untyped; it shadows the outer Event-typed param.
// The miner must detect this as an untyped shadow and emit 0 leads.
// Expected: 0 leads.

type Event = 'click' | 'hover';

function processEvents(event: Event): void {
  const xs: string[] = ['x', 'y'];
  xs.forEach(event => {
    // Inner 'event' is the untyped parameter of the paren-less arrow function.
    // It is NOT typed as Event — it comes from the string[] array.
    switch (event) {
      case 'bogus':  // would be a type-A FP if outer Event binding leaked
        break;
      case 'click':
        break;
      // 'hover' not covered — would be type-B FP if outer Event binding leaked
    }
  });
}
