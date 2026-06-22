// Shared replacement for window.confirm/window.prompt — same idea, but an actual
// in-app modal (matching the rest of the UI) instead of a bare, unstylable browser
// dialog, with a little personality on top since "are you sure?" doesn't have to be
// completely flat. Every destructive action in the app should funnel through one of
// these two, not call confirm()/prompt() directly.
let modal, icon, message, input, cancelBtn, confirmBtn;
let resolveFn = null;
let mode = 'confirm'; // 'confirm' | 'prompt' — decides what cancel/confirm resolve with

export function setupConfirmModal() {
  modal = document.getElementById('confirmModal');
  icon = document.getElementById('confirmModalIcon');
  message = document.getElementById('confirmModalMessage');
  input = document.getElementById('confirmModalInput');
  cancelBtn = document.getElementById('confirmModalCancel');
  confirmBtn = document.getElementById('confirmModalConfirm');

  const settle = (confirmed) => {
    modal.classList.add('hidden');
    if (!resolveFn) return;
    const resolve = resolveFn;
    resolveFn = null;
    if (mode === 'prompt') resolve(confirmed ? input.value.trim() || null : null);
    else resolve(confirmed);
  };
  cancelBtn.addEventListener('click', () => settle(false));
  confirmBtn.addEventListener('click', () => settle(true));
  modal.addEventListener('click', (e) => { if (e.target === modal) settle(false); });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); settle(true); }
    if (e.key === 'Escape') settle(false);
  });
}

const FUN_ICONS = ['🤔', '🧐', '👀', '✋'];
const DANGER_ICONS = ['⚠️', '🗑️', '💥', '😬'];

// danger=true swaps in a more pointed icon set and red confirm styling for anything
// that actually destroys something, vs. a routine "you sure?" for everything else.
export function confirmModal(text, { confirmLabel = 'Yes, do it', cancelLabel = 'Never mind', danger = false } = {}) {
  return new Promise((resolve) => {
    mode = 'confirm';
    resolveFn = resolve;
    message.textContent = text;
    input.classList.add('hidden');
    const icons = danger ? DANGER_ICONS : FUN_ICONS;
    icon.textContent = icons[Math.floor(Math.random() * icons.length)];
    confirmBtn.textContent = confirmLabel;
    confirmBtn.classList.toggle('danger', danger);
    cancelBtn.textContent = cancelLabel;
    modal.classList.remove('hidden');
  });
}

export function promptModal(text, { defaultValue = '', confirmLabel = 'OK', cancelLabel = 'Cancel' } = {}) {
  return new Promise((resolve) => {
    mode = 'prompt';
    resolveFn = resolve;
    message.textContent = text;
    input.classList.remove('hidden');
    input.value = defaultValue;
    icon.textContent = '✏️';
    confirmBtn.textContent = confirmLabel;
    confirmBtn.classList.remove('danger');
    cancelBtn.textContent = cancelLabel;
    modal.classList.remove('hidden');
    setTimeout(() => { input.focus(); input.select(); }, 50);
  });
}
