# Task: a Router nested at "/" panics when used as a fallback

You are working in an axum checkout (Rust; the crate is in `axum/`). Fix the
bug below in the axum source. Do not change the tests.

## Report

> **Axum 0.6.15 seems to break ServeDir completely**
>
> Used to work with axum `v0.6.12`, but since I just updated to `v0.6.15` the
> `ServeDir` service does not work anymore. With this kind of code:
>
> ```rust
> let routes_static =
>     Router::new().nest_service("/", get_service(ServeDir::new("./")));
>
> let routes_all = Router::new().fallback_service(routes_static);
>
> let addr = SocketAddr::from(([127, 0, 0, 1], 8080));
> axum::Server::bind(&addr)
>     .serve(routes_all.into_make_service())
>     .await
>     .unwrap();
> ```
>
> When doing a `GET` to a known path, like `/src/main.rs`:
>
> ```
> thread 'tokio-runtime-worker' panicked at 'assertion failed: matches!(extensions.remove :: < MatchedPath > (), None)', /Users/jeremychone/.cargo/registry/src/github.com-1ecc6299db9ec823/axum-0.6.15/src/extract/matched_path.rs:146:9
> ```
>
> Expected result: as in `v0.6.12`.

(Upstream issue text, abridged; the full text is in the SWE-bench Multilingual
record `tokio-rs__axum-1934`.)

## Building and testing

The dependencies are fetched and the tests are compiled; cargo works offline.
The routing tests run with:

```
cargo test -p axum --lib -- routing::tests        # about 20 s after a change to axum/src
```

`cargo test -p axum --lib -- extract::matched_path` covers `MatchedPath`. The
whole axum test suite (`cargo test -p axum`, with integration tests and
doctests) takes several minutes and is not needed.
