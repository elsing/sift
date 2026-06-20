import { dryRunHeaders } from './util.js';
import { getMailById } from './inbox.js';

let mailReaderPanel, mailReaderBody, mailReaderHtml, mailReaderSubject, mailReaderSender, mailReaderTime;

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
  const id = row.dataset.id;
  const mail = getMailById(id) || {};
  mailReaderSubject.textContent = mail.subject || '';
  renderSender(mail);
  mailReaderTime.textContent = mail.time || '';
  mailReaderBody.textContent = 'Loading…';
  mailReaderBody.classList.remove('hidden');
  mailReaderHtml.classList.add('hidden');
  mailReaderPanel.classList.remove('hidden');

  if (row.classList.contains('unread')) {
    row.classList.remove('unread');
    fetch(`/api/mails/${id}/read`, { method: 'POST', headers: dryRunHeaders() });
  }

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
    mailReaderHtml.srcdoc = data.html;
  } else {
    mailReaderBody.textContent = data.text || '(no readable body)';
  }
}
