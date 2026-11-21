//! A small JSON value, parser and writer: just enough for the host protocol
//! (SPEC.md section 9.2), the recorder's control stream (section 10.3) and the
//! conformance handler. Not a general-purpose library.

use std::fmt::Write as _;

/// A JSON value. Objects keep their keys in insertion order.
#[derive(Debug, Clone, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    Num(f64),
    Str(String),
    Arr(Vec<Value>),
    Obj(Vec<(String, Value)>),
}

impl Value {
    /// Builds an object from `(key, value)` pairs.
    pub fn obj<const N: usize>(pairs: [(&str, Value); N]) -> Value {
        Value::Obj(pairs.into_iter().map(|(k, v)| (k.to_string(), v)).collect())
    }

    /// Like [`Value::obj`], for pairs collected at run time.
    pub fn obj_vec(pairs: Vec<(&str, Value)>) -> Value {
        Value::Obj(pairs.into_iter().map(|(k, v)| (k.to_string(), v)).collect())
    }

    /// A string value.
    pub fn str(s: impl Into<String>) -> Value {
        Value::Str(s.into())
    }

    /// An integer value.
    pub fn int(n: i64) -> Value {
        Value::Num(n as f64)
    }

    /// The member `key` of an object.
    pub fn get(&self, key: &str) -> Option<&Value> {
        match self {
            Value::Obj(m) => m.iter().find(|(k, _)| k == key).map(|(_, v)| v),
            _ => None,
        }
    }

    pub fn as_str(&self) -> Option<&str> {
        match self {
            Value::Str(s) => Some(s),
            _ => None,
        }
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self {
            Value::Bool(b) => Some(*b),
            _ => None,
        }
    }

    pub fn as_f64(&self) -> Option<f64> {
        match self {
            Value::Num(n) => Some(*n),
            _ => None,
        }
    }

    /// A non-negative integer, if the value is a number that is one.
    pub fn as_u64(&self) -> Option<u64> {
        match self {
            Value::Num(n) if *n >= 0.0 && n.fract() == 0.0 && *n < 9.0e15 => Some(*n as u64),
            _ => None,
        }
    }

    pub fn as_array(&self) -> Option<&[Value]> {
        match self {
            Value::Arr(a) => Some(a),
            _ => None,
        }
    }

    pub fn as_object(&self) -> Option<&[(String, Value)]> {
        match self {
            Value::Obj(m) => Some(m),
            _ => None,
        }
    }

    /// Member `key` as a string.
    pub fn str_field(&self, key: &str) -> Option<&str> {
        self.get(key)?.as_str()
    }

    /// Serializes compactly: no whitespace, keys in order.
    pub fn to_json(&self) -> String {
        let mut s = String::new();
        self.write(&mut s);
        s
    }

    fn write(&self, out: &mut String) {
        match self {
            Value::Null => out.push_str("null"),
            Value::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
            Value::Num(n) => {
                if n.fract() == 0.0 && n.abs() < 9.0e15 {
                    let _ = write!(out, "{}", *n as i64);
                } else if n.is_finite() {
                    let _ = write!(out, "{n}");
                } else {
                    out.push_str("null");
                }
            }
            Value::Str(s) => write_string(s, out),
            Value::Arr(a) => {
                out.push('[');
                for (i, v) in a.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    v.write(out);
                }
                out.push(']');
            }
            Value::Obj(m) => {
                out.push('{');
                for (i, (k, v)) in m.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    write_string(k, out);
                    out.push(':');
                    v.write(out);
                }
                out.push('}');
            }
        }
    }
}

/// Appends `s` as a JSON string literal.
pub fn write_string(s: &str, out: &mut String) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

/// A JSON string literal for `s`.
pub fn quote(s: &str) -> String {
    let mut out = String::new();
    write_string(s, &mut out);
    out
}

/// Parses one JSON value; trailing whitespace is allowed, anything else is an error.
pub fn parse(input: &str) -> Result<Value, String> {
    let mut p = Parser {
        s: input.as_bytes(),
        i: 0,
        depth: 0,
    };
    p.ws();
    let v = p.value()?;
    p.ws();
    if p.i != p.s.len() {
        return Err(format!("trailing characters at offset {}", p.i));
    }
    Ok(v)
}

struct Parser<'a> {
    s: &'a [u8],
    i: usize,
    depth: usize,
}

impl Parser<'_> {
    fn ws(&mut self) {
        while matches!(self.s.get(self.i), Some(b' ' | b'\t' | b'\n' | b'\r')) {
            self.i += 1;
        }
    }

    fn err<T>(&self, what: &str) -> Result<T, String> {
        Err(format!("{what} at offset {}", self.i))
    }

    fn eat(&mut self, lit: &str) -> Result<(), String> {
        if self.s[self.i..].starts_with(lit.as_bytes()) {
            self.i += lit.len();
            Ok(())
        } else {
            self.err("invalid literal")
        }
    }

    fn value(&mut self) -> Result<Value, String> {
        match self.s.get(self.i) {
            None => self.err("unexpected end"),
            Some(b'n') => self.eat("null").map(|_| Value::Null),
            Some(b't') => self.eat("true").map(|_| Value::Bool(true)),
            Some(b'f') => self.eat("false").map(|_| Value::Bool(false)),
            Some(b'"') => self.string().map(Value::Str),
            Some(b'[') => self.nested(|p| {
                p.i += 1;
                let mut a = Vec::new();
                p.ws();
                if p.s.get(p.i) == Some(&b']') {
                    p.i += 1;
                    return Ok(Value::Arr(a));
                }
                loop {
                    p.ws();
                    a.push(p.value()?);
                    p.ws();
                    match p.s.get(p.i) {
                        Some(b',') => p.i += 1,
                        Some(b']') => {
                            p.i += 1;
                            return Ok(Value::Arr(a));
                        }
                        _ => return p.err("expected , or ]"),
                    }
                }
            }),
            Some(b'{') => self.nested(|p| {
                p.i += 1;
                let mut m = Vec::new();
                p.ws();
                if p.s.get(p.i) == Some(&b'}') {
                    p.i += 1;
                    return Ok(Value::Obj(m));
                }
                loop {
                    p.ws();
                    if p.s.get(p.i) != Some(&b'"') {
                        return p.err("expected object key");
                    }
                    let k = p.string()?;
                    p.ws();
                    if p.s.get(p.i) != Some(&b':') {
                        return p.err("expected :");
                    }
                    p.i += 1;
                    p.ws();
                    m.push((k, p.value()?));
                    p.ws();
                    match p.s.get(p.i) {
                        Some(b',') => p.i += 1,
                        Some(b'}') => {
                            p.i += 1;
                            return Ok(Value::Obj(m));
                        }
                        _ => return p.err("expected , or }"),
                    }
                }
            }),
            Some(b'-' | b'0'..=b'9') => self.number(),
            Some(_) => self.err("unexpected character"),
        }
    }

    fn nested(
        &mut self,
        f: impl FnOnce(&mut Self) -> Result<Value, String>,
    ) -> Result<Value, String> {
        self.depth += 1;
        if self.depth > 128 {
            return self.err("nesting too deep");
        }
        let v = f(self);
        self.depth -= 1;
        v
    }

    fn number(&mut self) -> Result<Value, String> {
        let start = self.i;
        while matches!(
            self.s.get(self.i),
            Some(b'-' | b'+' | b'.' | b'e' | b'E' | b'0'..=b'9')
        ) {
            self.i += 1;
        }
        let text = std::str::from_utf8(&self.s[start..self.i]).map_err(|e| e.to_string())?;
        text.parse::<f64>()
            .map(Value::Num)
            .map_err(|_| format!("invalid number {text:?}"))
    }

    fn hex4(&mut self) -> Result<u32, String> {
        let h = self
            .s
            .get(self.i..self.i + 4)
            .ok_or("truncated \\u escape")?;
        let h = std::str::from_utf8(h).map_err(|e| e.to_string())?;
        let n = u32::from_str_radix(h, 16).map_err(|_| "invalid \\u escape".to_string())?;
        self.i += 4;
        Ok(n)
    }

    fn string(&mut self) -> Result<String, String> {
        self.i += 1; // opening quote
        let mut out: Vec<u8> = Vec::new();
        loop {
            let Some(&c) = self.s.get(self.i) else {
                return self.err("unterminated string");
            };
            self.i += 1;
            match c {
                b'"' => break,
                b'\\' => {
                    let Some(&e) = self.s.get(self.i) else {
                        return self.err("unterminated escape");
                    };
                    self.i += 1;
                    let ch = match e {
                        b'"' => '"',
                        b'\\' => '\\',
                        b'/' => '/',
                        b'b' => '\u{8}',
                        b'f' => '\u{c}',
                        b'n' => '\n',
                        b'r' => '\r',
                        b't' => '\t',
                        b'u' => {
                            let mut n = self.hex4()?;
                            if (0xD800..0xDC00).contains(&n) {
                                if self.s[self.i..].starts_with(b"\\u") {
                                    self.i += 2;
                                    let lo = self.hex4()?;
                                    if !(0xDC00..0xE000).contains(&lo) {
                                        return self.err("invalid surrogate pair");
                                    }
                                    n = 0x10000 + ((n - 0xD800) << 10) + (lo - 0xDC00);
                                } else {
                                    return self.err("lone surrogate");
                                }
                            }
                            char::from_u32(n).ok_or_else(|| "invalid code point".to_string())?
                        }
                        _ => return self.err("invalid escape"),
                    };
                    let mut buf = [0u8; 4];
                    out.extend_from_slice(ch.encode_utf8(&mut buf).as_bytes());
                }
                c if c < 0x20 => return self.err("control character in string"),
                c => out.push(c),
            }
        }
        String::from_utf8(out).map_err(|_| "string is not UTF-8".to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_and_writes() {
        let v = parse(
            r#" {"t":"step","seq":"0","n":3,"x":[true,null,"a\n\u00e9\ud83d\ude00"],"o":{}} "#,
        )
        .unwrap();
        assert_eq!(v.str_field("t"), Some("step"));
        assert_eq!(v.get("n").and_then(Value::as_u64), Some(3));
        assert_eq!(
            v.to_json(),
            "{\"t\":\"step\",\"seq\":\"0\",\"n\":3,\"x\":[true,null,\"a\\n\u{e9}\u{1f600}\"],\"o\":{}}"
        );
    }

    #[test]
    fn rejects_bad_input() {
        for bad in ["", "{", "[1,]", "{\"a\" 1}", "\"\\x\"", "1 2", "nul"] {
            assert!(parse(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn escapes_controls() {
        assert_eq!(quote("a\"\\\u{1}"), "\"a\\\"\\\\\\u0001\"");
    }
}
