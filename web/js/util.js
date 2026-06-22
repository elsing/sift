export function dryRunHeaders() {
  return localStorage.getItem('dryRun') === '1' ? { 'X-Dry-Run': '1' } : {};
}

// Every button that kicks off a network request needs the same two things: disabled
// for the duration (a slow connection shouldn't let a second tap fire the same action
// twice) and some text saying it's actually doing something (a button that just sits
// there, still looking tappable, reads as broken — not "working on it"). This had been
// added ad-hoc to some buttons and quietly skipped on others; one helper means every
// new one gets both by default instead of by remembering to copy the pattern.
export async function withBusyButton(btn, busyText, fn) {
  const originalText = btn.textContent;
  btn.disabled = true;
  btn.textContent = busyText;
  try {
    return await fn();
  } finally {
    btn.disabled = false;
    btn.textContent = originalText;
  }
}
