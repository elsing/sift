// Tags are global to the account (owner), not per-mail-account or per-folder — a
// short, flat list the user builds up over time, applied to individual messages.
import { dryRunHeaders, withBusyButton } from './util.js';
import { confirmModal } from './confirmModal.js';

let cachedTags = null;

export async function fetchTags(force) {
  if (cachedTags && !force) return cachedTags;
  const res = await fetch('/api/tags');
  if (!res.ok) throw new Error(await res.text());
  cachedTags = await res.json();
  return cachedTags;
}

export async function createTag(name) {
  const res = await fetch('/api/tags', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) throw new Error(await res.text());
  cachedTags = null; // invalidate — next fetchTags() picks up the new one
  refreshFolderAutoTagSelect(); // the folder banner's auto-tag dropdown is built once
  // per folder visit and otherwise has no reason to know a brand-new tag just showed up
  return res.json();
}

export async function updateTag(id, changes) {
  const res = await fetch(`/api/tags/${id}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(changes),
  });
  if (!res.ok) throw new Error(await res.text());
  cachedTags = null;
  return res.json();
}

export async function deleteTag(id) {
  const res = await fetch(`/api/tags/${id}`, { method: 'DELETE' });
  if (!res.ok) throw new Error(await res.text());
  cachedTags = null;
}

export async function getMailTags(mailId) {
  const res = await fetch(`/api/mails/${mailId}/tags`);
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function setMailTags(mailId, tagIds) {
  const res = await fetch(`/api/mails/${mailId}/tags`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ tagIds }),
  });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

async function getFolderTagRules(accountId) {
  const res = await fetch(`/api/accounts/${accountId}/folder-tag-rules`);
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function setFolderTagRule(accountId, folder, tagId) {
  const res = await fetch(`/api/accounts/${accountId}/folder-tag-rules`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ folder, tagId }),
  });
  if (!res.ok) throw new Error(await res.text());
}

// Tag->folder, the other direction of the same folder_tag_rules table — "this tag
// goes to that folder" rather than "mail moved here gets that tag". Pooled across all
// the owner's accounts, like tag scoring already is.
export async function fetchTagDestinations() {
  const res = await fetch('/api/tag-destinations');
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function setTagDestination(accountId, tagId, folder) {
  const res = await fetch(`/api/accounts/${accountId}/tag-destination`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ folder, tagId }),
  });
  if (!res.ok) throw new Error(await res.text());
}

export async function deleteTagDestination(accountId, tagId) {
  const res = await fetch(`/api/accounts/${accountId}/tag-destination?tagId=${encodeURIComponent(tagId)}`, { method: 'DELETE' });
  if (!res.ok) throw new Error(await res.text());
}

async function deleteFolderTagRule(accountId, folder) {
  const res = await fetch(`/api/accounts/${accountId}/folder-tag-rules?folder=${encodeURIComponent(folder)}`, { method: 'DELETE' });
  if (!res.ok) throw new Error(await res.text());
}

// Streamed over SSE, not a single request-response — a large folder can take a while
// (it pages through the whole thing, not just the most recent batch), and a plain
// fetch() gave no sign anything was happening until it finally resolved. onProgress is
// called after each page with { page, applied } so far.
function applyTagToFolder(accountId, folder, tagId, onProgress) {
  return new Promise((resolve, reject) => {
    const params = new URLSearchParams({ folder, tagId });
    const source = new EventSource(`/api/accounts/${accountId}/folders/apply-tag?${params}`);
    source.addEventListener('progress', (e) => onProgress && onProgress(JSON.parse(e.data)));
    source.addEventListener('complete', (e) => {
      source.close();
      resolve(JSON.parse(e.data));
    });
    source.addEventListener('error', (e) => {
      source.close();
      const message = e.data ? JSON.parse(e.data).message : "Couldn't apply the tag — try again.";
      reject(new Error(message));
    });
  });
}

// Populates the folder banner's "auto-tag this folder" dropdown and wires it up —
// moving mail into a folder with a rule set auto-applies that tag (see
// applyFolderTagRule server-side, called from the move handler). applyBtn is the
// companion "Apply to all" button: the rule above only ever applies going forward, to
// mail moved in after it's set — this is the retroactive version, for everything
// already sitting in the folder.
// Tracks the most recently set-up folder auto-tag select so a brand-new tag (created
// from anywhere — Tags settings, the reader, a scan candidate) can refresh it without
// needing to know about the folder banner directly.
let activeFolderAutoTag = null;

export function refreshFolderAutoTagSelect() {
  if (!activeFolderAutoTag) return;
  const { selectEl, applyBtn, accountId, folder } = activeFolderAutoTag;
  setupFolderAutoTagSelect(selectEl, applyBtn, accountId, folder);
}

export async function setupFolderAutoTagSelect(selectEl, applyBtn, accountId, folder) {
  activeFolderAutoTag = { selectEl, applyBtn, accountId, folder };
  selectEl.innerHTML = '<option value="">Auto-tag: none</option>';
  selectEl.onchange = null;
  applyBtn.classList.add('hidden');
  applyBtn.onclick = null;
  try {
    const [allTags, rules] = await Promise.all([fetchTags(), getFolderTagRules(accountId)]);
    const current = rules.find((r) => r.folder === folder);
    for (const tag of allTags) {
      const opt = document.createElement('option');
      opt.value = tag.id;
      opt.textContent = 'Auto-tag: ' + tag.name;
      if (current && current.tag.id === tag.id) opt.selected = true;
      selectEl.appendChild(opt);
    }
    applyBtn.classList.toggle('hidden', !selectEl.value);
    selectEl.onchange = async () => {
      if (selectEl.value) await setFolderTagRule(accountId, folder, selectEl.value);
      else await deleteFolderTagRule(accountId, folder);
      applyBtn.classList.toggle('hidden', !selectEl.value);
    };
    applyBtn.onclick = async () => {
      if (!selectEl.value) return;
      const tagName = selectEl.selectedOptions[0].textContent.replace('Auto-tag: ', '');
      if (!(await confirmModal(`Apply "${tagName}" to every email already in this folder?`))) return;
      applyBtn.disabled = true;
      applyBtn.textContent = 'Applying…';
      try {
        const { applied } = await applyTagToFolder(accountId, folder, selectEl.value, ({ applied }) => {
          applyBtn.textContent = `Applying… (${applied} so far)`;
        });
        applyBtn.textContent = `Applied to ${applied}`;
        setTimeout(() => { applyBtn.textContent = 'Apply to all'; }, 2000);
      } catch (err) {
        applyBtn.textContent = 'Apply to all';
        alert(err.message);
      }
      applyBtn.disabled = false;
    };
  } catch {
    // best-effort — worst case the dropdown just stays on "none"
  }
}

export function renderTagChips(tags) {
  if (!tags || tags.length === 0) return null;
  const wrap = document.createElement('div');
  wrap.className = 'mail-tags';
  for (const tag of tags) {
    const chip = document.createElement('span');
    chip.className = 'mail-tag-chip';
    chip.style.background = tag.color;
    chip.textContent = tag.name;
    wrap.appendChild(chip);
  }
  return wrap;
}

// Smart-tagging: suggestions are a distinct concept from applied tags (nothing's been
// committed to message_tags yet), so they get their own chip style — dashed outline,
// with accept/dismiss baked right into the chip instead of a static label.
export async function fetchTagHistory(status, mailId, source) {
  const params = new URLSearchParams();
  if (status) params.set('status', status);
  if (mailId) params.set('mailId', mailId);
  if (source) params.set('source', source);
  const res = await fetch(`/api/tag-history?${params}`);
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

// The undo half of full-auto mode — reverses an already-applied auto-tag and teaches
// the scorer not to repeat it (same effect as a manual dismiss, see the server side).
export async function undoTagHistory(id) {
  const res = await fetch(`/api/tag-history/${id}/undo`, { method: 'POST' });
  if (!res.ok) throw new Error(await res.text());
}

export async function acceptSuggestion(id) {
  const res = await fetch(`/api/tag-history/${id}/accept`, { method: 'POST' });
  if (!res.ok) throw new Error(await res.text());
}

export async function dismissSuggestion(id) {
  const res = await fetch(`/api/tag-history/${id}/dismiss`, { method: 'POST' });
  if (!res.ok) throw new Error(await res.text());
}

// The broader, explicit-opt-in sibling of dismiss — "stop suggesting this tag for this
// sender at all", not just "wrong for this one email". A separate user choice, not
// something a plain dismiss should imply on its own.
export async function blockSenderForSuggestion(id) {
  const res = await fetch(`/api/tag-history/${id}/block-sender`, { method: 'POST' });
  if (!res.ok) throw new Error(await res.text());
}

// onResolved is called (with no args) after either button is tapped, once the request
// completes — callers re-fetch/re-render rather than this trying to patch state itself.
export function renderSuggestionChips(suggestions, onResolved) {
  if (!suggestions || suggestions.length === 0) return null;
  const wrap = document.createElement('div');
  wrap.className = 'mail-tags';
  for (const sug of suggestions) {
    const chip = document.createElement('span');
    chip.className = 'mail-tag-chip suggested';
    chip.style.borderColor = sug.tagColor;
    // "Suggest:" prefix, not just the bare tag name — otherwise a suggested chip
    // sitting next to real applied-tag chips just reads as another tag, with no link
    // between it and the accept/dismiss buttons tacked onto the end of it.
    const label = document.createElement('span');
    label.textContent = `Suggest: ${sug.tagName}`;
    const accept = document.createElement('button');
    accept.type = 'button';
    accept.textContent = '✓';
    accept.title = `Accept "${sug.tagName}"`;
    accept.addEventListener('click', async () => {
      dismiss.disabled = true; // same row's pair — neither should be tappable mid-request
      try {
        await withBusyButton(accept, '…', () => acceptSuggestion(sug.id)); // compact glyph-sized chip, no room for a full "Accepting…"
        onResolved();
      } catch (err) {
        console.error(err);
      } finally {
        dismiss.disabled = false;
      }
    });
    const dismiss = document.createElement('button');
    dismiss.type = 'button';
    dismiss.textContent = '✕';
    dismiss.title = `Dismiss "${sug.tagName}"`;
    dismiss.addEventListener('click', async () => {
      accept.disabled = true;
      try {
        await withBusyButton(dismiss, '…', () => dismissSuggestion(sug.id));
        onResolved();
      } catch (err) {
        console.error(err);
      } finally {
        accept.disabled = false;
      }
    });
    chip.append(label, accept, dismiss);
    wrap.appendChild(chip);
  }
  return wrap;
}

// Mail that's tagged with something that has a folder destination assigned, but isn't
// actually sitting there — autoMoveTaggedMail only ever moves mail *out of inbox*, on
// a delay, so a message that's already filed somewhere (just the wrong somewhere) was
// never touched no matter how clearly a tag said otherwise.
export async function fetchMisplacedMail() {
  const res = await fetch('/api/misplaced-mail');
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function moveMail(mailId, folder) {
  const res = await fetch(`/api/mails/${mailId}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...dryRunHeaders() },
    body: JSON.stringify({ folder }),
  });
  if (!res.ok) throw new Error(await res.text());
}
