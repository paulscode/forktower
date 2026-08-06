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

// Steps the user has chosen to skip.
//
// Kept in the browser rather than in the daemon, deliberately. Skipping is a
// statement about what this person wants to be shown, not a fact about the
// installation — and nothing is hidden by it: every skipped step is still in the
// readiness list below, still counted, still red if it is red. All that changes
// is whether the first-run panel keeps standing in front of it.
const SKIPPED_KEY = 'forktower.setup.skipped';

function skippedSteps() {
  try {
    const raw = window.localStorage.getItem(SKIPPED_KEY);
    return raw ? new Set(JSON.parse(raw)) : new Set();
  } catch {
    // A browser with storage disabled simply never skips anything, which is a
    // worse experience than intended and a better one than a broken page.
    return new Set();
  }
}

function skipStep(id) {
  try {
    const all = skippedSteps();
    all.add(id);
    window.localStorage.setItem(SKIPPED_KEY, JSON.stringify(Array.from(all)));
  } catch {
    // Nothing to do. The step reappears on the next pass, which is honest.
  }
}

// The first-run guidance: one thing to do, and what is being waited for.
function renderSetup(setup) {
  const card = el('setup-guide');
  if (!card) return;

  const step = setup && setup.step;
  // Gone for good once there is nothing left that would stop the user being
  // protected. The readiness list carries the same facts afterwards.
  if (!setup || setup.complete || !step || skippedSteps().has(step.id)) {
    show(card, false);
    return;
  }

  show(card, true);
  setText(el('setup-title'), step.label || 'One more thing');
  setText(el('setup-progress'),
    'Step ' + Math.min(setup.done + 1, setup.total) + ' of ' + setup.total);
  setText(el('setup-why'), step.why || '');
  setText(el('setup-detail'), step.detail || '');

  // The platform's own directions, when Forktower knows the platform. Absent
  // rather than invented: sending somebody to a screen that does not exist
  // wastes more of their time than saying nothing.
  const guidance = el('setup-guidance');
  clear(guidance);
  for (const line of step.guidance || []) {
    guidance.appendChild(make('li', null, line));
  }

  renderAction(el('setup-action'), step.action);

  // What is being waited for, so somebody who cannot finish knows they are
  // waiting rather than stuck.
  const waiting = setup.waiting || [];
  setText(el('setup-waiting'), waiting.length
    ? 'Also in progress, with nothing for you to do: ' + waiting.join(', ') + '.'
    : '');

  const skip = el('setup-skip');
  show(skip, Boolean(step.skippable));
  if (step.skippable) {
    setText(el('setup-skip-cost'), step.skip_cost || '');
    const button = el('setup-skip-confirm');
    button.onclick = null;
    button.addEventListener('click', () => {
      skipStep(step.id);
      show(card, false);
    }, { once: true });
  }
}

// The faster first sync.
//
// Shown only while it is worth showing: the offer, the transfer, and the short
// window after it succeeds. An installation past all of that never sees this
// card again, which is why it can afford to sit above the headline.
function renderBootstrap(view) {
  const card = el('bootstrap');
  if (!card) return;

  // Absent, switched off, or not applicable to this node. Nothing to say, and a
  // card explaining why a shortcut is unnecessary would be one more thing to
  // read on a page whose whole job is to be read quickly.
  if (!view || !view.available) {
    show(card, false);
    return;
  }

  show(card, true);
  setText(el('bootstrap-title'), view.title || '');
  setText(el('bootstrap-detail'), view.detail || '');
  setText(el('bootstrap-error'), view.error || '');

  // What the offer costs, listed before anybody agrees to it rather than after.
  const why = el('bootstrap-why');
  clear(why);
  for (const line of view.why || []) {
    why.appendChild(make('li', null, line));
  }

  const running = view.phase === 'downloading' || view.phase === 'loading';
  const bar = el('bootstrap-bar');
  show(bar, running);
  if (running) {
    // Clamped, because a percentage outside the bar is a rendering bug that
    // looks like a data bug.
    const percent = Math.max(0, Math.min(100, Number(view.percent) || 0));
    el('bootstrap-fill').style.width = percent.toFixed(1) + '%';
    bar.setAttribute('role', 'progressbar');
    bar.setAttribute('aria-valuenow', String(Math.round(percent)));
    bar.setAttribute('aria-valuemin', '0');
    bar.setAttribute('aria-valuemax', '100');
  }
  setText(el('bootstrap-progress'), view.human || '');

  renderAction(el('bootstrap-action'), view.action);
}

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
// The exposure table.
// ---------------------------------------------------------------------------

// The threat states that mean something is happening to a channel right now.
const AT_RISK_STATES = ['mempool', 'confirmed', 'loss'];

// formatSats says an amount the way the documentation says to: BTC for the
// magnitude a person thinks in, satoshis for the number that is exact. Built
// from integers throughout — an amount of money is not a floating-point
// question, and the string is assembled by hand rather than divided.
function formatSats(sats) {
  const n = Number(sats) || 0;
  if (n === 0) {
    return '—';
  }
  const whole = Math.floor(n / 100000000);
  const rest = String(n % 100000000).padStart(8, '0');
  return whole + '.' + rest + ' BTC';
}

function renderChannels(rows) {
  const card = el('exposure');
  const table = el('channels-table');
  const body = el('channels');
  const list = rows || [];

  // Hidden entirely when there is nothing to show, rather than an empty table
  // with headings: a page full of blank furniture reads as something broken.
  show(card, list.length > 0);
  show(el('channels-empty'), list.length === 0);
  show(table, list.length > 0);
  clear(body);

  let estimates = 0;
  for (const channel of list) {
    const display = channel.display || {};
    const threat = channel.threat || {};
    const row = make('tr', AT_RISK_STATES.includes(threat.state) ? 'at-risk' : '');

    // textContent throughout, via make(). The partner's name is chosen by the
    // counterparty and is the one string on this page an attacker controls.
    row.appendChild(make('td', 'partner', display.partner || ''));
    row.appendChild(make('td', 'amount', formatSats(display.at_risk_sat)));

    const time = make('td', 'time-left');
    if (display.time_left) {
      const value = make('span', display.time_left_is_estimate ? 'estimate' : '',
        display.time_left);
      time.appendChild(value);
      if (display.time_left_is_estimate) {
        estimates += 1;
      }
    } else {
      setText(time, '—');
    }
    row.appendChild(time);

    const status = make('td', 'status', display.status || '');
    if (display.status_action) {
      status.appendChild(make('span', 'row-action', display.status_action));
    }
    row.appendChild(status);

    body.appendChild(row);
  }

  // Said once, under the table, rather than repeated in every cell. A time
  // built from how fast a chain has recently been going is an estimate, and a
  // countdown that looks precise is one somebody will plan around.
  setText(el('channels-note'), estimates > 0
    ? 'Times are estimates, worked out from how fast the other chain has been going.'
    : '');
}

// ---------------------------------------------------------------------------
// Watchtowers.
// ---------------------------------------------------------------------------

// The states a tower can be in, as the card colours them. "not protecting"
// covers both a tower that is down and one that is perfectly healthy and
// receiving nothing — because those are the same thing from the point of view
// of whether the user's money is safe, even though the remedies differ.
const TOWER_BAD = 'not protecting';

// A tower's card: what it is doing, and per channel whether it actually covers
// it.
//
// **A reachable tower and a protecting tower are not the same thing.** A
// watchtower that is up, answering, and holding no sessions looks perfectly
// healthy from every angle except the only one that matters. So the summary
// leads with what is protected, not with whether the process is running.
function renderTowers(payload) {
  const card = el('towers-card');
  const list = el('towers');
  const towers = (payload && payload.towers) || [];

  // Hidden when there are none, and there is no "no watchtower" note inside it:
  // a note in a hidden card is a note nobody can read. Having no watchtower at
  // all is said in the readiness list instead, which is where "is this set up
  // properly" is answered and which is visible whether or not anything exists.
  show(card, towers.length > 0);
  clear(list);

  let anyUri = '';
  let anyKind = '';
  for (const tower of towers) {
    const display = tower.display || {};
    const item = make('li', display.state === TOWER_BAD ? 'tower at-risk' : 'tower');

    item.appendChild(make('p', 'tower-summary', display.summary || ''));

    // The uncovered channels first and by name. This is the failure that has no
    // other symptom, so it does not go below a fold.
    const uncovered = (tower.coverage || []).filter((c) => !c.coverable);
    if (uncovered.length > 0) {
      const reasons = make('ul', 'tower-gaps');
      for (const gap of uncovered) {
        reasons.appendChild(make('li', '', gap.reason || ''));
      }
      item.appendChild(reasons);
    }

    const fee = feeNote(tower.coverage || []);
    if (fee) {
      item.appendChild(make('p', 'quiet', fee));
    }

    if (tower.uri) {
      anyUri = tower.uri;
      anyKind = tower.kind;
    }
    list.appendChild(item);
  }

  renderTowerCommand(anyUri, anyKind);
}

// feeNote says what a justice transaction would pay, and that nobody can change
// it afterwards.
//
// Worth a line of its own because it is the one number here that is fixed
// forever at the moment a session is negotiated: the transaction is signed then,
// and the tower holds no keys with which to raise the fee later.
function feeNote(coverage) {
  const rates = coverage
    .map((c) => c.sweep_fee_sat_per_vbyte)
    .filter((r) => typeof r === 'number' && r > 0);
  if (rates.length === 0) {
    return '';
  }
  const lowest = Math.min(...rates);
  return 'A justice transaction from this tower would pay ' + lowest +
    ' sat/vB. That was fixed when the session was set up and cannot be raised ' +
    'afterwards — re-registering starts a new session at your node\'s current rate.';
}

// The command that registers each kind of tower with the node that speaks to it.
//
// An LND watchtower is registered from LND; a teos tower from Core Lightning.
// They are not interchangeable, and neither is the command.
const TOWER_COMMANDS = {
  lnd: 'lncli wtclient add ',
  teos: 'lightning-cli registertower ',
};

// renderTowerCommand fills in the copy-paste line in the wizard.
//
// The real address or nothing. A placeholder that looks like a command is worse
// than an empty box: somebody will paste it.
//
// **The same rule applies to the command itself, which is why this is keyed on
// the tower's kind.** It used to emit the LND form for every tower, so somebody
// running Core Lightning beside a teos tower was shown `lncli` — a program they
// do not have, driving a node they are not running. That is the failure the
// comment above was written about, arrived at from the other direction: the
// address was real and the verb was wrong.
function renderTowerCommand(uri, kind) {
  const box = el('tower-command');
  if (!box) {
    return;
  }
  const prefix = TOWER_COMMANDS[kind];
  const usable = Boolean(uri && prefix);
  setText(box, usable ? prefix + uri : '');
  show(box, usable);
}

// ---------------------------------------------------------------------------
// Copying between the chains.
// ---------------------------------------------------------------------------

// What the mirror decided about each of the user's transactions.
//
// **The refusals are shown, and they are the larger half.** Most of what the
// mirror sees it declines — on purpose, because copying the wrong thing puts
// money at risk on a chain where it was not at risk. A page showing only the
// copied ones would look like a feature that barely does anything, and would
// leave "why was that not copied?" unanswerable.
function renderMirror(payload) {
  const section = el('mirror-section');
  const list = el('mirror-list');
  const decisions = (payload && payload.decisions) || [];
  const summary = (payload && payload.summary) || {};

  show(section, decisions.length > 0);
  clear(list);
  setText(el('mirror-note'), summary.note || '');

  for (const decision of decisions) {
    const display = decision.display || {};
    const item = make('li', display.needs_you ? 'at-risk' : '');

    item.appendChild(make('span', 'detail-what', display.what || ''));
    if (display.short_txid) {
      item.appendChild(make('span', 'detail-id', display.short_txid));
    }
    list.appendChild(item);
  }
}

// The one control on this page that creates exposure rather than reducing it.
//
// Rendered as a list of channels with a switch each, never as a single global
// setting: the decision is about one channel's money, and a control that turned
// it on for everything at once would be a control somebody flips without
// thinking about any particular channel.
function renderFundingOptIn(rows) {
  const list = el('funding-optin-list');
  const channels = rows || [];

  clear(list);
  show(el('funding-optin-empty'), channels.length === 0);

  for (const channel of channels) {
    const display = channel.display || {};
    const item = make('li', '');

    const label = make('label', 'optin-row');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = Boolean(channel.mirror_funding_opt_in);
    box.addEventListener('change', () => setFundingOptIn(channel.id, box.checked, box));

    label.appendChild(box);
    label.appendChild(make('span', '', display.partner || 'a channel'));
    item.appendChild(label);
    item.appendChild(make('span', 'quiet', formatSats(channel.capacity_sat)));
    list.appendChild(item);
  }
}

// setFundingOptIn records the decision, and puts the switch back if it did not
// take. A control that looks changed and did not is worse than one that refuses
// visibly.
async function setFundingOptIn(channelId, enabled, box) {
  try {
    await api('POST', '/api/v1/channels/' + channelId + '/mirror-funding',
      { enabled });
  } catch (err) {
    box.checked = !enabled;
    setText(el('connection'), 'That change did not save. ' + (err.message || ''));
  }
}

// ---------------------------------------------------------------------------
// Details.
// ---------------------------------------------------------------------------

// What the exposure table's rows point at. The table answers who, how much and
// how long; this answers "what actually happened", for a reader who has asked.
//
// It has to exist, and that is not a stylistic point: a status telling somebody
// to open Details when there is no Details is worse than saying nothing, because
// they will go looking and conclude the page is broken.
function renderDetails(spends, deadlines) {
  const spendList = el('details-spends');
  const deadlineList = el('details-deadlines');
  clear(spendList);
  clear(deadlineList);

  const events = spends || [];
  const clocks = deadlines || [];
  show(el('details-empty'), events.length === 0 && clocks.length === 0);

  for (const spend of events) {
    const display = spend.display || {};
    const row = make('li');
    row.appendChild(make('span', 'what', display.what || ''));
    // Everything a reader opened this for: where, which transaction, which
    // block, and whether it is a fact about the chain or only a sighting.
    const facts = [display.where, display.short_txid];
    if (spend.block_height) {
      facts.push('block ' + spend.block_height);
    }
    if (!display.confirmed) {
      facts.push('not yet in a block');
    }
    row.appendChild(make('span', 'facts', facts.filter(Boolean).join(' · ')));
    spendList.appendChild(row);
  }

  for (const clock of clocks) {
    const display = clock.display || {};
    const row = make('li');
    row.appendChild(make('span', 'what', display.what || ''));
    const facts = [];
    if (display.time_left) {
      facts.push((display.time_left_is_estimate ? '~' : '') + display.time_left);
    }
    facts.push(clock.remaining_blocks + ' blocks left');
    facts.push('at block ' + clock.deadline_height);
    row.appendChild(make('span', 'facts', facts.join(' · ')));
    if (display.note) {
      row.appendChild(make('p', 'why', display.note));
    }
    deadlineList.appendChild(row);
  }
}

// ---------------------------------------------------------------------------
// Watching, and asking for a second look.
// ---------------------------------------------------------------------------

// The message shown while watching has been turned off. Written out rather than
// assembled, because it is the one line on this page that has to say "nothing is
// being checked" without any hedging.
const STOOD_DOWN =
  'Watching the other chain is turned off. Nothing there is being checked.';

function renderStandDown(items) {
  const banner = el('stood-down');
  const item = (items || []).find((entry) => entry.id === 'watching_active');
  const off = Boolean(item) && !item.ok;

  show(banner, off);
  setText(banner, off ? STOOD_DOWN : '');
}

async function requestRescan() {
  const result = el('rescan-result');
  setText(result, 'Asking…');
  try {
    const queued = await api('POST', '/api/v1/rescan', {});
    setText(result, queued.display || 'Re-reading the other chain.');
  } catch (err) {
    // The refusal this endpoint gives when there is nothing behind the current
    // position is a real answer, not a failure, and is shown as such.
    setText(result, err.message);
  }
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
    renderStandDown(status.readiness);
    renderAdvanced(status);
    setText(el('connection'), '');

    const channels = await api('GET', '/api/v1/channels');
    renderChannels(channels);

    const towers = await api('GET', '/api/v1/towers');
    renderTowers(towers);

    const mirror = await api('GET', '/api/v1/mirror');
    renderMirror(mirror);
    renderFundingOptIn(channels);

    const spends = await api('GET', '/api/v1/spends');
    const deadlines = await api('GET', '/api/v1/deadlines');
    renderDetails(spends, deadlines);

    const setup = await api('GET', '/api/v1/setup');
    renderSetup(setup);

    const bootstrap = await api('GET', '/api/v1/bootstrap');
    renderBootstrap(bootstrap);

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
  el('rescan').addEventListener('click', requestRescan);

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
    renderSetup, renderBootstrap,
    renderChannels, renderDetails, renderTowers, renderMirror,
    renderFundingOptIn, renderStandDown, formatSats,
    describeTestResults, formatDuration, formatPercent, setText, KNOWN_STATES,
    refresh, api,
  };
}
