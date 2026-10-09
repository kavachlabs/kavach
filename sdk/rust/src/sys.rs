//! The few system calls the SDK needs, declared directly so that the crate has
//! no dependencies.

use std::fs::File;
use std::io::{self, Read};
use std::os::raw::c_int;

extern "C" {
    fn dup(fd: c_int) -> c_int;
    fn dup2(src: c_int, dst: c_int) -> c_int;
    #[cfg(target_os = "linux")]
    fn fcntl(fd: c_int, cmd: c_int, ...) -> c_int;
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
