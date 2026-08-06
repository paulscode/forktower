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

// The summary is reported when the process exits rather than at the bottom of
// this file, and that is not a style choice. Three tests were once appended
// below an inline summary and were therefore never counted by it: they ran,
// they could have failed, and the run would still have reported success. Hung
// on the exit hook, a test added anywhere counts.
process.on('exit', () => {
  if (failures > 0) {
    console.error(failures + ' rendering test(s) failed');
    process.exitCode = 1;
    return;
  }
  console.log('all rendering tests passed');
});

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

// ---------------------------------------------------------------------------
// The exposure table.
// ---------------------------------------------------------------------------

// aChannel is one row of what the API sends, in the shape the page consumes.
function aChannel(overrides) {
  return Object.assign({
    id: 1,
    threat: { state: 'confirmed', headline_deadline: { remaining_blocks: 400 } },
    display: {
      partner: 'ACINQ',
      at_risk_sat: 2100000,
      time_left: 'about 3 days',
      time_left_is_estimate: true,
      status: 'We are handling it',
      status_action: '',
    },
  }, overrides || {});
}

test('the partner name a counterparty chose stays literal text', () => {
  app.renderChannels([aChannel({
    display: {
      partner: HOSTILE, at_risk_sat: 1000, time_left: '', status: 'Fine',
    },
  })]);

  const cell = byID['channels'].find((n) => n.tagName === 'TR')
    .find((n) => n.className === 'partner');
  assert.strictEqual(cell.textContent, HOSTILE,
    'the name was altered rather than shown as text');
  assert.strictEqual(cell.children.length, 0,
    'the counterparty got an element onto the page');
});

test('the table leads with who, how much and how long', () => {
  app.renderChannels([aChannel()]);

  const row = byID['channels'].find((n) => n.tagName === 'TR');
  const cells = row.children.filter((n) => n.tagName === 'TD');
  assert.strictEqual(cells.length, 4, 'wrong number of columns');
  assert.ok(cells[0].textContent.includes('ACINQ'), 'the partner is not first');
  assert.ok(cells[1].textContent.includes('0.02100000 BTC'),
    'the amount is not second: ' + cells[1].textContent);
  assert.ok(cells[2].textContent.includes('about 3 days'),
    'the time is not third: ' + cells[2].textContent);
  assert.ok(cells[3].textContent.includes('We are handling it'),
    'the status is not fourth');
});

test('an estimated time is marked as an estimate', () => {
  app.renderChannels([aChannel()]);

  const row = byID['channels'].find((n) => n.tagName === 'TR');
  const time = row.find((n) => n.className === 'time-left');
  assert.ok(time.find((n) => n.className === 'estimate'),
    'an estimated time was shown as though it were exact');
  assert.ok(byID['channels-note'].textContent.includes('estimates'),
    'nothing on the page says the times are estimates');
});

test('no countdown means no claim about time', () => {
  app.renderChannels([aChannel({
    threat: { state: 'watch', headline_deadline: null },
    display: {
      partner: 'alice', at_risk_sat: 0, time_left: '',
      time_left_is_estimate: false, status: 'Fine',
    },
  })]);

  const row = byID['channels'].find((n) => n.tagName === 'TR');
  const time = row.find((n) => n.className === 'time-left');
  assert.strictEqual(time.textContent, '\u2014',
    'invented a time with nothing to go on: ' + time.textContent);
  assert.strictEqual(byID['channels-note'].textContent, '',
    'claimed times are estimates when none were shown');
});

test('amounts are built from integers, not divided', () => {
  // The value that catches a float: 21 million BTC in satoshis, which cannot be
  // divided by a hundred million in binary floating point without drifting.
  assert.strictEqual(app.formatSats(2100000000000000), '21000000.00000000 BTC');
  assert.strictEqual(app.formatSats(1), '0.00000001 BTC');
  assert.strictEqual(app.formatSats(100000000), '1.00000000 BTC');
  assert.strictEqual(app.formatSats(0), '\u2014');
});

test('a channel with something happening to it is marked as such', () => {
  app.renderChannels([
    aChannel({ id: 1, threat: { state: 'confirmed' } }),
    aChannel({ id: 2, threat: { state: 'watch' } }),
  ]);

  const rows = byID['channels'].children.filter((n) => n.tagName === 'TR');
  assert.strictEqual(rows[0].className, 'at-risk', 'a live threat is not marked');
  assert.strictEqual(rows[1].className, '', 'a quiet channel was marked as at risk');
});

test('with no channels the table is not shown at all', () => {
  app.renderChannels([]);

  assert.strictEqual(byID['exposure'].className.includes('hidden'), true,
    'an empty table was left on the page');
  assert.strictEqual(byID['channels'].children.length, 0, 'rows were left behind');
});

test('nothing in the table is an internal name', () => {
  app.renderChannels([aChannel({
    threat: { state: 'confirmed', headline_deadline: { remaining_blocks: 400 } },
    display: {
      partner: 'ACINQ', at_risk_sat: 2100000, time_left: 'about 3 days',
      time_left_is_estimate: true,
      status: 'A channel was closed on the other chain \u2014 checking whether it was fair',
      status_action: 'Open Details to see what happened',
    },
  })]);

  const shown = textOf(byID['channels']);
  for (const leak of ['commitment_unknown', 'commitment_ours', 'mutual_close',
    'csv', 'sq', 'sf', 'reorged_out']) {
    assert.ok(!shown.toLowerCase().includes(leak),
      'an internal name reached the table: ' + leak);
  }
  // The block count belongs in Details, not in the row.
  assert.ok(!shown.includes('400'), 'a block count reached the table: ' + shown);
});

// The same contract as the status fixture, for the table a worried person reads
// first: a field renamed on one side and not the other produces a blank column
// rather than an error anyone would see.
test('a real exposure table from the daemon renders', () => {
  const channels = JSON.parse(
    fs.readFileSync(path.join(__dirname, 'testdata', 'channels.json'), 'utf8'));

  app.renderChannels(channels);

  const rows = byID['channels'].children.filter((n) => n.tagName === 'TR');
  assert.strictEqual(rows.length, channels.length,
    'the table did not render every channel');

  for (const channel of channels) {
    const shown = textOf(byID['channels']);
    assert.ok(shown.includes(channel.display.partner),
      'a partner is missing from the table: ' + channel.display.partner);
    assert.ok(shown.includes(channel.display.status),
      'a status is missing from the table: ' + channel.display.status);
    if (channel.display.time_left) {
      assert.ok(shown.includes(channel.display.time_left),
        'a time is missing from the table: ' + channel.display.time_left);
    }
  }

  // And none of the machinery behind it reached the screen.
  const shown = textOf(byID['channels']);
  for (const channel of channels) {
    assert.ok(!shown.includes(channel.funding_txid),
      'a funding transaction id reached the table');
    // Deliberately not asserting on the channel id: a one-digit id is
    // indistinguishable from any digit in an amount, so the check could only
    // produce false alarms. The transaction id and the block height below are
    // the ones long enough to mean something.
    if (channel.threat.headline_deadline) {
      assert.ok(!shown.includes(String(channel.threat.headline_deadline.deadline_height)),
        'a block height reached the table');
    }
  }
});

// The one condition where every other thing on the page can be green while
// nothing is being watched. Somebody who turned it off last month and forgot has
// to be told without having to look for it.
test('turning watching off puts a banner at the top of the page', () => {
  app.renderStandDown([
    { id: 'sq_synced', ok: true, label: 'Watching the other chain' },
    { id: 'watching_active', ok: false, label: 'You have turned off watching' },
  ]);

  assert.ok(!byID['stood-down'].className.includes('hidden'),
    'the banner is not shown while watching is off');
  assert.ok(byID['stood-down'].textContent.includes('Nothing there is being checked'),
    'the banner does not say what it means: ' + byID['stood-down'].textContent);
});

test('with watching on there is no banner', () => {
  app.renderStandDown([{ id: 'watching_active', ok: true, label: 'Watching' }]);

  assert.ok(byID['stood-down'].className.includes('hidden'),
    'a banner was left on the page while watching was on');
  assert.strictEqual(byID['stood-down'].textContent, '',
    'the banner still has text in it');
});

test('a page from a version without the check shows no banner', () => {
  app.renderStandDown([{ id: 'sq_synced', ok: true, label: 'Watching' }]);
  assert.ok(byID['stood-down'].className.includes('hidden'),
    'a missing check was treated as watching being off');

  app.renderStandDown([]);
  assert.ok(byID['stood-down'].className.includes('hidden'),
    'an empty readiness list was treated as watching being off');
});

// ---------------------------------------------------------------------------
// Details.
// ---------------------------------------------------------------------------

test('the Details the rows point at actually shows what happened', () => {
  app.renderDetails(
    [{
      block_height: 961753,
      display: {
        what: 'Somebody closed this channel \u2014 Forktower cannot tell whether it was fair.',
        where: 'the other chain',
        short_txid: 'deadbeef\u2026cafe1234',
        confirmed: true,
      },
    }],
    [{
      remaining_blocks: 847,
      deadline_height: 962600,
      display: {
        what: 'How long you have to respond to a channel close on the other chain',
        time_left: 'about 18 days',
        time_left_is_estimate: true,
      },
    }],
  );

  const shown = textOf(byID['details-spends']) + ' ' + textOf(byID['details-deadlines']);
  assert.ok(shown.includes('cannot tell whether it was fair'),
    'the explanation is missing');
  // A reader who opened Details asked for these.
  assert.ok(shown.includes('deadbeef'), 'the transaction is missing');
  assert.ok(shown.includes('961753'), 'the block height is missing');
  assert.ok(shown.includes('847 blocks left'), 'the block count is missing');
  assert.ok(shown.includes('about 18 days'), 'the time is missing');
  assert.ok(byID['details-empty'].className.includes('hidden'),
    '"nothing has happened" was shown alongside things that happened');
});

test('an unconfirmed sighting says so in Details', () => {
  app.renderDetails([{
    display: { what: 'A close is on its way.', where: 'the other chain',
      short_txid: 'aa\u2026bb', confirmed: false },
  }], []);

  assert.ok(textOf(byID['details-spends']).includes('not yet in a block'),
    'a sighting was shown as though it were confirmed');
});

test('a countdown resting on a guess says so in Details', () => {
  app.renderDetails([], [{
    remaining_blocks: 100, deadline_height: 600,
    display: {
      what: 'How long you have to respond to a channel close on the other chain',
      note: 'Your Lightning node did not say how long this window is.',
    },
  }]);

  assert.ok(textOf(byID['details-deadlines']).includes('did not say how long'),
    'a countdown built on a floor was shown as though it were known');
});

test('with nothing to show, Details says so rather than sitting empty', () => {
  app.renderDetails([], []);

  assert.ok(!byID['details-empty'].className.includes('hidden'),
    'an empty Details gave the reader nothing at all');
  assert.strictEqual(byID['details-spends'].children.length, 0, 'rows were left behind');
});

test('Details renders nothing at all from a response with no display block', () => {
  // A field renamed on the daemon's side must not throw and blank the page.
  app.renderDetails([{}], [{}]);
  assert.strictEqual(byID['details-spends'].children.length, 1, 'the row was dropped');
});

// The failure this card exists for: a watchtower that is up, answering, and
// holding nothing looks healthy from every angle except the one that matters.
test('a tower protecting nothing is marked at risk', () => {
  app.renderTowers({
    towers: [{
      uri: '03abc@abcdef.onion:9911',
      display: {
        state: 'not protecting',
        summary: 'This tower is running, but none of your channels are backed up to it.',
        covered: 0, uncovered: 2,
      },
      coverage: [
        { coverable: false, reason: 'no anchor session has been negotiated' },
        { coverable: false, reason: 'the tower is running v0.17.5, which accepts no taproot sessions' },
      ],
    }],
  });

  const list = byID['towers'];
  assert.ok(textOf(list).includes('none of your channels are backed up'),
    'the summary was not shown');
  assert.ok(textOf(list).includes('v0.17.5'),
    'the per-channel reasons were not shown');
  assert.ok(list.children[0].className.includes('at-risk'),
    'a tower protecting nothing was not marked at risk');
});

test('a tower covering everything is not marked at risk', () => {
  app.renderTowers({
    towers: [{
      uri: '03abc@abcdef.onion:9911',
      display: {
        state: 'protecting',
        summary: 'This tower is watching the other chain for a revoked commitment.',
        covered: 3, uncovered: 0,
      },
      coverage: [{ coverable: true, reason: 'the node holds an anchor session' }],
    }],
  });

  const list = byID['towers'];
  assert.ok(!list.children[0].className.includes('at-risk'),
    'a working tower was marked at risk');
});

// The line the user copies. A placeholder that looks like a command is worse
// than an empty box, because somebody will paste it.
test('the registration command carries the real address or nothing', () => {
  app.renderTowers({
    towers: [{ kind: 'lnd', uri: '03abc@abcdef.onion:9911', display: { state: 'settling', summary: 'Ready.' }, coverage: [] }],
  });
  assert.ok(textOf(byID['tower-command']).includes('lncli wtclient add 03abc@abcdef.onion:9911'),
    'the command did not carry the address');

  app.renderTowers({ towers: [{ display: { state: 'unknown', summary: 'Not asked yet.' }, coverage: [] }] });
  assert.strictEqual(textOf(byID['tower-command']), '',
    'a tower with no address still produced a command to paste');
});

// The rate is fixed when the session is negotiated and nobody can raise it, so
// saying the number without saying that would be half the truth.
test('the fee note says the rate cannot be raised afterwards', () => {
  app.renderTowers({
    towers: [{
      uri: '03abc@x.onion:9911',
      display: { state: 'protecting', summary: 'Watching.', covered: 1, uncovered: 0 },
      coverage: [{ coverable: true, reason: 'session held', sweep_fee_sat_per_vbyte: 10 }],
    }],
  });

  const text = textOf(byID['towers']);
  assert.ok(text.includes('10 sat/vB'), 'the rate was not shown');
  assert.ok(text.includes('cannot be raised'), 'the note does not say the rate is fixed');
});

test('no towers hides the card rather than showing empty furniture', () => {
  app.renderTowers({ towers: [] });
  assert.ok(byID['towers-card'].className.includes('hidden'),
    'the card was shown with no towers');
  // And there is no note inside it saying so: a note in a hidden card is one
  // nobody can read. Having no watchtower is said in the readiness list.
  assert.strictEqual(byID['towers-empty'], undefined,
    'a note lives inside the card that is hidden in exactly the case it is for');
});

test('a hostile reason from a tower stays literal text', () => {
  app.renderTowers({
    towers: [{
      display: { state: 'not protecting', summary: 'Channel with ' + HOSTILE + ' is not covered.' },
      coverage: [{ coverable: false, reason: HOSTILE }],
    }],
  });
  assert.ok(textOf(byID['towers']).includes(HOSTILE),
    'the hostile string was not rendered as text');
});

// **The refusals are the larger half and they are shown.** A page listing only
// what was copied would look like a feature that barely does anything, and would
// leave "why was that not copied?" with no answer.
test('the mirror shows what was refused and why', () => {
  app.renderMirror({
    summary: { copied: 1, refused: 2, waiting: 0, needs_you: 0, note: '' },
    decisions: [
      { display: { what: 'Copied to the other chain.', short_txid: 'aa…bb', copied: true } },
      {
        display: {
          what: 'Not copied — the other party closed this channel on this chain. '
            + 'Copying that would put your money at risk there.',
          short_txid: 'cc…dd', refused: true,
        },
      },
    ],
  });

  const text = textOf(byID['mirror-list']);
  assert.ok(text.includes('Copied to the other chain'), 'the copied one was not shown');
  assert.ok(text.includes('Not copied'), 'the refusal was not shown');
  assert.ok(text.includes('at risk there'), 'the reason for refusing was not shown');
});

// Something that needs the user is marked, because nothing further will happen
// on its own.
test('a transaction that could not be copied is marked', () => {
  app.renderMirror({
    summary: { needs_you: 1, note: 'Some transactions could not be copied.' },
    decisions: [{
      display: {
        what: 'Could not be copied to the other chain. Forktower has stopped trying.',
        short_txid: 'ee…ff', needs_you: true,
      },
    }],
  });

  assert.ok(byID['mirror-list'].children[0].className.includes('at-risk'),
    'a transaction needing the user was not marked');
  assert.ok(textOf(byID['mirror-note']).includes('could not be copied'),
    'the summary line was not shown');
});

test('nothing to copy hides the section rather than showing an empty heading', () => {
  app.renderMirror({ summary: {}, decisions: [] });
  assert.ok(byID['mirror-section'].className.includes('hidden'),
    'the section was shown with nothing in it');
});

test('a hostile reason from the mirror stays literal text', () => {
  app.renderMirror({
    summary: {},
    decisions: [{ display: { what: 'Not copied — ' + HOSTILE, short_txid: 'a…b' } }],
  });
  assert.ok(textOf(byID['mirror-list']).includes(HOSTILE),
    'the hostile string was not rendered as text');
});

// The opt-in is per channel, never global: the decision is about one channel's
// money, and one switch for everything is a switch somebody flips without
// thinking about any particular channel.
test('the funding opt-in is offered per channel and reflects what is stored', () => {
  app.renderFundingOptIn([
    { id: 1, capacity_sat: 2100000, mirror_funding_opt_in: false, display: { partner: 'ACINQ' } },
    { id: 2, capacity_sat: 500000, mirror_funding_opt_in: true, display: { partner: 'a friend' } },
  ]);

  const list = byID['funding-optin-list'];
  assert.strictEqual(list.children.length, 2, 'not one row per channel');
  assert.ok(textOf(list).includes('ACINQ'), 'the partner was not named');
  assert.ok(byID['funding-optin-empty'].className.includes('hidden'),
    'the empty note was shown alongside channels');
});

test('with no channels the opt-in says so rather than sitting empty', () => {
  app.renderFundingOptIn([]);
  assert.ok(!byID['funding-optin-empty'].className.includes('hidden'),
    'the empty note was hidden');
  assert.strictEqual(byID['funding-optin-list'].children.length, 0,
    'rows were rendered with no channels');
});

test('a hostile partner name in the opt-in stays literal text', () => {
  app.renderFundingOptIn([
    { id: 1, capacity_sat: 1, mirror_funding_opt_in: false, display: { partner: HOSTILE } },
  ]);
  assert.ok(textOf(byID['funding-optin-list']).includes(HOSTILE),
    'the hostile string was not rendered as text');
});

// **Every anchor the daemon hands out must point at something.**
//
// Three of them did not: an action saying "check your setup" scrolled the user
// nowhere at all. The earlier check on element ids only covered the ids the
// script reaches for, and an href target is reached by the browser instead — so
// nothing noticed. This closes that gap from the other side.
test('every anchor an action can send the user to exists in the page', () => {
  const markup = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const anchors = fs.readFileSync(
    path.join(__dirname, '..', 'internal', 'api', 'headline.go'), 'utf8');

  const targets = [...anchors.matchAll(/anchor[A-Za-z]+\s+=\s+"#([a-z-]+)"/g)]
    .map((m) => m[1]);
  assert.ok(targets.length >= 4, 'no anchors were found to check');

  for (const id of targets) {
    assert.ok(markup.includes(`id="${id}"`),
      `an action sends the user to #${id}, and no element has that id`);
  }
});

// The first-run panel disappears for good once there is nothing left to do.
test('the setup guidance is gone once setup is complete', () => {
  app.renderSetup({ complete: true, step: null, done: 4, total: 4, waiting: [] });
  assert.strictEqual(byID['setup-guide'].className.includes('hidden'), true,
    'the setup panel is still standing in front of a finished install');
});

// One step at a time, with its platform's own directions.
test('the setup guidance shows one step and the directions for it', () => {
  app.renderSetup({
    complete: false,
    done: 2,
    total: 5,
    waiting: ['Watching the other chain'],
    step: {
      id: 'tower_protection',
      label: 'Set up a watchtower',
      why: 'Nobody is watching your channels while your node cannot see the other chain.',
      detail: '',
      guidance: ['Open the LND service in StartOS.', 'Go to Actions → Watchtower.'],
      skippable: true,
      skip_cost: 'You would be the response.',
    },
  });

  assert.strictEqual(byID['setup-guide'].className.includes('hidden'), false);
  assert.match(byID['setup-progress'].textContent, /Step 3 of 5/);
  assert.strictEqual(byID['setup-guidance'].children.length, 2,
    'the platform directions were not shown');
  // What is being waited for is named, so somebody who cannot finish knows they
  // are waiting rather than stuck.
  assert.match(byID['setup-waiting'].textContent, /Watching the other chain/);
  // And the way out is offered, with what it costs.
  assert.strictEqual(byID['setup-skip'].className.includes('hidden'), false);
  assert.match(byID['setup-skip-cost'].textContent, /the response/);
});

// A step with no directions shows none, rather than an empty list pretending to
// be instructions.
test('a platform Forktower does not know gets no invented directions', () => {
  app.renderSetup({
    complete: false, done: 0, total: 3, waiting: [],
    step: { id: 'ln_connected', label: 'Connect your Lightning node', guidance: [] },
  });
  assert.strictEqual(byID['setup-guidance'].children.length, 0);
});

// ---------------------------------------------------------------------------
// The faster first sync.
// ---------------------------------------------------------------------------

// A card nobody needs is a card nobody should see. Most installations reach this
// page long after the shortcut stopped being relevant, and a permanent panel
// explaining why a shortcut is unnecessary is one more thing to read on a page
// whose whole job is to be read quickly.
test('the faster-sync card is hidden when there is nothing to offer', () => {
  for (const view of [null, undefined, { available: false, phase: 'unavailable' }]) {
    app.renderBootstrap(view);
    assert.strictEqual(byID['bootstrap'].className.includes('hidden'), true,
      'the faster-sync card is showing when it has nothing to say');
  }
});

// The offer states its costs before anybody agrees to it. Putting the benefit
// first and the costs in a footnote is how consent forms are written by people
// who do not want them read.
test('the offer lists what it costs, not only what it saves', () => {
  app.renderBootstrap({
    available: true,
    phase: 'offered',
    title: 'Start watching the other chain within the hour',
    detail: 'Left to itself the second node takes about three days.',
    why: [
      'Downloads about 8.7 GB, which is deleted as soon as it has been used.',
      'Fetched from this project’s release page.',
      'This is the only thing Forktower ever downloads.',
    ],
    action: { label: 'Use the faster sync', endpoint: '/api/v1/bootstrap/start' },
  });

  assert.strictEqual(byID['bootstrap'].className.includes('hidden'), false);
  assert.strictEqual(byID['bootstrap-why'].children.length, 3,
    'the costs of the offer were not shown');
  assert.strictEqual(byID['bootstrap-action'].className.includes('hidden'), false,
    'the offer has no button');
  // No progress bar before anything is running.
  assert.strictEqual(byID['bootstrap-bar'].className.includes('hidden'), true);
});

// While it runs there is a bar and a sentence saying the same thing, because a
// bar alone tells a screen reader nothing.
test('a running transfer shows both a bar and a sentence', () => {
  app.renderBootstrap({
    available: true,
    phase: 'downloading',
    title: 'Fetching the head start',
    detail: 'The second node keeps catching up on its own while this runs.',
    percent: 42.5,
    bytes_done: 4 * 1024 * 1024 * 1024,
    bytes_total: 9 * 1024 * 1024 * 1024,
    human: '4.0 GB of 8.7 GB, part 3 of 5, about 2 hours to go.',
    action: { label: 'Stop and sync the slow way', endpoint: '/api/v1/bootstrap/cancel' },
  });

  assert.strictEqual(byID['bootstrap-bar'].className.includes('hidden'), false);
  assert.strictEqual(byID['bootstrap-fill'].style.width, '42.5%');
  assert.strictEqual(byID['bootstrap-bar'].getAttribute('aria-valuenow'), '43');
  assert.match(byID['bootstrap-progress'].textContent, /4\.0 GB of 8\.7 GB/);
  assert.match(byID['bootstrap-progress'].textContent, /2 hours to go/);
});

// A percentage outside the bar is a rendering bug that reads as a data bug.
test('a percentage outside the bar is clamped rather than drawn', () => {
  for (const [percent, want] of [[-10, '0.0%'], [140, '100.0%'], ['nonsense', '0.0%']]) {
    app.renderBootstrap({
      available: true, phase: 'downloading', title: 'x', percent,
    });
    assert.strictEqual(byID['bootstrap-fill'].style.width, want,
      'a percent of ' + percent + ' was drawn as ' + byID['bootstrap-fill'].style.width);
  }
});

// A failure says so and offers the way back, rather than sitting on a bar that
// has stopped moving.
test('a failed transfer reports the reason and offers a retry', () => {
  app.renderBootstrap({
    available: true,
    phase: 'failed',
    title: 'The faster sync did not finish',
    detail: 'Forktower will try again shortly, resuming from where it stopped.',
    error: 'the connection was reset',
    human: '3.1 GB of 8.7 GB already fetched.',
    action: { label: 'Try again now', endpoint: '/api/v1/bootstrap/start' },
  });

  assert.match(byID['bootstrap-error'].textContent, /connection was reset/);
  assert.strictEqual(byID['bootstrap-action'].className.includes('hidden'), false);
  // And it says what would not be lost by retrying.
  assert.match(byID['bootstrap-progress'].textContent, /3\.1 GB/);
  assert.strictEqual(byID['bootstrap-bar'].className.includes('hidden'), true,
    'a stopped transfer is still showing a progress bar');
});

// Nothing from the server becomes markup here either. The shim refuses
// innerHTML, so this fails loudly rather than subtly if that ever changes.
test('hostile text in the faster-sync card stays text', () => {
  app.renderBootstrap({
    available: true,
    phase: 'failed',
    title: HOSTILE,
    detail: HOSTILE,
    error: HOSTILE,
    why: [HOSTILE],
  });
  assert.strictEqual(byID['bootstrap-title'].textContent, HOSTILE);
  assert.strictEqual(byID['bootstrap-error'].textContent, HOSTILE);
  assert.strictEqual(byID['bootstrap-why'].children[0].textContent, HOSTILE);
});

// A previous run's error must not survive into a clean state, or a user who has
// retried successfully is still looking at the last thing that went wrong.
test('an earlier error is cleared once it no longer applies', () => {
  app.renderBootstrap({
    available: true, phase: 'failed', title: 'It broke', error: 'a bad thing',
  });
  assert.match(byID['bootstrap-error'].textContent, /bad thing/);

  app.renderBootstrap({
    available: true, phase: 'done', title: 'The second node took the shortcut',
  });
  assert.strictEqual(byID['bootstrap-error'].textContent, '',
    'a stale error is still on screen after the shortcut succeeded');
  assert.strictEqual(byID['bootstrap-why'].children.length, 0,
    'the offer’s small print is still showing after it was accepted');
});

// The command has to match the node that speaks to the tower.
//
// An LND watchtower is registered from LND and a teos tower from Core
// Lightning; they are not interchangeable and neither is the verb. This used to
// emit the LND form for every tower, so somebody running Core Lightning beside
// a teos tower was shown `lncli` — a program they do not have, driving a node
// they are not running.
test('the registration command matches the kind of tower', () => {
  app.renderTowers({ towers: [{
    kind: 'lnd', uri: '02aa@abcdef.onion:9911',
    display: { state: 'settling', summary: 'Ready.' }, coverage: [],
  }] });
  assert.match(textOf(byID['tower-command']), /^lncli wtclient add 02aa@/);

  app.renderTowers({ towers: [{
    kind: 'teos', uri: '03bb@fedcba.onion:9814',
    display: { state: 'settling', summary: 'Ready.' }, coverage: [],
  }] });
  assert.match(textOf(byID['tower-command']),
    /^lightning-cli registertower 03bb@/);
});

// A tower of a kind this page does not know gets no command at all.
//
// A plausible-looking command is worse than an empty box, because somebody will
// paste it — and guessing a verb from an unfamiliar kind is precisely how the
// wrong one gets emitted.
test('an unrecognised tower kind produces no command rather than a guess', () => {
  app.renderTowers({ towers: [{
    kind: 'something-new', uri: '04cc@example.onion:9911',
    display: { state: 'settling', summary: 'Ready.' }, coverage: [],
  }] });
  assert.strictEqual(textOf(byID['tower-command']), '');
  assert.strictEqual(byID['tower-command'].className.includes('hidden'), true);
});
