//! The few system calls the SDK needs, declared directly so that the crate has
//! no dependencies.

use std::fs::File;
use std::io::{self, Read};
use std::os::raw::{c_int, c_void};

#[cfg(target_pointer_width = "64")]
type Off = i64;
#[cfg(not(target_pointer_width = "64"))]
type Off = i32;

extern "C" {
    fn dup(fd: c_int) -> c_int;
    fn dup2(src: c_int, dst: c_int) -> c_int;
    fn fcntl(fd: c_int, cmd: c_int, ...) -> c_int;
    fn mmap(
        addr: *mut c_void,
        len: usize,
        prot: c_int,
        flags: c_int,
        fd: c_int,
        off: Off,
    ) -> *mut c_void;
    fn munmap(addr: *mut c_void, len: usize) -> c_int;
}

/// Maps `len` bytes of `fd` shared and writable. The same constants on Linux
/// and macOS.
pub fn map_shared(fd: c_int, len: usize) -> io::Result<*mut u8> {
    const PROT_READ_WRITE: c_int = 3;
    const MAP_SHARED: c_int = 1;
    // SAFETY: a null hint and a fresh mapping; the caller owns the result.
    let p = unsafe {
        mmap(
            std::ptr::null_mut(),
            len,
            PROT_READ_WRITE,
            MAP_SHARED,
            fd,
            0,
        )
    };
    if p as isize == -1 {
        Err(io::Error::last_os_error())
    } else {
        Ok(p.cast())
    }
}

/// # Safety
/// `p` and `len` must be a mapping returned by [`map_shared`], unused afterwards.
pub unsafe fn unmap(p: *mut u8, len: usize) {
    munmap(p.cast(), len);
}

/// Clears close-on-exec on `fd`. Safe between fork and exec.
pub fn keep_across_exec(fd: c_int) {
    const F_SETFD: c_int = 2;
    // SAFETY: F_SETFD takes an int argument and touches no memory.
    unsafe {
        fcntl(fd, F_SETFD, 0);
    }
}

/// Duplicates `fd`.
pub fn dup_fd(fd: c_int) -> io::Result<c_int> {
    // SAFETY: dup has no memory-safety preconditions.
    let r = unsafe { dup(fd) };
    if r < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(r)
    }
}

/// Makes `dst` refer to what `src` does.
pub fn dup2_fd(src: c_int, dst: c_int) -> io::Result<()> {
    // SAFETY: dup2 has no memory-safety preconditions.
    let r = unsafe { dup2(src, dst) };
    if r < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}

/// Enlarges a pipe's buffer to `size` bytes (SPEC.md section 10.2). Linux only;
/// elsewhere, and on failure, it does nothing.
#[cfg(target_os = "linux")]
pub fn grow_pipe(fd: c_int, size: c_int) {
    const F_SETPIPE_SZ: c_int = 1031;
    // SAFETY: F_SETPIPE_SZ takes an int argument and touches no memory.
    unsafe {
        fcntl(fd, F_SETPIPE_SZ, size);
    }
}

#[cfg(not(target_os = "linux"))]
pub fn grow_pipe(_fd: c_int, _size: c_int) {}

/// Operating-system randomness from `/dev/urandom`.
pub struct Urandom(File);

impl Urandom {
    pub fn open() -> io::Result<Urandom> {
        File::open("/dev/urandom").map(Urandom)
    }

    pub fn fill(&mut self, buf: &mut [u8]) -> io::Result<()> {
        self.0.read_exact(buf)
    }
}
