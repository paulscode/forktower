// Forktower dashboard.
//
// No framework, no build step, no third-party code. That is not minimalism for
// its own sake: this page is served from a machine holding someone's Lightning
// funds, and every dependency is a way for someone else's mistake to end up
// here.
//
// Two rules hold everywhere below.
//
// 1. Nothing from the server is ever written as HTML. Every value goes into the
//    DOM through setText, which uses textContent. There is no framework doing
//    escaping here, and a later version of this API carries a channel partner's
//    chosen name — 32 bytes picked by the counterparty, who is the adversary.
//
// 2. The headline is rendered verbatim. Its wording is decided on the server so
//    that it exists in one reviewable place; reassembling or "improving" it here
//    would put a second copy in a file nobody reviews for tone.

'use strict';

const POLL_INTERVAL_MS = 5000;
const TIMELINE_LIMIT = 50;

// The five states the server may send. Anything else still renders — a value
// this page does not recognise must not produce a blank screen.
const KNOWN_STATES = [
  'getting_ready', 'protected', 'attention', 'action_needed', 'at_risk',
];

const STATE_LABELS = {
  getting_ready: 'Setting up',
  protected: 'All well',
  attention: 'Worth a look',
  action_needed: 'Needs you',
  at_risk: 'Urgent',
};

const TIER_WORDS = {
  info: 'Note',
  warning: 'Worth a look',
  critical: 'Urgent',
  resolved: 'Resolved',
  loss: 'Loss',
};

let lastTimelineID = 0;
let pollTimer = null;

// ---------------------------------------------------------------------------
// DOM helpers. Every one of them writes text, never markup.
// ---------------------------------------------------------------------------

function el(id) {
  return document.getElementById(id);
}

// setText is the only way anything from the server reaches the screen.
function setText(node, value) {
  node.textContent = value === undefined || value === null ? '' : String(value);
}

function make(tag, className, text) {
  const node = document.createElement(tag);
  if (className) {
    node.className = className;
  }
  if (text !== undefined) {
    setText(node, text);
  }
  return node;
}

function clear(node) {
  while (node.firstChild) {
    node.removeChild(node.firstChild);
  }
}

function show(node, visible) {
  if (visible) {
    node.classList.remove('hidden');
  } else {
    node.classList.add('hidden');
  }
}

// ---------------------------------------------------------------------------
// Talking to the daemon.
// ---------------------------------------------------------------------------

async function api(method, path, body) {
  const options = {
    method,
    // The daemon refuses an unsafe request whose origin is not this page, so
    // the browser's own Origin header is doing real work here.
    credentials: 'same-origin',
    headers: {},
  };
  if (body !== undefined) {
    options.headers['Content-Type'] = 'application/json';
    options.body = JSON.stringify(body);
  }

  const response = await fetch(path, options);
  if (response.status === 204) {
    return null;
  }

  let envelope = null;
  try {
    envelope = await response.json();
  } catch (err) {
    throw new Error('Forktower sent something this page could not read.');
  }
  if (envelope && envelope.error) {
    const failure = new Error(envelope.error.message || 'Something went wrong.');
    failure.code = envelope.error.code;
    failure.status = response.status;
    throw failure;
  }
  if (!response.ok) {
    const failure = new Error('Something went wrong.');
    failure.status = response.status;
    throw failure;
  }
  return envelope ? envelope.data : null;
}

// ---------------------------------------------------------------------------
// The headline. Rendered exactly as sent.
// ---------------------------------------------------------------------------

function renderHeadline(headline) {
  const card = el('headline');
  const state = headline && headline.state ? String(headline.state) : '';

  for (const known of KNOWN_STATES) {
    card.classList.remove('state-' + known);
  }
  if (KNOWN_STATES.indexOf(state) !== -1) {
    card.classList.add('state-' + state);
  }

  // A short word for the badge. Falling back to nothing rather than printing the
  // raw state keeps an internal name off the screen even if one ever arrives.
  setText(el('headline-state-label'), STATE_LABELS[state] || '');
  setText(el('headline-title'), headline ? headline.title : '');
  setText(el('headline-detail'), headline ? headline.detail : '');
  setText(el('headline-since'), sinceText(headline));

  renderAction(el('headline-action'), headline ? headline.action : null);
}

function sinceText(headline) {
  if (!headline || !headline.since) {
    return '';
  }
  return 'Since ' + formatTime(headline.since) + '.';
}

// renderAction wires the single next step a state may carry. At most one, ever:
// a screen offering three things to try is a screen nobody acts on.
function renderAction(button, action) {
  button.onclick = null;
  if (!action || !action.label) {
    show(button, false);
    return;
  }
  setText(button, action.label);
  show(button, true);
  button.addEventListener('click', () => {
    if (action.endpoint) {
      runEndpointAction(action.endpoint);
    } else if (action.href) {
      openAdvancedIfNeeded(action.href);
      window.location.hash = action.href.replace(/^#/, '');
    }
  }, { once: true });
}

function openAdvancedIfNeeded(href) {
  if (href === '#setup' || href === '#exposure') {
    const advanced = el('advanced');
    if (advanced) {
      advanced.open = true;
    }
  }
}

async function runEndpointAction(endpoint) {
  if (endpoint === '/api/v1/alerts/test') {
    await sendTestAlert();
    return;
  }
  try {
    await api('POST', endpoint, {});
    await refresh();
  } catch (err) {
    setText(el('connection'), err.message);
  }
}

// ---------------------------------------------------------------------------
// Readiness. Rendered from label / why / action — never from the id.
// ---------------------------------------------------------------------------

function renderReadiness(items) {
  const list = el('readiness');
  clear(list);

  for (const item of items || []) {
    const row = make('li', item.ok ? 'ok' : 'problem-item');
    row.appendChild(make('span', 'mark', item.ok ? '✓' : '!'));
    row.appendChild(make('span', 'label', item.label));

    if (item.why) {
      row.appendChild(make('p', 'why', item.why));
    }
    if (item.detail) {
      row.appendChild(make('p', 'detail', item.detail));
    }
    if (item.action && item.action.label) {
      const button = make('button', 'secondary');
      renderAction(button, item.action);
      row.appendChild(button);
    }
    list.appendChild(row);
  }
}

// ---------------------------------------------------------------------------
// Alerts.
// ---------------------------------------------------------------------------

function renderAlerts(alerts) {
  const list = el('alerts');
  clear(list);

  const rows = (alerts || []).slice().reverse();
  show(el('alerts-empty'), rows.length === 0);

  for (const alert of rows) {
    const row = make('li');
    const tier = String(alert.tier || '');
    row.appendChild(make('p', 'tier tier-' + tier, TIER_WORDS[tier] || ''));
    row.appendChild(make('p', 'message', alert.message));

    if (alert.acked_at) {
      row.appendChild(make('p', 'acked', 'You have seen this.'));
      list.appendChild(row);
      continue;
    }

    const got = make('button', 'secondary', 'Got it');
    got.addEventListener('click', async () => {
      got.disabled = true;
      try {
        await api('POST', '/api/v1/alerts/' + encodeURIComponent(alert.id) + '/ack');
        await refresh();
      } catch (err) {
        got.disabled = false;
        setText(el('connection'), err.message);
      }
    });
    row.appendChild(got);
    list.appendChild(row);
  }
}

async function sendTestAlert() {
  const button = el('test-alerts');
  const result = el('test-result');
  button.disabled = true;
  setText(result, 'Sending…');

  try {
    const results = await api('POST', '/api/v1/alerts/test', {});
    setText(result, describeTestResults(results));
  } catch (err) {
    setText(result, err.message);
  } finally {
    button.disabled = false;
    await refresh();
  }
}

function describeTestResults(results) {
  if (!results || results.length === 0) {
    return 'There is nowhere to send a test message yet.';
  }
  const failed = results.filter((r) => !r.ok).map((r) => r.transport);
  if (failed.length === 0) {
    return results.length === 1
      ? 'Sent. Check that it arrived.'
      : 'Sent to all ' + results.length + '. Check that they arrived.';
  }
  return 'Could not send to ' + failed.join(', ') + '.';
}

// ---------------------------------------------------------------------------
// Timeline.
// ---------------------------------------------------------------------------

function renderTimeline(entries) {
  const list = el('timeline');

  for (const entry of entries || []) {
    const row = make('li');
    row.appendChild(make('span', 'when', formatTime(entry.at) + ' — '));
    row.appendChild(make('span', 'what', entry.summary));
    list.insertBefore(row, list.firstChild);

    if (entry.id > lastTimelineID) {
      lastTimelineID = entry.id;
    }
  }

  while (list.childElementCount > TIMELINE_LIMIT) {
    list.removeChild(list.lastChild);
  }
  show(el('timeline-empty'), list.childElementCount === 0);
}

// ---------------------------------------------------------------------------
// Advanced. Everything with a hash or a height in it lives here.
// ---------------------------------------------------------------------------

function renderAdvanced(status) {
  const branches = el('branches');
  clear(branches);

  const names = { sf: 'Your node’s chain', sq: 'The other chain' };
  const split = status.split || {};

  for (const key of ['sf', 'sq']) {
    const info = (split.branches || {})[key] || {};
    const tile = make('div', 'tile');
    tile.appendChild(make('h4', null, names[key]));

    const dl = document.createElement('dl');
    addPair(dl, 'Latest block', info.tip_height ? String(info.tip_height) : 'not known yet');
    addPair(dl, 'Block hash', info.tip_hash || '—');
    if (split.fork) {
      addPair(dl, 'Blocks since the chains separated', String(info.since_fork_depth || 0));
    }
    if (info.avg_interval_secs) {
      addPair(dl, 'Average time between blocks', formatDuration(info.avg_interval_secs));
    }
    tile.appendChild(dl);
    branches.appendChild(tile);
  }

  if (split.fork) {
    const tile = make('div', 'tile');
    tile.appendChild(make('h4', null, 'Where the chains separated'));
    const dl = document.createElement('dl');
    addPair(dl, 'Height', String(split.fork.height));
    addPair(dl, 'Block hash', split.fork.hash);
    addPair(dl, 'Noticed', formatTime(split.fork.detected_at));
    tile.appendChild(dl);
    branches.appendChild(tile);
  }

  const views = el('views');
  clear(views);
  for (const key of ['sf', 'sq']) {
    const view = (status.views || {})[key] || {};
    const tile = make('div', 'tile');
    tile.appendChild(make('h4', null, names[key]));
    const dl = document.createElement('dl');
    addPair(dl, 'Connections to other nodes', String(view.peer_count || 0));
    addPair(dl, 'Caught up', formatPercent(view.sync_progress));
    if (view.software) {
      addPair(dl, 'Software', view.software);
    }
    if (view.detail) {
      addPair(dl, 'Note', view.detail);
    }
    tile.appendChild(dl);
    views.appendChild(tile);
  }
}

function addPair(dl, term, value) {
  dl.appendChild(make('dt', null, term));
  dl.appendChild(make('dd', null, value));
}

// ---------------------------------------------------------------------------
// Formatting. Times and durations as a person reads them, never as a count of
// seconds or a raw unix stamp.
// ---------------------------------------------------------------------------

function formatTime(unixSeconds) {
  if (!unixSeconds) {
    return '';
  }
  return new Date(unixSeconds * 1000).toLocaleString();
}

function formatDuration(seconds) {
  const value = Number(seconds);
  if (!isFinite(value) || value <= 0) {
    return 'not known yet';
  }
  if (value < 90) {
    return Math.round(value) + ' seconds';
  }
  if (value < 5400) {
    return 'about ' + Math.round(value / 60) + ' minutes';
  }
  return 'about ' + Math.round(value / 3600) + ' hours';
}

function formatPercent(fraction) {
  const value = Number(fraction);
  if (!isFinite(value)) {
    return 'not known';
  }
  if (value >= 1) {
    return 'fully';
  }
  return Math.round(value * 100) + '%';
}

// ---------------------------------------------------------------------------
// Polling.
// ---------------------------------------------------------------------------

async function refresh() {
  try {
    const status = await api('GET', '/api/v1/status');
    show(el('signin'), false);

    renderHeadline(status.headline);
    renderReadiness(status.readiness);
    renderAdvanced(status);
    setText(el('connection'), '');

    const alerts = await api('GET', '/api/v1/alerts');
    renderAlerts(alerts);

    const entries = await api('GET', '/api/v1/timeline?after_id=' + lastTimelineID);
    renderTimeline(entries);
  } catch (err) {
    if (err.status === 401) {
      show(el('signin'), true);
      setText(el('connection'), '');
      return;
    }
    // Say that the page is stale rather than replacing a true headline with a
    // false one: the last state shown was real, and silently blanking it would
    // be worse than admitting the page has lost touch.
    setText(el('connection'), 'Cannot reach Forktower right now. Showing the last thing it said.');
  }
}

async function signIn(event) {
  event.preventDefault();
  const error = el('signin-error');
  setText(error, '');
  try {
    await api('POST', '/api/v1/login', { password: el('password').value });
    el('password').value = '';
    await refresh();
  } catch (err) {
    setText(error, err.message);
  }
}

function start() {
  el('signin-form').addEventListener('submit', signIn);
  el('test-alerts').addEventListener('click', sendTestAlert);

  refresh();
  pollTimer = window.setInterval(refresh, POLL_INTERVAL_MS);
}

if (typeof document !== 'undefined' && typeof window !== 'undefined' && window.document) {
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
}

// Exported for the tests, which drive these against a minimal DOM. Guarded so
// the browser never sees it.
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    renderHeadline, renderReadiness, renderAlerts, renderTimeline, renderAdvanced,
    describeTestResults, formatDuration, formatPercent, setText, KNOWN_STATES,
    refresh, api,
  };
}
