//! The SDK half of the shared-memory ring (SPEC.md section 10.7).

use std::fs::{File, OpenOptions};
use std::io;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::io::AsRawFd;
use std::sync::atomic::{AtomicU64, Ordering};

use crate::sys;

const MAGIC: &[u8; 8] = b"KVRING01";
const OFF_CAPACITY: usize = 8;
const OFF_WRITE: usize = 64;
const OFF_READ: usize = 128;
const HEADER: usize = 256;
pub const MIN_CAPACITY: usize = 64 << 10;
pub const DEFAULT_CAPACITY: usize = 8 << 20;

/// A mapped ring: the SDK publishes, the recorder consumes, and neither writes
/// the other's header word.
pub struct Ring {
    mem: *mut u8,
    cap: u64,
}

impl Ring {
    /// Creates the ring file in `/dev/shm` if there is one, else the temporary
    /// directory, unlinks it at once, maps it and initializes the header. The
    /// returned file is what the recorder inherits as descriptor 3.
    pub fn create(capacity: usize) -> io::Result<(Ring, File)> {
        if capacity < MIN_CAPACITY || !capacity.is_power_of_two() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("ring capacity {capacity} is not a power of two of at least 64 KiB"),
            ));
        }
        let dir = if std::path::Path::new("/dev/shm").is_dir() {
            std::path::PathBuf::from("/dev/shm")
        } else {
            std::env::temp_dir()
        };
        static N: AtomicU64 = AtomicU64::new(0);
        let name = dir.join(format!(
            "kavach-ring-{}-{}",
            std::process::id(),
            N.fetch_add(1, Ordering::Relaxed)
        ));
        let f = OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&name)?;
        let _ = std::fs::remove_file(&name);
        let size = HEADER + capacity;
        f.set_len(size as u64)?;
        let mem = sys::map_shared(f.as_raw_fd(), size)?;
        // SAFETY: the mapping is HEADER + capacity bytes and nobody else has it yet.
        unsafe {
            std::ptr::copy_nonoverlapping(MAGIC.as_ptr(), mem, MAGIC.len());
            std::ptr::copy_nonoverlapping(
                (capacity as u64).to_le_bytes().as_ptr(),
                mem.add(OFF_CAPACITY),
                8,
            );
        }
        Ok((
            Ring {
                mem,
                cap: capacity as u64,
            },
            f,
        ))
    }

    pub fn capacity(&self) -> u64 {
        self.cap
    }

    fn word(&self, off: usize) -> &AtomicU64 {
        // SAFETY: the offsets are 8-aligned within the page-aligned mapping,
        // which lives as long as self.
        unsafe { &*self.mem.add(off).cast::<AtomicU64>() }
    }

    /// Copies `p` into the ring and publishes it with one release store of
    /// `write`, so the recorder sees all of it or none. If `p` does not fit in
    /// the free space it publishes nothing and returns 0, unless `p` is larger
    /// than the whole ring, in which case it publishes as much as fits. Also
    /// returns the bytes the recorder has not consumed, counting those just
    /// published.
    pub fn try_publish(&self, p: &[u8]) -> (usize, u64) {
        let write = self.word(OFF_WRITE);
        let w = write.load(Ordering::Relaxed);
        let used = w - self.word(OFF_READ).load(Ordering::Acquire);
        let mut n = p.len();
        let free = self.cap - used;
        if n as u64 > free {
            if n as u64 <= self.cap {
                return (0, used);
            }
            n = free as usize;
        }
        let off = (w & (self.cap - 1)) as usize;
        let first = n.min(self.cap as usize - off);
        // SAFETY: both ranges lie in the data area, and the recorder does not
        // read bytes beyond `write`, which is not yet advanced.
        unsafe {
            let data = self.mem.add(HEADER);
            std::ptr::copy_nonoverlapping(p.as_ptr(), data.add(off), first);
            std::ptr::copy_nonoverlapping(p.as_ptr().add(first), data, n - first);
        }
        write.store(w + n as u64, Ordering::Release);
        (n, used + n as u64)
    }
}

impl Drop for Ring {
    fn drop(&mut self) {
        // SAFETY: mem was mapped with this length and is not used again.
        unsafe { sys::unmap(self.mem, HEADER + self.cap as usize) }
    }
}
