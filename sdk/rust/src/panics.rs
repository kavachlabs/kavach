//! Panics as failures (SPEC.md section 4.5).

use std::any::Any;
use std::backtrace::Backtrace;
use std::cell::{Cell, RefCell};
use std::panic::{self, AssertUnwindSafe};
use std::sync::Once;

/// Panics with exactly `message`: the `panic` marker's message will be `message`
/// unchanged. (`panic!("{}", message)` does the same; this skips the formatting
/// and works with a runtime string.)
pub fn panic(message: impl Into<String>) -> ! {
    panic::panic_any(message.into())
}

/// The marker message for a panic payload: the payload if it is a `&str` or a
/// `String`, else `"Box<dyn Any>"`.
pub(crate) fn message(payload: &(dyn Any + Send)) -> String {
    if let Some(s) = payload.downcast_ref::<&str>() {
        (*s).to_string()
    } else if let Some(s) = payload.downcast_ref::<String>() {
        s.clone()
    } else {
        "Box<dyn Any>".to_string()
    }
}

thread_local! {
    static CAPTURING: Cell<bool> = const { Cell::new(false) };
    static LAST: RefCell<Option<String>> = const { RefCell::new(None) };
}

static HOOK: Once = Once::new();

/// Installs a panic hook that, while a handler step runs, captures a backtrace
/// for the `panic` marker's data, then calls the hook that was installed before
/// it, so the user's reporting is unchanged (the default hook still prints to
/// standard error). Idempotent. The host calls it itself; a [`Recorder`] calls
/// it only if built with `capture_backtraces(true)`.
///
/// [`Recorder`]: crate::Recorder
pub fn install_panic_hook() {
    HOOK.call_once(|| {
        let prev = panic::take_hook();
        panic::set_hook(Box::new(move |info| {
            if CAPTURING.with(Cell::get) {
                let bt = Backtrace::force_capture().to_string();
                LAST.with(|l| *l.borrow_mut() = Some(bt));
            }
            prev(info);
        }));
    });
}

/// How a step ended abnormally.
pub(crate) enum Caught<T> {
    Done(T),
    Panicked {
        payload: Box<dyn Any + Send>,
        message: String,
        backtrace: Vec<u8>,
    },
}

/// Runs `f`, catching a panic at the step boundary.
pub(crate) fn catch<T>(f: impl FnOnce() -> T) -> Caught<T> {
    LAST.with(|l| *l.borrow_mut() = None);
    CAPTURING.with(|c| c.set(true));
    let r = panic::catch_unwind(AssertUnwindSafe(f));
    CAPTURING.with(|c| c.set(false));
    match r {
        Ok(v) => Caught::Done(v),
        Err(payload) => {
            let message = message(&*payload);
            let backtrace = LAST
                .with(|l| l.borrow_mut().take())
                .unwrap_or_default()
                .into_bytes();
            Caught::Panicked {
                payload,
                message,
                backtrace,
            }
        }
    }
}
