import crypto from "node:crypto";
import type { ScrubMode } from "./config.js";

const PiiClass = {
  None: 0,
  Email: 1,
  SSN: 2,
  Phone: 3,
  Card: 4,
  Generic: 5,
} as const;

type PiiClassValue = (typeof PiiClass)[keyof typeof PiiClass];

const ALLOWLISTED_ENUM_KEYS = new Set<string>([
  "type",
  "kind",
  "status",
  "currency",
  "op",
  "action",
  "code",
  "event",
  "method",
  "path",
  "topic",
  "driver",
  "sink",
  "id",
  "transaction_id",
  "subscription_id",
  "external_subscription_id",
  "organization_id",
  "plan_id",
]);

const GENERIC_SENSITIVE_KEY_TOKENS: readonly string[] = [
  "_tax_id_", "_iban_", "_routing_", "_account_number_",
  "_name_", "_names_", "_first_name_", "_last_name_", "_full_name_",
  "_address_", "_addresses_", "_street_", "_dob_", "_birth_",
  "_password_", "_passwords_", "_passwd_",
  "_secret_", "_secrets_", "_client_secret_", "_private_key_", "_access_key_",
  "_token_", "_tokens_", "_api_key_", "_apikey_",
  "_authorization_", "_auth_", "_bearer_",
  "_cookie_", "_cookies_", "_ip_", "_ip_address_", "_dsn_",
];

const MAX_JSON_SCRUB_DEPTH = 64;

/**
 * Deterministically scrubs PII from `data` according to `mode` and HMAC-SHA256 `key`.
 * JSON objects/arrays are scrubbed at leaf granularity while preserving exact
 * original object key order, numbers, and whitespace/formatting, matching Go `sdk/kavach-go/pii.go`
 * byte-for-byte. Non-JSON text is scrubbed inline.
 */
export function scrubBytes(mode: ScrubMode, key: Uint8Array, data: Uint8Array): Uint8Array {
  if (mode === "off" || data.length === 0) {
    return data;
  }
  let start = 0;
  while (start < data.length && isSpaceByte(data[start])) start++;
  if (start < data.length && (data[start] === 0x7b /* { */ || data[start] === 0x5b /* [ */)) {
    const res = scrubJSONPreservingOrder(data, mode, key);
    if (res.ok) {
      return res.changed ? res.out : data;
    }
  }
  const text = Buffer.from(data.buffer, data.byteOffset, data.byteLength).toString("utf8");
  const inline = scrubInlineText(text, key);
  if (inline.changed) {
    return new Uint8Array(Buffer.from(inline.out, "utf8"));
  }
  return data;
}

/** Deterministically scrubs a single string value associated with `keyName`. */
export function scrubString(mode: ScrubMode, key: Uint8Array, keyName: string, val: string): string {
  if (mode === "off" || val === "") {
    return val;
  }
  const cls = detectLeafClass(keyName, val, mode);
  if (cls !== PiiClass.None) {
    return tokenizeLeaf(val, cls, key);
  }
  const inline = scrubInlineText(val, key);
  return inline.changed ? inline.out : val;
}

/** Deterministically scrubs a headers record (Authorization, Cookie, X-Api-Key, inline PII, etc.). */
export function scrubHeaders(
  mode: ScrubMode,
  key: Uint8Array,
  headers: Record<string, string>,
): Record<string, string> {
  if (mode === "off") return headers;
  const entries = Object.entries(headers);
  if (entries.length === 0) return headers;
  const out: Record<string, string> = {};
  for (const [k, v] of entries) {
    out[k] = scrubString(mode, key, k, v);
  }
  return out;
}

class JsonScrubber {
  private readonly src: Uint8Array;
  private readonly mode: ScrubMode;
  private readonly key: Uint8Array;
  private pos = 0;
  private copied = 0;
  private chunks: Uint8Array[] = [];
  changed = false;

  constructor(src: Uint8Array, mode: ScrubMode, key: Uint8Array) {
    this.src = src;
    this.mode = mode;
    this.key = key;
  }

  run(): { out: Uint8Array; changed: boolean; ok: boolean } {
    this.skipWS();
    if (!this.scanValue("", 0)) {
      return { out: this.src, changed: false, ok: false };
    }
    this.skipWS();
    if (this.pos !== this.src.length) {
      return { out: this.src, changed: false, ok: false };
    }
    if (!this.changed) {
      return { out: this.src, changed: false, ok: true };
    }
    if (this.copied < this.src.length) {
      this.chunks.push(this.src.subarray(this.copied));
    }
    let total = 0;
    for (const c of this.chunks) total += c.length;
    const out = new Uint8Array(total);
    let off = 0;
    for (const c of this.chunks) {
      out.set(c, off);
      off += c.length;
    }
    return { out, changed: true, ok: true };
  }

  private skipWS(): void {
    while (this.pos < this.src.length && isSpaceByte(this.src[this.pos])) {
      this.pos++;
    }
  }

  private scanValue(keyName: string, depth: number): boolean {
    if (depth > MAX_JSON_SCRUB_DEPTH || this.pos >= this.src.length) {
      return false;
    }
    const c = this.src[this.pos];
    if (c === 0x7b /* { */) return this.scanObject(depth + 1);
    if (c === 0x5b /* [ */) return this.scanArray(keyName, depth + 1);
    if (c === 0x22 /* " */) return this.scanStringLeaf(keyName);
    if (c === 0x74 /* t */) {
      if (this.matchAscii("true")) {
        this.pos += 4;
        return true;
      }
      return false;
    }
    if (c === 0x66 /* f */) {
      if (this.matchAscii("false")) {
        this.pos += 5;
        return true;
      }
      return false;
    }
    if (c === 0x6e /* n */) {
      if (this.matchAscii("null")) {
        this.pos += 4;
        return true;
      }
      return false;
    }
    return this.scanNumber();
  }

  private matchAscii(lit: string): boolean {
    if (this.pos + lit.length > this.src.length) return false;
    for (let i = 0; i < lit.length; i++) {
      if (this.src[this.pos + i] !== lit.charCodeAt(i)) return false;
    }
    return true;
  }

  private scanObject(depth: number): boolean {
    this.pos++; // '{'
    this.skipWS();
    if (this.pos < this.src.length && this.src[this.pos] === 0x7d /* } */) {
      this.pos++;
      return true;
    }
    for (;;) {
      this.skipWS();
      if (this.pos >= this.src.length || this.src[this.pos] !== 0x22 /* " */) {
        return false;
      }
      const keyStart = this.pos;
      if (!this.advanceString()) return false;
      let fieldKey: string;
      try {
        const raw = Buffer.from(this.src.buffer, this.src.byteOffset + keyStart, this.pos - keyStart).toString("utf8");
        const parsed: unknown = JSON.parse(raw);
        if (typeof parsed !== "string") return false;
        fieldKey = parsed;
      } catch {
        return false;
      }
      this.skipWS();
      if (this.pos >= this.src.length || this.src[this.pos] !== 0x3a /* : */) {
        return false;
      }
      this.pos++;
      this.skipWS();
      if (!this.scanValue(fieldKey, depth)) return false;
      this.skipWS();
      if (this.pos >= this.src.length) return false;
      if (this.src[this.pos] === 0x2c /* , */) {
        this.pos++;
        continue;
      }
      if (this.src[this.pos] === 0x7d /* } */) {
        this.pos++;
        return true;
      }
      return false;
    }
  }

  private scanArray(keyName: string, depth: number): boolean {
    this.pos++; // '['
    this.skipWS();
    if (this.pos < this.src.length && this.src[this.pos] === 0x5d /* ] */) {
      this.pos++;
      return true;
    }
    for (;;) {
      this.skipWS();
      if (!this.scanValue(keyName, depth)) return false;
      this.skipWS();
      if (this.pos >= this.src.length) return false;
      if (this.src[this.pos] === 0x2c /* , */) {
        this.pos++;
        continue;
      }
      if (this.src[this.pos] === 0x5d /* ] */) {
        this.pos++;
        return true;
      }
      return false;
    }
  }

  private scanStringLeaf(keyName: string): boolean {
    const start = this.pos;
    if (!this.advanceString()) return false;
    const end = this.pos;
    let rawVal: string;
    try {
      const raw = Buffer.from(this.src.buffer, this.src.byteOffset + start, end - start).toString("utf8");
      const parsed: unknown = JSON.parse(raw);
      if (typeof parsed !== "string") return false;
      rawVal = parsed;
    } catch {
      return false;
    }
    const scrubbed = scrubString(this.mode, this.key, keyName, rawVal);
    if (scrubbed === rawVal) {
      return true;
    }
    const encoded = new Uint8Array(Buffer.from(JSON.stringify(scrubbed), "utf8"));
    this.changed = true;
    if (start > this.copied) {
      this.chunks.push(this.src.subarray(this.copied, start));
    }
    this.chunks.push(encoded);
    this.copied = end;
    return true;
  }

  private advanceString(): boolean {
    if (this.pos >= this.src.length || this.src[this.pos] !== 0x22 /* " */) {
      return false;
    }
    this.pos++;
    while (this.pos < this.src.length) {
      const c = this.src[this.pos];
      if (c === 0x22 /* " */) {
        this.pos++;
        return true;
      }
      if (c === 0x5c /* \ */) {
        this.pos += 2;
        if (this.pos > this.src.length) return false;
        continue;
      }
      if (c < 0x20) return false;
      this.pos++;
    }
    return false;
  }

  private scanNumber(): boolean {
    const start = this.pos;
    if (this.pos < this.src.length && this.src[this.pos] === 0x2d /* - */) {
      this.pos++;
    }
    if (this.pos >= this.src.length || !isAsciiDigit(this.src[this.pos])) {
      return false;
    }
    if (this.src[this.pos] === 0x30 /* 0 */) {
      this.pos++;
    } else {
      while (this.pos < this.src.length && isAsciiDigit(this.src[this.pos])) {
        this.pos++;
      }
    }
    if (this.pos < this.src.length && this.src[this.pos] === 0x2e /* . */) {
      this.pos++;
      if (this.pos >= this.src.length || !isAsciiDigit(this.src[this.pos])) {
        return false;
      }
      while (this.pos < this.src.length && isAsciiDigit(this.src[this.pos])) {
        this.pos++;
      }
    }
    if (this.pos < this.src.length && (this.src[this.pos] === 0x65 /* e */ || this.src[this.pos] === 0x45 /* E */)) {
      this.pos++;
      if (this.pos < this.src.length && (this.src[this.pos] === 0x2b /* + */ || this.src[this.pos] === 0x2d /* - */)) {
        this.pos++;
      }
      if (this.pos >= this.src.length || !isAsciiDigit(this.src[this.pos])) {
        return false;
      }
      while (this.pos < this.src.length && isAsciiDigit(this.src[this.pos])) {
        this.pos++;
      }
    }
    return this.pos > start;
  }
}

function scrubJSONPreservingOrder(
  src: Uint8Array,
  mode: ScrubMode,
  key: Uint8Array,
): { out: Uint8Array; changed: boolean; ok: boolean } {
  return new JsonScrubber(src, mode, key).run();
}

function normalizeKeyForPII(key: string): string {
  let out = "_";
  let lastUnderscore = true;
  for (let i = 0; i < key.length; i++) {
    let c = key.charCodeAt(i);
    const isAlpha = (c >= 97 && c <= 122) || (c >= 65 && c <= 90);
    const isDigit = c >= 48 && c <= 57;
    if (!isAlpha && !isDigit) {
      if (!lastUnderscore) {
        out += "_";
        lastUnderscore = true;
      }
      continue;
    }
    if (!lastUnderscore && c >= 65 && c <= 90 && i > 0) {
      const prev = key.charCodeAt(i - 1);
      if ((prev >= 97 && prev <= 122) || (prev >= 48 && prev <= 57)) {
        out += "_";
      }
    }
    if (c >= 65 && c <= 90) {
      c += 32;
    }
    out += String.fromCharCode(c);
    lastUnderscore = false;
  }
  if (!lastUnderscore) out += "_";
  return out;
}

function classifySensitiveKey(key: string): PiiClassValue {
  if (!key) return PiiClass.None;
  const norm = normalizeKeyForPII(key);
  for (const p of ["_email_", "_emails_", "_mail_"]) {
    if (norm.includes(p)) return PiiClass.Email;
  }
  if (norm.includes("_ssn_")) return PiiClass.SSN;
  for (const p of ["_phone_", "_phones_", "_mobile_"]) {
    if (norm.includes(p)) return PiiClass.Phone;
  }
  for (const p of ["_pan_", "_card_", "_cards_", "_cc_num_", "_card_number_", "_credit_card_"]) {
    if (norm.includes(p)) return PiiClass.Card;
  }
  for (const p of GENERIC_SENSITIVE_KEY_TOKENS) {
    if (norm.includes(p)) return PiiClass.Generic;
  }
  return PiiClass.None;
}

function isAllowlistedKey(key: string): boolean {
  if (!key) return false;
  return ALLOWLISTED_ENUM_KEYS.has(key.trim().toLowerCase());
}

function detectLeafClass(key: string, val: string, mode: ScrubMode): PiiClassValue {
  if (!val) return PiiClass.None;
  if (isAllowlistedKey(key)) {
    return detectValueClass(val);
  }
  const vc = detectValueClass(val);
  if (vc !== PiiClass.None) return vc;
  const kc = classifySensitiveKey(key);
  if (kc !== PiiClass.None) return kc;
  if (mode === "strict") return PiiClass.Generic;
  return PiiClass.None;
}

function detectValueClass(val: string): PiiClassValue {
  const v = val.trim();
  if (!v) return PiiClass.None;
  if (isWholeEmail(v)) return PiiClass.Email;
  if (isWholeSSN(v)) return PiiClass.SSN;
  if (isWholePhone(v)) return PiiClass.Phone;
  if (isLuhnCard(v)) return PiiClass.Card;
  if (isWholeJWT(v) || isBearerSecret(v) || isAPIKeyToken(v)) return PiiClass.Generic;
  return PiiClass.None;
}

function tokenizeLeaf(val: string, cls: PiiClassValue, key: Uint8Array): string {
  const sum = crypto.createHmac("sha256", key).update(Buffer.from(val, "utf8")).digest();
  const hex16 = sum.subarray(0, 8).toString("hex");
  const num = sum.readBigUInt64BE(0);
  switch (cls) {
    case PiiClass.Email:
      return `u_${hex16}@redacted.invalid`;
    case PiiClass.SSN:
      return `900-00-${String(num % 10000n).padStart(4, "0")}`;
    case PiiClass.Phone:
      return `+155501${String(num % 100000n).padStart(5, "0")}`;
    default:
      return `tok_${hex16}`;
  }
}

function isAsciiDigit(c: number): boolean {
  return c >= 48 && c <= 57;
}
function isAsciiAlpha(c: number): boolean {
  return (c >= 97 && c <= 122) || (c >= 65 && c <= 90);
}
function isAsciiAlnum(c: number): boolean {
  return isAsciiAlpha(c) || isAsciiDigit(c);
}
function isB64URL(c: number): boolean {
  return isAsciiAlnum(c) || c === 45 /* - */ || c === 95 /* _ */;
}
function isSpaceByte(c: number): boolean {
  return c === 32 || c === 9 || c === 10 || c === 13;
}

function isWholeEmail(s: string): boolean {
  const at = s.indexOf("@");
  if (at <= 0 || at + 1 >= s.length || s.indexOf("@", at + 1) >= 0) return false;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c <= 0x20 || c >= 0x7f) return false;
  }
  const dom = s.slice(at + 1);
  const dot = dom.indexOf(".");
  if (dot <= 0 || dot + 1 >= dom.length || dom.endsWith(".") || dom.includes("..")) return false;
  const lastDot = dom.lastIndexOf(".");
  const tld = dom.slice(lastDot + 1);
  if (tld.length < 2) return false;
  for (let i = 0; i < tld.length; i++) {
    if (!isAsciiAlpha(tld.charCodeAt(i))) return false;
  }
  return true;
}

function isWholeSSN(s: string): boolean {
  if (s.length !== 11 || s.charCodeAt(3) !== 45 || s.charCodeAt(6) !== 45) return false;
  for (const idx of [0, 1, 2, 4, 5, 7, 8, 9, 10]) {
    if (!isAsciiDigit(s.charCodeAt(idx))) return false;
  }
  return true;
}

function isWholePhone(s: string): boolean {
  if (s.length < 8 || s.length > 20) return false;
  if (s.charCodeAt(0) === 43 /* + */) {
    if (s.length < 9 || s.charCodeAt(1) < 49 || s.charCodeAt(1) > 57) return false;
    let digits = 0;
    for (let i = 1; i < s.length; i++) {
      const c = s.charCodeAt(i);
      if (isAsciiDigit(c)) digits++;
      else if (c !== 45 && c !== 32) return false;
    }
    return digits >= 8 && digits <= 15;
  }
  if (
    s.length === 14 &&
    s.charCodeAt(0) === 40 &&
    s.charCodeAt(4) === 41 &&
    s.charCodeAt(5) === 32 &&
    s.charCodeAt(9) === 45
  ) {
    for (const idx of [1, 2, 3, 6, 7, 8, 10, 11, 12, 13]) {
      if (!isAsciiDigit(s.charCodeAt(idx))) return false;
    }
    return true;
  }
  return false;
}

function isLuhnCard(s: string): boolean {
  if (
    s.length < 13 ||
    s.length > 24 ||
    !isAsciiDigit(s.charCodeAt(0)) ||
    !isAsciiDigit(s.charCodeAt(s.length - 1))
  ) {
    return false;
  }
  const digits: number[] = [];
  let prevSep = false;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (isAsciiDigit(c)) {
      if (digits.length >= 19) return false;
      digits.push(c - 48);
      prevSep = false;
    } else if ((c === 32 || c === 45) && !prevSep) {
      prevSep = true;
    } else {
      return false;
    }
  }
  if (digits.length < 13) return false;
  let sum = 0;
  let dbl = false;
  for (let i = digits.length - 1; i >= 0; i--) {
    let x = digits[i];
    if (dbl) {
      x *= 2;
      if (x > 9) x -= 9;
    }
    sum += x;
    dbl = !dbl;
  }
  return sum % 10 === 0;
}

function isWholeJWT(s: string): boolean {
  if (!s.startsWith("eyJ") || s.length < 10) return false;
  const parts = s.split(".");
  if (parts.length !== 3 || parts[0].length < 4 || parts[1].length === 0 || parts[2].length === 0) {
    return false;
  }
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (!isB64URL(c) && c !== 46 && c !== 61) return false;
  }
  return true;
}

function isBearerSecret(s: string): boolean {
  if (s.length > 10 && s.slice(0, 7).toLowerCase() === "bearer ") {
    const tok = s.slice(7).trim();
    return tok.length >= 8 && !/[ \t\n\r]/.test(tok);
  }
  return false;
}

function isAPIKeyToken(s: string): boolean {
  if (s.length < 16 || /[ \t\n\r]/.test(s)) return false;
  for (const prefix of ["sk_live_", "sk_test_", "pk_live_", "pk_test_", "ghp_", "gho_", "glpat-", "AKIA", "xoxb-", "xoxp-"]) {
    if (s.startsWith(prefix)) return true;
  }
  return false;
}

function scrubInlineText(s: string, key: Uint8Array): { out: string; changed: boolean } {
  const n = s.length;
  if (n < 6) return { out: s, changed: false };
  let out = "";
  let copied = 0;
  let changed = false;

  for (let i = 0; i < n; ) {
    const c = s.charCodeAt(i);
    let matchEnd = 0;
    let cls: PiiClassValue = PiiClass.None;

    // Bearer token
    if (
      (c === 66 || c === 98) &&
      i + 8 < n &&
      s.slice(i, i + 7).toLowerCase() === "bearer " &&
      (i === 0 || !isAsciiAlnum(s.charCodeAt(i - 1)))
    ) {
      let j = i + 7;
      while (j < n) {
        const ch = s.charCodeAt(j);
        if (isSpaceByte(ch) || ch === 34 || ch === 39 || ch === 44 || ch === 59) break;
        j++;
      }
      if (j - (i + 7) >= 8) {
        changed = true;
        out += s.slice(copied, i + 7);
        out += tokenizeLeaf(s.slice(i + 7, j), PiiClass.Generic, key);
        copied = j;
        i = j;
        continue;
      }
    }

    // JWT
    if (
      c === 101 &&
      i + 8 <= n &&
      s.slice(i, i + 3) === "eyJ" &&
      (i === 0 || (!isB64URL(s.charCodeAt(i - 1)) && s.charCodeAt(i - 1) !== 46))
    ) {
      const end = scanInlineJWT(s, i);
      if (end > i) {
        matchEnd = end;
        cls = PiiClass.Generic;
      }
    }

    // E.164 Phone
    if (
      matchEnd === 0 &&
      c === 43 &&
      i + 9 <= n &&
      s.charCodeAt(i + 1) >= 49 &&
      s.charCodeAt(i + 1) <= 57 &&
      (i === 0 ||
        (!isAsciiAlnum(s.charCodeAt(i - 1)) &&
          s.charCodeAt(i - 1) !== 95 &&
          s.charCodeAt(i - 1) !== 43 &&
          s.charCodeAt(i - 1) !== 45))
    ) {
      const end = scanInlinePhone(s, i);
      if (end > i) {
        matchEnd = end;
        cls = PiiClass.Phone;
      }
    }

    // SSN or Luhn Card
    if (matchEnd === 0 && isAsciiDigit(c)) {
      const ssnEnd = scanInlineSSN(s, i);
      if (ssnEnd > i) {
        matchEnd = ssnEnd;
        cls = PiiClass.SSN;
      } else {
        const cardEnd = scanInlineCard(s, i);
        if (cardEnd > i) {
          matchEnd = cardEnd;
          cls = PiiClass.Card;
        }
      }
    }

    // Email
    if (
      matchEnd === 0 &&
      (isAsciiAlnum(c) || c === 95) &&
      (i === 0 ||
        (!isAsciiAlnum(s.charCodeAt(i - 1)) &&
          s.charCodeAt(i - 1) !== 95 &&
          s.charCodeAt(i - 1) !== 64 &&
          s.charCodeAt(i - 1) !== 46))
    ) {
      const end = scanInlineEmail(s, i);
      if (end > i) {
        matchEnd = end;
        cls = PiiClass.Email;
      }
    }

    if (matchEnd > i) {
      changed = true;
      if (i > copied) out += s.slice(copied, i);
      out += tokenizeLeaf(s.slice(i, matchEnd), cls, key);
      copied = matchEnd;
      i = matchEnd;
    } else {
      i++;
    }
  }

  if (!changed) return { out: s, changed: false };
  if (copied < n) out += s.slice(copied);
  return { out, changed: true };
}

function scanInlineJWT(s: string, i: number): number {
  let j = i;
  let dots = 0;
  while (j < s.length) {
    const c = s.charCodeAt(j);
    if (!isB64URL(c) && c !== 46 && c !== 61) break;
    if (c === 46) dots++;
    j++;
  }
  while (j > i && s.charCodeAt(j - 1) === 46) {
    j--;
    dots--;
  }
  return dots === 2 && isWholeJWT(s.slice(i, j)) ? j : 0;
}

function scanInlineSSN(s: string, i: number): number {
  if (i + 11 > s.length) return 0;
  if (i > 0) {
    const prev = s.charCodeAt(i - 1);
    if (isAsciiAlnum(prev) || prev === 95 || prev === 45 || prev === 43) return 0;
  }
  if (i + 11 < s.length) {
    const next = s.charCodeAt(i + 11);
    if (isAsciiAlnum(next) || next === 95 || next === 45) return 0;
  }
  return isWholeSSN(s.slice(i, i + 11)) ? i + 11 : 0;
}

function scanInlinePhone(s: string, i: number): number {
  let j = i + 1;
  let digits = 0;
  while (j < s.length) {
    const c = s.charCodeAt(j);
    if (isAsciiDigit(c)) {
      digits++;
      j++;
      if (digits > 15) return 0;
    } else if ((c === 45 || c === 32) && j + 1 < s.length && isAsciiDigit(s.charCodeAt(j + 1))) {
      j++;
    } else {
      break;
    }
  }
  if (digits < 8 || digits > 15) return 0;
  if (j < s.length) {
    const next = s.charCodeAt(j);
    if (isAsciiAlnum(next) || next === 95 || next === 64) return 0;
  }
  return j;
}

function scanInlineCard(s: string, i: number): number {
  if (i + 13 > s.length) return 0;
  if (i > 0) {
    const prev = s.charCodeAt(i - 1);
    if (isAsciiAlnum(prev) || prev === 95 || prev === 45 || prev === 43) return 0;
  }
  let j = i;
  let digits = 0;
  while (j < s.length) {
    const c = s.charCodeAt(j);
    if (isAsciiDigit(c)) {
      digits++;
      j++;
      if (digits > 19) return 0;
    } else if ((c === 45 || c === 32) && j + 1 < s.length && isAsciiDigit(s.charCodeAt(j + 1))) {
      j++;
    } else {
      break;
    }
  }
  if (digits < 13 || digits > 19) return 0;
  if (j < s.length) {
    const next = s.charCodeAt(j);
    if (isAsciiAlnum(next) || next === 95 || next === 45 || next === 64) return 0;
  }
  return isLuhnCard(s.slice(i, j)) ? j : 0;
}

function scanInlineEmail(s: string, i: number): number {
  const n = s.length;
  let p = i;
  while (p < n) {
    const c = s.charCodeAt(p);
    if (isAsciiAlnum(c) || c === 46 || c === 95 || c === 37 || c === 43 || c === 45) p++;
    else break;
  }
  if (p === i || p - i > 64 || p >= n || s.charCodeAt(p) !== 64 || s.charCodeAt(p - 1) === 46) return 0;
  let q = p + 1;
  while (q < n) {
    const c = s.charCodeAt(q);
    if (isAsciiAlnum(c) || c === 45 || c === 46) q++;
    else break;
  }
  while (q > p + 1 && (s.charCodeAt(q - 1) === 46 || s.charCodeAt(q - 1) === 45)) {
    q--;
  }
  if (q < n) {
    const next = s.charCodeAt(q);
    if (isAsciiAlnum(next) || next === 95 || next === 64) return 0;
  }
  return isWholeEmail(s.slice(i, q)) ? q : 0;
}
