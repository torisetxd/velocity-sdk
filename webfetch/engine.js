// Engine-level polyfills that run before the DOM prelude. Intl is missing
// from the engine and stood in for in English only: apps format dates,
// numbers and relative times while rendering, and the output here only needs
// to read naturally.
(function (g) {
  "use strict";
  if (g.Intl) return;

  var months = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];
  var days = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
  var locale = "en-US";

  function locales() { return [locale]; }
  function pad(n, width) { var s = String(n); while (s.length < width) s = "0" + s; return s; }
  function resolved(extra) {
    var out = { locale: locale, calendar: "gregory", numberingSystem: "latn", timeZone: "UTC" };
    for (var k in extra) if (extra[k] !== undefined) out[k] = extra[k];
    return out;
  }
  function literalParts(text) { return [{ type: "literal", value: text }]; }

  function DateTimeFormat(_, options) {
    if (!(this instanceof DateTimeFormat)) return new DateTimeFormat(_, options);
    this.__o = options || {};
  }
  DateTimeFormat.prototype.format = function (date) {
    var d = date === undefined ? new Date() : new Date(date);
    if (isNaN(d.getTime())) throw new RangeError("Invalid time value");
    var o = this.__o;
    var utc = o.timeZone === "UTC";
    var Y = utc ? d.getUTCFullYear() : d.getFullYear();
    var M = utc ? d.getUTCMonth() : d.getMonth();
    var D = utc ? d.getUTCDate() : d.getDate();
    var W = utc ? d.getUTCDay() : d.getDay();
    var h = utc ? d.getUTCHours() : d.getHours();
    var m = utc ? d.getUTCMinutes() : d.getMinutes();
    var s = utc ? d.getUTCSeconds() : d.getSeconds();
    var parts = [];
    var dateStyle = o.dateStyle;
    var timeStyle = o.timeStyle;
    var hasDate = dateStyle || o.year || o.month || o.day || o.weekday;
    var hasTime = timeStyle || o.hour || o.minute || o.second;
    if (!hasDate && !hasTime) hasDate = true;
    if (hasDate) {
      var month = o.month || (dateStyle === "long" || dateStyle === "full" ? "long" : dateStyle === "medium" ? "short" : dateStyle ? "numeric" : o.year || o.day ? undefined : "numeric");
      if (!o.month && !dateStyle && (o.year || o.day)) month = undefined;
      var weekday = o.weekday || (dateStyle === "full" ? "long" : undefined);
      var text;
      if (month === "long" || month === "short") {
        var name = month === "long" ? months[M] : months[M].slice(0, 3);
        text = name + (o.day || dateStyle || !o.month ? " " + D : "") + (o.year || dateStyle || !o.month ? ", " + Y : "");
      } else if (month === "2-digit" || month === "numeric" || !month) {
        var bits = [];
        if (month || dateStyle || (!o.year && !o.day)) bits.push(month === "2-digit" ? pad(M + 1, 2) : M + 1);
        if (o.day || dateStyle || month) bits.push(o.day === "2-digit" ? pad(D, 2) : D);
        if (o.year || dateStyle || month) bits.push(o.year === "2-digit" ? pad(Y % 100, 2) : Y);
        text = bits.join("/");
      }
      if (weekday) text = (weekday === "long" ? days[W] : days[W].slice(0, 3)) + (text ? ", " + text : "");
      parts.push(text);
    }
    if (hasTime) {
      var h12 = o.hour12 !== false && o.hourCycle !== "h23";
      var hh = h12 ? (h % 12 || 12) : pad(h, 2);
      var t = hh + ":" + pad(m, 2);
      if (o.second || timeStyle === "medium" || timeStyle === "long") t += ":" + pad(s, 2);
      if (h12) t += h < 12 ? " AM" : " PM";
      if (o.timeZoneName || timeStyle === "long" || timeStyle === "full") t += utc ? " UTC" : "";
      parts.push(t);
    }
    return parts.join(", ");
  };
  DateTimeFormat.prototype.formatToParts = function (date) { return literalParts(this.format(date)); };
  DateTimeFormat.prototype.formatRange = function (a, b) { return this.format(a) + " – " + this.format(b); };
  DateTimeFormat.prototype.resolvedOptions = function () { return resolved(this.__o); };
  DateTimeFormat.supportedLocalesOf = locales;

  function group(intPart) { return intPart.replace(/\B(?=(\d{3})+(?!\d))/g, ","); }
  function NumberFormat(_, options) {
    if (!(this instanceof NumberFormat)) return new NumberFormat(_, options);
    this.__o = options || {};
  }
  NumberFormat.prototype.format = function (value) {
    var o = this.__o;
    var n = Number(value);
    if (!isFinite(n)) return isNaN(n) ? "NaN" : (n < 0 ? "-∞" : "∞");
    var suffix = "";
    if (o.style === "percent") { n *= 100; suffix = "%"; }
    if (o.notation === "compact") {
      var abs = Math.abs(n);
      var units = [[1e12, "T"], [1e9, "B"], [1e6, "M"], [1e3, "K"]];
      for (var i = 0; i < units.length; i++) {
        if (abs >= units[i][0]) { n = n / units[i][0]; suffix = units[i][1] + suffix; break; }
      }
      if (o.maximumFractionDigits === undefined) n = Math.round(n * 10) / 10;
    }
    var max = o.maximumFractionDigits !== undefined ? o.maximumFractionDigits : (o.style === "currency" ? 2 : o.style === "percent" ? 0 : 3);
    var min = o.minimumFractionDigits !== undefined ? o.minimumFractionDigits : (o.style === "currency" ? 2 : 0);
    if (max < min) max = min;
    var fixed = Math.abs(n).toFixed(max);
    var dot = fixed.indexOf(".");
    var intPart = dot < 0 ? fixed : fixed.slice(0, dot);
    var frac = dot < 0 ? "" : fixed.slice(dot + 1);
    while (frac.length > min && frac.charAt(frac.length - 1) === "0") frac = frac.slice(0, -1);
    var text = (o.useGrouping === false ? intPart : group(intPart)) + (frac ? "." + frac : "");
    if (o.style === "currency") text = (o.currency === "EUR" ? "€" : o.currency === "GBP" ? "£" : "$") + text;
    if (o.style === "unit" && o.unit) text += " " + o.unit;
    return (n < 0 ? "-" : "") + text + suffix;
  };
  NumberFormat.prototype.formatToParts = function (v) { return literalParts(this.format(v)); };
  NumberFormat.prototype.resolvedOptions = function () { return resolved(this.__o); };
  NumberFormat.supportedLocalesOf = locales;

  function PluralRules(_, options) {
    if (!(this instanceof PluralRules)) throw new TypeError("Constructor Intl.PluralRules requires 'new'");
    this.__o = options || {};
  }
  PluralRules.prototype.select = function (n) {
    n = Number(n);
    if (this.__o.type === "ordinal") {
      var t = n % 10, h = n % 100;
      if (t === 1 && h !== 11) return "one";
      if (t === 2 && h !== 12) return "two";
      if (t === 3 && h !== 13) return "few";
      return "other";
    }
    return n === 1 ? "one" : "other";
  };
  PluralRules.prototype.resolvedOptions = function () { return resolved({ pluralCategories: ["one", "other"], type: this.__o.type || "cardinal" }); };
  PluralRules.supportedLocalesOf = locales;

  var relativeWords = { day: ["yesterday", "today", "tomorrow"], year: ["last year", "this year", "next year"], month: ["last month", "this month", "next month"], week: ["last week", "this week", "next week"] };
  function RelativeTimeFormat(_, options) {
    if (!(this instanceof RelativeTimeFormat)) throw new TypeError("Constructor Intl.RelativeTimeFormat requires 'new'");
    this.__o = options || {};
  }
  RelativeTimeFormat.prototype.format = function (value, unit) {
    unit = String(unit).replace(/s$/, "");
    var n = Number(value);
    if (this.__o.numeric === "auto" && relativeWords[unit] && Math.abs(n) <= 1) return relativeWords[unit][n + 1];
    var abs = Math.abs(n);
    var label = abs + " " + unit + (abs === 1 ? "" : "s");
    return n < 0 || Object.is(n, -0) ? label + " ago" : "in " + label;
  };
  RelativeTimeFormat.prototype.formatToParts = function (v, u) { return literalParts(this.format(v, u)); };
  RelativeTimeFormat.prototype.resolvedOptions = function () { return resolved(this.__o); };
  RelativeTimeFormat.supportedLocalesOf = locales;

  function ListFormat(_, options) {
    if (!(this instanceof ListFormat)) throw new TypeError("Constructor Intl.ListFormat requires 'new'");
    this.__o = options || {};
  }
  ListFormat.prototype.format = function (list) {
    var items = Array.from(list || [], String);
    var word = this.__o.type === "disjunction" ? "or" : "and";
    if (this.__o.type === "unit") return items.join(", ");
    if (items.length < 2) return items.join("");
    if (items.length === 2) return items[0] + " " + word + " " + items[1];
    return items.slice(0, -1).join(", ") + ", " + word + " " + items[items.length - 1];
  };
  ListFormat.prototype.formatToParts = function (l) { return literalParts(this.format(l)); };
  ListFormat.supportedLocalesOf = locales;

  // compareText orders like a root-locale collator closely enough for
  // sorting: case-insensitively first, lowercase before uppercase on a tie,
  // and digit runs by value when numeric. It allocates nothing, since apps
  // sort thousands of items with it.
  function compareText(a, b, numeric, fold) {
    var i = 0, j = 0, tie = 0;
    while (i < a.length && j < b.length) {
      var ca = a.charCodeAt(i), cb = b.charCodeAt(j);
      if (numeric && ca >= 48 && ca <= 57 && cb >= 48 && cb <= 57) {
        var si = i, sj = j;
        while (i < a.length && a.charCodeAt(i) === 48) i++;
        while (j < b.length && b.charCodeAt(j) === 48) j++;
        var ei = i, ej = j;
        while (ei < a.length && a.charCodeAt(ei) >= 48 && a.charCodeAt(ei) <= 57) ei++;
        while (ej < b.length && b.charCodeAt(ej) >= 48 && b.charCodeAt(ej) <= 57) ej++;
        if (ei - i !== ej - j) return ei - i < ej - j ? -1 : 1;
        for (; i < ei; i++, j++) {
          var da = a.charCodeAt(i), db = b.charCodeAt(j);
          if (da !== db) return da < db ? -1 : 1;
        }
        if (!tie && (ei - si) !== (ej - sj)) tie = (ei - si) < (ej - sj) ? -1 : 1;
        i = ei; j = ej;
        continue;
      }
      if (ca !== cb) {
        var la = ca >= 65 && ca <= 90 ? ca + 32 : ca;
        var lb = cb >= 65 && cb <= 90 ? cb + 32 : cb;
        if (la !== lb) return la < lb ? -1 : 1;
        if (!tie) tie = ca > cb ? -1 : 1;
      }
      i++; j++;
    }
    if (i < a.length) return 1;
    if (j < b.length) return -1;
    return fold ? 0 : tie;
  }
  function Collator(_, options) {
    if (!(this instanceof Collator)) return new Collator(_, options);
    var o = this.__o = options || {};
    var numeric = !!o.numeric;
    var fold = o.sensitivity === "base" || o.sensitivity === "accent";
    this.compare = function (a, b) { return compareText(String(a), String(b), numeric, fold); };
  }
  Collator.prototype.resolvedOptions = function () { return resolved(this.__o); };
  Collator.supportedLocalesOf = locales;

  function Locale(tag) {
    if (!(this instanceof Locale)) throw new TypeError("Constructor Intl.Locale requires 'new'");
    var parts = String(tag || locale).split(/[-_]/);
    this.language = parts[0].toLowerCase();
    this.region = parts.length > 1 && /^[A-Za-z]{2}$|^\d{3}$/.test(parts[parts.length - 1]) ? parts[parts.length - 1].toUpperCase() : undefined;
    this.baseName = this.language + (this.region ? "-" + this.region : "");
    this.script = undefined;
    this.calendar = undefined;
    this.numberingSystem = undefined;
    this.hourCycle = undefined;
  }
  Locale.prototype.toString = function () { return this.baseName; };
  Locale.prototype.maximize = function () { return this; };
  Locale.prototype.minimize = function () { return this; };
  Locale.prototype.getTextInfo = function () { return { direction: "ltr" }; };
  Locale.prototype.getWeekInfo = function () { return { firstDay: 7, weekend: [6, 7], minimalDays: 1 }; };

  function Segmenter(_, options) {
    if (!(this instanceof Segmenter)) throw new TypeError("Constructor Intl.Segmenter requires 'new'");
    this.__o = options || {};
  }
  Segmenter.prototype.segment = function (input) {
    var text = String(input);
    var pieces = this.__o.granularity === "word" ? text.match(/\w+|\W+/g) || [] :
      this.__o.granularity === "sentence" ? text.match(/[^.!?]+[.!?]*\s*/g) || [] : Array.from(text);
    var index = 0;
    var segments = pieces.map(function (p) {
      var seg = { segment: p, index: index, input: text, isWordLike: /\w/.test(p) };
      index += p.length;
      return seg;
    });
    segments.containing = function (i) { return segments.filter(function (s) { return i >= s.index && i < s.index + s.segment.length; })[0]; };
    return segments;
  };

  function DisplayNames(_, options) {
    if (!(this instanceof DisplayNames)) throw new TypeError("Constructor Intl.DisplayNames requires 'new'");
    this.__o = options || {};
  }
  DisplayNames.prototype.of = function (code) { return String(code); };
  DisplayNames.supportedLocalesOf = locales;

  var Intl = {
    DateTimeFormat: DateTimeFormat,
    NumberFormat: NumberFormat,
    PluralRules: PluralRules,
    RelativeTimeFormat: RelativeTimeFormat,
    ListFormat: ListFormat,
    Collator: Collator,
    Locale: Locale,
    Segmenter: Segmenter,
    DisplayNames: DisplayNames,
    getCanonicalLocales: function (l) { return l === undefined ? [] : [].concat(l).map(String); },
    supportedValuesOf: function () { return []; }
  };
  Object.defineProperty(g, "Intl", { value: Intl, writable: true, configurable: true });

  var dateFormat = new DateTimeFormat();
  var dateTimeFormat = new DateTimeFormat(locale, { year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric", second: "numeric" });
  var timeFormat = new DateTimeFormat(locale, { hour: "numeric", minute: "numeric", second: "numeric" });
  Date.prototype.toLocaleDateString = function (_, o) { return (o ? new DateTimeFormat(_, o) : dateFormat).format(this); };
  Date.prototype.toLocaleString = function (_, o) { return (o ? new DateTimeFormat(_, o) : dateTimeFormat).format(this); };
  Date.prototype.toLocaleTimeString = function (_, o) { return (o ? new DateTimeFormat(_, o) : timeFormat).format(this); };
  Number.prototype.toLocaleString = function (_, o) { return new NumberFormat(_, o).format(this); };
  String.prototype.localeCompare = function (other, _, o) {
    return compareText(String(this), String(other), !!(o && o.numeric), !!(o && (o.sensitivity === "base" || o.sensitivity === "accent")));
  };
})(this);
