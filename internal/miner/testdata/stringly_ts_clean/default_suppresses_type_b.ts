// Negative fixture: explicit-subset idiom — switch handles only some members
// but has a default clause. Type-B suppression must fire: 0 leads.

type LogLevel = 'debug' | 'info' | 'warn' | 'error';

function shouldAlert(level: LogLevel): boolean {
  switch (level) {
    case 'warn':
      return true;
    case 'error':
      return true;
    default:
      return false;
  }
}
