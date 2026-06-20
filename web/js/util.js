export function dryRunHeaders() {
  return localStorage.getItem('dryRun') === '1' ? { 'X-Dry-Run': '1' } : {};
}
