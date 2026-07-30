// Rendering tests for the dashboard, run by `go test ./web`.
//
// Driven against the shim next door, which implements textContent and refuses
// innerHTML — so the rule that nothing from the server becomes markup is
// enforced at runtime here, not merely asserted by reading the code.

'use strict';

const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const { install } = require('./domshim.js');

const { byID } = install();
const app = require('./app.js');

// The string this test exists for. A later version of the API carries a channel
// partner's chosen name — bytes picked by the counterparty, who is the adversary
// in this threat model — and there is no framework here doing escaping.
const HOSTILE = '<img src=x onerror=alert(1)>';

let failures = 0;

function test(name, fn) {
  try {
    fn();
    console.log('  ok   ' + name);
  } catch (err) {
    failures++;
    console.log('  FAIL ' + name + '\n       ' + err.message);
  }
}

// textOf collects everything the user would actually read.
function textOf(node) {
  return node.textContent;
}

test('a hostile string in a headline stays literal text', () => {
  app.renderHeadline({
    state: 'at_risk',
    title: HOSTILE,
    detail: 'A channel with ' + HOSTILE + ' is being closed.',
    action: null,
    since: 0,
  });

  assert.strictEqual(byID['headline-title'].textContent, HOSTILE,
    'the title was not rendered as the exact characters sent');
  assert.ok(byID['headline-detail'].textContent.includes(HOSTILE),
    'the detail lost the literal text');
  // No element was created from it: if it had been parsed as markup there would
  // be a child node rather than a text value.
  assert.strictEqual(byID['headline-title'].children.length, 0,
    'the hostile string produced elements');
});

test('a hostile alert message stays literal text', () => {
  app.renderAlerts([{ id: 1, tier: 'critical', message: HOSTILE, acked_at: 0 }]);

  const row = byID['alerts'].find((n) => n.className === 'message');
  assert.ok(row, 'the alert message was not rendered at all');
  assert.strictEqual(row.textContent, HOSTILE);
  assert.strictEqual(row.children.length, 0, 'the message produced elements');
});

test('a hostile readiness label and reason stay literal text', () => {
  app.renderReadiness([{
    id: 'sq_synced', ok: false, label: HOSTILE, why: HOSTILE, detail: HOSTILE, action: null,
  }]);

  const rendered = textOf(byID['readiness']);
  assert.ok(rendered.includes(HOSTILE), 'the label was altered');
  assert.strictEqual(
    byID['readiness'].find((n) => n.tagName === 'IMG'), null,
    'an element was created from a server value');
});

test('a hostile timeline summary stays literal text', () => {
  app.renderTimeline([{ id: 1, at: 1790000000, kind: 'x', summary: HOSTILE }]);
  assert.ok(textOf(byID['timeline']).includes(HOSTILE));
});

test('a hostile block hash stays literal text', () => {
  app.renderAdvanced({
    split: {
      state: 'SPLIT',
      fork: { hash: HOSTILE, height: 1, detected_at: 1790000000 },
      branches: { sf: { tip_hash: HOSTILE, tip_height: 2 }, sq: { tip_hash: HOSTILE, tip_height: 3 } },
    },
    views: { sf: { peer_count: 1, sync_progress: 1, detail: HOSTILE }, sq: {} },
  });
  assert.ok(textOf(byID['branches']).includes(HOSTILE));
  assert.ok(textOf(byID['views']).includes(HOSTILE));
});

// The wording is decided on the server so it exists in one reviewable place.
// Reassembling it here would put a second copy in a file nobody reviews for tone.
test('the headline is rendered exactly as sent', () => {
  const headline = {
    state: 'protected',
    title: 'Watching. Your channels look fine.',
    detail: 'Your node and the rest of the network are on the same chain.',
    action: null,
    since: 0,
  };
  app.renderHeadline(headline);

  assert.strictEqual(byID['headline-title'].textContent, headline.title);
  assert.strictEqual(byID['headline-detail'].textContent, headline.detail);
});

test('each of the five states gets its own appearance', () => {
  const seen = new Set();
  for (const state of app.KNOWN_STATES) {
    app.renderHeadline({ state, title: 't', detail: 'd', action: null });
    const className = byID['headline'].className;
    assert.ok(className.includes('state-' + state),
      state + ' produced no class of its own: ' + className);
    seen.add(className);
  }
  assert.strictEqual(seen.size, app.KNOWN_STATES.length,
    'two states look identical, so urgency is not distinguishable');
});

// A state this page does not know must still render. A blank screen during a
// situation the daemon considered worth naming is the worst possible failure.
test('an unrecognised state still renders, without leaking its name', () => {
  app.renderHeadline({ state: 'SOMETHING_NEW', title: 'A title', detail: 'A detail' });

  assert.strictEqual(byID['headline-title'].textContent, 'A title');
  assert.strictEqual(byID['headline-state-label'].textContent, '',
    'an internal state name reached the screen');
  assert.ok(!byID['headline'].className.includes('SOMETHING_NEW'));
});

test('no internal state name appears in the badge for any known state', () => {
  for (const state of app.KNOWN_STATES) {
    app.renderHeadline({ state, title: 't', detail: 'd' });
    const label = byID['headline-state-label'].textContent;
    assert.ok(label.length > 0, state + ' has no badge');
    assert.ok(!label.includes('_'), 'the badge shows an internal name: ' + label);
    assert.strictEqual(label, label.replace(/[A-Z]{3,}/g, ''),
      'the badge shouts an internal constant: ' + label);
  }
});

test('a state with no action offers nothing to click', () => {
  app.renderHeadline({ state: 'protected', title: 't', detail: 'd', action: null });
  assert.ok(byID['headline-action'].className.includes('hidden'),
    'the calm state still shows a button');
});

test('an alert already seen offers nothing to click', () => {
  app.renderAlerts([{ id: 1, tier: 'info', message: 'something', acked_at: 100 }]);
  assert.strictEqual(byID['alerts'].find((n) => n.tagName === 'BUTTON'), null,
    'an acknowledged alert still asks to be acknowledged');
});

test('test results are reported in words, not counts of failures', () => {
  assert.match(app.describeTestResults([{ transport: 'a', ok: true }]), /Sent/);
  assert.match(app.describeTestResults([{ transport: 'a', ok: false }]), /Could not send to a/);
  assert.match(app.describeTestResults([]), /nowhere/);
  assert.match(app.describeTestResults(null), /nowhere/);
});

// Times as human durations, never a count of seconds.
test('durations read as time, not as numbers', () => {
  assert.strictEqual(app.formatDuration(600), 'about 10 minutes');
  assert.strictEqual(app.formatDuration(7200), 'about 2 hours');
  assert.strictEqual(app.formatDuration(45), '45 seconds');
  assert.strictEqual(app.formatDuration(0), 'not known yet');
  assert.strictEqual(app.formatDuration('nonsense'), 'not known yet');
});

test('sync progress reads as a proportion, not a float', () => {
  assert.strictEqual(app.formatPercent(1), 'fully');
  assert.strictEqual(app.formatPercent(0.42), '42%');
  assert.strictEqual(app.formatPercent('x'), 'not known');
});

// Rendering the same data twice must not double it: the page polls every five
// seconds, so a list that grew each time would be unusable within a minute.
test('rendering twice does not duplicate anything', () => {
  const items = [{ id: 'sq_synced', ok: true, label: 'Watching the other chain' }];
  app.renderReadiness(items);
  const first = byID['readiness'].childElementCount;
  app.renderReadiness(items);
  assert.strictEqual(byID['readiness'].childElementCount, first);

  const alerts = [{ id: 1, tier: 'info', message: 'x', acked_at: 0 }];
  app.renderAlerts(alerts);
  const alertCount = byID['alerts'].childElementCount;
  app.renderAlerts(alerts);
  assert.strictEqual(byID['alerts'].childElementCount, alertCount);
});

test('empty responses render as reassurance, not as a blank area', () => {
  app.renderAlerts([]);
  assert.ok(!byID['alerts-empty'].className.includes('hidden'),
    'an empty alert list leaves an unexplained gap');

  app.renderReadiness([]);
  assert.strictEqual(byID['readiness'].childElementCount, 0);
});

// A real response from the daemon, committed alongside a test in internal/api
// that regenerates it. Rendering the actual shape is what catches a field
// renamed on one side and not the other — a mismatch that shows a user a blank
// dashboard rather than an error anyone would see.
test('a real response from the daemon renders', () => {
  const status = JSON.parse(
    fs.readFileSync(path.join(__dirname, 'testdata', 'status.json'), 'utf8'));

  app.renderHeadline(status.headline);
  app.renderReadiness(status.readiness);
  app.renderAdvanced(status);

  assert.strictEqual(byID['headline-title'].textContent, status.headline.title,
    'the headline is not being rendered verbatim');
  assert.ok(byID['headline'].className.includes('state-' + status.headline.state),
    'the state produced no appearance of its own');

  // Every readiness label reaches the screen, and no id does.
  const checklist = textOf(byID['readiness']);
  for (const item of status.readiness) {
    assert.ok(checklist.includes(item.label), 'missing from the screen: ' + item.label);
    assert.ok(!checklist.includes(item.id), 'an internal id reached the screen: ' + item.id);
  }

  // The heights and hashes are in Advanced, and they are the real ones.
  const advanced = textOf(byID['branches']);
  assert.ok(advanced.includes(String(status.split.branches.sf.tip_height)),
    'the block height is not shown');
  assert.ok(advanced.includes(status.split.fork.hash),
    'the separation point is not shown');

  // And none of it leaked out of Advanced into the part a worried person reads.
  const above = byID['headline-title'].textContent + ' ' + byID['headline-detail'].textContent
    + ' ' + checklist;
  for (const leak of ['SPLIT', 'ARMED', 'WRONG_BRANCH', status.split.fork.hash,
    String(status.split.fork.height)]) {
    assert.ok(!above.includes(leak), 'this reached the page above Advanced: ' + leak);
  }
});

if (failures > 0) {
  console.error(failures + ' rendering test(s) failed');
  process.exit(1);
}
console.log('all rendering tests passed');
