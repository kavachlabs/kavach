//! The record stream's frames (SPEC.md sections 2, 4 and 10.2).

pub const FRAME_OPEN: u8 = 0x01;
pub const FRAME_RECORD: u8 = 0x02;
pub const FRAME_STEP_END: u8 = 0x03;
pub const FRAME_FACTS: u8 = 0x04;
pub const FRAME_SNAPSHOT: u8 = 0x05;
pub const FRAME_FLUSH: u8 = 0x06;
pub const FRAME_CLOSE: u8 = 0x07;

pub const REC_INPUT: u8 = 0x01;
pub const REC_CLOCK: u8 = 0x02;
pub const REC_RAND: u8 = 0x03;
pub const REC_OUTPUT: u8 = 0x04;
pub const REC_MARKER: u8 = 0x05;
pub const REC_GATEWAY: u8 = 0x07;
pub const REC_CONFIG: u8 = 0x09;

/// The critical flag (SPEC.md section 3.4).
pub const CRITICAL: u8 = 1;

pub fn put_uvarint(buf: &mut Vec<u8>, mut x: u64) {
    while x >= 0x80 {
        buf.push((x as u8) | 0x80);
        x >>= 7;
    }
    buf.push(x as u8);
}

pub fn put_bytes(buf: &mut Vec<u8>, b: &[u8]) {
    put_uvarint(buf, b.len() as u64);
    buf.extend_from_slice(b);
}

pub fn put_str(buf: &mut Vec<u8>, s: &str) {
    put_bytes(buf, s.as_bytes());
}

/// Appends a frame: `len` (of kind and payload), `kind`, `payload`.
pub fn put_frame(buf: &mut Vec<u8>, kind: u8, payload: &[u8]) {
    put_uvarint(buf, payload.len() as u64 + 1);
    buf.push(kind);
    buf.extend_from_slice(payload);
}

/// Appends a `record` frame holding a record of `rtype` with `flags`.
pub fn put_record(buf: &mut Vec<u8>, rtype: u8, flags: u8, fields: &[u8]) {
    put_uvarint(buf, fields.len() as u64 + 3);
    buf.extend_from_slice(&[FRAME_RECORD, rtype, flags]);
    buf.extend_from_slice(fields);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn uvarint_matches_go() {
        for (x, want) in [
            (0u64, &[0u8][..]),
            (127, &[127]),
            (128, &[0x80, 1]),
            (300, &[0xAC, 2]),
        ] {
            let mut b = Vec::new();
            put_uvarint(&mut b, x);
            assert_eq!(b, want);
        }
    }

    #[test]
    fn frames() {
        let mut b = Vec::new();
        put_record(&mut b, REC_CLOCK, 0, &5i64.to_le_bytes());
        assert_eq!(b[..4], [11, FRAME_RECORD, REC_CLOCK, 0]);
        assert_eq!(b.len(), 12);
    }
}
