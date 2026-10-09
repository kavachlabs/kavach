//! The few system calls the SDK needs, declared directly so that the crate has
//! no dependencies.

use std::fs::File;
use std::io::{self, Read};
#[cfg(target_os = "linux")]
use std::os::raw::{c_char, c_uint};
use std::os::raw::{c_int, c_void};
#[cfg(target_os = "linux")]
use std::os::unix::io::FromRawFd;

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
    #[cfg(target_os = "linux")]
    fn memfd_create(name: *const c_char, flags: c_uint) -> c_int;
}

/// An anonymous memory file, so the ring has no name at all (SPEC.md section
/// 10.7). Linux only; an error there means the caller falls back to a file.
#[cfg(target_os = "linux")]
pub fn memfd() -> io::Result<File> {
    const MFD_CLOEXEC: c_uint = 1;
    // SAFETY: the name is NUL-terminated; on success the descriptor is new and
    // owned by the File.
    let fd = unsafe { memfd_create(b"kavach-ring\0".as_ptr().cast(), MFD_CLOEXEC) };
    if fd < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(unsafe { File::from_raw_fd(fd) })
    }
}

/// Maps the first `header + cap` bytes of `fd` shared and writable, then the
/// `cap` bytes at file offset `header` again right after them, inside one
/// reserved region so nothing else can land between the two. The returned
/// mapping is `header + 2 * cap` bytes. The same constants on Linux and macOS,
/// except `MAP_ANON`.
pub fn map_ring(fd: c_int, header: usize, cap: usize) -> io::Result<*mut u8> {
    const PROT_NONE: c_int = 0;
    const PROT_READ_WRITE: c_int = 3;
    const MAP_SHARED: c_int = 1;
    const MAP_PRIVATE: c_int = 2;
    const MAP_FIXED: c_int = 0x10;
    #[cfg(target_os = "linux")]
    const MAP_ANON: c_int = 0x20;
    #[cfg(not(target_os = "linux"))]
    const MAP_ANON: c_int = 0x1000;
    let len = header + cap;
    // SAFETY: a null hint and a fresh reservation; the caller owns the result.
    let base = unsafe {
        mmap(
            std::ptr::null_mut(),
            len + cap,
            PROT_NONE,
            MAP_PRIVATE | MAP_ANON,
            -1,
            0,
        )
    };
    if base as isize == -1 {
        return Err(io::Error::last_os_error());
    }
    for (at, n, off) in [(0, len, 0), (len, cap, header)] {
        // SAFETY: both ranges lie inside the reservation made above.
        let p = unsafe {
            mmap(
                base.cast::<u8>().add(at).cast(),
                n,
                PROT_READ_WRITE,
                MAP_SHARED | MAP_FIXED,
                fd,
                off as Off,
            )
        };
        if p as isize == -1 {
            let e = io::Error::last_os_error();
            // SAFETY: the reservation is ours and unused.
            unsafe { munmap(base, len + cap) };
            return Err(e);
        }
    }
    Ok(base.cast())
}

/// # Safety
/// `p` and `len` must be a mapping returned by [`map_ring`], unused afterwards.
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
