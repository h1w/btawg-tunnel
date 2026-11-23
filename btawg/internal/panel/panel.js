const view = document.querySelector('[data-live]');
if (view) {
  const rows = Array.from(view.querySelectorAll('[data-client]'));
  const health = view.querySelector('[data-health]');
  const reload = view.querySelector('[data-reload]');
  const labels = {active: 'Active', idle: 'Idle', never: 'Never seen', revoked: 'Revoked'};
  const date = new Intl.DateTimeFormat(undefined, {dateStyle: 'medium', timeStyle: 'short'});
  const number = new Intl.NumberFormat(undefined, {maximumFractionDigits: 1});
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  function bytes(value) {
    let i = 0;
    while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
    return `${number.format(value)} ${units[i]}`;
  }
  const rate = n => n === null ? '—' : `${bytes(n)}/s`;
  const elapsed = (n, before, seconds) => seconds > 0 && n >= before ? (n - before) / seconds : null;
  function apply(snapshot, prev) {
    const seconds = prev ? (snapshot.at - prev.at) / 1000 : 0;
    let active = 0, up = 0, down = 0, total = 0;
    for (const [id, now] of Object.entries(snapshot.clients)) {
      const was = prev?.clients[id];
      up += was ? elapsed(now.up, was.up, seconds) ?? 0 : 0;
      down += was ? elapsed(now.down, was.down, seconds) ?? 0 : 0;
      total += now.up + now.down;
      if (now.status === 'active') active++;
    }
    const summary = {active: `${active} / ${Object.keys(snapshot.clients).length}`, up: rate(seconds > 0 ? up : null), down: rate(seconds > 0 ? down : null), total: bytes(total)};
    for (const cell of view.querySelectorAll('[data-metric]')) cell.textContent = summary[cell.dataset.metric];
    for (const row of rows) {
      const now = snapshot.clients[row.dataset.client];
      if (!now) continue;
      const was = prev?.clients[row.dataset.client];
      const badge = row.querySelector('[data-status]');
      badge.textContent = labels[now.status];
      badge.className = 'tag' + (now.status === 'active' ? ' ok' : now.status === 'revoked' ? ' revoked' : '');
      const seen = row.querySelector('[data-seen]');
      seen.textContent = now.last_seen ? date.format(now.last_seen) : '—';
      row.querySelector('[data-rates]').textContent = `↑ ${rate(was ? elapsed(now.up, was.up, seconds) : null)} · ↓ ${rate(was ? elapsed(now.down, was.down, seconds) : null)}`;
      row.querySelector('[data-total]').textContent = bytes(now.up + now.down);
      row.querySelector('[data-breakdown]').textContent = `↑ ${bytes(now.up)} · ↓ ${bytes(now.down)}`;
    }
  }
  const filter = view.querySelector('[data-filter]');
  filter?.addEventListener('input', () => {
    const query = filter.value.trim().toLocaleLowerCase();
    let visible = 0;
    for (const row of rows) {
      row.hidden = !row.dataset.search.toLocaleLowerCase().includes(query);
      if (!row.hidden) visible++;
    }
    view.querySelector('[data-no-matches]').hidden = visible !== 0;
  });
  let previous, timer, stopped = false, pending = false;
  function schedule(delay) {
    clearTimeout(timer);
    if (!stopped && !document.hidden) timer = setTimeout(refresh, delay);
  }
  async function refresh() {
    if (stopped || pending || document.hidden) return;
    pending = true;
    try {
      const res = await fetch('/api/traffic', {cache: 'no-store', signal: AbortSignal.timeout(5000)});
      if (res.redirected || res.status === 401) {
        stopped = true;
        health.textContent = 'Session expired. Reload to sign in.';
        return;
      }
      if (!res.ok) throw new Error('Stats request failed');
      const snapshot = await res.json();
      const rosterChanged = Object.keys(snapshot.clients).length !== rows.length || rows.some(row => {
        const client = snapshot.clients[row.dataset.client];
        return !client || (client.status === 'revoked') !== (row.dataset.revoked === 'true');
      });
      reload.hidden = !rosterChanged;
      // Keep any in-progress access edits on the Clients page intact.
      if (rosterChanged && location.pathname === '/' && !filter?.value) {
        stopped = true;
        location.reload();
        return;
      }
      apply(snapshot, previous);
      previous = snapshot;
      const label = rosterChanged ? 'Live metrics · client list changed' : 'Live · updates every 2 s';
      if (health.textContent !== label) health.textContent = label;
    } catch {
      previous = undefined;
      health.textContent = 'Live updates unavailable · showing last known values';
    } finally {
      pending = false;
      schedule(2000);
    }
  }
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) { clearTimeout(timer); previous = undefined; }
    else schedule(0);
  });
  schedule(0);
}
document.querySelectorAll('[data-copy]').forEach(copy => copy.addEventListener('click', async () => {
  const text = document.getElementById(copy.dataset.copy);
  const message = document.querySelector(`[data-copy-status="${copy.dataset.copy}"]`);
  try {
    await navigator.clipboard.writeText(text.textContent.trim());
    message.textContent = 'Copied.';
  } catch {
    const range = document.createRange();
    range.selectNodeContents(text);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    message.textContent = 'Selected. Copy using your browser or keyboard.';
  }
}));
