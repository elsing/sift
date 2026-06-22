import { dryRunHeaders } from './util.js';
import { getMailById, updateMailTags, removeMailById, render } from './inbox.js';
import { fetchTags, createTag, getMailTags, setMailTags, renderTagChips, fetchTagHistory, renderSuggestionChips } from './tags.js';
import { openMoveModalForMail } from './folders.js';

let mailReaderPanel, mailReaderScroll, mailReaderBody, mailReaderHtml, mailReaderHtmlWrap, mailReaderLoading, mailReaderLoadingQuip, mailReaderSubject, mailReaderSender, mailReaderTime, mailReaderTags;
let imagesBlockedBanner, showImagesBtn, trustSenderBtn, mailReaderAttachments;

// Shown while a mail's body is loading — every single email open hits this, however
// briefly, so it's worth being a bit more fun than a bare spinner.
const LOADING_QUIPS = [
  'Decrypting secrets…', 'Untangling the HTML…', 'Waking up the hamsters…',
  'Politely knocking on the mail server…', 'Summoning pixels…', 'Reticulating splines…',
  'Bribing the inbox gremlins…', 'Unfolding the paper airplane…',
];
let currentMailId = null;
// Full mail data for whatever's currently open — not just relying on inbox.js's
// mails array, since this can be opened from search/a deep link, where the mail was
// never in that array at all. Needed for the action buttons (Move needs accountId).
let currentMail = null;

// Real email HTML routinely has fixed-pixel-width layouts (the classic 600px
// newsletter table, but plenty of newer templates use fixed-width divs instead of
// tables) built for desktop, with no idea it'll ever render in a narrow iframe —
// without this, that forces horizontal scroll on a phone. Clamping every element
// rather than just table/img, since a fixed-width div is just as common a culprit.
// Prepended ahead of whatever the message contains; later, more specific author CSS
// in the message can still override anything here that isn't !important.
// <base target="_blank"> is plain HTML, no script needed — without it, tapping a link
// navigates the iframe itself away from the email to whatever site it points at. Most
// of the time that just looks broken; sites that refuse to be framed (e.g. Medium sets
// frame-ancestors) actively block it, leaving Chrome's blank error page sitting where
// the email used to be. Sandbox needs allow-popups too, or this has nothing to open
// into — see the sandbox attribute on the iframe itself.
const RESPONSIVE_RESET = `<base target="_blank"><style>
  html, body { max-width: 100%; overflow-x: hidden; word-wrap: break-word; }
  * { max-width: 100% !important; box-sizing: border-box !important; }
  img, table { height: auto !important; }
  table { table-layout: auto !important; }
  td, th { white-space: normal !important; }
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
  mailReaderScroll = document.getElementById('mailReaderScroll');
  mailReaderBody = document.getElementById('mailReaderBody');
  mailReaderHtml = document.getElementById('mailReaderHtml');
  mailReaderHtmlWrap = document.getElementById('mailReaderHtmlWrap');
  mailReaderLoading = document.getElementById('mailReaderLoading');
  mailReaderLoadingQuip = document.getElementById('mailReaderLoadingQuip');
  mailReaderSubject = document.getElementById('mailReaderSubject');
  mailReaderSender = document.getElementById('mailReaderSender');
  mailReaderTime = document.getElementById('mailReaderTime');
  mailReaderTags = document.getElementById('mailReaderTags');
  mailReaderAttachments = document.getElementById('mailReaderAttachments');
  imagesBlockedBanner = document.getElementById('imagesBlockedBanner');
  showImagesBtn = document.getElementById('showImagesBtn');
  trustSenderBtn = document.getElementById('trustSenderBtn');
  showImagesBtn.addEventListener('click', () => {
    imagesBlockedBanner.classList.add('hidden');
    renderMailHtml(lastHtml, 'proxy');
  });
  trustSenderBtn.addEventListener('click', async () => {
    trustSenderBtn.disabled = true;
    try {
      await fetch('/api/trusted-senders', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ senderEmail: lastSenderEmail }),
      });
    } catch {
      // best-effort — worst case images stay blocked and "Show images" still works this time
    }
    imagesBlockedBanner.classList.add('hidden');
    renderMailHtml(lastHtml, 'proxy');
  });
  document.getElementById('mailReaderTagsBtn').addEventListener('click', () => openTagSheet(currentMailId));
  document.getElementById('mailReaderBack').addEventListener('click', () => closeReaderPanel());

  document.getElementById('mailReaderArchiveBtn').addEventListener('click', () => performReaderAction('archive'));
  document.getElementById('mailReaderDeleteBtn').addEventListener('click', () => performReaderAction('delete'));
  document.getElementById('mailReaderUnreadBtn').addEventListener('click', async (e) => {
    const id = currentMailId;
    // opening always marks read first, so this button only ever means "flip it back
    // to unread" — the endpoint itself is a toggle, but that's the only direction it
    // can realistically go from here. No text swap — this is an SVG icon button, not
    // a label — but it still needs disabling for the moment before the panel closes.
    e.currentTarget.disabled = true;
    await fetch(`/api/mails/${id}/read`, { method: 'POST', headers: dryRunHeaders() });
    const mail = getMailById(id);
    if (mail) mail.unread = true;
    render();
    closeReaderPanel();
    e.currentTarget.disabled = false;
  });
  document.getElementById('mailReaderMoveBtn').addEventListener('click', () => {
    if (!currentMail || !currentMail.accountId) return; // mock mail has no real folders to move into
    const id = currentMailId;
    openMoveModalForMail(currentMail.accountId, id, () => {
      removeMailById(id);
      render();
      closeReaderPanel();
    });
  });

  // iOS's native "tap the status bar to scroll to top" only ever reaches the
  // document's own scroll — this panel is a position:fixed overlay with its own
  // separate scroll (.mail-reader-scroll), which that gesture can't touch at all.
  // Tapping the header is the conventional substitute apps use for exactly this
  // situation. (A plain-text body scrolls with that container and this reaches it; an
  // HTML email now renders at full natural size and is scaled to fit rather than
  // scrolling internally, so this reaches that too.)
  document.getElementById('mailReaderHeader').addEventListener('click', (e) => {
    if (e.target.closest('button')) return;
    mailReaderScroll.scrollTo({ top: 0, behavior: 'smooth' });
  });
}

function closeReaderPanel() {
  mailReaderPanel.classList.add('hidden');
  mailReaderHtml.removeAttribute('srcdoc'); // stop any media/connections the message kicked off
  imagesBlockedBanner.classList.add('hidden');
  const cb = onBack;
  onBack = null;
  if (cb) cb();
}

// Archive/delete from the reader, not just by swiping the inbox row — there was no
// way to act on a mail at all once you'd actually opened it.
async function performReaderAction(action) {
  const id = currentMailId;
  try {
    const res = await fetch(`/api/mails/${id}/${action}`, { method: 'POST', headers: dryRunHeaders() });
    if (!res.ok) throw new Error((await res.text()) || `${action} failed`);
  } catch (err) {
    // the server didn't actually do it (e.g. no Trash/Archive folder found) — leave
    // the reader open rather than closing it and pretending the action worked
    console.error(err);
    return;
  }
  removeMailById(id);
  render();
  closeReaderPanel();
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
  currentMailId = id;
  const mail = getMailById(id) || {};
  currentMail = mail;
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
  currentMailId = id;
  showMailMeta({});
  const metaRes = await fetch(`/api/mails/${id}`);
  if (!metaRes.ok) {
    mailReaderLoading.classList.add('hidden');
    mailReaderBody.classList.remove('hidden');
    mailReaderBody.textContent = await metaRes.text();
    return;
  }
  const mail = await metaRes.json();
  currentMail = mail;
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
  mailReaderTags.innerHTML = '';
  const chips = renderTagChips(mail.tags);
  if (chips) mailReaderTags.appendChild(chips);
  loadSuggestionChips();
  mailReaderLoadingQuip.textContent = LOADING_QUIPS[Math.floor(Math.random() * LOADING_QUIPS.length)];
  mailReaderLoading.classList.remove('hidden');
  mailReaderBody.classList.add('hidden');
  mailReaderHtmlWrap.classList.add('hidden');
  imagesBlockedBanner.classList.add('hidden');
  mailReaderAttachments.innerHTML = '';
  mailReaderPanel.classList.remove('hidden');
  mailReaderScroll.scrollTop = 0; // don't carry over scroll position from whatever was open before
}

// Pending smart-tag suggestions for whatever's currently open, shown alongside applied
// tags rather than replacing them — accepting/dismissing just re-fetches both since
// either action changes what counts as "applied" or "still pending".
async function loadSuggestionChips() {
  const id = currentMailId;
  let suggestions;
  try {
    suggestions = await fetchTagHistory('suggested', id);
  } catch {
    return; // best-effort — no suggestions shown is a safe fallback, not an error to surface
  }
  if (id !== currentMailId) return; // the reader moved on to a different mail while this was in flight
  const chips = renderSuggestionChips(suggestions, () => {
    if (id === currentMailId) {
      getMailTags(id).then((tags) => {
        mailReaderTags.innerHTML = '';
        const tagChips = renderTagChips(tags);
        if (tagChips) mailReaderTags.appendChild(tagChips);
        loadSuggestionChips();
        updateMailTags(id, tags);
      });
    }
  });
  if (chips) mailReaderTags.appendChild(chips);
}

let tagSheetModal, tagSheetList, tagSheetError, tagNameInput;
let lastAllTags = []; // re-filtered locally as you type, rather than re-fetched per keystroke
let currentChecked = new Set(); // persists across re-renders triggered by filtering, not just the initial open

export function setupTagSheet() {
  tagSheetModal = document.getElementById('tagSheetModal');
  tagSheetList = document.getElementById('tagSheetList');
  tagSheetError = document.getElementById('tagSheetError');
  tagNameInput = document.getElementById('tagNameInput');

  document.getElementById('tagSheetClose').addEventListener('click', closeTagSheet);
  tagSheetModal.addEventListener('click', (e) => {
    if (e.target === tagSheetModal) closeTagSheet();
  });

  // Doubles as a filter, not just "type a brand-new tag's name" — with more than a
  // handful of tags, this box looked like a search field (it's right above the list)
  // but typing into it did nothing until you hit Create. Now it narrows the list live;
  // Create still only ever fires on submit, so filtering never accidentally makes one.
  tagNameInput.addEventListener('input', () => {
    const term = tagNameInput.value.trim().toLowerCase();
    const filtered = term ? lastAllTags.filter((t) => t.name.toLowerCase().includes(term)) : lastAllTags;
    renderTagSheetRows(filtered);
  });

  document.getElementById('tagCreateForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    const name = tagNameInput.value.trim();
    if (!name) return;
    tagSheetError.textContent = '';
    try {
      const tag = await createTag(name);
      tagNameInput.value = '';
      // a freshly created tag is presumably what you meant to apply right now
      const current = await getMailTags(currentMailId);
      const tagIds = current.map((t) => t.id);
      tagIds.push(tag.id);
      const updated = await setMailTags(currentMailId, tagIds);
      lastAllTags = await fetchTags(true);
      currentChecked = new Set(updated.map((t) => t.id));
      renderTagSheetRows(lastAllTags);
      refreshReaderTags(updated);
    } catch (err) {
      tagSheetError.textContent = err.message;
    }
  });
}

function closeTagSheet() {
  tagSheetModal.classList.add('hidden');
}

// Used both by the reader's own Tags button and the "tag" swipe action — swiping
// doesn't open the full reader, just this picker, and the row stays in the inbox
// (tagging isn't a remove-from-list action like archive/delete/move).
export function openTagSheetForRow(row) {
  currentMailId = row.dataset.id;
  openTagSheet(row.dataset.id);
}

async function openTagSheet(mailId) {
  if (!mailId) return;
  tagSheetModal.classList.remove('hidden');
  tagSheetError.textContent = '';
  tagNameInput.value = '';
  tagSheetList.innerHTML = '<li class="folder-empty-status dot-loader">Loading</li>';
  try {
    const [allTags, mailTags] = await Promise.all([fetchTags(), getMailTags(mailId)]);
    lastAllTags = allTags;
    currentChecked = new Set(mailTags.map((t) => t.id));
    renderTagSheetRows(allTags);
  } catch (err) {
    tagSheetList.innerHTML = '';
    tagSheetError.textContent = err.message;
  }
}

// allTags here may be a filtered subset (typing in the name box narrows the list) —
// currentChecked tracks the real selection regardless of what's currently filtered in.
function renderTagSheetRows(allTags) {
  tagSheetList.innerHTML = '';
  if (allTags.length === 0) {
    tagSheetList.innerHTML = `<li class="folder-empty-status">${lastAllTags.length === 0 ? 'No tags yet — add one above.' : 'No matching tags.'}</li>`;
    return;
  }
  for (const tag of allTags) {
    const li = document.createElement('li');
    li.className = 'tag-sheet-row';
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.id = 'tagrow-' + tag.id;
    checkbox.checked = currentChecked.has(tag.id);
    const dot = document.createElement('span');
    dot.className = 'tag-sheet-dot';
    dot.style.background = tag.color;
    const label = document.createElement('label');
    label.htmlFor = checkbox.id;
    label.textContent = tag.name;
    li.append(checkbox, dot, label);
    checkbox.addEventListener('change', async () => {
      if (checkbox.checked) currentChecked.add(tag.id);
      else currentChecked.delete(tag.id);
      const updated = await setMailTags(currentMailId, Array.from(currentChecked));
      refreshReaderTags(updated);
    });
    tagSheetList.appendChild(li);
  }
}

// Updates the chips shown in the open reader right away, without waiting for the
// inbox row underneath to catch up (it'll show the change next time it reloads).
function refreshReaderTags(tags) {
  mailReaderTags.innerHTML = '';
  const chips = renderTagChips(tags);
  if (chips) mailReaderTags.appendChild(chips);
  // also patches the inbox row underneath, whether this came from the reader's Tags
  // button or the swipe-to-tag action (which never opens the reader at all)
  updateMailTags(currentMailId, tags);
}

// Blocking remote images outright (the only option before) and fully trusting a
// sender's direct links (raw passthrough) were the only two choices, and they're both
// worse than they need to be: a tracking pixel is indistinguishable from a real image
// at the HTTP level, so there's no way to inspect-and-allow; but a direct fetch to the
// sender's own server still hands them your IP and exact open time regardless of
// whether the image itself was "real". Proxying every image through this server gets
// the visual result of "images loaded" while the sender only ever sees this server's
// IP, not yours — so "allow images" now always means "proxy them", never raw passthrough.
const IMAGE_BLOCK_CSP = `<meta http-equiv="Content-Security-Policy" content="img-src data: cid:;">`;
const IMAGE_PROXY_CSP = `<meta http-equiv="Content-Security-Policy" content="img-src data: cid: 'self';">`;

let lastHtml = '';
let lastSenderEmail = '';

// Rewrites <img src="http(s)://..."> to route through this server's image proxy
// instead of fetching the sender's URL directly from the reader's own device/IP.
function proxyImageSrcs(html) {
  const doc = new DOMParser().parseFromString(html, 'text/html');
  doc.querySelectorAll('img[src]').forEach((img) => {
    const src = img.getAttribute('src');
    if (/^https?:\/\//i.test(src)) {
      img.setAttribute('src', `/api/image-proxy?u=${encodeURIComponent(src)}`);
    }
  });
  return '<!doctype html>' + doc.documentElement.outerHTML;
}

function renderMailHtml(html, mode) {
  mailReaderBody.classList.add('hidden');
  mailReaderHtmlWrap.classList.remove('hidden');
  mailReaderHtml.style.width = '';
  mailReaderHtml.style.transform = '';
  mailReaderHtml.onload = resizeMailReaderIframe;
  const csp = mode === 'proxy' ? IMAGE_PROXY_CSP : IMAGE_BLOCK_CSP;
  const body = mode === 'proxy' ? proxyImageSrcs(html) : html;
  mailReaderHtml.srcdoc = csp + RESPONSIVE_RESET + body;
}

function autoLoadImagesEnabled() {
  return localStorage.getItem('autoLoadImages') !== '0'; // on by default — see settings.js
}

async function loadMailBody(id) {
  const res = await fetch(`/api/mails/${id}/body`);
  mailReaderLoading.classList.add('hidden');
  imagesBlockedBanner.classList.add('hidden');
  if (!res.ok) {
    mailReaderBody.classList.remove('hidden');
    mailReaderBody.textContent = await res.text();
    return;
  }
  const data = await res.json();
  lastHtml = data.html || '';
  lastSenderEmail = data.senderEmail || '';
  if (data.html) {
    // sandbox="allow-same-origin" only (no allow-scripts) — embedded scripts still
    // can't execute, but the parent can read the iframe's own DOM to size it to its
    // content instead of trapping the email in its own internally-scrolling box.
    const proxied = data.trustedSender || autoLoadImagesEnabled();
    if (!proxied) imagesBlockedBanner.classList.remove('hidden');
    renderMailHtml(data.html, proxied ? 'proxy' : 'block');
  } else {
    mailReaderBody.classList.remove('hidden');
    mailReaderBody.textContent = data.text || '(no readable body)';
  }
  renderAttachments(id, data.attachments);
}

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function renderAttachments(mailId, attachments) {
  mailReaderAttachments.innerHTML = '';
  for (const att of attachments || []) {
    const li = document.createElement('li');
    li.className = 'mail-reader-attachment';
    const link = document.createElement('a');
    link.href = `/api/mails/${mailId}/attachments/${att.index}`;
    link.download = att.filename;
    link.textContent = `📎 ${att.filename}`;
    const size = document.createElement('span');
    size.className = 'mail-reader-attachment-size';
    size.textContent = formatBytes(att.size);
    li.append(link, size);
    mailReaderAttachments.appendChild(li);
  }
}

let mailReaderResizeObserver = null;

function resizeMailReaderIframe() {
  let doc;
  try {
    doc = mailReaderHtml.contentDocument;
  } catch {
    return; // shouldn't happen with allow-same-origin, but fall back to internal scroll if it does
  }
  if (!doc || !doc.documentElement) return;
  const root = doc.documentElement;

  // Measured once, while the iframe is still at the wrapper's (cramped, mobile) width —
  // some email HTML, rigid newsletter footers especially, has elements that just won't
  // shrink no matter what CSS gets injected into them, so this reports how wide the
  // content actually wants to be, overflow and all. Re-rendering the iframe at exactly
  // that width gives every element all the room it wants (nothing left to clip or wrap
  // awkwardly), then a CSS transform visually scales the whole thing back down to fit —
  // the same trick a "fit to screen" zoom does, rather than fighting each fixed-width
  // newsletter footer's CSS one quirk at a time.
  const naturalWidth = root.scrollWidth;
  const available = mailReaderHtmlWrap.clientWidth;
  const scale = naturalWidth > available ? available / naturalWidth : 1;
  mailReaderHtml.style.width = naturalWidth + 'px';
  mailReaderHtml.style.transform = scale < 1 ? `scale(${scale})` : '';

  const resizeHeight = () => {
    mailReaderHtml.style.height = root.scrollHeight + 'px';
    // the transform shrinks the iframe visually without affecting layout flow on its
    // own — without an explicit height here, the page would reserve the iframe's full
    // unscaled height's worth of space, leaving a tall blank gap below the shrunk email.
    mailReaderHtmlWrap.style.height = (root.scrollHeight * scale) + 'px';
  };
  resizeHeight();

  // Images load asynchronously and can grow the content well after the load event
  // fires — keep tracking height as that happens, not just once.
  if (mailReaderResizeObserver) mailReaderResizeObserver.disconnect();
  mailReaderResizeObserver = new ResizeObserver(resizeHeight);
  mailReaderResizeObserver.observe(root);
}
