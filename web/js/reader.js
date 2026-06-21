import { dryRunHeaders } from './util.js';
import { getMailById } from './inbox.js';

let mailReaderPanel, mailReaderBody, mailReaderHtml, mailReaderSubject, mailReaderSender, mailReaderTime;

// Real email HTML routinely has fixed-pixel-width tables/layouts (the classic 600px
// newsletter table) built for desktop, with no idea it'll ever render in a narrow
// iframe — without this, that forces horizontal scroll on a phone. Prepended ahead of
// whatever the message contains; later, more specific author CSS in the message can
// still override anything here that isn't !important.
const RESPONSIVE_RESET = `<style>
  html, body { max-width: 100%; overflow-x: hidden; word-wrap: break-word; }
  img, table { max-width: 100% !important; height: auto !important; }
  table { width: auto !important; }
</style>`;

// One-shot hook for "where did opening this mail come from". Defaults to nothing extra
// (just close, landing on whatever's underneath — the inbox/folder). Set by a caller
// like search.js that wants Back to return somewhere specific instead, e.g. reopening
// the search panel rather than leaving you on the folder it jumped to.
let onBack = null;
export function setReaderBack(fn) {
  onBack = fn;
}

export function setupMailReader() {
  mailReaderPanel = document.getElementById('mailReaderPanel');
  mailReaderBody = document.getElementById('mailReaderBody');
  mailReaderHtml = document.getElementById('mailReaderHtml');
  mailReaderSubject = document.getElementById('mailReaderSubject');
  mailReaderSender = document.getElementById('mailReaderSender');
  mailReaderTime = document.getElementById('mailReaderTime');
  document.getElementById('mailReaderBack').addEventListener('click', () => {
    mailReaderPanel.classList.add('hidden');
    mailReaderHtml.removeAttribute('srcdoc'); // stop any media/connections the message kicked off
    const cb = onBack;
    onBack = null;
    if (cb) cb();
  });
}

// Tapping the sender's name reveals the real address next to it (animated open) with
// a copy button. Built with DOM methods, not innerHTML — it's untrusted mail content.
function renderSender(mail) {
  mailReaderSender.innerHTML = '';
  const nameEl = document.createElement('span');
  nameEl.className = 'sender-name';
  nameEl.textContent = mail.sender || '';
  mailReaderSender.appendChild(nameEl);

  if (!mail.senderEmail) return;
  nameEl.classList.add('revealable');

  const emailEl = document.createElement('span');
  emailEl.className = 'sender-email';
  const emailText = document.createElement('span');
  emailText.className = 'sender-email-text';
  emailText.textContent = mail.senderEmail;
  const copyBtn = document.createElement('button');
  copyBtn.className = 'sender-copy';
  copyBtn.textContent = '⧉';
  copyBtn.title = 'Copy address';
  emailEl.append(emailText, copyBtn);
  mailReaderSender.appendChild(emailEl);

  nameEl.addEventListener('click', () => {
    mailReaderSender.classList.toggle('revealed');
  });
  copyBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    navigator.clipboard.writeText(mail.senderEmail);
    copyBtn.textContent = '✓';
    setTimeout(() => { copyBtn.textContent = '⧉'; }, 1200);
  });
}

export async function openMailReader(row) {
  onBack = null; // plain inbox-row tap — Back just closes, no special destination
  const id = row.dataset.id;
  const mail = getMailById(id) || {};
  showMailMeta(mail);

  if (row.classList.contains('unread')) {
    row.classList.remove('unread');
    fetch(`/api/mails/${id}/read`, { method: 'POST', headers: dryRunHeaders() });
  }

  await loadMailBody(id);
}

// Opens the reader from just an id, with no inbox row to read cached metadata or
// swipe state from — used to deep-link a push notification straight to its mail,
// since that mail may not be on the currently-loaded page (or in the inbox at all).
export async function openMailReaderById(id) {
  onBack = null; // caller sets a destination via setReaderBack right after, if it wants one
  showMailMeta({});
  const metaRes = await fetch(`/api/mails/${id}`);
  if (!metaRes.ok) {
    mailReaderBody.textContent = await metaRes.text();
    return;
  }
  const mail = await metaRes.json();
  showMailMeta(mail);

  if (mail.unread) {
    fetch(`/api/mails/${id}/read`, { method: 'POST', headers: dryRunHeaders() });
  }

  await loadMailBody(id);
}

function showMailMeta(mail) {
  mailReaderSubject.textContent = mail.subject || '';
  renderSender(mail);
  mailReaderTime.textContent = mail.time || '';
  mailReaderBody.textContent = 'Loading…';
  mailReaderBody.classList.remove('hidden');
  mailReaderHtml.classList.add('hidden');
  mailReaderPanel.classList.remove('hidden');
}

async function loadMailBody(id) {
  const res = await fetch(`/api/mails/${id}/body`);
  if (!res.ok) {
    mailReaderBody.textContent = await res.text();
    return;
  }
  const data = await res.json();
  if (data.html) {
    // sandboxed with no tokens at all: no script execution, no same-origin access —
    // this is untrusted content, so render it but never let it run anything.
    mailReaderBody.classList.add('hidden');
    mailReaderHtml.classList.remove('hidden');
    mailReaderHtml.srcdoc = RESPONSIVE_RESET + data.html;
  } else {
    mailReaderBody.textContent = data.text || '(no readable body)';
  }
}
