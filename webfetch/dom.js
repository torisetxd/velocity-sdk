// A minimal browser surface for running a challenge page's inline scripts:
// enough DOM, location, cookie, timer and encoding API for an interstitial to
// compute its answer and navigate. __host is the Go side; it records the
// navigation and cookies and supplies URL parsing, HTML parsing and hashing.
(function (g) {
  "use strict";
  var host = g.__host;
  var tree = JSON.parse(g.__tree);
  var pageURL = g.__url;
  var userAgent = g.__ua;
  delete g.__host;
  delete g.__tree;
  delete g.__url;
  delete g.__ua;

  var clock = 0;
  var timers = [];
  var timerSeq = 0;
  var noop = function () {};

  function Event(type, init) {
    init = init || {};
    this.type = type;
    this.bubbles = !!init.bubbles;
    this.cancelable = !!init.cancelable;
    this.defaultPrevented = false;
    this.isTrusted = true;
    this.target = null;
    this.currentTarget = null;
    this.timeStamp = clock;
    this.submitter = init.submitter || null;
  }
  Event.prototype.preventDefault = function () { if (this.cancelable) this.defaultPrevented = true; };
  Event.prototype.stopPropagation = function () { this.__stopped = true; };
  Event.prototype.stopImmediatePropagation = function () { this.__stopped = true; this.__halted = true; };

  function EventTarget() {}
  EventTarget.prototype.addEventListener = function (type, fn, opts) {
    if (!fn) return;
    var all = this.__listeners || (this.__listeners = {});
    var list = all[type] || (all[type] = []);
    list.push({ fn: fn, once: !!(opts && opts.once) });
  };
  EventTarget.prototype.removeEventListener = function (type, fn) {
    var list = this.__listeners && this.__listeners[type];
    if (!list) return;
    for (var i = 0; i < list.length; i++) if (list[i].fn === fn) { list.splice(i, 1); return; }
  };
  EventTarget.prototype.dispatchEvent = function (event) {
    if (!event.target) event.target = this;
    var node = this;
    while (node && !event.__stopped) {
      event.currentTarget = node;
      var handler = node["on" + event.type];
      try {
        if (typeof handler === "function" && handler.call(node, event) === false) event.preventDefault();
      } catch (e) {}
      var list = node.__listeners && node.__listeners[event.type];
      if (list) {
        list = list.slice();
        for (var i = 0; i < list.length && !event.__halted; i++) {
          if (list[i].once) node.removeEventListener(event.type, list[i].fn);
          var fn = list[i].fn;
          try {
            if (typeof fn === "function") fn.call(node, event);
            else if (fn && typeof fn.handleEvent === "function") fn.handleEvent(event);
          } catch (e) {}
        }
      }
      if (!event.bubbles) break;
      node = node === document ? g : node.parentNode || (node === document.documentElement ? document : null);
    }
    return !event.defaultPrevented;
  };

  function Node() { EventTarget.call(this); this.parentNode = null; this.childNodes = []; }
  Node.prototype = Object.create(EventTarget.prototype);
  Node.ELEMENT_NODE = 1;
  Node.TEXT_NODE = 3;
  Node.DOCUMENT_NODE = 9;
  Object.defineProperties(Node.prototype, {
    firstChild: { get: function () { return this.childNodes[0] || null; } },
    lastChild: { get: function () { return this.childNodes[this.childNodes.length - 1] || null; } },
    parentElement: { get: function () { var p = this.parentNode; return p && p.nodeType === 1 ? p : null; } },
    nextSibling: { get: function () { return sibling(this, 1); } },
    previousSibling: { get: function () { return sibling(this, -1); } },
    ownerDocument: { get: function () { return this === document ? null : document; } },
    isConnected: { get: function () { return document.contains(this); } },
    textContent: {
      get: function () { return this.childNodes.map(function (c) { return c.textContent; }).join(""); },
      set: function (v) { this.childNodes = []; this.appendChild(new Text(String(v))); }
    }
  });
  function sibling(node, step) {
    var p = node.parentNode;
    if (!p) return null;
    return p.childNodes[p.childNodes.indexOf(node) + step] || null;
  }
  Node.prototype.appendChild = function (child) {
    if (child.nodeType === 11) { child.childNodes.slice().forEach(this.appendChild, this); return child; }
    if (child.parentNode) child.parentNode.removeChild(child);
    child.parentNode = this;
    this.childNodes.push(child);
    return child;
  };
  Node.prototype.removeChild = function (child) {
    var i = this.childNodes.indexOf(child);
    if (i >= 0) this.childNodes.splice(i, 1);
    child.parentNode = null;
    return child;
  };
  Node.prototype.insertBefore = function (child, ref) {
    if (!ref) return this.appendChild(child);
    if (child.parentNode) child.parentNode.removeChild(child);
    var i = this.childNodes.indexOf(ref);
    child.parentNode = this;
    this.childNodes.splice(i < 0 ? this.childNodes.length : i, 0, child);
    return child;
  };
  Node.prototype.replaceChild = function (child, old) { this.insertBefore(child, old); return this.removeChild(old); };
  Node.prototype.append = function () {
    for (var i = 0; i < arguments.length; i++) {
      var n = arguments[i];
      this.appendChild(typeof n === "string" ? new Text(n) : n);
    }
  };
  Node.prototype.prepend = function () {
    for (var i = arguments.length - 1; i >= 0; i--) {
      var n = arguments[i];
      this.insertBefore(typeof n === "string" ? new Text(n) : n, this.firstChild);
    }
  };
  Node.prototype.remove = function () { if (this.parentNode) this.parentNode.removeChild(this); };
  Node.prototype.contains = function (n) { for (; n; n = n.parentNode) if (n === this) return true; return false; };
  Node.prototype.hasChildNodes = function () { return this.childNodes.length > 0; };
  Node.prototype.cloneNode = function (deep) {
    var copy = this.nodeType === 3 ? new Text(this.data) : createElement(this.localName, this.__attrs.map(function (a) { return a.slice(); }));
    if (deep) this.childNodes.forEach(function (c) { copy.appendChild(c.cloneNode(true)); });
    return copy;
  };

  function Text(data) { Node.call(this); this.data = data; }
  Text.prototype = Object.create(Node.prototype);
  Text.prototype.nodeType = 3;
  Text.prototype.nodeName = "#text";
  Object.defineProperties(Text.prototype, {
    textContent: { get: function () { return this.data; }, set: function (v) { this.data = String(v); } },
    nodeValue: { get: function () { return this.data; }, set: function (v) { this.data = String(v); } }
  });

  function DocumentFragment() { Node.call(this); }
  DocumentFragment.prototype = Object.create(Node.prototype);
  DocumentFragment.prototype.nodeType = 11;

  function Element(tag, attrs) {
    Node.call(this);
    this.localName = tag;
    this.__attrs = attrs || [];
    this.style = {};
  }
  Element.prototype = Object.create(Node.prototype);
  Element.prototype.nodeType = 1;
  Element.prototype.getAttribute = function (name) {
    name = String(name).toLowerCase();
    for (var i = 0; i < this.__attrs.length; i++) if (this.__attrs[i][0] === name) return this.__attrs[i][1];
    return null;
  };
  Element.prototype.hasAttribute = function (name) { return this.getAttribute(name) !== null; };
  Element.prototype.setAttribute = function (name, value) {
    name = String(name).toLowerCase();
    value = String(value);
    for (var i = 0; i < this.__attrs.length; i++) if (this.__attrs[i][0] === name) { this.__attrs[i][1] = value; return; }
    this.__attrs.push([name, value]);
  };
  Element.prototype.removeAttribute = function (name) {
    name = String(name).toLowerCase();
    this.__attrs = this.__attrs.filter(function (a) { return a[0] !== name; });
  };
  Element.prototype.getAttributeNames = function () { return this.__attrs.map(function (a) { return a[0]; }); };
  Element.prototype.toggleAttribute = function (name, force) {
    var on = force === undefined ? !this.hasAttribute(name) : !!force;
    if (on) this.setAttribute(name, ""); else this.removeAttribute(name);
    return on;
  };
  Element.prototype.getElementsByTagName = function (tag) {
    tag = String(tag).toLowerCase();
    return descendants(this).filter(function (e) { return tag === "*" || e.localName === tag; });
  };
  Element.prototype.getElementsByClassName = function (names) {
    var wanted = String(names).split(/\s+/).filter(Boolean);
    return descendants(this).filter(function (e) {
      var have = e.className.split(/\s+/);
      return wanted.every(function (n) { return have.indexOf(n) >= 0; });
    });
  };
  Element.prototype.querySelectorAll = function (selector) { return selectAll(this, selector); };
  Element.prototype.querySelector = function (selector) { return selectAll(this, selector)[0] || null; };
  Element.prototype.matches = function (selector) { return matchesSelector(this, selector); };
  Element.prototype.closest = function (selector) {
    for (var e = this; e && e.nodeType === 1; e = e.parentNode) if (matchesSelector(e, selector)) return e;
    return null;
  };
  Element.prototype.click = function () {
    var event = new Event("click", { bubbles: true, cancelable: true });
    if (!this.dispatchEvent(event)) return;
    if (this.localName === "a" && this.hasAttribute("href")) { navigate("GET", this.href, null); return; }
    var type = (this.getAttribute("type") || "").toLowerCase();
    var form = this.form;
    if (form && ((this.localName === "button" && type !== "button" && type !== "reset") ||
        (this.localName === "input" && (type === "submit" || type === "image")))) {
      form.requestSubmit(this);
    }
  };
  Element.prototype.submit = function () { submitForm(this, null); };
  Element.prototype.requestSubmit = function (submitter) {
    if (this.checkValidity && !this.checkValidity()) return;
    if (this.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true, submitter: submitter }))) {
      submitForm(this, submitter || null);
    }
  };
  Element.prototype.reset = noop;
  Element.prototype.checkValidity = function () { return true; };
  Element.prototype.reportValidity = function () { return true; };
  Element.prototype.focus = noop;
  Element.prototype.blur = noop;
  Element.prototype.scrollIntoView = noop;
  Element.prototype.attachShadow = function () { var f = new DocumentFragment(); this.shadowRoot = f; return f; };
  Element.prototype.getBoundingClientRect = function () {
    return { x: 0, y: 0, top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 };
  };
  Element.prototype.getClientRects = function () { return []; };
  Element.prototype.insertAdjacentHTML = function (where, html) {
    var nodes = parseFragment(html);
    var self = this;
    nodes.forEach(function (n) {
      switch (String(where).toLowerCase()) {
        case "beforebegin": if (self.parentNode) self.parentNode.insertBefore(n, self); break;
        case "afterbegin": self.insertBefore(n, self.firstChild); break;
        case "afterend": if (self.parentNode) self.parentNode.insertBefore(n, self.nextSibling); break;
        default: self.appendChild(n);
      }
    });
  };

  function reflect(prop, attr) {
    Object.defineProperty(Element.prototype, prop, {
      get: function () { var v = this.getAttribute(attr); return v === null ? "" : v; },
      set: function (v) { this.setAttribute(attr, v); }
    });
  }
  reflect("id", "id");
  reflect("className", "class");
  reflect("name", "name");
  reflect("title", "title");
  reflect("lang", "lang");
  reflect("rel", "rel");
  reflect("target", "target");
  reflect("placeholder", "placeholder");
  reflect("content", "content");
  reflect("httpEquiv", "http-equiv");
  reflect("enctype", "enctype");
  reflect("nonce", "nonce");
  function reflectBool(prop) {
    Object.defineProperty(Element.prototype, prop, {
      get: function () { return this.hasAttribute(prop); },
      set: function (v) { this.toggleAttribute(prop, !!v); }
    });
  }
  ["disabled", "hidden", "required", "readOnly", "multiple", "selected", "async", "defer"].forEach(reflectBool);
  function reflectURL(prop, fallbackToPage) {
    Object.defineProperty(Element.prototype, prop, {
      get: function () {
        var v = this.getAttribute(prop);
        if (v === null) return fallbackToPage ? location.href : "";
        return host.resolve(v, location.href) || v;
      },
      set: function (v) { this.setAttribute(prop, v); }
    });
  }
  reflectURL("href", false);
  reflectURL("src", false);
  reflectURL("action", true);
  Object.defineProperties(Element.prototype, {
    tagName: { get: function () { return this.localName.toUpperCase(); } },
    nodeName: { get: function () { return this.localName.toUpperCase(); } },
    children: { get: function () { return this.childNodes.filter(function (c) { return c.nodeType === 1; }); } },
    childElementCount: { get: function () { return this.children.length; } },
    firstElementChild: { get: function () { return this.children[0] || null; } },
    lastElementChild: { get: function () { var c = this.children; return c[c.length - 1] || null; } },
    nextElementSibling: { get: function () { return elementSibling(this, 1); } },
    previousElementSibling: { get: function () { return elementSibling(this, -1); } },
    innerText: {
      get: function () { return this.textContent; },
      set: function (v) { this.textContent = v; }
    },
    innerHTML: {
      get: function () { return this.childNodes.map(serialize).join(""); },
      set: function (html) {
        this.childNodes = [];
        parseFragment(String(html)).forEach(this.appendChild, this);
      }
    },
    outerHTML: { get: function () { return serialize(this); } },
    method: {
      get: function () { return (this.getAttribute("method") || "get").toLowerCase() === "post" ? "post" : "get"; },
      set: function (v) { this.setAttribute("method", v); }
    },
    type: {
      get: function () {
        var t = this.getAttribute("type");
        if (t !== null) return t.toLowerCase();
        return this.localName === "input" ? "text" : this.localName === "button" ? "submit" : "";
      },
      set: function (v) { this.setAttribute("type", v); }
    },
    value: {
      get: function () {
        if (this.__value !== undefined) return this.__value;
        if (this.localName === "textarea") return this.textContent;
        if (this.localName === "select") {
          var opts = this.getElementsByTagName("option");
          var chosen = opts.filter(function (o) { return o.hasAttribute("selected"); })[0] || opts[0];
          return chosen ? chosen.value : "";
        }
        if (this.localName === "option" && !this.hasAttribute("value")) return this.textContent;
        var v = this.getAttribute("value");
        if (v === null && (this.type === "checkbox" || this.type === "radio")) return "on";
        return v === null ? "" : v;
      },
      set: function (v) { this.__value = String(v); }
    },
    defaultValue: { get: function () { return this.getAttribute("value") || ""; }, set: function (v) { this.setAttribute("value", v); } },
    checked: {
      get: function () { return this.__checked !== undefined ? this.__checked : this.hasAttribute("checked"); },
      set: function (v) { this.__checked = !!v; }
    },
    form: { get: function () { return this.closest("form"); } },
    elements: {
      get: function () {
        var list = descendants(this).filter(function (e) {
          return ["input", "select", "textarea", "button", "fieldset", "output"].indexOf(e.localName) >= 0;
        });
        list.namedItem = function (name) {
          for (var i = 0; i < list.length; i++) if (list[i].name === name || list[i].id === name) return list[i];
          return null;
        };
        list.item = function (i) { return list[i] || null; };
        list.forEach(function (e) { if (e.name && !(e.name in list)) list[e.name] = e; });
        return list;
      }
    },
    options: { get: function () { return this.getElementsByTagName("option"); } },
    text: { get: function () { return this.textContent; }, set: function (v) { this.textContent = v; } },
    classList: {
      get: function () {
        var el = this;
        var names = function () { return el.className.split(/\s+/).filter(Boolean); };
        return {
          contains: function (n) { return names().indexOf(n) >= 0; },
          add: function () { var s = names(); for (var i = 0; i < arguments.length; i++) if (s.indexOf(arguments[i]) < 0) s.push(arguments[i]); el.className = s.join(" "); },
          remove: function () { var drop = Array.prototype.slice.call(arguments); el.className = names().filter(function (n) { return drop.indexOf(n) < 0; }).join(" "); },
          toggle: function (n, force) { var on = force === undefined ? !this.contains(n) : !!force; if (on) this.add(n); else this.remove(n); return on; },
          get length() { return names().length; }
        };
      }
    },
    dataset: {
      get: function () {
        var out = {};
        this.__attrs.forEach(function (a) {
          if (a[0].indexOf("data-") === 0) out[a[0].slice(5).replace(/-([a-z])/g, function (_, c) { return c.toUpperCase(); })] = a[1];
        });
        return out;
      }
    },
    offsetWidth: { get: function () { return 0; } },
    offsetHeight: { get: function () { return 0; } },
    clientWidth: { get: function () { return 0; } },
    clientHeight: { get: function () { return 0; } }
  });
  function elementSibling(node, step) {
    var p = node.parentNode;
    if (!p) return null;
    var siblings = p.children;
    return siblings[siblings.indexOf(node) + step] || null;
  }

  var voidTags = ["area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"];
  function serialize(n) {
    if (n.nodeType === 3) return n.data.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
    var attrs = n.__attrs.map(function (a) { return " " + a[0] + '="' + a[1].replace(/&/g, "&amp;").replace(/"/g, "&quot;") + '"'; }).join("");
    if (voidTags.indexOf(n.localName) >= 0) return "<" + n.localName + attrs + ">";
    return "<" + n.localName + attrs + ">" + n.childNodes.map(serialize).join("") + "</" + n.localName + ">";
  }

  function descendants(root) {
    var out = [];
    (function walk(n) {
      for (var i = 0; i < n.childNodes.length; i++) {
        var c = n.childNodes[i];
        if (c.nodeType === 1) { out.push(c); walk(c); }
      }
    })(root);
    return out;
  }

  function parseCompound(text) {
    var c = { tag: null, ids: [], classes: [], attrs: [] };
    var m = /^([a-zA-Z*][\w-]*)/.exec(text);
    if (m) { c.tag = m[1].toLowerCase(); text = text.slice(m[1].length); }
    var re = /#([\w-]+)|\.([\w-]+)|\[\s*([\w-]+)\s*(?:([\^$*~|]?=)\s*(?:"([^"]*)"|'([^']*)'|([^\]\s]*))\s*)?\]|:([\w-]+)/g;
    while ((m = re.exec(text))) {
      if (m[1]) c.ids.push(m[1]);
      else if (m[2]) c.classes.push(m[2]);
      else if (m[3]) c.attrs.push({ name: m[3].toLowerCase(), op: m[4], value: m[5] !== undefined ? m[5] : m[6] !== undefined ? m[6] : m[7] });
    }
    return c;
  }
  function parseSelector(selector) {
    return String(selector).split(",").map(function (group) {
      var parts = [];
      var re = /\s*(>|\+|~)?\s*((?:[^\s>+~\[]|\[[^\]]*\])+)/g;
      var m;
      group = group.trim();
      while ((m = re.exec(group))) parts.push({ combinator: m[1] || " ", compound: parseCompound(m[2]) });
      return parts;
    });
  }
  function matchesCompound(el, c) {
    if (c.tag && c.tag !== "*" && el.localName !== c.tag) return false;
    for (var i = 0; i < c.ids.length; i++) if (el.id !== c.ids[i]) return false;
    var classes = el.className.split(/\s+/);
    for (i = 0; i < c.classes.length; i++) if (classes.indexOf(c.classes[i]) < 0) return false;
    for (i = 0; i < c.attrs.length; i++) {
      var a = c.attrs[i];
      var v = el.getAttribute(a.name);
      if (v === null) return false;
      if (!a.op) continue;
      if (a.op === "=" && v !== a.value) return false;
      if (a.op === "^=" && v.indexOf(a.value) !== 0) return false;
      if (a.op === "$=" && v.slice(-a.value.length) !== a.value) return false;
      if (a.op === "*=" && v.indexOf(a.value) < 0) return false;
      if (a.op === "~=" && v.split(/\s+/).indexOf(a.value) < 0) return false;
      if (a.op === "|=" && v !== a.value && v.indexOf(a.value + "-") !== 0) return false;
    }
    return true;
  }
  function matchesParts(el, parts, i) {
    if (!el || el.nodeType !== 1 || !matchesCompound(el, parts[i].compound)) return false;
    if (i === 0) return true;
    switch (parts[i].combinator) {
      case ">": return matchesParts(el.parentNode, parts, i - 1);
      case "+": return matchesParts(el.previousElementSibling, parts, i - 1);
      case "~":
        for (var s = el.previousElementSibling; s; s = s.previousElementSibling) if (matchesParts(s, parts, i - 1)) return true;
        return false;
      default:
        for (var p = el.parentNode; p && p.nodeType === 1; p = p.parentNode) if (matchesParts(p, parts, i - 1)) return true;
        return false;
    }
  }
  function matchesSelector(el, selector) {
    return parseSelector(selector).some(function (parts) { return parts.length > 0 && matchesParts(el, parts, parts.length - 1); });
  }
  function selectAll(root, selector) {
    var groups = parseSelector(selector);
    return descendants(root).filter(function (el) {
      return groups.some(function (parts) { return parts.length > 0 && matchesParts(el, parts, parts.length - 1); });
    });
  }

  function createElement(tag, attrs) { return new Element(String(tag).toLowerCase(), attrs); }
  function build(node) {
    if (typeof node === "string") return new Text(node);
    var el = createElement(node.t, node.a);
    node.c.forEach(function (c) { el.appendChild(build(c)); });
    return el;
  }
  function parseFragment(html) { return JSON.parse(host.parseFragment(html)).map(build); }

  function Document() { Node.call(this); }
  Document.prototype = Object.create(Node.prototype);
  Document.prototype.nodeType = 9;
  Document.prototype.nodeName = "#document";
  ["getElementsByTagName", "getElementsByClassName", "querySelector", "querySelectorAll"].forEach(function (name) {
    Document.prototype[name] = Element.prototype[name];
  });
  Document.prototype.getElementById = function (id) {
    return descendants(this).filter(function (e) { return e.id === id; })[0] || null;
  };
  Document.prototype.getElementsByName = function (name) {
    return descendants(this).filter(function (e) { return e.getAttribute("name") === name; });
  };
  Document.prototype.createElement = function (tag) { return createElement(tag); };
  Document.prototype.createElementNS = function (_, tag) { return createElement(tag); };
  Document.prototype.createTextNode = function (data) { return new Text(String(data)); };
  Document.prototype.createDocumentFragment = function () { return new DocumentFragment(); };
  Document.prototype.createComment = function () { return new Text(""); };
  Document.prototype.createEvent = function () {
    var e = new Event("");
    e.initEvent = function (type, bubbles, cancelable) { this.type = type; this.bubbles = !!bubbles; this.cancelable = !!cancelable; };
    return e;
  };
  Document.prototype.write = function () {
    var html = Array.prototype.join.call(arguments, "");
    var target = this.body || this.documentElement;
    parseFragment(html).forEach(target.appendChild, target);
  };
  Document.prototype.writeln = function () { this.write(Array.prototype.join.call(arguments, "") + "\n"); };
  Document.prototype.open = noop;
  Document.prototype.close = noop;
  Document.prototype.hasFocus = function () { return true; };
  function firstByTag(tag) { return function () { return this.getElementsByTagName(tag)[0] || null; }; }
  Object.defineProperties(Document.prototype, {
    documentElement: { get: function () { return this.children[0] || null; } },
    children: Object.getOwnPropertyDescriptor(Element.prototype, "children"),
    head: { get: firstByTag("head") },
    body: { get: firstByTag("body") },
    forms: { get: function () { return this.getElementsByTagName("form"); } },
    links: { get: function () { return this.querySelectorAll("a[href]"); } },
    images: { get: function () { return this.getElementsByTagName("img"); } },
    scripts: { get: function () { return this.getElementsByTagName("script"); } },
    title: {
      get: function () { var t = this.getElementsByTagName("title")[0]; return t ? t.textContent.trim() : ""; },
      set: noop
    },
    cookie: { get: function () { return host.getCookie(); }, set: function (v) { host.setCookie(String(v)); } },
    location: { get: function () { return location; }, set: function (v) { location.href = v; } },
    URL: { get: function () { return location.href; } },
    documentURI: { get: function () { return location.href; } },
    domain: { get: function () { return location.hostname; } },
    referrer: { value: "" },
    characterSet: { value: "UTF-8" },
    compatMode: { value: "CSS1Compat" },
    hidden: { value: false },
    visibilityState: { value: "visible" },
    defaultView: { get: function () { return g; } },
    activeElement: { get: function () { return this.body; } }
  });

  var document = new Document();
  document.readyState = "loading";
  document.currentScript = null;
  document.appendChild(build(tree));

  function encodeForm(s) { return encodeURIComponent(s).replace(/%20/g, "+"); }
  function decodeForm(s) { return decodeURIComponent(s.replace(/\+/g, " ")); }

  function URLSearchParams(init) {
    this.__pairs = [];
    if (init == null) return;
    if (init instanceof URLSearchParams) { this.__pairs = init.__pairs.map(function (p) { return p.slice(); }); return; }
    if (typeof init === "object") {
      var self = this;
      if (typeof init[Symbol.iterator] === "function") {
        for (var pair of init) self.append(pair[0], pair[1]);
      } else {
        Object.keys(init).forEach(function (k) { self.append(k, init[k]); });
      }
      return;
    }
    var text = String(init);
    if (text.charAt(0) === "?") text = text.slice(1);
    var pairs = this.__pairs;
    text.split("&").forEach(function (part) {
      if (!part) return;
      var i = part.indexOf("=");
      pairs.push(i < 0 ? [decodeForm(part), ""] : [decodeForm(part.slice(0, i)), decodeForm(part.slice(i + 1))]);
    });
  }
  URLSearchParams.prototype.append = function (k, v) { this.__pairs.push([String(k), String(v)]); };
  URLSearchParams.prototype.get = function (k) {
    for (var i = 0; i < this.__pairs.length; i++) if (this.__pairs[i][0] === k) return this.__pairs[i][1];
    return null;
  };
  URLSearchParams.prototype.getAll = function (k) {
    return this.__pairs.filter(function (p) { return p[0] === k; }).map(function (p) { return p[1]; });
  };
  URLSearchParams.prototype.has = function (k) { return this.get(k) !== null; };
  URLSearchParams.prototype.set = function (k, v) {
    var i = -1;
    this.__pairs = this.__pairs.filter(function (p, j) { if (p[0] !== k) return true; if (i < 0) { i = j; return true; } return false; });
    if (i < 0) this.append(k, v); else this.__pairs[i][1] = String(v);
  };
  URLSearchParams.prototype["delete"] = function (k) { this.__pairs = this.__pairs.filter(function (p) { return p[0] !== k; }); };
  URLSearchParams.prototype.forEach = function (fn, self) {
    var params = this;
    this.__pairs.slice().forEach(function (p) { fn.call(self, p[1], p[0], params); });
  };
  URLSearchParams.prototype.entries = function () { return this.__pairs.map(function (p) { return p.slice(); })[Symbol.iterator](); };
  URLSearchParams.prototype.keys = function () { return this.__pairs.map(function (p) { return p[0]; })[Symbol.iterator](); };
  URLSearchParams.prototype.values = function () { return this.__pairs.map(function (p) { return p[1]; })[Symbol.iterator](); };
  URLSearchParams.prototype[Symbol.iterator] = URLSearchParams.prototype.entries;
  URLSearchParams.prototype.toString = function () {
    return this.__pairs.map(function (p) { return encodeForm(p[0]) + "=" + encodeForm(p[1]); }).join("&");
  };
  Object.defineProperty(URLSearchParams.prototype, "size", { get: function () { return this.__pairs.length; } });

  var urlParts = ["href", "protocol", "username", "password", "host", "hostname", "port", "pathname", "search", "hash", "origin"];
  function URL(href, base) {
    var parts = host.parseURL(String(href), base === undefined ? "" : String(base));
    if (!parts) throw new TypeError("Invalid URL: " + href);
    var self = this;
    urlParts.forEach(function (k) { self[k] = parts[k]; });
    this.searchParams = new URLSearchParams(this.search);
  }
  URL.prototype.toString = function () {
    var q = this.searchParams.toString();
    var parts = host.parseURL(this.href, "");
    return parts.href.split("#")[0].split("?")[0] + (q ? "?" + q : "") + this.hash;
  };
  URL.prototype.toJSON = URL.prototype.toString;
  URL.canParse = function (href, base) { return !!host.parseURL(String(href), base === undefined ? "" : String(base)); };
  URL.createObjectURL = function () { return "blob:" + location.origin + "/0"; };
  URL.revokeObjectURL = noop;

  function navigate(method, target, pairs) {
    host.navigate(method, String(target), pairs ? JSON.stringify(pairs) : "", clock);
  }

  function submitForm(form, submitter) {
    var pairs = [];
    form.elements.forEach(function (el) {
      var name = el.getAttribute("name");
      if (!name || el.disabled || el.localName === "fieldset" || el.localName === "output") return;
      var type = el.type;
      if (["submit", "image", "button", "reset"].indexOf(type) >= 0 || el.localName === "button") {
        if (el === submitter) pairs.push([name, el.value]);
        return;
      }
      if ((type === "checkbox" || type === "radio") && !el.checked) return;
      if (type === "file") return;
      pairs.push([name, el.value]);
    });
    var action = submitter && submitter.hasAttribute("formaction") ? host.resolve(submitter.getAttribute("formaction"), location.href) : form.action;
    var method = submitter && submitter.hasAttribute("formmethod") ? submitter.getAttribute("formmethod") : form.method;
    navigate(String(method).toUpperCase() === "POST" ? "POST" : "GET", action, pairs);
  }

  var location = {};
  urlParts.forEach(function (k) {
    if (k === "href") return;
    Object.defineProperty(location, k, {
      enumerable: true,
      get: function () { return host.parseURL(pageURL, "")[k]; },
      set: function (v) {
        if (k === "hash" || k === "origin") return;
        navigate("GET", host.withPart(pageURL, k, String(v)), null);
      }
    });
  });
  Object.defineProperty(location, "href", {
    enumerable: true,
    get: function () { return pageURL; },
    set: function (v) { navigate("GET", host.resolve(String(v), pageURL), null); }
  });
  location.assign = function (v) { navigate("GET", host.resolve(String(v), pageURL), null); };
  location.replace = location.assign;
  location.reload = function () { host.navigate("GET", pageURL, "", clock, true); };
  location.toString = function () { return pageURL; };
  location.ancestorOrigins = [];

  function Storage() { this.__items = {}; }
  Storage.prototype.getItem = function (k) { return Object.prototype.hasOwnProperty.call(this.__items, k) ? this.__items[k] : null; };
  Storage.prototype.setItem = function (k, v) { this.__items[k] = String(v); };
  Storage.prototype.removeItem = function (k) { delete this.__items[k]; };
  Storage.prototype.clear = function () { this.__items = {}; };
  Storage.prototype.key = function (i) { return Object.keys(this.__items)[i] || null; };
  Object.defineProperty(Storage.prototype, "length", { get: function () { return Object.keys(this.__items).length; } });

  function addTimer(fn, delay, args, repeat) {
    var id = ++timerSeq;
    if (typeof fn === "string") { var code = fn; fn = function () { (0, eval)(code); }; }
    delay = Math.max(0, Number(delay) || 0);
    timers.push({ id: id, fn: fn, at: clock + delay, delay: delay, args: args, repeat: repeat });
    return id;
  }
  function clearTimer(id) { timers = timers.filter(function (t) { return t.id !== id; }); }

  function TextEncoder() {}
  TextEncoder.prototype.encoding = "utf-8";
  TextEncoder.prototype.encode = function (s) { return new Uint8Array(host.utf8Encode(String(s === undefined ? "" : s))); };
  function TextDecoder() {}
  TextDecoder.prototype.encoding = "utf-8";
  TextDecoder.prototype.decode = function (b) {
    if (b === undefined) return "";
    return host.utf8Decode(b instanceof ArrayBuffer ? b : b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength));
  };

  function bufferOf(data) {
    if (data instanceof ArrayBuffer) return data;
    return data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
  }
  var crypto = {
    getRandomValues: function (arr) { for (var i = 0; i < arr.length; i++) arr[i] = Math.floor(Math.random() * 256); return arr; },
    randomUUID: function () {
      return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, function (c) {
        var r = Math.floor(Math.random() * 16);
        return (c === "x" ? r : (r & 3) | 8).toString(16);
      });
    },
    subtle: {
      digest: function (alg, data) {
        try {
          return Promise.resolve(host.digest(typeof alg === "string" ? alg : alg.name, bufferOf(data)));
        } catch (e) {
          return Promise.reject(e);
        }
      }
    }
  };

  var navigator = {
    userAgent: userAgent,
    appVersion: userAgent.replace(/^Mozilla\//, ""),
    appName: "Netscape",
    appCodeName: "Mozilla",
    product: "Gecko",
    vendor: /Chrome\//.test(userAgent) ? "Google Inc." : "",
    platform: /Windows/.test(userAgent) ? "Win32" : /Mac OS/.test(userAgent) ? "MacIntel" : "Linux x86_64",
    language: "en-US",
    languages: ["en-US", "en"],
    onLine: true,
    cookieEnabled: true,
    webdriver: false,
    doNotTrack: null,
    hardwareConcurrency: 8,
    deviceMemory: 8,
    maxTouchPoints: 0,
    plugins: [],
    mimeTypes: [],
    javaEnabled: function () { return false; },
    sendBeacon: function () { return true; },
    userAgentData: undefined
  };

  function unsupported(name) {
    return function () { return Promise.reject(new TypeError(name + " is not available")); };
  }
  function MutationObserver() {}
  MutationObserver.prototype.observe = noop;
  MutationObserver.prototype.disconnect = noop;
  MutationObserver.prototype.takeRecords = function () { return []; };

  var globals = {
    window: g, self: g, top: g, parent: g, frames: g, globalThis: g,
    document: document, navigator: navigator, crypto: crypto,
    Event: Event, CustomEvent: function (type, init) { Event.call(this, type, init); this.detail = init && init.detail; },
    EventTarget: EventTarget, Node: Node, Element: Element, HTMLElement: Element, Text: Text, Document: Document,
    HTMLFormElement: Element, HTMLInputElement: Element, DocumentFragment: DocumentFragment,
    URL: URL, URLSearchParams: URLSearchParams, TextEncoder: TextEncoder, TextDecoder: TextDecoder,
    Storage: Storage, localStorage: new Storage(), sessionStorage: new Storage(),
    MutationObserver: MutationObserver, IntersectionObserver: MutationObserver, ResizeObserver: MutationObserver,
    history: { length: 1, state: null, pushState: noop, replaceState: noop, back: noop, forward: noop, go: noop },
    screen: { width: 1920, height: 1080, availWidth: 1920, availHeight: 1040, colorDepth: 24, pixelDepth: 24 },
    innerWidth: 1920, innerHeight: 960, outerWidth: 1920, outerHeight: 1040, devicePixelRatio: 1,
    scrollX: 0, scrollY: 0, pageXOffset: 0, pageYOffset: 0, isSecureContext: true, name: "", closed: false,
    origin: host.parseURL(pageURL, "").origin,
    console: { log: noop, info: noop, warn: noop, error: noop, debug: noop, trace: noop, dir: noop, table: noop, group: noop, groupEnd: noop },
    performance: { now: function () { return clock; }, timeOrigin: Date.now(), mark: noop, measure: noop, getEntriesByType: function () { return []; } },
    setTimeout: function (fn, delay) { return addTimer(fn, delay, Array.prototype.slice.call(arguments, 2), false); },
    setInterval: function (fn, delay) { return addTimer(fn, delay, Array.prototype.slice.call(arguments, 2), true); },
    clearTimeout: clearTimer, clearInterval: clearTimer,
    requestAnimationFrame: function (fn) { return addTimer(function () { fn(clock); }, 16, [], false); },
    cancelAnimationFrame: clearTimer,
    requestIdleCallback: function (fn) { return addTimer(function () { fn({ didTimeout: false, timeRemaining: function () { return 50; } }); }, 1, [], false); },
    cancelIdleCallback: clearTimer,
    queueMicrotask: function (fn) { Promise.resolve().then(fn); },
    atob: function (s) { return host.atob(String(s)); },
    btoa: function (s) { return host.btoa(String(s)); },
    getComputedStyle: function (el) { return el.style || {}; },
    matchMedia: function (q) { return { matches: false, media: q, addListener: noop, removeListener: noop, addEventListener: noop, removeEventListener: noop }; },
    alert: noop, confirm: function () { return false; }, prompt: function () { return null; },
    focus: noop, blur: noop, scrollTo: noop, scroll: noop, postMessage: noop, open: function () { return null; }, close: noop,
    fetch: unsupported("fetch"),
    Image: function () { return createElement("img"); },
    XMLHttpRequest: function () { throw new TypeError("XMLHttpRequest is not available"); },
    Worker: function () { throw new TypeError("Worker is not available"); }
  };
  Object.keys(globals).forEach(function (k) {
    Object.defineProperty(g, k, { value: globals[k], writable: true, configurable: true });
  });
  Object.defineProperty(g, "location", { get: function () { return location; }, set: function (v) { location.href = v; }, configurable: true });
  ["addEventListener", "removeEventListener", "dispatchEvent"].forEach(function (k) {
    Object.defineProperty(g, k, { value: EventTarget.prototype[k], writable: true, configurable: true });
  });

  var scripts = document.getElementsByTagName("script").filter(function (s) {
    var type = (s.getAttribute("type") || "").toLowerCase().split(";")[0].trim();
    return !s.hasAttribute("src") && (type === "" || /^(text|application)\/(x-)?(java|ecma)script$/.test(type));
  });

  Object.defineProperty(g, "__page", {
    value: {
      scripts: scripts.map(function (s) { return s.textContent; }),
      enter: function (i) { document.currentScript = scripts[i]; },
      leave: function () { document.currentScript = null; },
      fire: function (phase) {
        if (phase === "DOMContentLoaded") {
          document.readyState = "interactive";
          document.dispatchEvent(new Event("readystatechange"));
          document.dispatchEvent(new Event("DOMContentLoaded", { bubbles: true }));
        } else {
          document.readyState = "complete";
          document.dispatchEvent(new Event("readystatechange"));
          g.dispatchEvent(new Event("load"));
          g.dispatchEvent(new Event("pageshow"));
        }
      },
      tick: function () {
        if (timers.length === 0) return false;
        var next = timers.reduce(function (a, b) { return b.at < a.at ? b : a; });
        clock = Math.max(clock, next.at);
        if (next.repeat) next.at = clock + Math.max(next.delay, 1);
        else clearTimer(next.id);
        try {
          next.fn.apply(g, next.args);
        } catch (e) {}
        return true;
      }
    }
  });
})(this);
