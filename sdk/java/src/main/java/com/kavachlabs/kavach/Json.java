package com.kavachlabs.kavach;

import java.util.ArrayList;
import java.util.Collection;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * A small JSON reader and writer, enough for the protocols and for handlers
 * that want no dependency. Objects parse to {@code LinkedHashMap}, arrays to
 * {@code ArrayList}, integral numbers to {@code Long}, other numbers to
 * {@code Double}.
 */
public final class Json {
    private Json() {}

    /** Malformed JSON. */
    public static final class ParseException extends IllegalArgumentException {
        public ParseException(String message) {
            super(message);
        }
    }

    public static Object parse(String s) {
        Parser p = new Parser(s);
        p.ws();
        Object v = p.value(0);
        p.ws();
        if (p.i != s.length()) {
            throw p.err("trailing data");
        }
        return v;
    }

    /** Compact JSON; {@link Map} keys keep their iteration order. */
    public static String write(Object v) {
        StringBuilder sb = new StringBuilder();
        write(sb, v);
        return sb.toString();
    }

    public static void write(StringBuilder sb, Object v) {
        if (v == null) {
            sb.append("null");
        } else if (v instanceof String s) {
            quote(sb, s);
        } else if (v instanceof Boolean || v instanceof Long || v instanceof Integer || v instanceof Short
                || v instanceof Byte) {
            sb.append(v);
        } else if (v instanceof Double || v instanceof Float) {
            double d = ((Number) v).doubleValue();
            if (Double.isNaN(d) || Double.isInfinite(d)) {
                throw new IllegalArgumentException("JSON cannot hold " + d);
            }
            if (d == Math.rint(d) && Math.abs(d) < 1e15) {
                sb.append((long) d);
            } else {
                sb.append(d);
            }
        } else if (v instanceof Map<?, ?> m) {
            sb.append('{');
            boolean first = true;
            for (Map.Entry<?, ?> e : m.entrySet()) {
                if (!first) {
                    sb.append(',');
                }
                first = false;
                quote(sb, String.valueOf(e.getKey()));
                sb.append(':');
                write(sb, e.getValue());
            }
            sb.append('}');
        } else if (v instanceof Collection<?> c) {
            sb.append('[');
            boolean first = true;
            for (Object o : c) {
                if (!first) {
                    sb.append(',');
                }
                first = false;
                write(sb, o);
            }
            sb.append(']');
        } else {
            throw new IllegalArgumentException("cannot encode " + v.getClass().getName() + " as JSON");
        }
    }

    /**
     * A JSON string literal. Escapes {@code "} {@code \} newline, carriage
     * return and tab, and other characters below U+0020 as lowercase
     * backslash-u-00xx; everything else is written as it is.
     */
    public static String quote(String s) {
        StringBuilder sb = new StringBuilder(s.length() + 2);
        quote(sb, s);
        return sb.toString();
    }

    public static void quote(StringBuilder sb, String s) {
        sb.append('"');
        for (int k = 0; k < s.length(); k++) {
            char c = s.charAt(k);
            switch (c) {
                case '"' -> sb.append("\\\"");
                case '\\' -> sb.append("\\\\");
                case '\n' -> sb.append("\\n");
                case '\r' -> sb.append("\\r");
                case '\t' -> sb.append("\\t");
                default -> {
                    if (c < 0x20) {
                        sb.append("\\u00").append(Character.forDigit(c >> 4, 16)).append(Character.forDigit(c & 15, 16));
                    } else {
                        sb.append(c);
                    }
                }
            }
        }
        sb.append('"');
    }

    private static final class Parser {
        private static final int MAX_DEPTH = 512;
        final String s;
        int i;

        Parser(String s) {
            this.s = s;
        }

        ParseException err(String what) {
            return new ParseException("invalid JSON at offset " + i + ": " + what);
        }

        void ws() {
            while (i < s.length()) {
                char c = s.charAt(i);
                if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
                    i++;
                } else {
                    break;
                }
            }
        }

        Object value(int depth) {
            if (depth > MAX_DEPTH) {
                throw err("nesting too deep");
            }
            if (i >= s.length()) {
                throw err("unexpected end");
            }
            char c = s.charAt(i);
            switch (c) {
                case '{':
                    return object(depth);
                case '[':
                    return array(depth);
                case '"':
                    return string();
                case 't':
                    return literal("true", Boolean.TRUE);
                case 'f':
                    return literal("false", Boolean.FALSE);
                case 'n':
                    return literal("null", null);
                default:
                    if (c == '-' || (c >= '0' && c <= '9')) {
                        return number();
                    }
                    throw err("unexpected character '" + c + "'");
            }
        }

        Object literal(String word, Object v) {
            if (!s.startsWith(word, i)) {
                throw err("expected " + word);
            }
            i += word.length();
            return v;
        }

        Object number() {
            int start = i;
            if (s.charAt(i) == '-') {
                i++;
            }
            boolean integral = true;
            while (i < s.length()) {
                char c = s.charAt(i);
                if (c >= '0' && c <= '9') {
                    i++;
                } else if (c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
                    integral = false;
                    i++;
                } else {
                    break;
                }
            }
            String text = s.substring(start, i);
            try {
                if (integral) {
                    try {
                        return Long.parseLong(text);
                    } catch (NumberFormatException e) {
                        return Double.parseDouble(text);
                    }
                }
                return Double.parseDouble(text);
            } catch (NumberFormatException e) {
                i = start;
                throw err("bad number " + text);
            }
        }

        String string() {
            i++; // opening quote
            StringBuilder sb = new StringBuilder();
            while (true) {
                if (i >= s.length()) {
                    throw err("unterminated string");
                }
                char c = s.charAt(i++);
                if (c == '"') {
                    return sb.toString();
                }
                if (c < 0x20) {
                    throw err("control character in string");
                }
                if (c != '\\') {
                    sb.append(c);
                    continue;
                }
                if (i >= s.length()) {
                    throw err("unterminated escape");
                }
                char e = s.charAt(i++);
                switch (e) {
                    case '"' -> sb.append('"');
                    case '\\' -> sb.append('\\');
                    case '/' -> sb.append('/');
                    case 'b' -> sb.append('\b');
                    case 'f' -> sb.append('\f');
                    case 'n' -> sb.append('\n');
                    case 'r' -> sb.append('\r');
                    case 't' -> sb.append('\t');
                    case 'u' -> {
                        if (i + 4 > s.length()) {
                            throw err("short \\u escape");
                        }
                        try {
                            sb.append((char) Integer.parseInt(s.substring(i, i + 4), 16));
                        } catch (NumberFormatException ex) {
                            throw err("bad \\u escape");
                        }
                        i += 4;
                    }
                    default -> throw err("bad escape \\" + e);
                }
            }
        }

        List<Object> array(int depth) {
            i++;
            List<Object> out = new ArrayList<>();
            ws();
            if (i < s.length() && s.charAt(i) == ']') {
                i++;
                return out;
            }
            while (true) {
                ws();
                out.add(value(depth + 1));
                ws();
                if (i >= s.length()) {
                    throw err("unterminated array");
                }
                char c = s.charAt(i++);
                if (c == ']') {
                    return out;
                }
                if (c != ',') {
                    throw err("expected , or ]");
                }
            }
        }

        Map<String, Object> object(int depth) {
            i++;
            Map<String, Object> out = new LinkedHashMap<>();
            ws();
            if (i < s.length() && s.charAt(i) == '}') {
                i++;
                return out;
            }
            while (true) {
                ws();
                if (i >= s.length() || s.charAt(i) != '"') {
                    throw err("expected a string key");
                }
                String key = string();
                ws();
                if (i >= s.length() || s.charAt(i++) != ':') {
                    throw err("expected :");
                }
                ws();
                out.put(key, value(depth + 1));
                ws();
                if (i >= s.length()) {
                    throw err("unterminated object");
                }
                char c = s.charAt(i++);
                if (c == '}') {
                    return out;
                }
                if (c != ',') {
                    throw err("expected , or }");
                }
            }
        }
    }
}
