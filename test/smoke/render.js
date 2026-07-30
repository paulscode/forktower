// Drives the real dashboard script against a real running daemon.
//
// Not a browser: there is no layout here, and no rendering engine. What it does
// check is the half that actually breaks — that `app.js` can fetch the daemon's
// real responses, read the fields it expects to find, and render them without
// throwing. A field renamed on one side, or a value shaped differently from the
// fixture, shows up here as an exception rather than as an empty page nobody
// notices.
//
// Run by the smoke test with the daemon's address as its only argument.

'use strict';

const path = require('node:path');

const base = process.argv[2];
if (!base) {
  console.error('usage: render.js http://host:port [session-cookie]');
  process.exit(2);
}
const session = process.argv[3];

// Captured before the shim is installed: installing replaces global.fetch with a
// stub that throws, so taking a reference afterwards would hand the page the stub
// and every request would "fail" for a reason that had nothing to do with the
// daemon.
const realFetch = global.fetch;

const webDir = path.join(__dirname, '..', '..', 'web');
const { install } = require(path.join(webDir, 'domshim.js'));
const { byID } = install();

// Anything the script does that a browser would report in its console is an
// error here. A dashboard that throws while rendering is one that stops updating
// without saying so.
const problems = [];
process.on('uncaughtException', (err) => problems.push('uncaught: ' + err.message));
process.on('unhandledRejection', (err) => problems.push('unhandled: ' + err));

// The page asks for absolute paths on its own origin; give it one.
global.fetch = (input, init) => {
  const options = Object.assign({}, init);
  if (session) {
    options.headers = Object.assign({}, options.headers,
      { cookie: 'forktower_session=' + session });
  }
  return realFetch(base + input, options);
};

const app = require(path.join(webDir, 'app.js'));

function fail(message) {
  console.error('FAIL ' + message);
  process.exitCode = 1;
}

async function main() {
  // The same call the page makes every five seconds.
  await app.refresh();

  if (problems.length > 0) {
    for (const problem of problems) {
      fail(problem);
    }
    return;
  }

  // With no session the right outcome is the sign-in form, not a headline and
  // not an error: "you need to sign in" and "something is broken" must not look
  // the same.
  if (!byID['signin'].className.includes('hidden')) {
    if (byID['connection'].textContent) {
      fail('signing in was reported as a failure: ' + byID['connection'].textContent);
    }
    console.log('sign-in shown');
    return;
  }

  const title = byID['headline-title'].textContent;
  if (!title) {
    fail('the headline is empty after fetching real data');
  }
  if (title === 'Connecting to Forktower…') {
    fail('the page never got past its placeholder, so no request succeeded');
  }
  if (byID['readiness'].childElementCount === 0) {
    fail('nothing rendered in the readiness list');
  }
  if (byID['branches'].childElementCount === 0) {
    fail('nothing rendered in the advanced section');
  }

  const connection = byID['connection'].textContent;
  if (connection) {
    fail('the page reports a problem talking to the daemon: ' + connection);
  }

  // Whatever state it is in, no internal name may reach the screen.
  const visible = [
    byID['headline-title'], byID['headline-detail'], byID['headline-state-label'],
    byID['readiness'],
  ].map((n) => n.textContent).join(' ');
  for (const leak of ['UNARMED', 'ARMED', 'SPLIT', 'RESOLVING', 'WRONG_BRANCH',
    'sq_', 'sf_', 'ln_connected']) {
    if (visible.includes(leak)) {
      fail('an internal name reached the screen: ' + leak);
    }
  }

  if (process.exitCode) {
    return;
  }
  console.log('rendered: ' + title);
}

main().catch((err) => fail('rendering threw: ' + err.stack));
