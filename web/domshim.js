// A DOM small enough to reason about, for testing the dashboard's rendering.
//
// The point is not to emulate a browser. It is that this shim implements
// textContent and *refuses* innerHTML: if the dashboard ever reaches for markup,
// the test throws rather than quietly passing. A shim that implemented both
// would prove nothing about the rule that matters here.

'use strict';

const fs = require('node:fs');
const path = require('node:path');

const FORBIDDEN = ['innerHTML', 'outerHTML', 'insertAdjacentHTML'];

class Element {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.parent = null;
    this._text = '';
    this.className = '';
    this.id = '';
    this.listeners = {};
    this.disabled = false;
    this.open = false;
    this.value = '';

    for (const name of FORBIDDEN) {
      Object.defineProperty(this, name, {
        set() {
          throw new Error(
            'the dashboard wrote ' + name + ', which would let a value chosen by ' +
            'someone else become markup');
        },
        get() {
          throw new Error('the dashboard read ' + name);
        },
      });
    }

    const self = this;
    this.classList = {
      add(name) {
        const parts = self.className.split(/\s+/).filter(Boolean);
        if (parts.indexOf(name) === -1) {
          parts.push(name);
        }
        self.className = parts.join(' ');
      },
      remove(name) {
        self.className = self.className.split(/\s+/)
          .filter((part) => part && part !== name).join(' ');
      },
      contains(name) {
        return self.className.split(/\s+/).indexOf(name) !== -1;
      },
    };
  }

  get textContent() {
    if (this.children.length === 0) {
      return this._text;
    }
    return this.children.map((child) => child.textContent).join('');
  }

  set textContent(value) {
    this.children = [];
    this._text = String(value);
  }

  get firstChild() {
    return this.children.length ? this.children[0] : null;
  }

  get lastChild() {
    return this.children.length ? this.children[this.children.length - 1] : null;
  }

  get childElementCount() {
    return this.children.length;
  }

  appendChild(child) {
    child.parent = this;
    this.children.push(child);
    return child;
  }

  insertBefore(child, before) {
    child.parent = this;
    const at = before ? this.children.indexOf(before) : -1;
    if (at === -1) {
      this.children.push(child);
    } else {
      this.children.splice(at, 0, child);
    }
    return child;
  }

  removeChild(child) {
    const at = this.children.indexOf(child);
    if (at !== -1) {
      this.children.splice(at, 1);
    }
    child.parent = null;
    return child;
  }

  addEventListener(name, fn) {
    (this.listeners[name] = this.listeners[name] || []).push(fn);
  }

  // fire runs the handlers a test wants to exercise.
  fire(name) {
    for (const fn of this.listeners[name] || []) {
      fn({ preventDefault() {} });
    }
  }

  // find walks the tree, for assertions.
  find(predicate) {
    if (predicate(this)) {
      return this;
    }
    for (const child of this.children) {
      const hit = child.find(predicate);
      if (hit) {
        return hit;
      }
    }
    return null;
  }
}

// idsIn extracts every id declared in the page, so a test proves the dashboard
// only reaches for elements that actually exist. A mistyped id would otherwise
// fail silently in a browser and loudly in front of a user.
function idsIn(html) {
  const ids = [];
  const pattern = /\bid="([^"]+)"/g;
  let match = pattern.exec(html);
  while (match) {
    ids.push(match[1]);
    match = pattern.exec(html);
  }
  return ids;
}

function install() {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');

  const byID = {};
  for (const id of idsIn(html)) {
    const node = new Element('div');
    node.id = id;
    byID[id] = node;
  }

  const document = {
    readyState: 'complete',
    getElementById(id) {
      return Object.prototype.hasOwnProperty.call(byID, id) ? byID[id] : null;
    },
    createElement(tag) {
      return new Element(tag);
    },
    addEventListener() {},
  };

  global.document = document;
  global.window = {
    document,
    location: { hash: '' },
    setInterval() { return 0; },
    clearInterval() {},
  };
  global.fetch = async () => {
    throw new Error('a test rendered something without stubbing fetch');
  };

  return { document, byID, Element };
}

module.exports = { install, Element, idsIn, FORBIDDEN };
