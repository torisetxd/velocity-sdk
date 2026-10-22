// A small browser for running a page's scripts without one: enough DOM,
// location, history, cookie, timer, encoding and network API for an
// interstitial to compute its answer and navigate, or for a client-rendered
// app (React, Ember, ...) to boot, fetch its data and render. __host is the Go
// side: it records navigation and cookies, parses URLs and HTML, runs and
// loads scripts and, when the page is online, performs requests.
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

  var online = typeof host.request === "function";
  var HTML_NS = "http://www.w3.org/1999/xhtml";
  var SVG_NS = "http://www.w3.org/2000/svg";
  var MATH_NS = "http://www.w3.org/1998/Math/MathML";

  // The clock is virtual: timers run in order of due time without waiting, and
  // performance.now reports how far the page believes it has got.
  var clock = 0;
  var timers = [];
  var timerSeq = 0;
  // Timers further out than idleHorizon are polling, not rendering; the page
  // counts as settled without them.
  var idleHorizon = 10000;
  var maxClock = 60000;
  var netQueue = [];
  var bootQueue = [];
  var booted = false;
  var noop = function () {};

  function Event(type, init) {
    init = init || {};
    this.type = String(type);
    this.bubbles = !!init.bubbles;
    this.cancelable = !!init.cancelable;
    this.composed = !!init.composed;
    this.defaultPrevented = false;
    this.isTrusted = true;
    this.target = null;
    this.currentTarget = null;
    this.eventPhase = 0;
    this.timeStamp = clock;
    this.submitter = init.submitter || null;
  }
  Event.prototype.preventDefault = function () { if (this.cancelable) this.defaultPrevented = true; };
  Event.prototype.stopPropagation = function () { this.__stopped = true; };
  Event.prototype.stopImmediatePropagation = function () { this.__stopped = true; this.__halted = true; };
  Event.prototype.composedPath = function () {
    var path = [];
    for (var n = this.target; n; n = n.parentNode) path.push(n);
    path.push(g);
    return path;
  };
  function CustomEvent(type, init) { Event.call(this, type, init); this.detail = init && init.detail !== undefined ? init.detail : null; }
  CustomEvent.prototype = Object.create(Event.prototype);

  function EventTarget() {}
  EventTarget.prototype.addEventListener = function (type, fn, opts) {
    if (!fn) return;
    var all = this.__listeners || (this.__listeners = {});
    var list = all[type] || (all[type] = []);
    for (var i = 0; i < list.length; i++) if (list[i].fn === fn) return;
    list.push({ fn: fn, once: !!(opts && opts.once) });
    if (opts && opts.signal) {
      var self = this;
      opts.signal.addEventListener("abort", function () { self.removeEventListener(type, fn); });
    }
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
      } catch (e) { report(e); }
      var list = node.__listeners && node.__listeners[event.type];
      if (list) {
        list = list.slice();
        for (var i = 0; i < list.length && !event.__halted; i++) {
          if (list[i].once) node.removeEventListener(event.type, list[i].fn);
          var fn = list[i].fn;
          try {
            if (typeof fn === "function") fn.call(node, event);
            else if (fn && typeof fn.handleEvent === "function") fn.handleEvent(event);
          } catch (e) { report(e); }
        }
      }
      if (!event.bubbles) break;
      node = node === document ? g : node === g ? null : node.parentNode;
    }
    event.currentTarget = null;
    return !event.defaultPrevented;
  };

  function safeJSON(v) { try { return JSON.stringify(v); } catch (e) { return String(v); } }
  function report(e) {
    try { host.report(e && e.stack ? String(e.stack) : String(e)); } catch (ignored) {}
  }

  // Children are a doubly linked list, so sibling walks and insertions are
  // constant time however wide the parent; childNodes is an array built on
  // demand and kept until the next mutation.
  function Node() {
    EventTarget.call(this);
    this.parentNode = null;
    this.__first = null;
    this.__last = null;
    this.__prev = null;
    this.__next = null;
    this.__cache = null;
  }
  Node.prototype = Object.create(EventTarget.prototype);
  Node.ELEMENT_NODE = 1;
  Node.ATTRIBUTE_NODE = 2;
  Node.TEXT_NODE = 3;
  Node.COMMENT_NODE = 8;
  Node.DOCUMENT_NODE = 9;
  Node.DOCUMENT_FRAGMENT_NODE = 11;
  Node.DOCUMENT_POSITION_DISCONNECTED = 1;
  Node.DOCUMENT_POSITION_PRECEDING = 2;
  Node.DOCUMENT_POSITION_FOLLOWING = 4;
  Node.DOCUMENT_POSITION_CONTAINS = 8;
  Node.DOCUMENT_POSITION_CONTAINED_BY = 16;
  ["ELEMENT_NODE", "ATTRIBUTE_NODE", "TEXT_NODE", "COMMENT_NODE", "DOCUMENT_NODE", "DOCUMENT_FRAGMENT_NODE"].forEach(function (k) {
    Node.prototype[k] = Node[k];
  });
  Object.defineProperties(Node.prototype, {
    childNodes: {
      get: function () {
        if (!this.__cache) {
          var list = [];
          for (var c = this.__first; c; c = c.__next) list.push(c);
          this.__cache = list;
        }
        return this.__cache;
      }
    },
    firstChild: { get: function () { return this.__first; } },
    lastChild: { get: function () { return this.__last; } },
    parentElement: { get: function () { var p = this.parentNode; return p && p.nodeType === 1 ? p : null; } },
    nextSibling: { get: function () { return this.__next; } },
    previousSibling: { get: function () { return this.__prev; } },
    ownerDocument: { get: function () { return this === document ? null : document; } },
    isConnected: { get: function () { return document.contains(this); } },
    baseURI: { get: function () { return pageURL; } },
    textContent: {
      get: function () {
        var out = "";
        for (var c = this.__first; c; c = c.__next) if (c.nodeType !== 8) out += c.textContent;
        return out;
      },
      set: function (v) {
        while (this.__first) this.removeChild(this.__first);
        if (v !== null && v !== undefined && String(v) !== "") this.appendChild(new Text(String(v)));
      }
    }
  });
  function unlink(child) {
    var parent = child.parentNode;
    if (!parent) return;
    if (child.__prev) child.__prev.__next = child.__next; else parent.__first = child.__next;
    if (child.__next) child.__next.__prev = child.__prev; else parent.__last = child.__prev;
    child.parentNode = child.__prev = child.__next = null;
    parent.__cache = null;
  }
  Node.prototype.getRootNode = function () { var n = this; while (n.parentNode) n = n.parentNode; return n; };
  Node.prototype.appendChild = function (child) { return this.insertBefore(child, null); };
  Node.prototype.removeChild = function (child) {
    if (!child || child.parentNode !== this) throw new DOMException("The node to be removed is not a child of this node.", "NotFoundError");
    unlink(child);
    return child;
  };
  Node.prototype.insertBefore = function (child, ref) {
    if (ref === undefined) ref = null;
    if (ref && ref.parentNode !== this) throw new DOMException("The node before which the new node is to be inserted is not a child of this node.", "NotFoundError");
    if (child.nodeType === 11) {
      while (child.__first) this.insertBefore(child.__first, ref);
      return child;
    }
    if (child === ref) return child;
    unlink(child);
    child.parentNode = this;
    child.__next = ref;
    child.__prev = ref ? ref.__prev : this.__last;
    if (child.__prev) child.__prev.__next = child; else this.__first = child;
    if (ref) ref.__prev = child; else this.__last = child;
    this.__cache = null;
    if (booted) connected(child);
    return child;
  };
  Node.prototype.replaceChild = function (child, old) { this.insertBefore(child, old); return this.removeChild(old); };
  function toNodes(args) {
    return Array.prototype.map.call(args, function (n) { return typeof n === "string" ? new Text(n) : n; });
  }
  Node.prototype.append = function () { toNodes(arguments).forEach(this.appendChild, this); };
  Node.prototype.prepend = function () {
    var first = this.firstChild;
    toNodes(arguments).forEach(function (n) { this.insertBefore(n, first); }, this);
  };
  Node.prototype.replaceChildren = function () {
    while (this.__first) this.removeChild(this.__first);
    this.append.apply(this, arguments);
  };
  Node.prototype.before = function () {
    var p = this.parentNode;
    if (p) toNodes(arguments).forEach(function (n) { p.insertBefore(n, this); }, this);
  };
  Node.prototype.after = function () {
    var p = this.parentNode;
    var next = this.nextSibling;
    if (p) toNodes(arguments).forEach(function (n) { p.insertBefore(n, next); });
  };
  Node.prototype.replaceWith = function () {
    var p = this.parentNode;
    if (!p) return;
    this.before.apply(this, arguments);
    p.removeChild(this);
  };
  Node.prototype.remove = function () { if (this.parentNode) this.parentNode.removeChild(this); };
  Node.prototype.contains = function (n) { for (; n; n = n.parentNode) if (n === this) return true; return false; };
  Node.prototype.hasChildNodes = function () { return this.__first !== null; };
  Node.prototype.isSameNode = function (n) { return n === this; };
  Node.prototype.isEqualNode = function (n) { return !!n && serialize(this) === serialize(n); };
  Node.prototype.normalize = noop;
  Node.prototype.compareDocumentPosition = function (other) {
    if (other === this) return 0;
    if (this.contains(other)) return 20;
    if (other.contains(this)) return 10;
    if (this.getRootNode() !== other.getRootNode()) return 1;
    var all = [];
    (function walk(n) { all.push(n); n.childNodes.forEach(walk); })(this.getRootNode());
    return all.indexOf(other) < all.indexOf(this) ? 2 : 4;
  };
  Node.prototype.cloneNode = function (deep) {
    var copy;
    if (this.nodeType === 3) copy = new Text(this.data);
    else if (this.nodeType === 8) copy = new Comment(this.data);
    else if (this.nodeType === 11) copy = new DocumentFragment();
    else copy = createElement(this.localName, this.__attrs.map(function (a) { return a.slice(); }), this.namespaceURI);
    if (deep) for (var c = this.__first; c; c = c.__next) copy.appendChild(c.cloneNode(true));
    return copy;
  };

  function CharacterData() {}
  function characterData(Ctor, type, name) {
    Ctor.prototype = Object.create(Node.prototype);
    Ctor.prototype.constructor = Ctor;
    Ctor.prototype.nodeType = type;
    Ctor.prototype.nodeName = name;
    var data = {
      get: function () { return this.data; },
      set: function (v) { this.data = v === null || v === undefined ? "" : String(v); }
    };
    Object.defineProperties(Ctor.prototype, {
      textContent: data,
      nodeValue: data,
      length: { get: function () { return this.data.length; } }
    });
    Ctor.prototype.appendData = function (s) { this.data += s; };
    Ctor.prototype.substringData = function (o, n) { return this.data.substr(o, n); };
  }
  function Text(data) { Node.call(this); this.data = data === undefined ? "" : String(data); }
  characterData(Text, 3, "#text");
  Object.defineProperty(Text.prototype, "wholeText", { get: function () { return this.data; } });
  function Comment(data) { Node.call(this); this.data = data === undefined ? "" : String(data); }
  characterData(Comment, 8, "#comment");

  function DocumentFragment() { Node.call(this); }
  DocumentFragment.prototype = Object.create(Node.prototype);
  DocumentFragment.prototype.constructor = DocumentFragment;
  DocumentFragment.prototype.nodeType = 11;
  DocumentFragment.prototype.nodeName = "#document-fragment";

  var styleProto = {};
  Object.defineProperties(styleProto, {
    setProperty: { value: function (k, v) { if (v === null || v === "") delete this[k]; else this[k] = String(v); } },
    removeProperty: { value: function (k) { var v = this[k] || ""; delete this[k]; return v; } },
    getPropertyValue: { value: function (k) { return this[k] || ""; } },
    getPropertyPriority: { value: function () { return ""; } },
    item: { value: function (i) { return Object.keys(this)[i] || ""; } },
    length: { get: function () { return Object.keys(this).length; } },
    cssText: {
      get: function () {
        var self = this;
        return Object.keys(this).map(function (k) { return k + ": " + self[k] + ";"; }).join(" ");
      },
      set: function (text) {
        var self = this;
        Object.keys(this).forEach(function (k) { delete self[k]; });
        String(text).split(";").forEach(function (decl) {
          var i = decl.indexOf(":");
          if (i > 0) self[decl.slice(0, i).trim()] = decl.slice(i + 1).trim();
        });
      }
    }
  });

  function Element(tag, attrs) {
    Node.call(this);
    this.localName = tag;
    this.namespaceURI = HTML_NS;
    this.__attrs = attrs || [];
    this.style = Object.create(styleProto);
    for (var i = 0; i < this.__attrs.length; i++) {
      if (this.__attrs[i][0] === "style") this.style.cssText = this.__attrs[i][1];
    }
  }
  Element.prototype = Object.create(Node.prototype);
  Element.prototype.constructor = Element;
  Element.prototype.nodeType = 1;
  Element.prototype.getAttribute = function (name) {
    name = String(name).toLowerCase();
    if (name === "style" && this.style.length) return this.style.cssText;
    for (var i = 0; i < this.__attrs.length; i++) if (this.__attrs[i][0] === name) return this.__attrs[i][1];
    return null;
  };
  Element.prototype.hasAttribute = function (name) { return this.getAttribute(name) !== null; };
  Element.prototype.setAttribute = function (name, value) {
    name = String(name).toLowerCase();
    value = String(value);
    if (name === "style") this.style.cssText = value;
    for (var i = 0; i < this.__attrs.length; i++) if (this.__attrs[i][0] === name) { this.__attrs[i][1] = value; return; }
    this.__attrs.push([name, value]);
  };
  Element.prototype.removeAttribute = function (name) {
    name = String(name).toLowerCase();
    if (name === "style") this.style.cssText = "";
    this.__attrs = this.__attrs.filter(function (a) { return a[0] !== name; });
  };
  Element.prototype.getAttributeNS = function (_, name) { return this.getAttribute(name); };
  Element.prototype.setAttributeNS = function (_, name, value) { this.setAttribute(name, value); };
  Element.prototype.removeAttributeNS = function (_, name) { this.removeAttribute(name); };
  Element.prototype.hasAttributeNS = function (_, name) { return this.hasAttribute(name); };
  Element.prototype.hasAttributes = function () { return this.__attrs.length > 0; };
  Element.prototype.getAttributeNames = function () { return this.__attrs.map(function (a) { return a[0]; }); };
  Element.prototype.toggleAttribute = function (name, force) {
    var on = force === undefined ? !this.hasAttribute(name) : !!force;
    if (on) { if (!this.hasAttribute(name)) this.setAttribute(name, ""); } else this.removeAttribute(name);
    return on;
  };
  Element.prototype.getElementsByTagName = function (tag) {
    tag = String(tag).toLowerCase();
    return descendants(this).filter(function (e) { return tag === "*" || e.localName.toLowerCase() === tag; });
  };
  Element.prototype.getElementsByTagNameNS = function (_, tag) { return this.getElementsByTagName(tag); };
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
  Element.prototype.webkitMatchesSelector = Element.prototype.matches;
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
    if (this.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true, submitter: submitter }))) {
      submitForm(this, submitter || null);
    }
  };
  Element.prototype.reset = noop;
  Element.prototype.checkValidity = function () { return true; };
  Element.prototype.reportValidity = function () { return true; };
  Element.prototype.setCustomValidity = noop;
  Element.prototype.focus = noop;
  Element.prototype.blur = noop;
  Element.prototype.select = noop;
  Element.prototype.setSelectionRange = noop;
  Element.prototype.scrollIntoView = noop;
  Element.prototype.scrollTo = noop;
  Element.prototype.scroll = noop;
  Element.prototype.animate = function () { return { cancel: noop, finish: noop, play: noop, pause: noop, finished: Promise.resolve(), onfinish: null }; };
  Element.prototype.getAnimations = function () { return []; };
  Element.prototype.attachShadow = function () {
    var root = new DocumentFragment();
    root.host = this;
    root.mode = "open";
    this.shadowRoot = root;
    return root;
  };
  Element.prototype.getBoundingClientRect = function () {
    return { x: 0, y: 0, top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0, toJSON: function () { return this; } };
  };
  Element.prototype.getClientRects = function () { return []; };
  Element.prototype.insertAdjacentHTML = function (where, html) {
    var self = this;
    var nodes = parseFragment(html);
    var anchor;
    switch (String(where).toLowerCase()) {
      case "beforebegin": nodes.forEach(function (n) { self.parentNode.insertBefore(n, self); }); break;
      case "afterbegin": anchor = this.firstChild; nodes.forEach(function (n) { self.insertBefore(n, anchor); }); break;
      case "afterend": anchor = this.nextSibling; nodes.forEach(function (n) { self.parentNode.insertBefore(n, anchor); }); break;
      default: nodes.forEach(function (n) { self.appendChild(n); });
    }
  };
  Element.prototype.insertAdjacentElement = function (where, el) {
    switch (String(where).toLowerCase()) {
      case "beforebegin": this.parentNode.insertBefore(el, this); break;
      case "afterbegin": this.insertBefore(el, this.firstChild); break;
      case "afterend": this.parentNode.insertBefore(el, this.nextSibling); break;
      default: this.appendChild(el);
    }
    return el;
  };
  Element.prototype.insertAdjacentText = function (where, text) { this.insertAdjacentElement(where, new Text(text)); };

  function reflect(prop, attr) {
    Object.defineProperty(Element.prototype, prop, {
      get: function () { var v = this.getAttribute(attr); return v === null ? "" : v; },
      set: function (v) { this.setAttribute(attr, v); }
    });
  }
  [["id", "id"], ["className", "class"], ["name", "name"], ["title", "title"], ["lang", "lang"], ["dir", "dir"],
   ["rel", "rel"], ["target", "target"], ["placeholder", "placeholder"], ["content", "content"],
   ["httpEquiv", "http-equiv"], ["enctype", "enctype"], ["nonce", "nonce"], ["alt", "alt"], ["role", "role"],
   ["htmlFor", "for"], ["slot", "slot"], ["crossOrigin", "crossorigin"], ["integrity", "integrity"],
   ["as", "as"], ["media", "media"], ["charset", "charset"], ["autocomplete", "autocomplete"]].forEach(function (p) {
    reflect(p[0], p[1]);
  });
  function reflectBool(prop) {
    Object.defineProperty(Element.prototype, prop, {
      get: function () { return this.hasAttribute(prop); },
      set: function (v) { this.toggleAttribute(prop, !!v); }
    });
  }
  ["disabled", "hidden", "required", "readOnly", "multiple", "selected", "async", "defer", "noModule", "open", "autofocus"].forEach(reflectBool);
  function reflectURL(prop, fallbackToPage) {
    Object.defineProperty(Element.prototype, prop, {
      get: function () {
        var v = this.getAttribute(prop);
        if (v === null) return fallbackToPage ? pageURL : "";
        return host.resolve(v, pageURL) || v;
      },
      set: function (v) { this.setAttribute(prop, v); }
    });
  }
  reflectURL("href", false);
  reflectURL("src", false);
  reflectURL("action", true);
  function urlPart(part) {
    Object.defineProperty(Element.prototype, part, {
      get: function () { var u = this.href && host.parseURL(this.href, ""); return u ? u[part] : ""; }
    });
  }
  ["protocol", "host", "hostname", "port", "pathname", "search", "hash", "origin"].forEach(urlPart);
  Object.defineProperties(Element.prototype, {
    tagName: { get: function () { return this.namespaceURI === HTML_NS ? this.localName.toUpperCase() : this.localName; } },
    nodeName: { get: function () { return this.tagName; } },
    prefix: { value: null },
    children: {
      get: function () {
        var out = [];
        for (var c = this.__first; c; c = c.__next) if (c.nodeType === 1) out.push(c);
        return out;
      }
    },
    childElementCount: { get: function () { return this.children.length; } },
    firstElementChild: { get: function () { var c = this.__first; while (c && c.nodeType !== 1) c = c.__next; return c; } },
    lastElementChild: { get: function () { var c = this.__last; while (c && c.nodeType !== 1) c = c.__prev; return c; } },
    nextElementSibling: { get: function () { return elementSibling(this, 1); } },
    previousElementSibling: { get: function () { return elementSibling(this, -1); } },
    innerText: {
      get: function () { return this.textContent; },
      set: function (v) { this.textContent = v; }
    },
    outerText: { get: function () { return this.textContent; } },
    innerHTML: {
      get: function () { return this.childNodes.map(serialize).join(""); },
      set: function (html) {
        while (this.__first) this.removeChild(this.__first);
        parseFragment(String(html)).forEach(this.appendChild, this);
      }
    },
    outerHTML: {
      get: function () { return serialize(this); },
      set: function (html) { this.replaceWith.apply(this, parseFragment(String(html))); }
    },
    attributes: {
      get: function () {
        var el = this;
        var list = this.__attrs.map(function (a) {
          return { name: a[0], localName: a[0], value: a[1], namespaceURI: null, prefix: null, specified: true, ownerElement: el };
        });
        list.getNamedItem = function (n) { n = String(n).toLowerCase(); return list.filter(function (a) { return a.name === n; })[0] || null; };
        list.item = function (i) { return list[i] || null; };
        return list;
      }
    },
    method: {
      get: function () { return (this.getAttribute("method") || "get").toLowerCase() === "post" ? "post" : "get"; },
      set: function (v) { this.setAttribute("method", v); }
    },
    type: {
      get: function () {
        var t = this.getAttribute("type");
        if (t !== null) return this.localName === "script" ? t : t.toLowerCase();
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
      set: function (v) { this.__value = v === null || v === undefined ? "" : String(v); }
    },
    defaultValue: { get: function () { return this.getAttribute("value") || ""; }, set: function (v) { this.setAttribute("value", v); } },
    checked: {
      get: function () { return this.__checked !== undefined ? this.__checked : this.hasAttribute("checked"); },
      set: function (v) { this.__checked = !!v; }
    },
    defaultChecked: { get: function () { return this.hasAttribute("checked"); }, set: function (v) { this.toggleAttribute("checked", !!v); } },
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
    selectedIndex: {
      get: function () { var o = this.options; for (var i = 0; i < o.length; i++) if (o[i].selected) return i; return o.length ? 0 : -1; },
      set: noop
    },
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
          replace: function (a, b) { if (!this.contains(a)) return false; this.remove(a); this.add(b); return true; },
          item: function (i) { return names()[i] || null; },
          forEach: function (fn, self) { names().forEach(fn, self); },
          supports: function () { return true; },
          get length() { return names().length; },
          get value() { return el.className; },
          toString: function () { return el.className; }
        };
      }
    },
    relList: { get: function () { return { supports: function () { return true; }, contains: function () { return false; } }; } },
    dataset: {
      get: function () {
        var el = this;
        var out = {};
        this.__attrs.forEach(function (a) {
          if (a[0].indexOf("data-") !== 0) return;
          var key = a[0].slice(5).replace(/-([a-z])/g, function (_, c) { return c.toUpperCase(); });
          Object.defineProperty(out, key, {
            enumerable: true,
            get: function () { return el.getAttribute(a[0]); },
            set: function (v) { el.setAttribute(a[0], v); }
          });
        });
        return out;
      }
    },
    offsetWidth: { get: function () { return 0; } },
    offsetHeight: { get: function () { return 0; } },
    offsetTop: { get: function () { return 0; } },
    offsetLeft: { get: function () { return 0; } },
    offsetParent: { get: function () { return null; } },
    clientWidth: { get: function () { return 0; } },
    clientHeight: { get: function () { return 0; } },
    scrollWidth: { get: function () { return 0; } },
    scrollHeight: { get: function () { return 0; } },
    scrollTop: { get: function () { return 0; }, set: noop },
    scrollLeft: { get: function () { return 0; }, set: noop },
    isContentEditable: { get: function () { return false; } },
    tabIndex: { get: function () { return -1; }, set: noop },
    sheet: { get: function () { return this.localName === "style" || this.localName === "link" ? { cssRules: [], insertRule: function () { return 0; }, deleteRule: noop } : null; } }
  });
  function elementSibling(node, step) {
    var n = step > 0 ? node.__next : node.__prev;
    while (n && n.nodeType !== 1) n = step > 0 ? n.__next : n.__prev;
    return n;
  }

  function HTMLElement() { throw new TypeError("Illegal constructor"); }
  HTMLElement.prototype = Object.create(Element.prototype);
  HTMLElement.prototype.constructor = HTMLElement;
  function SVGElement() { throw new TypeError("Illegal constructor"); }
  SVGElement.prototype = Object.create(Element.prototype);
  SVGElement.prototype.constructor = SVGElement;
  // Per-tag interfaces exist so that instanceof checks (React's
  // HTMLIFrameElement walk, form handling, ...) tell elements apart.
  var interfaces = {
    a: "HTMLAnchorElement", area: "HTMLAreaElement", audio: "HTMLAudioElement", body: "HTMLBodyElement",
    br: "HTMLBRElement", button: "HTMLButtonElement", canvas: "HTMLCanvasElement", dialog: "HTMLDialogElement",
    div: "HTMLDivElement", form: "HTMLFormElement", head: "HTMLHeadElement", html: "HTMLHtmlElement",
    iframe: "HTMLIFrameElement", img: "HTMLImageElement", input: "HTMLInputElement", label: "HTMLLabelElement",
    li: "HTMLLIElement", link: "HTMLLinkElement", meta: "HTMLMetaElement", ol: "HTMLOListElement",
    option: "HTMLOptionElement", p: "HTMLParagraphElement", pre: "HTMLPreElement", script: "HTMLScriptElement",
    select: "HTMLSelectElement", slot: "HTMLSlotElement", span: "HTMLSpanElement", style: "HTMLStyleElement",
    table: "HTMLTableElement", template: "HTMLTemplateElement", textarea: "HTMLTextAreaElement",
    title: "HTMLTitleElement", ul: "HTMLUListElement", video: "HTMLVideoElement"
  };
  var interfaceByTag = {};
  var interfaceGlobals = { HTMLElement: HTMLElement, SVGElement: SVGElement };
  Object.keys(interfaces).forEach(function (tag) {
    var C = function () { throw new TypeError("Illegal constructor"); };
    C.prototype = Object.create(HTMLElement.prototype);
    C.prototype.constructor = C;
    Object.defineProperty(C, "name", { value: interfaces[tag] });
    interfaceByTag[tag] = C;
    interfaceGlobals[interfaces[tag]] = C;
  });
  Object.defineProperty(interfaceByTag.template.prototype, "content", {
    get: function () {
      var content = this.__content || (this.__content = new DocumentFragment());
      while (this.__first) content.appendChild(this.__first);
      return content;
    }
  });
  Object.defineProperty(interfaceByTag.iframe.prototype, "contentWindow", { get: function () { return null; } });
  Object.defineProperty(interfaceByTag.iframe.prototype, "contentDocument", { get: function () { return null; } });
  ["play", "pause", "load"].forEach(function (m) {
    interfaceByTag.video.prototype[m] = interfaceByTag.audio.prototype[m] = function () { return Promise.resolve(); };
  });
  interfaceByTag.canvas.prototype.getContext = function () { return null; };
  interfaceByTag.canvas.prototype.toDataURL = function () { return "data:,"; };
  interfaceByTag.dialog.prototype.showModal = interfaceByTag.dialog.prototype.show = function () { this.open = true; };
  interfaceByTag.dialog.prototype.close = function () { this.open = false; };

  var voidTags = ["area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"];
  var rawTextTags = ["script", "style"];
  function escapeText(s) { return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;"); }
  function serialize(n) {
    if (n.nodeType === 3) return n.parentNode && rawTextTags.indexOf(n.parentNode.localName) >= 0 ? n.data : escapeText(n.data);
    if (n.nodeType === 8) return "<!--" + n.data + "-->";
    if (n.nodeType === 11) return n.childNodes.map(serialize).join("");
    var attrs = n.__attrs.map(function (a) {
      var v = a[0] === "style" && n.style.length ? n.style.cssText : a[1];
      return " " + a[0] + '="' + v.replace(/&/g, "&amp;").replace(/"/g, "&quot;") + '"';
    }).join("");
    if (n.style.length && !n.hasAttributeOwn("style")) attrs += ' style="' + n.style.cssText.replace(/"/g, "&quot;") + '"';
    if (voidTags.indexOf(n.localName) >= 0) return "<" + n.localName + attrs + ">";
    var inner = n.childNodes;
    if (n.localName === "template" && n.__content) inner = n.__content.childNodes.concat(inner);
    return "<" + n.localName + attrs + ">" + inner.map(serialize).join("") + "</" + n.localName + ">";
  }
  Element.prototype.hasAttributeOwn = function (name) {
    for (var i = 0; i < this.__attrs.length; i++) if (this.__attrs[i][0] === name) return true;
    return false;
  };

  function descendants(root) {
    var out = [];
    var n = root.__first;
    while (n) {
      if (n.nodeType === 1) out.push(n);
      if (n.__first) { n = n.__first; continue; }
      while (n && n !== root && !n.__next) n = n.parentNode;
      if (!n || n === root) break;
      n = n.__next;
    }
    return out;
  }

  function parseCompound(text) {
    var c = { tag: null, ids: [], classes: [], attrs: [], pseudos: [] };
    var m = /^([a-zA-Z*][\w-]*)/.exec(text);
    if (m) { c.tag = m[1].toLowerCase(); text = text.slice(m[1].length); }
    var re = /#([\w-]+)|\.([\w-]+)|\[\s*([\w:-]+)\s*(?:([\^$*~|]?=)\s*(?:"([^"]*)"|'([^']*)'|([^\]\s]*))\s*(?:[is]\s*)?)?\]|:([\w-]+)(?:\(([^)]*)\))?/g;
    while ((m = re.exec(text))) {
      if (m[1]) c.ids.push(m[1]);
      else if (m[2]) c.classes.push(m[2]);
      else if (m[3]) c.attrs.push({ name: m[3].toLowerCase(), op: m[4], value: m[5] !== undefined ? m[5] : m[6] !== undefined ? m[6] : m[7] });
      else if (m[8]) c.pseudos.push({ name: m[8].toLowerCase(), arg: m[9] });
    }
    return c;
  }
  function splitTopLevel(text, sep) {
    var parts = [];
    var depth = 0;
    var quote = null;
    var start = 0;
    for (var i = 0; i < text.length; i++) {
      var ch = text.charAt(i);
      if (quote) { if (ch === quote) quote = null; continue; }
      if (ch === '"' || ch === "'") quote = ch;
      else if (ch === "[" || ch === "(") depth++;
      else if (ch === "]" || ch === ")") depth--;
      else if (ch === sep && depth === 0) { parts.push(text.slice(start, i)); start = i + 1; }
    }
    parts.push(text.slice(start));
    return parts;
  }
  var selectorCache = {};
  function parseSelector(selector) {
    selector = String(selector);
    if (selectorCache[selector]) return selectorCache[selector];
    var groups = splitTopLevel(selector, ",").map(function (group) {
      var parts = [];
      var re = /\s*(>|\+|~)?\s*((?:[^\s>+~\[(]|\[[^\]]*\]|\([^)]*\))+)/g;
      var m;
      group = group.trim();
      while ((m = re.exec(group))) parts.push({ combinator: m[1] || " ", compound: parseCompound(m[2]) });
      return parts;
    });
    selectorCache[selector] = groups;
    return groups;
  }
  function matchesPseudo(el, p) {
    switch (p.name) {
      case "first-child": return !el.previousElementSibling;
      case "last-child": return !el.nextElementSibling;
      case "only-child": return !el.previousElementSibling && !el.nextElementSibling;
      case "checked": return !!el.checked;
      case "disabled": return el.hasAttribute("disabled");
      case "enabled": return !el.hasAttribute("disabled");
      case "empty": return el.childNodes.length === 0;
      case "root": return el === document.documentElement;
      case "scope": return true;
      case "not": return !matchesSelector(el, p.arg || "");
      case "is": case "where": return matchesSelector(el, p.arg || "*");
      case "has": return selectAll(el, p.arg || "").length > 0;
      case "nth-child":
        var i = el.parentNode ? el.parentNode.children.indexOf(el) + 1 : 1;
        var arg = (p.arg || "").trim();
        if (arg === "odd") return i % 2 === 1;
        if (arg === "even") return i % 2 === 0;
        return String(i) === arg;
      default: return true;
    }
  }
  function matchesCompound(el, c) {
    if (c.tag && c.tag !== "*" && el.localName.toLowerCase() !== c.tag) return false;
    for (var i = 0; i < c.ids.length; i++) if (el.id !== c.ids[i]) return false;
    if (c.classes.length) {
      var classes = el.className.split(/\s+/);
      for (i = 0; i < c.classes.length; i++) if (classes.indexOf(c.classes[i]) < 0) return false;
    }
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
    for (i = 0; i < c.pseudos.length; i++) if (!matchesPseudo(el, c.pseudos[i])) return false;
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

  function createElement(tag, attrs, ns) {
    ns = ns || HTML_NS;
    tag = String(tag);
    if (ns === HTML_NS) tag = tag.toLowerCase();
    var el = new Element(tag, attrs);
    el.namespaceURI = ns;
    var proto = ns === HTML_NS ? (interfaceByTag[tag] || HTMLElement).prototype : ns === SVG_NS ? SVGElement.prototype : Element.prototype;
    Object.setPrototypeOf(el, proto);
    return el;
  }
  function build(node, ns) {
    if (typeof node === "string") return new Text(node);
    if (node.x !== undefined) return new Comment(node.x);
    var childNS = node.n === "svg" ? SVG_NS : node.n === "math" ? MATH_NS : HTML_NS;
    var el = createElement(node.t, node.a, childNS);
    node.c.forEach(function (c) { el.appendChild(build(c)); });
    return el;
  }
  function parseFragment(html) { return JSON.parse(host.parseFragment(html)).map(function (n) { return build(n); }); }

  function Document() { Node.call(this); }
  Document.prototype = Object.create(Node.prototype);
  Document.prototype.constructor = Document;
  Document.prototype.nodeType = 9;
  Document.prototype.nodeName = "#document";
  ["getElementsByTagName", "getElementsByTagNameNS", "getElementsByClassName", "querySelector", "querySelectorAll"].forEach(function (name) {
    Document.prototype[name] = Element.prototype[name];
    DocumentFragment.prototype[name] = Element.prototype[name];
  });
  Document.prototype.getElementById = DocumentFragment.prototype.getElementById = function (id) {
    return descendants(this).filter(function (e) { return e.id === id; })[0] || null;
  };
  Document.prototype.getElementsByName = function (name) {
    return descendants(this).filter(function (e) { return e.getAttribute("name") === name; });
  };
  Document.prototype.createElement = function (tag) { return createElement(tag); };
  Document.prototype.createElementNS = function (ns, tag) {
    var local = String(tag);
    var colon = local.indexOf(":");
    if (colon >= 0) local = local.slice(colon + 1);
    return createElement(local, null, ns || HTML_NS);
  };
  Document.prototype.createTextNode = function (data) { return new Text(String(data)); };
  Document.prototype.createComment = function (data) { return new Comment(String(data)); };
  Document.prototype.createDocumentFragment = function () { return new DocumentFragment(); };
  Document.prototype.importNode = function (node, deep) { return node.cloneNode(!!deep); };
  Document.prototype.adoptNode = function (node) { if (node.parentNode) node.parentNode.removeChild(node); return node; };
  Document.prototype.createEvent = function () {
    var e = new Event("");
    e.initEvent = function (type, bubbles, cancelable) { this.type = type; this.bubbles = !!bubbles; this.cancelable = !!cancelable; };
    e.initCustomEvent = function (type, bubbles, cancelable, detail) { this.initEvent(type, bubbles, cancelable); this.detail = detail; };
    return e;
  };
  Document.prototype.createRange = function () {
    return {
      setStart: noop, setEnd: noop, setStartBefore: noop, setEndAfter: noop, selectNode: noop, selectNodeContents: noop,
      collapse: noop, detach: noop, collapsed: true, commonAncestorContainer: document.body,
      createContextualFragment: function (html) {
        var f = new DocumentFragment();
        parseFragment(html).forEach(f.appendChild, f);
        return f;
      },
      getBoundingClientRect: Element.prototype.getBoundingClientRect,
      getClientRects: function () { return []; },
      cloneRange: function () { return document.createRange(); }
    };
  };
  Document.prototype.createTreeWalker = function (root, whatToShow, filter) {
    var nodes = [];
    var show = whatToShow === undefined ? 0xffffffff : whatToShow;
    (function walk(n) {
      n.childNodes.forEach(function (c) {
        if (show & (1 << (c.nodeType - 1))) {
          var verdict = filter ? (typeof filter === "function" ? filter(c) : filter.acceptNode(c)) : 1;
          if (verdict === 1) nodes.push(c);
          if (verdict === 2) return;
        }
        walk(c);
      });
    })(root);
    var index = -1;
    return {
      root: root,
      currentNode: root,
      nextNode: function () { index++; this.currentNode = nodes[index] || this.currentNode; return nodes[index] || null; },
      previousNode: function () { index--; return nodes[index] || null; }
    };
  };
  Document.prototype.createNodeIterator = Document.prototype.createTreeWalker;
  Document.prototype.write = function () {
    var html = Array.prototype.join.call(arguments, "");
    var target = this.body || this.documentElement;
    parseFragment(html).forEach(target.appendChild, target);
  };
  Document.prototype.writeln = function () { this.write(Array.prototype.join.call(arguments, "") + "\n"); };
  Document.prototype.open = noop;
  Document.prototype.close = noop;
  Document.prototype.hasFocus = function () { return true; };
  Document.prototype.execCommand = function () { return false; };
  Document.prototype.getSelection = function () { return g.getSelection(); };
  Document.prototype.elementFromPoint = function () { return null; };
  function firstByTag(tag) { return function () { return this.getElementsByTagName(tag)[0] || null; }; }
  Object.defineProperties(Document.prototype, {
    documentElement: { get: function () { return this.children[0] || null; } },
    children: Object.getOwnPropertyDescriptor(Element.prototype, "children"),
    firstElementChild: Object.getOwnPropertyDescriptor(Element.prototype, "firstElementChild"),
    childElementCount: Object.getOwnPropertyDescriptor(Element.prototype, "childElementCount"),
    head: { get: firstByTag("head") },
    body: { get: firstByTag("body") },
    forms: { get: function () { return this.getElementsByTagName("form"); } },
    links: { get: function () { return this.querySelectorAll("a[href]"); } },
    images: { get: function () { return this.getElementsByTagName("img"); } },
    scripts: { get: function () { return this.getElementsByTagName("script"); } },
    styleSheets: { get: function () { return []; } },
    fonts: { value: { ready: Promise.resolve(), check: function () { return true; }, load: function () { return Promise.resolve([]); }, add: noop, forEach: noop } },
    title: {
      get: function () { var t = this.getElementsByTagName("title")[0]; return t ? t.textContent.trim() : ""; },
      set: function (v) {
        var t = this.getElementsByTagName("title")[0];
        if (!t && this.head) t = this.head.appendChild(createElement("title"));
        if (t) t.textContent = v;
      }
    },
    cookie: { get: function () { return host.getCookie(); }, set: function (v) { host.setCookie(String(v)); } },
    location: { get: function () { return location; }, set: function (v) { location.href = v; } },
    URL: { get: function () { return pageURL; } },
    documentURI: { get: function () { return pageURL; } },
    domain: { get: function () { return location.hostname; } },
    referrer: { value: "" },
    characterSet: { value: "UTF-8" },
    charset: { value: "UTF-8" },
    contentType: { value: "text/html" },
    compatMode: { value: "CSS1Compat" },
    hidden: { value: false },
    visibilityState: { value: "visible" },
    defaultView: { get: function () { return g; } },
    activeElement: { get: function () { return this.body; } },
    scrollingElement: { get: function () { return this.documentElement; } },
    implementation: {
      value: {
        createHTMLDocument: function () { return document; },
        hasFeature: function () { return true; }
      }
    }
  });

  var document = new Document();
  document.readyState = "loading";
  document.currentScript = null;
  document.appendChild(build(tree));

  function encodeForm(s) { return encodeURIComponent(s).replace(/%20/g, "+"); }
  function decodeForm(s) { try { return decodeURIComponent(s.replace(/\+/g, " ")); } catch (e) { return s; } }

  function URLSearchParams(init) {
    this.__pairs = [];
    this.__owner = null;
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
  function touched(params) {
    if (params.__owner) {
      var q = params.toString();
      params.__owner.__search = q ? "?" + q : "";
    }
  }
  URLSearchParams.prototype.append = function (k, v) { this.__pairs.push([String(k), String(v)]); touched(this); };
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
    if (i < 0) this.__pairs.push([String(k), String(v)]); else this.__pairs[i][1] = String(v);
    touched(this);
  };
  URLSearchParams.prototype["delete"] = function (k) { this.__pairs = this.__pairs.filter(function (p) { return p[0] !== k; }); touched(this); };
  URLSearchParams.prototype.sort = function () { this.__pairs.sort(function (a, b) { return a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0; }); touched(this); };
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
    var parts = host.parseURL(String(href), base === undefined || base === null ? "" : String(base));
    if (!parts) throw new TypeError("Invalid URL: " + href);
    this.__load(parts);
  }
  URL.prototype.__load = function (parts) {
    var self = this;
    urlParts.forEach(function (k) { if (k !== "search") self["__" + k] = parts[k]; });
    this.__search = parts.search;
    this.__params = new URLSearchParams(parts.search);
    this.__params.__owner = this;
  };
  urlParts.forEach(function (k) {
    Object.defineProperty(URL.prototype, k, {
      get: function () {
        if (k === "href") return this.toString();
        return this["__" + k];
      },
      set: function (v) {
        if (k === "href") { this.__load(host.parseURL(String(v), "")); return; }
        if (k === "hash") { v = String(v); this.__hash = v && v.charAt(0) !== "#" ? "#" + v : v; return; }
        if (k === "origin") return;
        if (k === "search") {
          v = String(v);
          this.__search = v && v.charAt(0) !== "?" ? "?" + v : v;
          this.__params = new URLSearchParams(this.__search);
          this.__params.__owner = this;
          return;
        }
        this.__load(host.parseURL(host.withPart(this.toString(), k, String(v)), ""));
      }
    });
  });
  Object.defineProperty(URL.prototype, "searchParams", { get: function () { return this.__params; } });
  URL.prototype.toString = function () {
    var auth = this.__username ? this.__username + (this.__password ? ":" + this.__password : "") + "@" : "";
    return this.__protocol + "//" + auth + this.__host + this.__pathname + (this.__search === "?" ? "" : this.__search) + this.__hash;
  };
  URL.prototype.toJSON = URL.prototype.toString;
  URL.canParse = function (href, base) { return !!host.parseURL(String(href), base === undefined ? "" : String(base)); };
  URL.parse = function (href, base) { try { return new URL(href, base); } catch (e) { return null; } };
  URL.createObjectURL = function () { return "blob:" + location.origin + "/" + (++timerSeq); };
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
    var action = submitter && submitter.hasAttribute("formaction") ? host.resolve(submitter.getAttribute("formaction"), pageURL) : form.action;
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
        if (k === "origin") return;
        if (k === "hash") { pageURL = host.withPart(pageURL, "hash", String(v)); return; }
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

  // pushState and replaceState move the URL without loading anything, which
  // is how client-side routers navigate.
  var historyEntries = [{ state: null, url: pageURL }];
  var historyIndex = 0;
  var history = {
    scrollRestoration: "auto",
    get length() { return historyEntries.length; },
    get state() { return historyEntries[historyIndex].state; },
    pushState: function (state, _, url) {
      if (url !== undefined && url !== null) pageURL = host.resolve(String(url), pageURL);
      historyEntries = historyEntries.slice(0, historyIndex + 1);
      historyEntries.push({ state: cloneState(state), url: pageURL });
      historyIndex++;
    },
    replaceState: function (state, _, url) {
      if (url !== undefined && url !== null) pageURL = host.resolve(String(url), pageURL);
      historyEntries[historyIndex] = { state: cloneState(state), url: pageURL };
    },
    go: function (delta) {
      var next = historyIndex + (delta || 0);
      if (!delta || next < 0 || next >= historyEntries.length) return;
      historyIndex = next;
      pageURL = historyEntries[next].url;
      addTimer(function () {
        var e = new Event("popstate");
        e.state = historyEntries[historyIndex].state;
        g.dispatchEvent(e);
      }, 0, [], false);
    },
    back: function () { this.go(-1); },
    forward: function () { this.go(1); }
  };
  function cloneState(state) {
    try { return state === undefined ? null : JSON.parse(JSON.stringify(state)); } catch (e) { return state; }
  }

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
    if (typeof fn !== "function") return id;
    delay = Math.max(0, Number(delay) || 0);
    timers.push({ id: id, fn: fn, at: clock + delay, delay: delay, args: args, repeat: repeat, seq: id });
    return id;
  }
  function clearTimer(id) { timers = timers.filter(function (t) { return t.id !== id; }); }
  function nextTimer() {
    var best = null;
    for (var i = 0; i < timers.length; i++) {
      var t = timers[i];
      if (!best || t.at < best.at || (t.at === best.at && t.seq < best.seq)) best = t;
    }
    return best;
  }
  function runTimer(t) {
    clock = Math.max(clock, t.at);
    if (t.repeat) { t.at = clock + Math.max(t.delay, 1); t.seq = ++timerSeq; } else clearTimer(t.id);
    try { t.fn.apply(g, t.args); } catch (e) { report(e); }
  }

  function MessagePort() { EventTarget.call(this); this.onmessage = null; this.__peer = null; }
  MessagePort.prototype = Object.create(EventTarget.prototype);
  MessagePort.prototype.postMessage = function (data) {
    var peer = this.__peer;
    addTimer(function () {
      var e = new Event("message");
      e.data = data;
      e.ports = [];
      peer.dispatchEvent(e);
    }, 0, [], false);
  };
  MessagePort.prototype.start = noop;
  MessagePort.prototype.close = noop;
  function MessageChannel() {
    this.port1 = new MessagePort();
    this.port2 = new MessagePort();
    this.port1.__peer = this.port2;
    this.port2.__peer = this.port1;
  }

  function AbortSignal() { EventTarget.call(this); this.aborted = false; this.reason = undefined; this.onabort = null; }
  AbortSignal.prototype = Object.create(EventTarget.prototype);
  AbortSignal.prototype.throwIfAborted = function () { if (this.aborted) throw this.reason; };
  function abortSignal(signal, reason) {
    if (signal.aborted) return;
    signal.aborted = true;
    signal.reason = reason === undefined ? abortError() : reason;
    signal.dispatchEvent(new Event("abort"));
  }
  AbortSignal.abort = function (reason) { var s = new AbortSignal(); abortSignal(s, reason); return s; };
  AbortSignal.timeout = function (ms) { var s = new AbortSignal(); addTimer(function () { abortSignal(s); }, ms, [], false); return s; };
  AbortSignal.any = function (signals) {
    var s = new AbortSignal();
    signals.forEach(function (o) { if (o.aborted) abortSignal(s, o.reason); else o.addEventListener("abort", function () { abortSignal(s, o.reason); }); });
    return s;
  };
  function AbortController() { this.signal = new AbortSignal(); }
  AbortController.prototype.abort = function (reason) { abortSignal(this.signal, reason); };
  function DOMException(message, name) { this.message = String(message || ""); this.name = name || "Error"; }
  DOMException.prototype = Object.create(Error.prototype);
  DOMException.prototype.constructor = DOMException;
  function abortError() { return new DOMException("The operation was aborted.", "AbortError"); }

  function TextEncoder() {}
  TextEncoder.prototype.encoding = "utf-8";
  TextEncoder.prototype.encode = function (s) { return new Uint8Array(host.utf8Encode(String(s === undefined ? "" : s))); };
  function TextDecoder(label) { this.encoding = label || "utf-8"; }
  TextDecoder.prototype.decode = function (b) {
    if (b === undefined) return "";
    return host.utf8Decode(bufferOf(b));
  };
  function bufferOf(data) {
    if (data instanceof ArrayBuffer) return data;
    return data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
  }

  function Blob(parts, options) {
    this.__text = (parts || []).map(function (p) {
      if (typeof p === "string") return p;
      if (p instanceof Blob) return p.__text;
      try { return host.utf8Decode(bufferOf(p)); } catch (e) { return String(p); }
    }).join("");
    this.type = options && options.type ? String(options.type) : "";
    this.size = this.__text.length;
  }
  Blob.prototype.text = function () { return Promise.resolve(this.__text); };
  Blob.prototype.arrayBuffer = function () { return Promise.resolve(host.utf8Encode(this.__text)); };
  Blob.prototype.slice = function (a, b) { return new Blob([this.__text.slice(a, b)], { type: this.type }); };
  function File(parts, name, options) { Blob.call(this, parts, options); this.name = name; this.lastModified = Date.now(); }
  File.prototype = Object.create(Blob.prototype);
  function FormData(form) {
    this.__pairs = [];
    var self = this;
    if (form && form.elements) form.elements.forEach(function (el) { if (el.name && !el.disabled) self.append(el.name, el.value); });
  }
  ["append", "get", "getAll", "has", "set", "delete", "forEach", "entries", "keys", "values"].forEach(function (k) {
    FormData.prototype[k] = URLSearchParams.prototype[k];
  });
  FormData.prototype[Symbol.iterator] = URLSearchParams.prototype.entries;

  function Headers(init) {
    this.__pairs = [];
    if (!init) return;
    var self = this;
    if (init instanceof Headers) init.forEach(function (v, k) { self.append(k, v); });
    else if (Array.isArray(init)) init.forEach(function (p) { self.append(p[0], p[1]); });
    else Object.keys(init).forEach(function (k) { self.append(k, init[k]); });
  }
  Headers.prototype.append = function (k, v) { this.__pairs.push([String(k).toLowerCase(), String(v)]); };
  Headers.prototype.set = function (k, v) { this["delete"](k); this.append(k, v); };
  Headers.prototype.get = function (k) {
    k = String(k).toLowerCase();
    var values = this.__pairs.filter(function (p) { return p[0] === k; }).map(function (p) { return p[1]; });
    return values.length ? values.join(", ") : null;
  };
  Headers.prototype.has = function (k) { return this.get(k) !== null; };
  Headers.prototype["delete"] = function (k) { k = String(k).toLowerCase(); this.__pairs = this.__pairs.filter(function (p) { return p[0] !== k; }); };
  Headers.prototype.forEach = function (fn, self) { var h = this; this.__pairs.forEach(function (p) { fn.call(self, p[1], p[0], h); }); };
  Headers.prototype.entries = function () { return this.__pairs.map(function (p) { return p.slice(); })[Symbol.iterator](); };
  Headers.prototype.keys = function () { return this.__pairs.map(function (p) { return p[0]; })[Symbol.iterator](); };
  Headers.prototype.values = function () { return this.__pairs.map(function (p) { return p[1]; })[Symbol.iterator](); };
  Headers.prototype[Symbol.iterator] = Headers.prototype.entries;

  function bodyText(body) {
    if (body === undefined || body === null) return "";
    if (typeof body === "string") return body;
    if (body instanceof URLSearchParams) return body.toString();
    if (body instanceof Blob) return body.__text;
    if (body instanceof FormData) return URLSearchParams.prototype.toString.call(body);
    if (body instanceof ArrayBuffer || ArrayBuffer.isView(body)) return host.utf8Decode(bufferOf(body));
    return String(body);
  }
  function Request(input, init) {
    init = init || {};
    if (input instanceof Request) {
      this.url = input.url;
      this.method = init.method ? String(init.method).toUpperCase() : input.method;
      this.headers = new Headers(init.headers || input.headers);
      this.__body = init.body !== undefined ? bodyText(init.body) : input.__body;
      this.signal = init.signal || input.signal;
    } else {
      this.url = host.resolve(String(input), pageURL);
      this.method = String(init.method || "GET").toUpperCase();
      this.headers = new Headers(init.headers);
      this.__body = bodyText(init.body);
      this.signal = init.signal || null;
    }
    if (init.body instanceof URLSearchParams && !this.headers.has("content-type")) {
      this.headers.set("content-type", "application/x-www-form-urlencoded;charset=UTF-8");
    }
    this.credentials = init.credentials || "same-origin";
    this.mode = init.mode || "cors";
    this.cache = init.cache || "default";
    this.redirect = init.redirect || "follow";
  }
  Request.prototype.clone = function () { return new Request(this); };
  function Response(body, init) {
    init = init || {};
    this.status = init.status === undefined ? 200 : init.status;
    this.statusText = init.statusText || "";
    this.ok = this.status >= 200 && this.status < 300;
    this.headers = init.headers instanceof Headers ? init.headers : new Headers(init.headers);
    this.url = init.url || "";
    this.type = "basic";
    this.redirected = false;
    this.bodyUsed = false;
    this.__body = bodyText(body);
  }
  Response.prototype.text = function () { this.bodyUsed = true; return Promise.resolve(this.__body); };
  Response.prototype.json = function () { var self = this; this.bodyUsed = true; return Promise.resolve().then(function () { return JSON.parse(self.__body); }); };
  Response.prototype.arrayBuffer = function () { this.bodyUsed = true; return Promise.resolve(host.utf8Encode(this.__body)); };
  Response.prototype.blob = function () { this.bodyUsed = true; return Promise.resolve(new Blob([this.__body], { type: this.headers.get("content-type") || "" })); };
  Response.prototype.formData = function () { return Promise.reject(new TypeError("formData is not supported")); };
  Response.prototype.clone = function () {
    return new Response(this.__body, { status: this.status, statusText: this.statusText, headers: new Headers(this.headers), url: this.url });
  };
  Response.json = function (data, init) {
    var r = new Response(JSON.stringify(data), init);
    if (!r.headers.has("content-type")) r.headers.set("content-type", "application/json");
    return r;
  };
  Response.error = function () { return new Response("", { status: 0 }); };

  function fetch(input, init) {
    var request = new Request(input, init);
    return new Promise(function (resolve, reject) {
      if (!online) { reject(new TypeError("Failed to fetch")); return; }
      if (request.signal && request.signal.aborted) { reject(request.signal.reason); return; }
      netQueue.push({ fetch: request, resolve: resolve, reject: reject });
    });
  }
  function performFetch(job) {
    var request = job.fetch;
    if (request.signal && request.signal.aborted) { job.reject(request.signal.reason); return; }
    var raw = host.request(request.method, request.url, JSON.stringify(request.headers.__pairs), request.__body);
    if (!raw) { job.reject(new TypeError("Failed to fetch")); return; }
    var r = JSON.parse(raw);
    var response = new Response(r.body, { status: r.status, statusText: r.statusText, headers: r.headers, url: r.url });
    response.redirected = r.url !== request.url;
    job.resolve(response);
  }

  function XMLHttpRequest() {
    EventTarget.call(this);
    this.readyState = 0;
    this.status = 0;
    this.statusText = "";
    this.responseText = "";
    this.response = "";
    this.responseType = "";
    this.responseURL = "";
    this.responseXML = null;
    this.timeout = 0;
    this.withCredentials = false;
    this.upload = new EventTarget();
    this.__headers = [];
    this.__response = null;
  }
  XMLHttpRequest.UNSENT = 0;
  XMLHttpRequest.OPENED = 1;
  XMLHttpRequest.HEADERS_RECEIVED = 2;
  XMLHttpRequest.LOADING = 3;
  XMLHttpRequest.DONE = 4;
  XMLHttpRequest.prototype = Object.create(EventTarget.prototype);
  XMLHttpRequest.prototype.constructor = XMLHttpRequest;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__method = String(method).toUpperCase();
    this.__url = host.resolve(String(url), pageURL);
    this.readyState = 1;
    this.__aborted = false;
  };
  XMLHttpRequest.prototype.setRequestHeader = function (k, v) { this.__headers.push([String(k).toLowerCase(), String(v)]); };
  XMLHttpRequest.prototype.overrideMimeType = noop;
  XMLHttpRequest.prototype.send = function (body) {
    if (!online) { var self = this; addTimer(function () { finishXHR(self, null); }, 0, [], false); return; }
    netQueue.push({ xhr: this, body: bodyText(body) });
  };
  XMLHttpRequest.prototype.abort = function () { this.__aborted = true; this.readyState = 0; };
  XMLHttpRequest.prototype.getResponseHeader = function (k) {
    if (!this.__response) return null;
    k = String(k).toLowerCase();
    var values = this.__response.headers.filter(function (p) { return p[0].toLowerCase() === k; }).map(function (p) { return p[1]; });
    return values.length ? values.join(", ") : null;
  };
  XMLHttpRequest.prototype.getAllResponseHeaders = function () {
    if (!this.__response) return "";
    return this.__response.headers.map(function (p) { return p[0].toLowerCase() + ": " + p[1]; }).join("\r\n");
  };
  function finishXHR(xhr, r) {
    if (xhr.__aborted) return;
    var fire = function (type) { xhr.dispatchEvent(new Event(type)); };
    if (!r) {
      xhr.readyState = 4;
      fire("readystatechange");
      fire("error");
      fire("loadend");
      return;
    }
    xhr.__response = r;
    xhr.status = r.status;
    xhr.statusText = r.statusText;
    xhr.responseURL = r.url;
    xhr.responseText = r.body;
    xhr.response = r.body;
    if (xhr.responseType === "json") { try { xhr.response = JSON.parse(r.body); } catch (e) { xhr.response = null; } }
    else if (xhr.responseType === "arraybuffer") xhr.response = host.utf8Encode(r.body);
    else if (xhr.responseType === "blob") xhr.response = new Blob([r.body]);
    xhr.readyState = 2;
    fire("readystatechange");
    xhr.readyState = 4;
    fire("readystatechange");
    fire("load");
    fire("loadend");
  }
  function performXHR(job) {
    var xhr = job.xhr;
    if (xhr.__aborted) return;
    var raw = host.request(xhr.__method, xhr.__url, JSON.stringify(xhr.__headers), job.body);
    finishXHR(xhr, raw ? JSON.parse(raw) : null);
  }

  // Scripts and stylesheets inserted after load behave as they would in a
  // browser: a script is fetched and run, then fires load; a stylesheet or
  // preload just fires load, since nothing here renders.
  function connected(node) {
    if (node.nodeType !== 1 || !document.contains(node)) return;
    var all = [node].concat(descendants(node));
    all.forEach(function (el) {
      if (el.localName === "script" && !el.__started) {
        var desc = scriptDescriptor(el);
        if (!desc) return;
        el.__started = true;
        if (!desc.src) { runDescriptor(desc); return; }
        netQueue.push({ script: desc });
      } else if (el.localName === "link" && !el.__started) {
        el.__started = true;
        addTimer(function () { el.dispatchEvent(new Event("load")); }, 0, [], false);
      } else if (el.localName === "img" && !el.__started) {
        el.__started = true;
        addTimer(function () { el.dispatchEvent(new Event(online ? "load" : "error")); }, 0, [], false);
      }
    });
  }

  function scriptDescriptor(el) {
    var type = (el.getAttribute("type") || "").toLowerCase().split(";")[0].trim();
    var module = type === "module";
    if (!module && type !== "" && !/^(text|application)\/(x-)?(java|ecma)script$/.test(type)) return null;
    if (!module && el.hasAttribute("nomodule")) return null;
    var src = el.getAttribute("src");
    return {
      el: el,
      module: module,
      src: src ? host.resolve(src, pageURL) : "",
      code: src ? "" : el.textContent,
      deferred: module || (!!src && (el.hasAttribute("defer") || el.hasAttribute("async")))
    };
  }

  var modules = {};
  var inlineModules = 0;
  function requireModule(spec, base) {
    var url = host.resolve(String(spec), base);
    var record = modules[url];
    if (record) return record.exports;
    var fn = host.loadModule(url);
    if (!fn) throw new Error("Cannot load module " + url);
    return evaluateModule(fn, url);
  }
  function evaluateModule(fn, url) {
    var record = { exports: {} };
    modules[url] = record;
    var meta = { url: url, env: {}, resolve: function (s) { return host.resolve(String(s), url); } };
    fn.call(undefined, record.exports, function (s) { return requireModule(s, url); }, record, meta);
    return record.exports;
  }

  function runDescriptor(desc) {
    var el = desc.el;
    document.currentScript = desc.module ? null : el;
    var ok = true;
    try {
      if (desc.module) {
        if (!online) return;
        if (desc.src) requireModule(desc.src, pageURL);
        else {
          var name = pageURL + "#inline-module-" + (++inlineModules);
          var fn = host.compileModule(desc.code, name);
          if (fn) evaluateModule(fn, name);
        }
      } else if (desc.src) {
        if (!online) return;
        ok = host.loadScript(desc.src);
      } else {
        host.runScript(desc.code, pageURL + "#inline");
      }
    } catch (e) {
      report(e);
      ok = false;
    } finally {
      document.currentScript = null;
    }
    if (desc.src) {
      addTimer(function () { el.dispatchEvent(new Event(ok ? "load" : "error")); }, 0, [], false);
    }
  }

  // Streams (EventSource, WebSocket) stay connecting forever: nothing is sent
  // or received, which is how a page sees a slow live feed, not a crash.
  function Stream(url) {
    EventTarget.call(this);
    this.url = String(url);
    this.readyState = 0;
    this.withCredentials = false;
    this.protocol = "";
    this.extensions = "";
    this.bufferedAmount = 0;
    this.binaryType = "blob";
  }
  Stream.prototype = Object.create(EventTarget.prototype);
  Stream.prototype.close = function () { this.readyState = 2; };
  Stream.prototype.send = noop;
  Stream.CONNECTING = Stream.prototype.CONNECTING = 0;
  Stream.OPEN = Stream.prototype.OPEN = 1;
  Stream.CLOSED = Stream.prototype.CLOSED = 2;

  function TreeObserver() {}
  TreeObserver.prototype.observe = noop;
  TreeObserver.prototype.unobserve = noop;
  TreeObserver.prototype.disconnect = noop;
  TreeObserver.prototype.takeRecords = function () { return []; };

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
    clipboard: { writeText: function () { return Promise.resolve(); }, readText: function () { return Promise.resolve(""); } },
    permissions: { query: function () { return Promise.resolve({ state: "prompt", addEventListener: noop }); } },
    serviceWorker: undefined,
    javaEnabled: function () { return false; },
    sendBeacon: function () { return true; },
    vibrate: function () { return false; }
  };

  var selection = {
    rangeCount: 0, isCollapsed: true, type: "None", anchorNode: null, focusNode: null,
    removeAllRanges: noop, addRange: noop, collapse: noop, getRangeAt: function () { return document.createRange(); },
    toString: function () { return ""; }
  };

  var customRegistry = {};
  var customElements = {
    define: function (name, ctor) { customRegistry[name] = ctor; },
    get: function (name) { return customRegistry[name]; },
    whenDefined: function () { return Promise.resolve(); },
    upgrade: noop
  };

  var globals = {
    window: g, self: g, top: g, parent: g, frames: g, globalThis: g,
    document: document, navigator: navigator, crypto: crypto, history: history,
    Event: Event, CustomEvent: CustomEvent,
    EventTarget: EventTarget, Node: Node, Element: Element, CharacterData: CharacterData, Text: Text, Comment: Comment,
    Document: Document, HTMLDocument: Document, DocumentFragment: DocumentFragment, ShadowRoot: DocumentFragment,
    URL: URL, URLSearchParams: URLSearchParams, TextEncoder: TextEncoder, TextDecoder: TextDecoder,
    Blob: Blob, File: File, FormData: FormData, Headers: Headers, Request: Request, Response: Response,
    AbortController: AbortController, AbortSignal: AbortSignal, DOMException: DOMException,
    MessageChannel: MessageChannel, MessagePort: MessagePort,
    Storage: Storage, localStorage: new Storage(), sessionStorage: new Storage(),
    MutationObserver: TreeObserver, IntersectionObserver: TreeObserver, ResizeObserver: TreeObserver, PerformanceObserver: TreeObserver,
    customElements: customElements,
    screen: { width: 1920, height: 1080, availWidth: 1920, availHeight: 1040, colorDepth: 24, pixelDepth: 24, orientation: { type: "landscape-primary", angle: 0, addEventListener: noop } },
    innerWidth: 1920, innerHeight: 960, outerWidth: 1920, outerHeight: 1040, devicePixelRatio: 1,
    scrollX: 0, scrollY: 0, pageXOffset: 0, pageYOffset: 0, isSecureContext: true, name: "", closed: false,
    origin: host.parseURL(pageURL, "").origin,
    console: { log: noop, info: noop, warn: noop, error: function () {
      report(Array.prototype.map.call(arguments, function (a) { return a && a.stack ? a.stack : a && typeof a === "object" ? safeJSON(a) : String(a); }).join(" "));
    }, debug: noop, trace: noop, dir: noop, table: noop, group: noop, groupCollapsed: noop, groupEnd: noop, time: noop, timeEnd: noop, assert: noop, count: noop },
    performance: {
      now: function () { return clock; }, timeOrigin: Date.now(), mark: noop, measure: noop, clearMarks: noop, clearMeasures: noop,
      getEntriesByType: function () { return []; }, getEntriesByName: function () { return []; }, getEntries: function () { return []; },
      timing: { navigationStart: Date.now() }
    },
    setTimeout: function (fn, delay) { return addTimer(fn, delay, Array.prototype.slice.call(arguments, 2), false); },
    setInterval: function (fn, delay) { return addTimer(fn, delay, Array.prototype.slice.call(arguments, 2), true); },
    clearTimeout: clearTimer, clearInterval: clearTimer,
    requestAnimationFrame: function (fn) { return addTimer(function () { fn(clock); }, 16, [], false); },
    cancelAnimationFrame: clearTimer,
    requestIdleCallback: function (fn) { return addTimer(function () { fn({ didTimeout: false, timeRemaining: function () { return 50; } }); }, 1, [], false); },
    cancelIdleCallback: clearTimer,
    queueMicrotask: function (fn) { Promise.resolve().then(fn); },
    structuredClone: function (v) { return v === undefined ? undefined : JSON.parse(JSON.stringify(v)); },
    atob: function (s) { return host.atob(String(s)); },
    btoa: function (s) { return host.btoa(String(s)); },
    getComputedStyle: function (el) { return el && el.style ? el.style : Object.create(styleProto); },
    getSelection: function () { return selection; },
    alert: noop, confirm: function () { return false; }, prompt: function () { return null; }, print: noop,
    focus: noop, blur: noop, scrollTo: noop, scroll: noop, scrollBy: noop, postMessage: noop, open: function () { return null; }, close: noop,
    fetch: fetch,
    XMLHttpRequest: XMLHttpRequest,
    Image: function (w, h) { var img = createElement("img"); if (w) img.setAttribute("width", w); if (h) img.setAttribute("height", h); return img; },
    Worker: function () { throw new TypeError("Worker is not available"); },
    WebSocket: Stream,
    EventSource: Stream
  };
  Object.keys(interfaceGlobals).forEach(function (k) { globals[k] = interfaceGlobals[k]; });
  // Event interfaces are named in instanceof checks and feature tests far
  // more often than they are constructed; each copies its init dictionary.
  ["UIEvent", "MouseEvent", "KeyboardEvent", "FocusEvent", "PointerEvent", "InputEvent", "ErrorEvent",
   "PopStateEvent", "HashChangeEvent", "MediaQueryListEvent", "AnimationEvent", "TransitionEvent", "WheelEvent",
   "TouchEvent", "DragEvent", "ClipboardEvent", "CompositionEvent", "ProgressEvent", "MessageEvent", "StorageEvent",
   "PageTransitionEvent", "SubmitEvent", "BeforeUnloadEvent", "PromiseRejectionEvent", "SecurityPolicyViolationEvent",
   "ToggleEvent", "FormDataEvent"].forEach(function (name) {
    var C = function (type, init) {
      Event.call(this, type, init);
      if (init) for (var k in init) if (!(k in this)) this[k] = init[k];
    };
    C.prototype = Object.create(Event.prototype);
    C.prototype.constructor = C;
    Object.defineProperty(C, "name", { value: name });
    globals[name] = C;
  });
  function MediaQueryList(query) { EventTarget.call(this); this.media = String(query); this.matches = false; this.onchange = null; }
  MediaQueryList.prototype = Object.create(EventTarget.prototype);
  MediaQueryList.prototype.addListener = noop;
  MediaQueryList.prototype.removeListener = noop;
  globals.MediaQueryList = MediaQueryList;
  globals.matchMedia = function (q) { return new MediaQueryList(q); };
  function DOMRect(x, y, w, h) {
    this.x = this.left = x || 0;
    this.y = this.top = y || 0;
    this.width = w || 0;
    this.height = h || 0;
    this.right = this.x + this.width;
    this.bottom = this.y + this.height;
  }
  globals.DOMRect = globals.DOMRectReadOnly = DOMRect;
  globals.DOMParser = function () {};
  globals.DOMParser.prototype.parseFromString = function (markup) {
    var doc = new Document();
    doc.readyState = "complete";
    doc.appendChild(build(JSON.parse(host.parseDocument(String(markup)))));
    return doc;
  };
  globals.XMLSerializer = function () {};
  globals.XMLSerializer.prototype.serializeToString = function (node) { return serialize(node); };
  globals.NodeFilter = {
    SHOW_ALL: 0xffffffff, SHOW_ELEMENT: 1, SHOW_TEXT: 4, SHOW_COMMENT: 128,
    FILTER_ACCEPT: 1, FILTER_REJECT: 2, FILTER_SKIP: 3
  };
  globals.CSS = {
    supports: function () { return false; },
    escape: function (s) { return String(s).replace(/([^\w-])/g, "\\$1"); }
  };
  globals.NodeList = globals.HTMLCollection = globals.NamedNodeMap = Array;
  globals.HTMLUnknownElement = HTMLElement;
  globals.Attr = function () {};
  globals.Range = function () { return document.createRange(); };
  globals.Selection = function () {};
  globals.CSSStyleDeclaration = function () {};
  globals.CSSStyleSheet = function () { this.cssRules = []; };
  globals.CSSStyleSheet.prototype.insertRule = function () { return 0; };
  globals.CSSStyleSheet.prototype.replaceSync = noop;
  globals.CSSStyleSheet.prototype.replace = function () { return Promise.resolve(this); };
  // Frameworks tell a browser from a server by the global's shape (Ember
  // checks self.constructor === Window), so the global is a Window.
  function Window() { throw new TypeError("Illegal constructor"); }
  Window.prototype = Object.create(EventTarget.prototype);
  Window.prototype.constructor = Window;
  globals.Window = Window;
  Object.setPrototypeOf(g, Window.prototype);
  Object.keys(globals).forEach(function (k) {
    Object.defineProperty(g, k, { value: globals[k], writable: true, configurable: true });
  });
  Object.defineProperty(g, "location", { get: function () { return location; }, set: function (v) { location.href = v; }, configurable: true });


  var initial = document.getElementsByTagName("script").map(function (el) {
    el.__started = true;
    return scriptDescriptor(el);
  }).filter(Boolean);
  booted = true;

  function fire(phase) {
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
  }

  Object.defineProperty(g, "__page", {
    value: {
      // boot queues the document's scripts in browser order: parser-blocking
      // classic scripts, then deferred and module scripts, then the load events.
      boot: function () {
        initial.forEach(function (d) { if (!d.deferred) bootQueue.push({ script: d }); });
        initial.forEach(function (d) { if (d.deferred) bootQueue.push({ script: d }); });
        bootQueue.push({ event: "DOMContentLoaded" }, { event: "load" });
      },
      // pump does one unit of work and reports whether there was any: the next
      // boot step, a due timer, the queued network requests, or the next timer
      // within the idle horizon.
      pump: function () {
        if (bootQueue.length) {
          var item = bootQueue.shift();
          if (item.event) fire(item.event);
          else runDescriptor(item.script);
          return true;
        }
        var t = nextTimer();
        if (t && t.at <= clock) { runTimer(t); return true; }
        if (netQueue.length) {
          var jobs = netQueue.splice(0);
          jobs.forEach(function (job) {
            try {
              if (job.fetch) performFetch(job);
              else if (job.xhr) performXHR(job);
              else runDescriptor(job.script);
            } catch (e) { report(e); }
          });
          return true;
        }
        if (t && t.at - clock <= idleHorizon && t.at <= maxClock) { runTimer(t); return true; }
        return false;
      },
      html: function () { return "<!DOCTYPE html>" + serialize(document.documentElement); },
      url: function () { return pageURL; }
    }
  });
})(this);
