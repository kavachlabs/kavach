export {
  createMiddleware,
  wrapHandler,
  type FastifyLike,
  type FetchAppLike,
  type HttpHandlerTarget,
  type RecordedHttpRequest,
  type RecordedHttpResponse,
  type WrapHandlerOptions,
  type WrappedHttpHandler,
} from "./server.js";

export { wrapFetch, type WrapFetchOptions } from "./fetch.js";
